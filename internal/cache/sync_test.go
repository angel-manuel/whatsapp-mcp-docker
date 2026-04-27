package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// fakeSyncWA is a scriptable SyncWAClient for the orchestrator tests.
// It also accepts an Ingestor pointer so FetchAppState can simulate the
// dispatcher fanning events into the ingestor (which is what the
// production path does via the EventHook).
type fakeSyncWA struct {
	mu sync.Mutex

	loggedIn       bool
	groups         []*types.GroupInfo
	groupsErr      error
	groupsBlock    chan struct{} // if set, GetJoinedGroups blocks on receive
	newsletters    []*types.NewsletterMetadata
	newslettersErr error
	appStateErr    error
	appStateCalls  int
	ingestor       *Ingestor
}

func (f *fakeSyncWA) IsLoggedIn() bool { return f.loggedIn }

func (f *fakeSyncWA) GetJoinedGroups(_ context.Context) ([]*types.GroupInfo, error) {
	if f.groupsBlock != nil {
		<-f.groupsBlock
	}
	if f.groupsErr != nil {
		return nil, f.groupsErr
	}
	return f.groups, nil
}

func (f *fakeSyncWA) GetSubscribedNewsletters(_ context.Context) ([]*types.NewsletterMetadata, error) {
	if f.newslettersErr != nil {
		return nil, f.newslettersErr
	}
	return f.newsletters, nil
}

func (f *fakeSyncWA) FetchAppState(_ context.Context, _ appstate.WAPatchName, _, _ bool) error {
	f.mu.Lock()
	f.appStateCalls++
	f.mu.Unlock()
	if f.appStateErr != nil {
		return f.appStateErr
	}
	if f.ingestor != nil {
		// Simulate one MarkChatAsRead + one Pin event landing in the
		// ingestor per FetchAppState call. The orchestrator counts the
		// delta in atomic counters, so we must fire through HandleEvent.
		jid, _ := types.ParseJID("1234567890@s.whatsapp.net")
		readFalse := false
		f.ingestor.HandleEvent(&events.MarkChatAsRead{
			JID:    jid,
			Action: &waSyncAction.MarkChatAsReadAction{Read: &readFalse},
		})
		pinned := true
		gjid, _ := types.ParseJID("120363999000000999@g.us")
		f.ingestor.HandleEvent(&events.Pin{
			JID:    gjid,
			Action: &waSyncAction.PinAction{Pinned: &pinned},
		})
	}
	return nil
}

func TestSyncOrchestrator_RunsAllStages(t *testing.T) {
	store := newTestStore(t)
	ingestor := NewIngestor(store, nil)
	orch := NewSyncOrchestrator(store, ingestor, nil)

	groupJID, _ := types.ParseJID("120363100000000001@g.us")
	commJID, _ := types.ParseJID("120363100000000002@g.us")
	nlJID, _ := types.ParseJID("120363999000000099@newsletter")

	wa := &fakeSyncWA{
		loggedIn: true,
		groups: []*types.GroupInfo{
			{JID: groupJID, GroupName: types.GroupName{Name: "Hiking"}},
			{JID: commJID, GroupName: types.GroupName{Name: "Town"}, GroupParent: types.GroupParent{IsParent: true}},
		},
		newsletters: []*types.NewsletterMetadata{
			{ID: nlJID, ThreadMeta: types.NewsletterThreadMetadata{Name: types.NewsletterText{Text: "Brief"}}},
		},
		ingestor: ingestor,
	}

	report, started, err := orch.Start(wa)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !started {
		t.Fatal("expected started=true on first call")
	}
	if report.SyncID == "" || report.Status != SyncStatusRunning {
		t.Fatalf("initial report: %+v", report)
	}

	// Wait until the goroutine flips status to done. Bound the wait so
	// a stuck stage doesn't hang the test.
	deadline := time.After(3 * time.Second)
	for {
		snap := orch.Snapshot()
		if snap.Status == SyncStatusDone || snap.Status == SyncStatusFailed {
			report = snap
			break
		}
		select {
		case <-deadline:
			t.Fatalf("sync did not terminate: %+v", snap)
		case <-time.After(20 * time.Millisecond):
		}
	}

	if report.Status != SyncStatusDone {
		t.Fatalf("status=%s error=%q stages=%+v", report.Status, report.Error, report.Stages)
	}
	if len(report.Stages) != 3 {
		t.Fatalf("stages len = %d, want 3", len(report.Stages))
	}
	for _, s := range report.Stages {
		if s.Status != StageStatusDone {
			t.Errorf("stage %s status=%s err=%q", s.Name, s.Status, s.Error)
		}
		if s.DurationMs < 0 {
			t.Errorf("stage %s duration negative: %d", s.Name, s.DurationMs)
		}
	}
	if report.Stages[0].Items != 2 {
		t.Errorf("groups items = %d, want 2", report.Stages[0].Items)
	}
	if report.Stages[1].Items != 1 {
		t.Errorf("newsletters items = %d, want 1", report.Stages[1].Items)
	}
	// app_state fires 2 events per FetchAppState call * 4 patches = 8.
	if report.Stages[2].Items != 8 {
		t.Errorf("app_state items = %d, want 8", report.Stages[2].Items)
	}

	// Verify rows actually landed in the cache.
	for _, jid := range []string{groupJID.String(), commJID.String(), nlJID.String()} {
		var ct string
		err := store.DB().QueryRowContext(context.Background(),
			`SELECT chat_type FROM chats WHERE jid = ?`, jid).Scan(&ct)
		if err != nil {
			t.Errorf("chat %s missing: %v", jid, err)
		}
	}
}

func TestSyncOrchestrator_SecondCallReturnsInProgress(t *testing.T) {
	store := newTestStore(t)
	ingestor := NewIngestor(store, nil)
	orch := NewSyncOrchestrator(store, ingestor, nil)

	block := make(chan struct{})
	wa := &fakeSyncWA{loggedIn: true, groupsBlock: block, ingestor: ingestor}

	first, started, _ := orch.Start(wa)
	if !started || first.Status != SyncStatusRunning {
		t.Fatalf("first: started=%v status=%s", started, first.Status)
	}

	second, secondStarted, _ := orch.Start(wa)
	if secondStarted {
		t.Fatal("second call should report started=false while running")
	}
	if second.SyncID != first.SyncID {
		t.Errorf("second sync_id=%q, want %q (in-progress)", second.SyncID, first.SyncID)
	}
	if second.Status != SyncStatusRunning {
		t.Errorf("second status=%s, want running", second.Status)
	}

	// Release the blocked GetJoinedGroups so the goroutine can finish.
	close(block)

	deadline := time.After(2 * time.Second)
	for {
		if orch.Snapshot().Status != SyncStatusRunning {
			break
		}
		select {
		case <-deadline:
			t.Fatal("sync did not finish after unblock")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestSyncOrchestrator_StageFailureContinues(t *testing.T) {
	store := newTestStore(t)
	ingestor := NewIngestor(store, nil)
	orch := NewSyncOrchestrator(store, ingestor, nil)

	nlJID, _ := types.ParseJID("120363999000000099@newsletter")
	wa := &fakeSyncWA{
		loggedIn:  true,
		groupsErr: errors.New("boom"),
		newsletters: []*types.NewsletterMetadata{
			{ID: nlJID, ThreadMeta: types.NewsletterThreadMetadata{Name: types.NewsletterText{Text: "Brief"}}},
		},
		ingestor: ingestor,
	}

	_, _, _ = orch.Start(wa)

	deadline := time.After(3 * time.Second)
	for {
		snap := orch.Snapshot()
		if snap.Status == SyncStatusDone || snap.Status == SyncStatusFailed {
			if snap.Status != SyncStatusFailed {
				t.Fatalf("expected failed, got %s", snap.Status)
			}
			if snap.Error == "" || !contains(snap.Error, "groups") {
				t.Errorf("error %q should mention 'groups'", snap.Error)
			}
			if snap.Stages[0].Status != StageStatusFailed {
				t.Errorf("groups stage status = %s", snap.Stages[0].Status)
			}
			if snap.Stages[1].Status != StageStatusDone {
				t.Errorf("newsletters stage status = %s (should still run after failure)", snap.Stages[1].Status)
			}
			if snap.Stages[2].Status != StageStatusDone {
				t.Errorf("app_state stage status = %s", snap.Stages[2].Status)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("sync did not terminate: %+v", snap)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// Compile-time guard: keep the proto import used so future refactors
// that drop fakeSyncWA don't drag a dangling import comment with them.
var _ = proto.String

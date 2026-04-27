package cache

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
)

// SyncStatus is the top-level state of a sync run.
type SyncStatus string

// StageStatus is the per-stage state inside a sync run.
type StageStatus string

const (
	SyncStatusIdle    SyncStatus = "idle"
	SyncStatusRunning SyncStatus = "running"
	SyncStatusDone    SyncStatus = "done"
	SyncStatusFailed  SyncStatus = "failed"

	StageStatusPending StageStatus = "pending"
	StageStatusRunning StageStatus = "running"
	StageStatusDone    StageStatus = "done"
	StageStatusFailed  StageStatus = "failed"
)

const (
	stageGroups      = "groups"
	stageNewsletters = "newsletters"
	stageAppState    = "app_state"
)

// Stage is a single phase of a sync run.
type Stage struct {
	Name       string      `json:"name"`
	Status     StageStatus `json:"status"`
	Items      int         `json:"items"`
	StartedAt  time.Time   `json:"started_at,omitempty"`
	FinishedAt time.Time   `json:"finished_at,omitempty"`
	DurationMs int64       `json:"duration_ms"`
	Error      string      `json:"error,omitempty"`
}

// SyncReport is the snapshot returned to callers (cache_sync tool +
// cache_sync_status tool). Snapshots are deep copies so concurrent updates
// don't race with consumers.
type SyncReport struct {
	SyncID     string     `json:"sync_id"`
	Status     SyncStatus `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt time.Time  `json:"finished_at,omitempty"`
	Stages     []Stage    `json:"stages"`
	Error      string     `json:"error,omitempty"`
}

// SyncWAClient is the narrow whatsmeow surface the orchestrator needs.
// Declared here (not in internal/tools) so cache stays free of a back-edge
// dep on tools. *wa.Client satisfies it structurally.
type SyncWAClient interface {
	IsLoggedIn() bool
	GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error)
	GetSubscribedNewsletters(ctx context.Context) ([]*types.NewsletterMetadata, error)
	FetchAppState(ctx context.Context, name appstate.WAPatchName, fullSync, onlyIfNotSynced bool) error
}

// SyncOrchestrator drives a one-at-a-time reconciliation of the local cache
// against authoritative whatsmeow endpoints. Modeled after pairSession in
// internal/wa: a single mutex protects the running flag and the in-progress
// SyncReport; consumers read snapshots.
type SyncOrchestrator struct {
	store    *Store
	ingestor *Ingestor
	logger   *slog.Logger

	mu      sync.Mutex
	running bool
	current SyncReport
}

// NewSyncOrchestrator constructs an orchestrator. A nil logger is replaced
// with a discarding one so callers that don't care about diagnostics can
// pass nil (matches Ingestor's policy).
func NewSyncOrchestrator(store *Store, ingestor *Ingestor, logger *slog.Logger) *SyncOrchestrator {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &SyncOrchestrator{store: store, ingestor: ingestor, logger: logger}
}

// Start kicks off a new sync. If a sync is already running, the
// in-progress report is returned with started=false (idempotent).
// Otherwise a fresh report is initialised, the goroutine spawned, and
// (initialReport, true) returned. The goroutine uses context.Background()
// so a fast HTTP request timeout cannot abort mid-sync.
func (o *SyncOrchestrator) Start(wa SyncWAClient) (SyncReport, bool, error) {
	o.mu.Lock()
	if o.running {
		report := cloneReport(o.current)
		o.mu.Unlock()
		return report, false, nil
	}
	now := time.Now().UTC()
	o.current = SyncReport{
		SyncID:    strconv.FormatInt(now.UnixMilli(), 10),
		Status:    SyncStatusRunning,
		StartedAt: now,
		Stages: []Stage{
			{Name: stageGroups, Status: StageStatusPending},
			{Name: stageNewsletters, Status: StageStatusPending},
			{Name: stageAppState, Status: StageStatusPending},
		},
	}
	o.running = true
	report := cloneReport(o.current)
	o.mu.Unlock()

	go o.run(wa)
	return report, true, nil
}

// Snapshot returns a deep copy of the current sync report. When no sync
// has ever started the SyncID is empty and StartedAt is zero — callers
// (e.g. cache_sync_status) should treat the zero StartedAt as "absent".
func (o *SyncOrchestrator) Snapshot() SyncReport {
	o.mu.Lock()
	defer o.mu.Unlock()
	return cloneReport(o.current)
}

// run executes each stage worker in order. Per-stage failures are
// recorded but do NOT abort subsequent stages (continue-on-failure
// policy chosen by the user). After all stages: top-level Status is
// done if every stage is done, else failed with a summary error.
func (o *SyncOrchestrator) run(wa SyncWAClient) {
	ctx := context.Background()
	o.runStage(ctx, 0, func(s *Stage) error { return o.runGroups(ctx, wa, s) })
	o.runStage(ctx, 1, func(s *Stage) error { return o.runNewsletters(ctx, wa, s) })
	o.runStage(ctx, 2, func(s *Stage) error { return o.runAppState(ctx, wa, s) })

	o.mu.Lock()
	defer o.mu.Unlock()
	o.current.FinishedAt = time.Now().UTC()
	failed := []string{}
	for _, st := range o.current.Stages {
		if st.Status == StageStatusFailed {
			failed = append(failed, st.Name)
		}
	}
	if len(failed) == 0 {
		o.current.Status = SyncStatusDone
	} else {
		o.current.Status = SyncStatusFailed
		o.current.Error = "stages failed: " + strings.Join(failed, ", ")
	}
	o.running = false
	o.logger.Info("sync done",
		slog.String("event_type", "cache.sync.done"),
		slog.String("sync_id", o.current.SyncID),
		slog.String("status", string(o.current.Status)),
		slog.Int("failed_stages", len(failed)),
	)
}

// runStage manages the lifecycle of a single stage: marks it running,
// invokes the worker, captures error/duration, marks it done or failed.
// The worker mutates the stage's Items via setStageItems; everything else
// runStage handles.
func (o *SyncOrchestrator) runStage(_ context.Context, idx int, worker func(*Stage) error) {
	o.mu.Lock()
	o.current.Stages[idx].Status = StageStatusRunning
	o.current.Stages[idx].StartedAt = time.Now().UTC()
	stageName := o.current.Stages[idx].Name
	o.mu.Unlock()

	o.logger.Info("sync stage starting",
		slog.String("event_type", "cache.sync.stage.start"),
		slog.String("stage", stageName),
	)

	// Worker writes to its own copy of the Stage; runStage reconciles the
	// final Items count under the lock at the end.
	var workerStage Stage
	o.mu.Lock()
	workerStage = o.current.Stages[idx]
	o.mu.Unlock()

	err := worker(&workerStage)

	o.mu.Lock()
	st := &o.current.Stages[idx]
	st.Items = workerStage.Items
	st.FinishedAt = time.Now().UTC()
	st.DurationMs = st.FinishedAt.Sub(st.StartedAt).Milliseconds()
	if err != nil {
		st.Status = StageStatusFailed
		st.Error = err.Error()
		o.logger.Warn("sync stage failed",
			slog.String("event_type", "cache.sync.stage.fail"),
			slog.String("stage", stageName),
			slog.String("err", err.Error()),
		)
	} else {
		st.Status = StageStatusDone
		o.logger.Info("sync stage done",
			slog.String("event_type", "cache.sync.stage.done"),
			slog.String("stage", stageName),
			slog.Int("items", st.Items),
			slog.Int64("duration_ms", st.DurationMs),
		)
	}
	o.mu.Unlock()
}

// runGroups reconciles the chats table against the authoritative joined
// groups list. Each group is upserted; chat_type is inferred (community
// for parent groups, group otherwise).
func (o *SyncOrchestrator) runGroups(ctx context.Context, wa SyncWAClient, stage *Stage) error {
	groups, err := wa.GetJoinedGroups(ctx)
	if err != nil {
		return fmt.Errorf("get joined groups: %w", err)
	}
	for _, g := range groups {
		if g == nil || g.JID.User == "" {
			continue
		}
		chatType := ChatTypeGroup
		if g.IsParent {
			chatType = ChatTypeCommunity
		}
		chat := Chat{
			JID:           g.JID.String(),
			IsGroup:       true,
			Type:          chatType,
			Name:          g.Name,
			LastMessageTS: g.GroupCreated,
		}
		if err := o.store.UpsertChat(ctx, chat); err != nil {
			o.logger.Warn("sync groups upsert",
				slog.String("jid", g.JID.String()), slog.String("err", err.Error()))
			continue
		}
		stage.Items++
	}
	return nil
}

// runNewsletters reconciles the chats table against the authoritative
// subscribed newsletters list. JIDs landing here come from the
// @newsletter family; chat_type is forced to newsletter even when the
// JID suffix is unusual.
func (o *SyncOrchestrator) runNewsletters(ctx context.Context, wa SyncWAClient, stage *Stage) error {
	newsletters, err := wa.GetSubscribedNewsletters(ctx)
	if err != nil {
		return fmt.Errorf("get subscribed newsletters: %w", err)
	}
	for _, n := range newsletters {
		if n == nil || n.ID.User == "" {
			continue
		}
		chat := Chat{
			JID:  n.ID.String(),
			Type: ChatTypeNewsletter,
			Name: n.ThreadMeta.Name.Text,
		}
		if t := n.ThreadMeta.CreationTime.Time; !t.IsZero() {
			chat.LastMessageTS = t
		}
		if err := o.store.UpsertChat(ctx, chat); err != nil {
			o.logger.Warn("sync newsletters upsert",
				slog.String("jid", n.ID.String()), slog.String("err", err.Error()))
			continue
		}
		stage.Items++
	}
	return nil
}

// runAppState pulls each cache-relevant app-state patch and counts the
// resulting Ingestor events as the stage's items. Skips the
// CriticalBlock patch (blocklist is not cached). Per-patch failures are
// collected; the stage fails if every patch failed, otherwise items
// reflects the partial progress and the stage is reported as done.
func (o *SyncOrchestrator) runAppState(ctx context.Context, wa SyncWAClient, stage *Stage) error {
	if o.ingestor == nil {
		return fmt.Errorf("ingestor not wired; cannot count app_state events")
	}
	patches := []appstate.WAPatchName{
		appstate.WAPatchRegular,
		appstate.WAPatchRegularLow,
		appstate.WAPatchRegularHigh,
		appstate.WAPatchCriticalUnblockLow,
	}
	var failed []string
	for _, name := range patches {
		mr0, p0, a0, c0 := o.ingestor.AppStateCounts()
		if err := wa.FetchAppState(ctx, name, false, false); err != nil {
			o.logger.Warn("sync app_state patch",
				slog.String("patch", string(name)), slog.String("err", err.Error()))
			failed = append(failed, string(name)+": "+err.Error())
			continue
		}
		mr1, p1, a1, c1 := o.ingestor.AppStateCounts()
		stage.Items += int((mr1 - mr0) + (p1 - p0) + (a1 - a0) + (c1 - c0))
	}
	if len(failed) == len(patches) {
		return fmt.Errorf("all app_state patches failed: %s", strings.Join(failed, "; "))
	}
	return nil
}

// cloneReport returns a deep copy of r so callers see a stable snapshot
// even while o.current is being mutated by the run goroutine.
func cloneReport(r SyncReport) SyncReport {
	c := r
	if r.Stages != nil {
		c.Stages = make([]Stage, len(r.Stages))
		copy(c.Stages, r.Stages)
	}
	return c
}

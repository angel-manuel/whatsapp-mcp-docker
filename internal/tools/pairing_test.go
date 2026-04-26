package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// fakeWA is a scriptable WAClient focused on the pairing surface. The
// non-pairing methods return zero values; the cache/messaging tests
// elsewhere use a different mock.
type fakeWA struct {
	startErr error

	pairPhoneFn func(ctx context.Context, phone string) (string, error)
	statusOut   wa.Status

	mu       sync.Mutex
	cond     *sync.Cond
	latest   wa.PairEvent
	hasEvent bool
	seq      uint64
	closed   bool             // no more events; PairWaitNext returns immediately
	active   bool             // a flow has been started
	outCh    chan wa.PairEvent // optional: returned from StartPairing for drainer tests
}

func newFakeWA() *fakeWA {
	f := &fakeWA{}
	f.cond = sync.NewCond(&f.mu)
	return f
}

func (f *fakeWA) GroupInfo(context.Context, types.JID) (*types.GroupInfo, error) {
	return nil, nil
}
func (f *fakeWA) UserInfo(context.Context, []types.JID) (map[types.JID]types.UserInfo, error) {
	return nil, nil
}
func (f *fakeWA) IsOnWhatsApp(context.Context, []string) ([]types.IsOnWhatsAppResponse, error) {
	return nil, nil
}
func (f *fakeWA) ProfilePictureURL(context.Context, types.JID) (string, error) { return "", nil }
func (f *fakeWA) SendMessage(context.Context, types.JID, *waE2E.Message) (whatsmeow.SendResponse, error) {
	return whatsmeow.SendResponse{}, nil
}
func (f *fakeWA) OwnJID() types.JID { return types.JID{} }

func (f *fakeWA) StartPairing(context.Context) (<-chan wa.PairEvent, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.mu.Lock()
	f.active = true
	ch := f.outCh
	f.mu.Unlock()
	// The real wa.Client returns a channel that the admin SSE consumer
	// drains. Most tests ignore that channel (the tools layer uses
	// PairWaitNext / PairLatest), but TestPairingStart_DrainsChannel
	// supplies one to exercise the no-op drainer.
	return ch, nil
}

func (f *fakeWA) PairPhone(ctx context.Context, phone string) (string, error) {
	if f.pairPhoneFn != nil {
		return f.pairPhoneFn(ctx, phone)
	}
	return "", nil
}

func (f *fakeWA) PairLatest() (wa.PairEvent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.active {
		return wa.PairEvent{}, false
	}
	if !f.hasEvent {
		return wa.PairEvent{}, true
	}
	return f.latest, true
}

func (f *fakeWA) PairWaitNext(ctx context.Context) (wa.PairEvent, bool, error) {
	f.mu.Lock()
	if !f.active {
		f.mu.Unlock()
		return wa.PairEvent{}, false, nil
	}
	startSeq := f.seq
	if f.closed {
		evt := f.latest
		f.mu.Unlock()
		return evt, true, nil
	}
	f.mu.Unlock()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			f.cond.Broadcast()
		case <-done:
		}
	}()

	f.mu.Lock()
	for f.seq == startSeq && !f.closed && ctx.Err() == nil {
		f.cond.Wait()
	}
	evt := f.latest
	f.mu.Unlock()
	return evt, true, ctx.Err()
}

func (f *fakeWA) Status() wa.Status { return f.statusOut }

// publish simulates a wa.Client pair-goroutine emitting evt. Updates
// the observation cond, and — if outCh is wired — sends on it too,
// blocking on a full buffer just like the real producer does (so the
// drainer test can detect an absent consumer).
func (f *fakeWA) publish(evt wa.PairEvent) {
	f.mu.Lock()
	f.latest = evt
	f.hasEvent = true
	f.seq++
	if evt.IsTerminal() {
		f.closed = true
	}
	ch := f.outCh
	f.cond.Broadcast()
	f.mu.Unlock()
	if ch != nil {
		ch <- evt
	}
}

func callTool(t *testing.T, h func(ctx context.Context, args json.RawMessage) (any, error), in any) any {
	t.Helper()
	var raw json.RawMessage
	if in != nil {
		var err error
		raw, err = json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
	}
	out, err := h(context.Background(), raw)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return out
}

func TestPairingStart_QRMode(t *testing.T) {
	f := newFakeWA()
	// First event arrives shortly after StartPairing.
	go func() {
		time.Sleep(10 * time.Millisecond)
		f.publish(wa.PairEvent{Type: wa.PairEventCode, Code: "qr-payload-1", TimeoutMs: 20000})
	}()

	out := callTool(t, pairingStart(Deps{Cache: nil, WA: f}), nil)
	res, ok := out.(PairingStartResult)
	if !ok {
		t.Fatalf("expected PairingStartResult, got %T: %+v", out, out)
	}
	if res.Status != pairStatusAwaitingScan {
		t.Errorf("status=%q, want %q", res.Status, pairStatusAwaitingScan)
	}
	if res.Code != "qr-payload-1" {
		t.Errorf("code=%q, want qr-payload-1", res.Code)
	}
	if res.TimeoutMs != 20000 {
		t.Errorf("timeout_ms=%d, want 20000", res.TimeoutMs)
	}
	if res.LinkingCode != "" {
		t.Errorf("linking_code=%q, want empty", res.LinkingCode)
	}
}

func TestPairingStart_AlreadyPaired(t *testing.T) {
	f := newFakeWA()
	f.startErr = wa.ErrAlreadyPaired
	out := callTool(t, pairingStart(Deps{WA: f}), nil)
	assertErrorCode(t, out, "already_paired")
}

func TestPairingStart_PairInProgress(t *testing.T) {
	f := newFakeWA()
	f.startErr = wa.ErrPairInProgress
	out := callTool(t, pairingStart(Deps{WA: f}), nil)
	assertErrorCode(t, out, "pair_in_progress")
}

func TestPairingStart_PhoneMode(t *testing.T) {
	f := newFakeWA()
	go func() {
		time.Sleep(10 * time.Millisecond)
		f.publish(wa.PairEvent{Type: wa.PairEventCode, Code: "qr-payload", TimeoutMs: 20000})
	}()
	f.pairPhoneFn = func(_ context.Context, phone string) (string, error) {
		if phone != "34600111222" {
			t.Errorf("phone=%q, want 34600111222", phone)
		}
		return "ABCD-EFGH", nil
	}

	out := callTool(t, pairingStart(Deps{WA: f}), map[string]string{"phone": "34600111222"})
	res, ok := out.(PairingStartResult)
	if !ok {
		t.Fatalf("expected PairingStartResult, got %T: %+v", out, out)
	}
	if res.Status != pairStatusAwaitingPhoneLink {
		t.Errorf("status=%q, want %q", res.Status, pairStatusAwaitingPhoneLink)
	}
	if res.LinkingCode != "ABCD-EFGH" {
		t.Errorf("linking_code=%q, want ABCD-EFGH", res.LinkingCode)
	}
	if res.Code != "qr-payload" {
		t.Errorf("code=%q, want qr-payload", res.Code)
	}
}

func TestPairingStart_PhoneError(t *testing.T) {
	f := newFakeWA()
	go func() {
		time.Sleep(10 * time.Millisecond)
		f.publish(wa.PairEvent{Type: wa.PairEventCode, Code: "qr-payload"})
	}()
	f.pairPhoneFn = func(context.Context, string) (string, error) {
		return "", errors.New("boom")
	}
	out := callTool(t, pairingStart(Deps{WA: f}), map[string]string{"phone": "34600111222"})
	assertErrorCode(t, out, "internal")
}

func TestPairingComplete_NotPairing(t *testing.T) {
	f := newFakeWA()
	out := callTool(t, pairingComplete(Deps{WA: f}), map[string]int{"wait_seconds": 0})
	res, ok := out.(PairingCompleteResult)
	if !ok {
		t.Fatalf("expected PairingCompleteResult, got %T: %+v", out, out)
	}
	if res.Status != pairStatusNotPairing {
		t.Errorf("status=%q, want %q", res.Status, pairStatusNotPairing)
	}
}

func TestPairingComplete_WaitZeroReturnsLatest(t *testing.T) {
	f := newFakeWA()
	if _, err := f.StartPairing(context.Background()); err != nil {
		t.Fatalf("StartPairing: %v", err)
	}
	f.publish(wa.PairEvent{Type: wa.PairEventCode, Code: "rotating-1", TimeoutMs: 15000})

	out := callTool(t, pairingComplete(Deps{WA: f}), map[string]int{"wait_seconds": 0})
	res, ok := out.(PairingCompleteResult)
	if !ok {
		t.Fatalf("expected PairingCompleteResult, got %T", out)
	}
	if res.Status != pairStatusPending {
		t.Errorf("status=%q, want %q", res.Status, pairStatusPending)
	}
	if res.Code != "rotating-1" {
		t.Errorf("code=%q, want rotating-1", res.Code)
	}
}

func TestPairingComplete_WaitForTerminal(t *testing.T) {
	f := newFakeWA()
	if _, err := f.StartPairing(context.Background()); err != nil {
		t.Fatalf("StartPairing: %v", err)
	}
	f.publish(wa.PairEvent{Type: wa.PairEventCode, Code: "rotating-1"})
	f.statusOut = wa.Status{JID: "12345@s.whatsapp.net", Pushname: "Tester"}

	go func() {
		time.Sleep(20 * time.Millisecond)
		f.publish(wa.PairEvent{Type: wa.PairEventSuccess})
	}()

	out := callTool(t, pairingComplete(Deps{WA: f}), map[string]int{"wait_seconds": 2})
	res, ok := out.(PairingCompleteResult)
	if !ok {
		t.Fatalf("expected PairingCompleteResult, got %T", out)
	}
	if res.Status != pairStatusSuccess {
		t.Errorf("status=%q, want %q", res.Status, pairStatusSuccess)
	}
	if res.JID != "12345@s.whatsapp.net" || res.Pushname != "Tester" {
		t.Errorf("status fields not propagated: jid=%q pushname=%q", res.JID, res.Pushname)
	}

	// Subsequent call: session is closed (publish set closed=true on terminal),
	// so PairWaitNext returns immediately with the same terminal event.
	// The handler treats that as terminal again — that's fine for repeat
	// calls; agents only need one successful terminal.
}

func TestPairingComplete_RotationOnlyReturnsPending(t *testing.T) {
	f := newFakeWA()
	if _, err := f.StartPairing(context.Background()); err != nil {
		t.Fatalf("StartPairing: %v", err)
	}
	f.publish(wa.PairEvent{Type: wa.PairEventCode, Code: "rot-1", TimeoutMs: 15000})

	// Schedule a rotation (still non-terminal) before the wait elapses.
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.publish(wa.PairEvent{Type: wa.PairEventCode, Code: "rot-2", TimeoutMs: 15000})
	}()

	out := callTool(t, pairingComplete(Deps{WA: f}), map[string]int{"wait_seconds": 1})
	res, ok := out.(PairingCompleteResult)
	if !ok {
		t.Fatalf("expected PairingCompleteResult, got %T", out)
	}
	if res.Status != pairStatusPending {
		t.Errorf("status=%q, want %q", res.Status, pairStatusPending)
	}
	if res.Code != "rot-2" {
		t.Errorf("code=%q, want rot-2 (latest rotation)", res.Code)
	}
}

// TestPairingStart_DrainsChannel pins the contract that pairing_start
// drains the (admin SSE) channel returned by wa.Client.StartPairing.
// Without that drainer, the real wa producer wedges at buffer-full
// after 8 rotations and stalls every subsequent pair event — including
// terminal — so a regression here would silently break long pair flows.
func TestPairingStart_DrainsChannel(t *testing.T) {
	f := newFakeWA()
	// Cap the channel so a missing drainer would block at the second
	// publish; if the test passes with `bursts` > capacity, the
	// drainer is genuinely consuming.
	f.outCh = make(chan wa.PairEvent, 2)

	// Emit the first event eagerly so pairing_start can return.
	go func() {
		time.Sleep(5 * time.Millisecond)
		f.publish(wa.PairEvent{Type: wa.PairEventCode, Code: "rot-0", TimeoutMs: 15000})
	}()

	out := callTool(t, pairingStart(Deps{WA: f}), nil)
	if _, ok := out.(PairingStartResult); !ok {
		t.Fatalf("expected PairingStartResult, got %T", out)
	}

	// Now publish many more events. Without the drainer the second
	// publish would block on the channel send and deadlock the test.
	bursts := 10
	doneCh := make(chan struct{})
	go func() {
		for i := 1; i <= bursts; i++ {
			f.publish(wa.PairEvent{Type: wa.PairEventCode, Code: "rot-N"})
		}
		close(doneCh)
	}()

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("publishes blocked — drainer is missing or broken")
	}
}

func TestPairingComplete_InvalidWaitSeconds(t *testing.T) {
	f := newFakeWA()
	out := callTool(t, pairingComplete(Deps{WA: f}), map[string]int{"wait_seconds": 999})
	assertErrorCode(t, out, "invalid_argument")
}

// assertErrorCode walks the *mcpgo.CallToolResult shape used by
// mcp.ErrorResult and checks its `code` field. Since this test file
// already imports json, we reflect via JSON marshalling rather than
// pulling in mcp-go just for the type assertion.
func assertErrorCode(t *testing.T, out any, want string) {
	t.Helper()
	type structured struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	type resultLike struct {
		StructuredContent structured `json:"structuredContent"`
		IsError           bool       `json:"isError"`
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got resultLike
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v (raw=%s)", err, raw)
	}
	if !got.IsError {
		t.Fatalf("expected isError=true, got payload=%s", raw)
	}
	if got.StructuredContent.Code != want {
		t.Errorf("code=%q, want %q (full=%s)", got.StructuredContent.Code, want, raw)
	}
}

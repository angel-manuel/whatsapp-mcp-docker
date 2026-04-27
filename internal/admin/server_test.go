package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// fakeWA is a stub WAService for deterministic testing of the admin HTTP
// surface without touching whatsmeow's network code.
type fakeWA struct {
	mu sync.Mutex

	status wa.Status

	subCh chan wa.Event

	startPairFn func(ctx context.Context) (<-chan wa.PairEvent, error)
	pairPhoneFn func(ctx context.Context, phone string) (string, error)
	unpairFn    func(ctx context.Context) error
}

func (f *fakeWA) Status() wa.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeWA) Subscribe() (<-chan wa.Event, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subCh == nil {
		f.subCh = make(chan wa.Event, 16)
	}
	ch := f.subCh
	return ch, func() {}
}

func (f *fakeWA) StartPairing(ctx context.Context, _ string) (<-chan wa.PairEvent, error) {
	if f.startPairFn != nil {
		return f.startPairFn(ctx)
	}
	return nil, errors.New("StartPairing not implemented")
}

func (f *fakeWA) PairPhone(ctx context.Context, phone string) (string, error) {
	if f.pairPhoneFn != nil {
		return f.pairPhoneFn(ctx, phone)
	}
	return "", errors.New("PairPhone not implemented")
}

func (f *fakeWA) Unpair(ctx context.Context) error {
	if f.unpairFn != nil {
		return f.unpairFn(ctx)
	}
	return nil
}

func newTestServer(t *testing.T, cfg Config, svc WAService) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cfg, log, svc)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func doReq(t *testing.T, ts *httptest.Server, method, path, token, body string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, r)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func TestHealth_NoAuth(t *testing.T) {
	ts := newTestServer(t, Config{AuthToken: "secret", RequireAuth: true}, &fakeWA{})

	resp := doReq(t, ts, http.MethodGet, "/admin/health", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
}

func TestAuth_RejectsMissingToken(t *testing.T) {
	ts := newTestServer(t, Config{AuthToken: "secret", RequireAuth: true}, &fakeWA{})

	resp := doReq(t, ts, http.MethodGet, "/admin/status", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if h := resp.Header.Get("WWW-Authenticate"); !strings.Contains(h, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want Bearer challenge", h)
	}
}

func TestAuth_RejectsWrongToken(t *testing.T) {
	ts := newTestServer(t, Config{AuthToken: "secret", RequireAuth: true}, &fakeWA{})

	resp := doReq(t, ts, http.MethodGet, "/admin/status", "nope", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_AcceptsValidToken(t *testing.T) {
	ts := newTestServer(t, Config{AuthToken: "secret", RequireAuth: true}, &fakeWA{})

	resp := doReq(t, ts, http.MethodGet, "/admin/status", "secret", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestAuth_StdioNoTokenPasses(t *testing.T) {
	// Transport=stdio, no AuthToken, RequireAuth=false -> unauth requests pass.
	ts := newTestServer(t, Config{AuthToken: "", RequireAuth: false}, &fakeWA{})

	resp := doReq(t, ts, http.MethodGet, "/admin/status", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestAuth_RequireAuthWithoutTokenRejectsAll(t *testing.T) {
	// Defense-in-depth: should never happen in production (config validation
	// rejects it) but the middleware must still refuse rather than serve open.
	ts := newTestServer(t, Config{AuthToken: "", RequireAuth: true}, &fakeWA{})

	resp := doReq(t, ts, http.MethodGet, "/admin/status", "anything", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestStatus_ShapeAndUptime(t *testing.T) {
	evt := &wa.Event{Type: wa.EventConnected, Timestamp: time.Unix(1700000000, 0).UTC()}
	svc := &fakeWA{status: wa.Status{
		State:     wa.StateConnected,
		Connected: true,
		LoggedIn:  true,
		JID:       "1234567890@s.whatsapp.net",
		Pushname:  "Alice",
		LastEvent: evt,
	}}
	ts := newTestServer(t, Config{}, svc)

	resp := doReq(t, ts, http.MethodGet, "/admin/status", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.State != string(wa.StateConnected) {
		t.Errorf("State = %q", body.State)
	}
	if !body.Connected || !body.LoggedIn {
		t.Errorf("Connected/LoggedIn = %v/%v", body.Connected, body.LoggedIn)
	}
	if body.JID != "1234567890@s.whatsapp.net" || body.Pushname != "Alice" {
		t.Errorf("JID/Pushname = %q/%q", body.JID, body.Pushname)
	}
	if body.LastEvent == nil || body.LastEvent.Type != wa.EventConnected {
		t.Errorf("LastEvent = %+v", body.LastEvent)
	}
	if body.UptimeS < 0 {
		t.Errorf("UptimeS = %d, want >= 0", body.UptimeS)
	}
}

func TestReady_503WhenNotReady(t *testing.T) {
	svc := &fakeWA{status: wa.Status{State: wa.StateNotPaired}}
	ts := newTestServer(t, Config{}, svc)

	resp := doReq(t, ts, http.MethodGet, "/admin/ready", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestReady_200WhenReady(t *testing.T) {
	svc := &fakeWA{status: wa.Status{
		State:     wa.StateConnected,
		Connected: true,
		LoggedIn:  true,
	}}
	ts := newTestServer(t, Config{}, svc)

	resp := doReq(t, ts, http.MethodGet, "/admin/ready", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestEvents_SSEPrimesAndForwards(t *testing.T) {
	svc := &fakeWA{status: wa.Status{State: wa.StateNotPaired}}
	ts := newTestServer(t, Config{}, svc)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/events", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	r := bufio.NewReader(resp.Body)
	// First frame is the primed status snapshot.
	gotEvent, gotData, err := readSSEFrame(r)
	if err != nil {
		t.Fatalf("readSSEFrame (primer): %v", err)
	}
	if gotEvent != "status" {
		t.Fatalf("primer event = %q, want status", gotEvent)
	}
	var primer statusResponse
	if err := json.Unmarshal(gotData, &primer); err != nil {
		t.Fatalf("primer json: %v (%s)", err, gotData)
	}
	if primer.State != string(wa.StateNotPaired) {
		t.Errorf("primer State = %q", primer.State)
	}

	// Push a lifecycle event through the subscription channel.
	go func() {
		time.Sleep(10 * time.Millisecond)
		svc.subCh <- wa.Event{Type: wa.EventConnected, Timestamp: time.Unix(1, 0).UTC()}
	}()

	gotEvent, gotData, err = readSSEFrame(r)
	if err != nil {
		t.Fatalf("readSSEFrame (connected): %v", err)
	}
	if gotEvent != string(wa.EventConnected) {
		t.Fatalf("event = %q, want %q", gotEvent, wa.EventConnected)
	}
	var evt wa.Event
	if err := json.Unmarshal(gotData, &evt); err != nil {
		t.Fatalf("evt json: %v (%s)", err, gotData)
	}
	if evt.Type != wa.EventConnected {
		t.Errorf("Type = %q", evt.Type)
	}
}

func TestPairStart_AlreadyPaired(t *testing.T) {
	svc := &fakeWA{
		startPairFn: func(ctx context.Context) (<-chan wa.PairEvent, error) {
			return nil, wa.ErrAlreadyPaired
		},
	}
	ts := newTestServer(t, Config{}, svc)

	resp := doReq(t, ts, http.MethodPost, "/admin/pair/start", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var body errorBody
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Code != "already_paired" {
		t.Errorf("code = %q, want already_paired", body.Code)
	}
}

func TestPairStart_StreamsEventsUntilTerminal(t *testing.T) {
	svc := &fakeWA{
		startPairFn: func(ctx context.Context) (<-chan wa.PairEvent, error) {
			ch := make(chan wa.PairEvent, 4)
			ch <- wa.PairEvent{Type: wa.PairEventCode, Code: "abc123", TimeoutMs: 20000}
			ch <- wa.PairEvent{Type: wa.PairEventSuccess}
			close(ch)
			return ch, nil
		},
	}
	ts := newTestServer(t, Config{}, svc)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/pair/start", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	r := bufio.NewReader(resp.Body)

	gotEvent, gotData, err := readSSEFrame(r)
	if err != nil {
		t.Fatalf("frame 1: %v", err)
	}
	if gotEvent != string(wa.PairEventCode) {
		t.Fatalf("frame 1 event = %q", gotEvent)
	}
	var code wa.PairEvent
	_ = json.Unmarshal(gotData, &code)
	if code.Code != "abc123" || code.TimeoutMs != 20000 {
		t.Errorf("code frame = %+v", code)
	}

	gotEvent, _, err = readSSEFrame(r)
	if err != nil {
		t.Fatalf("frame 2: %v", err)
	}
	if gotEvent != string(wa.PairEventSuccess) {
		t.Fatalf("frame 2 event = %q, want success", gotEvent)
	}

	// After terminal event the handler should return and the body EOF.
	if _, _, err := readSSEFrame(r); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF after terminal, got %v", err)
	}
}

func TestPairPhone_RequiresPhone(t *testing.T) {
	ts := newTestServer(t, Config{}, &fakeWA{})

	resp := doReq(t, ts, http.MethodPost, "/admin/pair/phone", "", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPairPhone_ReturnsLinkingCode(t *testing.T) {
	svc := &fakeWA{
		pairPhoneFn: func(ctx context.Context, phone string) (string, error) {
			if phone != "34600111222" {
				t.Errorf("phone = %q", phone)
			}
			return "ABCD-1234", nil
		},
	}
	ts := newTestServer(t, Config{}, svc)

	resp := doReq(t, ts, http.MethodPost, "/admin/pair/phone", "", `{"phone":"34600111222"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body pairPhoneResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.LinkingCode != "ABCD-1234" {
		t.Errorf("linking_code = %q", body.LinkingCode)
	}
}

func TestPairPhone_NotPairing(t *testing.T) {
	svc := &fakeWA{
		pairPhoneFn: func(ctx context.Context, phone string) (string, error) {
			return "", wa.ErrNotPairing
		},
	}
	ts := newTestServer(t, Config{}, svc)

	resp := doReq(t, ts, http.MethodPost, "/admin/pair/phone", "", `{"phone":"34600111222"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body errorBody
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Code != "not_pairing" {
		t.Errorf("code = %q", body.Code)
	}
}

func TestUnpair_CallsService(t *testing.T) {
	called := false
	svc := &fakeWA{
		unpairFn: func(ctx context.Context) error {
			called = true
			return nil
		},
	}
	ts := newTestServer(t, Config{}, svc)

	resp := doReq(t, ts, http.MethodPost, "/admin/unpair", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !called {
		t.Error("Unpair was not called")
	}
}

// TestUnpair_AgainstRealWAClient runs the admin unpair endpoint against a
// real wa.Client backed by a tmp DATA_DIR (in-memory-ish whatsmeow store).
// It asserts the session.db file is removed and the client transitions back
// to not_paired.
func TestUnpair_AgainstRealWAClient(t *testing.T) {
	dir := t.TempDir()
	c, err := wa.Open(context.Background(), wa.Config{DataDir: dir})
	if err != nil {
		t.Fatalf("wa.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Drop a sentinel file at session.db-journal (a path whatsmeow does not
	// maintain in WAL mode) to prove removal actually runs. A plain -wal
	// test would fail because sqlstore recreates its WAL on reopen.
	sentinel := filepath.Join(dir, "session.db-journal")
	if err := os.WriteFile(sentinel, []byte("x"), 0o600); err != nil {
		t.Fatalf("touch sentinel: %v", err)
	}

	ts := newTestServer(t, Config{}, c)

	resp := doReq(t, ts, http.MethodPost, "/admin/unpair", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Errorf("sentinel journal file still exists: err=%v", err)
	}
	// The session.db was recreated by the post-unpair openWhatsmeow; just
	// assert state via the admin /status endpoint.
	sresp := doReq(t, ts, http.MethodGet, "/admin/status", "", "")
	defer sresp.Body.Close()
	var body statusResponse
	_ = json.NewDecoder(sresp.Body).Decode(&body)
	if body.State != string(wa.StateNotPaired) {
		t.Errorf("state = %q, want not_paired", body.State)
	}
	if body.LoggedIn || body.Connected {
		t.Errorf("logged_in/connected = %v/%v, want false/false", body.LoggedIn, body.Connected)
	}
}

// readSSEFrame parses one "event: X\ndata: Y\n\n" frame off the stream.
// Returns the event name (empty if the frame had no event: line) and the raw
// data bytes. io.EOF means the stream closed cleanly between frames.
func readSSEFrame(r *bufio.Reader) (string, []byte, error) {
	var eventName string
	var data []byte
	sawAny := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if !sawAny && errors.Is(err, io.EOF) {
				return "", nil, io.EOF
			}
			return "", nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if !sawAny {
				continue
			}
			return eventName, data, nil
		}
		sawAny = true
		switch {
		case strings.HasPrefix(line, "event: "):
			eventName = line[len("event: "):]
		case strings.HasPrefix(line, "data: "):
			data = append(data, []byte(line[len("data: "):])...)
		}
	}
}

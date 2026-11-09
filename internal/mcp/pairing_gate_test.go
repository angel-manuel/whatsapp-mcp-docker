package mcp

import (
	"context"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// fixedState is a PairingState whose two predicates can disagree, which
// AlwaysPaired / NeverPaired (both PairingStateFunc, so both report the
// same value twice) structurally cannot express.
type fixedState struct {
	paired    bool
	connected bool
}

func (f fixedState) IsPaired() bool    { return f.paired }
func (f fixedState) IsConnected() bool { return f.connected }

// pairedOffline is the state that produced the original bug: device
// credentials on disk, socket dead.
var pairedOffline = fixedState{paired: true, connected: false}

// startedStdioClient returns an initialized stdio client against srv.
func startedStdioClient(t *testing.T, srv *Server) (callTool func(string) *mcpgo.CallToolResult) {
	t.Helper()

	client, cleanup := stdioClient(t, srv)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := client.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test-client", Version: "0"}
	if _, err := client.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	return func(name string) *mcpgo.CallToolResult {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Name = name
		res, err := client.CallTool(ctx, req)
		if err != nil {
			t.Fatalf("CallTool %s: %v", name, err)
		}
		return res
	}
}

func structuredOf(t *testing.T, res *mcpgo.CallToolResult) map[string]any {
	t.Helper()
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected structured map, got %T", res.StructuredContent)
	}
	return m
}

// TestGate_PairedButDisconnectedIsNotConnected is the regression test for
// the closed loop this taxonomy split exists to break: a linked device
// with a dead socket used to report not_paired, which points callers at
// pairing_start, which then refuses with already_paired because the
// device row is still on disk — leaving no way forward. The gate must
// name the actual condition instead.
func TestGate_PairedButDisconnectedIsNotConnected(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, TransportStdio, pairedOffline)
	registerFixtureTool(t, srv, "fixture_blocked")
	call := startedStdioClient(t, srv)

	res := call("fixture_blocked")
	if !res.IsError {
		t.Fatalf("expected IsError=true, got %+v", res)
	}
	structured := structuredOf(t, res)
	if got := structured["code"]; got != string(ErrNotConnected) {
		t.Errorf("code=%v, want %q", got, ErrNotConnected)
	}
	if got := structured["code"]; got == string(ErrNotPaired) {
		t.Errorf("paired-but-offline must not report %q: that sends callers to pairing_start, which refuses with already_paired", ErrNotPaired)
	}
	if got, _ := structured["message"].(string); got != NotConnectedMessage {
		t.Errorf("message=%q, want %q", got, NotConnectedMessage)
	}
}

// TestGate_UnpairedStillReportsNotPaired pins the other half of the
// split: with no credentials on disk, not_paired remains the answer and
// pairing_start is genuinely the right next step.
func TestGate_UnpairedStillReportsNotPaired(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, TransportStdio, fixedState{paired: false, connected: false})
	registerFixtureTool(t, srv, "fixture_blocked")
	call := startedStdioClient(t, srv)

	structured := structuredOf(t, call("fixture_blocked"))
	if got := structured["code"]; got != string(ErrNotPaired) {
		t.Errorf("code=%v, want %q", got, ErrNotPaired)
	}
}

// TestGate_NotPairedTakesPrecedence covers the ordering: an unpaired
// client is necessarily disconnected too, and the durable condition the
// caller must act on is the missing link, not the dead socket.
func TestGate_NotPairedTakesPrecedence(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, TransportStdio, fixedState{paired: false, connected: true})
	registerFixtureTool(t, srv, "fixture_blocked")
	call := startedStdioClient(t, srv)

	structured := structuredOf(t, call("fixture_blocked"))
	if got := structured["code"]; got != string(ErrNotPaired) {
		t.Errorf("code=%v, want %q", got, ErrNotPaired)
	}
}

// TestGate_ExemptToolsReachableWhenOffline verifies the exempt set still
// bypasses the gate in the paired-but-offline state. cache_sync_status in
// particular is the tool that lets an operator see when the last event
// arrived — precisely the diagnostic wanted during an outage.
func TestGate_ExemptToolsReachableWhenOffline(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, TransportStdio, pairedOffline)
	registerNoopExemptTool(t, srv, "pairing_start")
	registerNoopExemptTool(t, srv, "pairing_complete")
	registerNoopExemptTool(t, srv, "cache_sync_status")
	call := startedStdioClient(t, srv)

	for _, name := range []string{"ping", "pairing_start", "pairing_complete", "cache_sync_status"} {
		if res := call(name); res.IsError {
			t.Errorf("%s should bypass the gate while offline, got IsError=true: %+v", name, res)
		}
	}
}

// TestPing_ReportsPairedAndConnectedSeparately verifies ping surfaces the
// two facts independently, so a caller can tell "never linked" from
// "linked but offline" without having to trigger an error first.
func TestPing_ReportsPairedAndConnectedSeparately(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		state     PairingState
		paired    bool
		connected bool
	}{
		{"offline", pairedOffline, true, false},
		{"unpaired", fixedState{}, false, false},
		{"healthy", fixedState{paired: true, connected: true}, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := newTestServer(t, TransportStdio, tc.state)
			call := startedStdioClient(t, srv)

			structured := structuredOf(t, call("ping"))
			if got, _ := structured["paired"].(bool); got != tc.paired {
				t.Errorf("paired=%v, want %v", got, tc.paired)
			}
			if got, _ := structured["connected"].(bool); got != tc.connected {
				t.Errorf("connected=%v, want %v", got, tc.connected)
			}
		})
	}
}

// newCacheOnlyServer builds a stdio server whose gate treats the named
// tools as cache-backed.
func newCacheOnlyServer(t *testing.T, pairing PairingState, cacheOnly ...string) *Server {
	t.Helper()
	srv, err := New(Config{
		Transport:      TransportStdio,
		Name:           "whatsapp-mcp-test",
		Version:        "test",
		CacheOnlyTools: cacheOnly,
	}, newTestLogger(), nil, pairing)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// TestGate_CacheOnlyToolsAnswerWhileOffline is the point of the
// cache-only tier: during the six-day outage the local cache held every
// chat and message, needing no socket to read, yet every read was refused.
func TestGate_CacheOnlyToolsAnswerWhileOffline(t *testing.T) {
	t.Parallel()

	srv := newCacheOnlyServer(t, pairedOffline, "list_chats")
	registerFixtureTool(t, srv, "list_chats")
	registerFixtureTool(t, srv, "send_message")
	call := startedStdioClient(t, srv)

	if res := call("list_chats"); res.IsError {
		t.Errorf("cache-only tool must answer while offline, got %+v", structuredOf(t, res))
	}

	// A tool that genuinely needs the socket is unaffected by the tier.
	res := call("send_message")
	if !res.IsError {
		t.Fatalf("network tool must still be gated while offline, got %+v", res)
	}
	if got := structuredOf(t, res)["code"]; got != string(ErrNotConnected) {
		t.Errorf("code=%v, want %q", got, ErrNotConnected)
	}
}

// TestGate_CacheOnlyToolsStillRequirePairing pins the deliberate boundary:
// the tier waives the connection check only. With no device ever linked
// there is no cache to read, and the pre-pair contract is unchanged.
func TestGate_CacheOnlyToolsStillRequirePairing(t *testing.T) {
	t.Parallel()

	srv := newCacheOnlyServer(t, fixedState{}, "list_chats")
	registerFixtureTool(t, srv, "list_chats")
	call := startedStdioClient(t, srv)

	res := call("list_chats")
	if !res.IsError {
		t.Fatalf("cache-only tool must still be gated pre-pair, got %+v", res)
	}
	if got := structuredOf(t, res)["code"]; got != string(ErrNotPaired) {
		t.Errorf("code=%v, want %q", got, ErrNotPaired)
	}
}

// TestGate_CacheOnlyToolsUnaffectedWhenHealthy verifies the tier changes
// nothing on the happy path.
func TestGate_CacheOnlyToolsUnaffectedWhenHealthy(t *testing.T) {
	t.Parallel()

	srv := newCacheOnlyServer(t, fixedState{paired: true, connected: true}, "list_chats")
	registerFixtureTool(t, srv, "list_chats")
	call := startedStdioClient(t, srv)

	if res := call("list_chats"); res.IsError {
		t.Errorf("cache-only tool must answer when healthy, got %+v", structuredOf(t, res))
	}
}

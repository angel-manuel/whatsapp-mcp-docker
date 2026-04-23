package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpclienttransport "github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestServer(t *testing.T, transport TransportMode, pairing PairingState) *Server {
	t.Helper()
	cfg := Config{
		Transport: transport,
		Name:      "whatsapp-mcp-test",
		Version:   "test",
	}
	if transport == TransportHTTP {
		cfg.AuthToken = "testtoken"
		cfg.BindAddr = "127.0.0.1"
		cfg.Port = 65000 // only used if Run is called; tests go through HTTPHandler
	}
	srv, err := New(cfg, newTestLogger(), nil, pairing)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func TestServer_NewValidatesConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "http without auth token",
			cfg:     Config{Transport: TransportHTTP, Name: "x", Port: 1},
			wantErr: "AuthToken",
		},
		{
			name:    "unknown transport",
			cfg:     Config{Transport: TransportMode("foo"), Name: "x"},
			wantErr: "unknown transport",
		},
		{
			name:    "empty name",
			cfg:     Config{Transport: TransportStdio},
			wantErr: "server name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg, newTestLogger(), nil, AlwaysPaired)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestStdio_PingRoundtrip exercises the stdio transport end-to-end:
// initialize, then call the built-in ping tool, and verify the
// response shape.
func TestStdio_PingRoundtrip(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, TransportStdio, AlwaysPaired)

	client, cleanup := stdioClient(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}

	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test-client", Version: "0"}
	if _, err := client.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	callReq := mcpgo.CallToolRequest{}
	callReq.Params.Name = "ping"
	callReq.Params.Arguments = map[string]any{"echo": "hi"}

	result, err := client.CallTool(ctx, callReq)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported error: %+v", result)
	}

	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected structured map, got %T", result.StructuredContent)
	}
	if pong, _ := structured["pong"].(bool); !pong {
		t.Errorf("pong=%v, want true", structured["pong"])
	}
	if echo, _ := structured["echo"].(string); echo != "hi" {
		t.Errorf("echo=%q, want %q", echo, "hi")
	}
	if paired, _ := structured["paired"].(bool); !paired {
		t.Errorf("paired=%v, want true", structured["paired"])
	}
}

// TestStdio_NotPairedShortCircuits ensures the global middleware
// intercepts every tool call when pairing is down and returns the
// structured not_paired error instead of the tool's own output.
func TestStdio_NotPairedShortCircuits(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, TransportStdio, NeverPaired)

	client, cleanup := stdioClient(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test-client", Version: "0"}
	if _, err := client.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	callReq := mcpgo.CallToolRequest{}
	callReq.Params.Name = "ping"
	result, err := client.CallTool(ctx, callReq)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected IsError=true, got %+v", result)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected structured map, got %T", result.StructuredContent)
	}
	if got := structured["code"]; got != string(ErrNotPaired) {
		t.Errorf("code=%v, want %q", got, ErrNotPaired)
	}
	if got, _ := structured["message"].(string); got != NotPairedMessage {
		t.Errorf("message=%q, want %q", got, NotPairedMessage)
	}
}

// TestHTTP_ValidBearerSucceeds exercises a full initialize+call over
// the HTTP/SSE transport with a matching bearer token.
func TestHTTP_ValidBearerSucceeds(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, TransportHTTP, AlwaysPaired)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	client, err := mcpclient.NewStreamableHttpClient(ts.URL+"/mcp",
		mcpclienttransport.WithHTTPHeaders(map[string]string{
			"Authorization": "Bearer testtoken",
		}),
	)
	if err != nil {
		t.Fatalf("NewStreamableHttpClient: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test-client", Version: "0"}
	if _, err := client.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	callReq := mcpgo.CallToolRequest{}
	callReq.Params.Name = "ping"
	result, err := client.CallTool(ctx, callReq)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %+v", result)
	}
}

// TestHTTP_MissingBearerRejected verifies requests without the bearer
// header are rejected with 401 before reaching the MCP handler.
func TestHTTP_MissingBearerRejected(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, TransportHTTP, AlwaysPaired)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got == "" {
		t.Errorf("missing WWW-Authenticate header on 401")
	}
}

// TestHTTP_WrongBearerRejected verifies a bearer token that does not
// match the configured one is rejected with 401.
func TestHTTP_WrongBearerRejected(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, TransportHTTP, AlwaysPaired)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer not-the-right-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// TestHTTP_NotPairedShortCircuits verifies the not_paired pre-handler
// still runs over HTTP. The call is authenticated but the pairing
// state reports disconnected.
func TestHTTP_NotPairedShortCircuits(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, TransportHTTP, NeverPaired)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	client, err := mcpclient.NewStreamableHttpClient(ts.URL+"/mcp",
		mcpclienttransport.WithHTTPHeaders(map[string]string{
			"Authorization": "Bearer testtoken",
		}),
	)
	if err != nil {
		t.Fatalf("NewStreamableHttpClient: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test-client", Version: "0"}
	if _, err := client.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	callReq := mcpgo.CallToolRequest{}
	callReq.Params.Name = "ping"
	result, err := client.CallTool(ctx, callReq)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected IsError=true, got %+v", result)
	}

	// Structured content deserialises as map[string]any over HTTP.
	payload, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured: %v", err)
	}
	var decoded struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if decoded.Code != string(ErrNotPaired) {
		t.Errorf("code=%q, want %q", decoded.Code, ErrNotPaired)
	}
	if decoded.Message != NotPairedMessage {
		t.Errorf("message=%q, want %q", decoded.Message, NotPairedMessage)
	}
}

// stdioClient wires the in-memory stdio server to a mcp-go stdio
// client via io.Pipe. Cleanup blocks until the server loop returns.
func stdioClient(t *testing.T, srv *Server) (*mcpclient.Client, func()) {
	t.Helper()

	// Two pipes: client→server and server→client.
	cliToSrvReader, cliToSrvWriter := io.Pipe()
	srvToCliReader, srvToCliWriter := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.ListenStdio(ctx, cliToSrvReader, srvToCliWriter)
	}()

	tr := mcpclienttransport.NewIO(srvToCliReader, writeCloser{cliToSrvWriter}, readCloser{io.NopCloser(strings.NewReader(""))})
	client := mcpclient.NewClient(tr)

	cleanup := func() {
		_ = client.Close()
		cancel()
		_ = cliToSrvReader.Close()
		_ = cliToSrvWriter.Close()
		_ = srvToCliReader.Close()
		_ = srvToCliWriter.Close()
		wg.Wait()
	}
	return client, cleanup
}

type writeCloser struct{ *io.PipeWriter }

func (writeCloser) Close() error { return nil }

type readCloser struct{ io.ReadCloser }

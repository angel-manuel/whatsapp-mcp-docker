package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpclienttransport "github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/sessions"
)

// testAPI is an HTTP server hosting real (unpaired, offline) sessions:
// real whatsmeow stores, caches and media stores in a temp dir. Unpaired
// sessions never dial WhatsApp, so this runs without a network.
type testAPI struct {
	t     *testing.T
	url   string
	root  string
	admin string
}

func newTestAPI(t *testing.T, adminToken string) *testAPI {
	t.Helper()
	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	reg, err := sessions.OpenRegistry(context.Background(), filepath.Join(dataDir, "sessions.db"))
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	opts := sessions.Options{
		Name:               "whatsapp-mcp-test",
		Version:            "test",
		Transport:          mcp.TransportHTTP,
		MediaSweepInterval: time.Hour,
		Logger:             logger,
	}
	root := filepath.Join(dataDir, "sessions")
	mgr := sessions.NewManager(reg, root, 5,
		func(id, dir string) (sessions.Instance, error) { return sessions.Open(id, dir, opts) },
		logger)
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ts := httptest.NewServer(NewHandler(mgr, adminToken, logger))
	t.Cleanup(func() {
		ts.Close()
		_ = mgr.Close()
		_ = reg.Close()
	})
	return &testAPI{t: t, url: ts.URL, root: root, admin: adminToken}
}

func (a *testAPI) do(method, path, token, contentType string, body io.Reader) (*http.Response, []byte) {
	a.t.Helper()
	req, err := http.NewRequest(method, a.url+path, body)
	if err != nil {
		a.t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func (a *testAPI) create(id string) sessions.Created {
	a.t.Helper()
	resp, raw := a.do(http.MethodPost, "/sessions", a.admin, "application/json",
		strings.NewReader(`{"id":"`+id+`"}`))
	if resp.StatusCode != http.StatusCreated {
		a.t.Fatalf("POST /sessions %s: %d %s", id, resp.StatusCode, raw)
	}
	var c sessions.Created
	if err := json.Unmarshal(raw, &c); err != nil {
		a.t.Fatalf("decode: %v", err)
	}
	return c
}

func (a *testAPI) list() []sessions.Info {
	a.t.Helper()
	resp, raw := a.do(http.MethodGet, "/sessions", a.admin, "", nil)
	if resp.StatusCode != http.StatusOK {
		a.t.Fatalf("GET /sessions: %d %s", resp.StatusCode, raw)
	}
	var body struct {
		Sessions []sessions.Info `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		a.t.Fatalf("decode: %v", err)
	}
	return body.Sessions
}

func TestAdminAPI_RequiresAdminTokenWhenSet(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, "admin-secret")

	for _, tc := range []struct {
		method, path, token string
	}{
		{http.MethodGet, "/sessions", ""},
		{http.MethodGet, "/sessions", "wrong"},
		{http.MethodPost, "/sessions", ""},
		{http.MethodDelete, "/sessions/x", "wrong"},
	} {
		resp, _ := api.do(tc.method, tc.path, tc.token, "application/json", strings.NewReader(`{"id":"x"}`))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with %q: status %d, want 401", tc.method, tc.path, tc.token, resp.StatusCode)
		}
	}
	if got := api.list(); len(got) != 0 {
		t.Fatalf("unauthorized requests created sessions: %+v", got)
	}

	// A session token is not an admin token.
	c := api.create("alice")
	resp, _ := api.do(http.MethodGet, "/sessions", c.Token, "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("session token on admin API: status %d, want 401", resp.StatusCode)
	}
}

func TestAdminAPI_AnonymousWhenAdminTokenUnset(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, "")
	c := api.create("alice")
	if c.Token == "" || c.ID != "alice" {
		t.Fatalf("Created = %+v", c)
	}
	if got := api.list(); len(got) != 1 || got[0].Status != sessions.StatusUnpaired {
		t.Fatalf("list = %+v", got)
	}
}

func TestAdminAPI_Errors(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, "")
	api.create("alice")

	for _, tc := range []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"bad json", http.MethodPost, "/sessions", `nope`, 400, "bad_request"},
		{"unknown field", http.MethodPost, "/sessions", `{"id":"x","extra":1}`, 400, "bad_request"},
		{"invalid id", http.MethodPost, "/sessions", `{"id":"../etc"}`, 400, "invalid_id"},
		{"duplicate", http.MethodPost, "/sessions", `{"id":"alice"}`, 409, "exists"},
		{"delete unknown", http.MethodDelete, "/sessions/nobody", ``, 404, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := api.do(tc.method, tc.path, "", "application/json", strings.NewReader(tc.body))
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d (%s), want %d", resp.StatusCode, raw, tc.status)
			}
			var e struct{ Code string }
			_ = json.Unmarshal(raw, &e)
			if e.Code != tc.code {
				t.Errorf("code %q, want %q", e.Code, tc.code)
			}
		})
	}
}

func TestSessionRoutes_RejectMissingOrUnknownToken(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, "")
	api.create("alice")
	for _, path := range []string{"/mcp", "/media", "/media/" + strings.Repeat("ab", 32)} {
		for _, token := range []string{"", "not-a-token", strings.Repeat("A", 43)} {
			resp, _ := api.do(http.MethodPost, path, token, "application/json", strings.NewReader(`{}`))
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("POST %s with %q: status %d, want 401", path, token, resp.StatusCode)
			}
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Errorf("POST %s: missing WWW-Authenticate", path)
			}
		}
	}
}

// Each token reaches its own MCP server: ping answers through both, and
// both report unpaired because neither has linked a phone.
func TestSessionRoutes_MCPPerSession(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, "")
	for _, id := range []string{"alice", "bob"} {
		c := api.create(id)
		client, err := mcpclient.NewStreamableHttpClient(api.url+"/mcp",
			mcpclienttransport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + c.Token}))
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := client.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		initReq := mcpgo.InitializeRequest{}
		initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
		initReq.Params.ClientInfo = mcpgo.Implementation{Name: "t", Version: "0"}
		if _, err := client.Initialize(ctx, initReq); err != nil {
			t.Fatalf("Initialize %s: %v", id, err)
		}
		req := mcpgo.CallToolRequest{}
		req.Params.Name = "ping"
		res, err := client.CallTool(ctx, req)
		if err != nil || res.IsError {
			t.Fatalf("ping %s: %v %+v", id, err, res)
		}
		cancel()
		_ = client.Close()
	}
}

// The property per-session media directories exist for: a digest uploaded
// through one session's token is invisible to every other session, so a
// tenant can neither read another's attachment nor learn that it exists.
func TestSessionRoutes_MediaIsolatedBetweenSessions(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, "")
	alice := api.create("alice")
	bob := api.create("bob")

	resp, raw := api.do(http.MethodPost, "/media?filename=secret.txt", alice.Token, "text/plain",
		strings.NewReader("alice's secret"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %s", resp.StatusCode, raw)
	}
	var desc struct {
		MediaPath string `json:"media_path"`
	}
	if err := json.Unmarshal(raw, &desc); err != nil || desc.MediaPath == "" {
		t.Fatalf("descriptor %s: %v", raw, err)
	}

	resp, raw = api.do(http.MethodGet, desc.MediaPath, alice.Token, "", nil)
	if resp.StatusCode != http.StatusOK || string(raw) != "alice's secret" {
		t.Fatalf("owner GET: %d %q", resp.StatusCode, raw)
	}
	resp, raw = api.do(http.MethodGet, desc.MediaPath, bob.Token, "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other session GET: %d %q, want 404", resp.StatusCode, raw)
	}
}

func TestDeleteSession_RevokesTokenAndWipesData(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, "admin-secret")
	alice := api.create("alice")
	api.create("bob")

	resp, raw := api.do(http.MethodDelete, "/sessions/alice", api.admin, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: %d %s", resp.StatusCode, raw)
	}
	var del sessions.Deleted
	_ = json.Unmarshal(raw, &del)
	if del.ID != "alice" || del.RemoteLogout {
		t.Errorf("Deleted = %+v; an unpaired session has nothing to log out", del)
	}

	resp, _ = api.do(http.MethodPost, "/mcp", alice.Token, "application/json", strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("deleted token: status %d, want 401", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(api.root, "alice")); !os.IsNotExist(err) {
		t.Errorf("session directory survived: %v", err)
	}
	if got := api.list(); len(got) != 1 || got[0].ID != "bob" {
		t.Errorf("list = %+v, want only bob", got)
	}
}

func TestHealthz_CountsWithoutIDs(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, "admin-secret")
	api.create("alice-private-name")

	resp, raw := api.do(http.MethodGet, "/healthz", "", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if strings.Contains(string(raw), "alice") {
		t.Fatalf("healthz leaks a session id: %s", raw)
	}
	var body struct {
		Status   string         `json:"status"`
		Sessions map[string]int `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if body.Status != "ok" || body.Sessions["unpaired"] != 1 {
		t.Errorf("body = %s", raw)
	}

	resp, _ = api.do(http.MethodPost, "/healthz", "", "", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz: %d, want 405", resp.StatusCode)
	}
}

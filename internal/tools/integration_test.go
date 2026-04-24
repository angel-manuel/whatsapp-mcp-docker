package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpclienttransport "github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/tools"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// mockWA is a lightweight stand-in for *wa.Client. Tests wire up only
// the behaviours they care about; the rest falls through to defaults.
type mockWA struct {
	groupInfo     map[string]*types.GroupInfo
	groupInfoErr  map[string]error
	userInfo      map[types.JID]types.UserInfo
	userInfoErr   error
	isOnWhatsApp  []types.IsOnWhatsAppResponse
	isOnErr       error
	profileURL    string
	profileErr    error
	groupInfoCall int
	userInfoCall  int
	picCall       int
}

func (m *mockWA) GroupInfo(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
	m.groupInfoCall++
	if m.groupInfoErr != nil {
		if err, ok := m.groupInfoErr[jid.String()]; ok {
			return nil, err
		}
	}
	if info, ok := m.groupInfo[jid.String()]; ok {
		return info, nil
	}
	return nil, whatsmeow.ErrGroupNotFound
}

func (m *mockWA) UserInfo(_ context.Context, jids []types.JID) (map[types.JID]types.UserInfo, error) {
	m.userInfoCall++
	if m.userInfoErr != nil {
		return nil, m.userInfoErr
	}
	out := map[types.JID]types.UserInfo{}
	for _, j := range jids {
		if v, ok := m.userInfo[j]; ok {
			out[j] = v
		}
	}
	return out, nil
}

func (m *mockWA) IsOnWhatsApp(_ context.Context, _ []string) ([]types.IsOnWhatsAppResponse, error) {
	if m.isOnErr != nil {
		return nil, m.isOnErr
	}
	return m.isOnWhatsApp, nil
}

func (m *mockWA) ProfilePictureURL(_ context.Context, _ types.JID) (string, error) {
	m.picCall++
	if m.profileErr != nil {
		return "", m.profileErr
	}
	return m.profileURL, nil
}

// Confirm the mock satisfies the tools.WAClient interface at compile time.
var _ tools.WAClient = (*mockWA)(nil)

// testHarness bundles the wired stdio client + its shutdown hook.
type testHarness struct {
	client *mcpclient.Client
	cancel func()
	mock   *mockWA
	store  *cache.Store
}

func (h *testHarness) close() { h.cancel() }

// newHarness constructs an mcp.Server with the tools package registered,
// a fresh in-memory cache store, and a mock wa client. It wires a stdio
// client at the other end so tests can drive the real MCP protocol.
func newHarness(t *testing.T, paired bool, seed func(*cache.Store), mock *mockWA) *testHarness {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	// Each harness opens its own cache DB in a per-test temp dir. The
	// in-memory DSN uses cache=shared which would collide across parallel
	// tests.
	store, err := cache.Open(ctx, t.TempDir())
	if err != nil {
		cancel()
		t.Fatalf("cache open: %v", err)
	}
	if seed != nil {
		seed(store)
	}

	pairing := mcp.AlwaysPaired
	if !paired {
		pairing = mcp.NeverPaired
	}
	srv, err := mcp.New(mcp.Config{
		Transport: mcp.TransportStdio,
		Name:      "whatsapp-mcp-tools-test",
		Version:   "test",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, pairing)
	if err != nil {
		cancel()
		t.Fatalf("mcp.New: %v", err)
	}
	if mock == nil {
		mock = &mockWA{}
	}
	if err := tools.Register(srv.Registry(), tools.Deps{Cache: store, WA: mock}); err != nil {
		cancel()
		t.Fatalf("tools.Register: %v", err)
	}

	cliToSrvR, cliToSrvW := io.Pipe()
	srvToCliR, srvToCliW := io.Pipe()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.ListenStdio(ctx, cliToSrvR, srvToCliW)
	}()

	tr := mcpclienttransport.NewIO(srvToCliR, writeCloser{cliToSrvW}, readCloser{io.NopCloser(strings.NewReader(""))})
	client := mcpclient.NewClient(tr)

	startCtx, startCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer startCancel()
	if err := client.Start(startCtx); err != nil {
		cancel()
		t.Fatalf("client.Start: %v", err)
	}

	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test-client", Version: "0"}
	if _, err := client.Initialize(startCtx, initReq); err != nil {
		cancel()
		t.Fatalf("client.Initialize: %v", err)
	}

	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		_ = cliToSrvR.Close()
		_ = cliToSrvW.Close()
		_ = srvToCliR.Close()
		_ = srvToCliW.Close()
		wg.Wait()
		_ = store.Close()
	})

	return &testHarness{client: client, cancel: cancel, mock: mock, store: store}
}

func callTool(t *testing.T, h *testHarness, name string, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := mcpgo.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := h.client.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}

func structured(t *testing.T, res *mcpgo.CallToolResult) map[string]any {
	t.Helper()
	payload, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured: %v", err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	return out
}

func expectError(t *testing.T, res *mcpgo.CallToolResult, wantCode mcp.ErrorCode) map[string]any {
	t.Helper()
	if !res.IsError {
		t.Fatalf("expected IsError=true, got result=%+v", res)
	}
	s := structured(t, res)
	if got, _ := s["code"].(string); got != string(wantCode) {
		t.Errorf("code=%q, want %q (message=%q)", got, wantCode, s["message"])
	}
	return s
}

// seedContacts mirrors the cache-package reader test fixture so the
// integration tests have realistic data to exercise.
func seedContacts(s *cache.Store) {
	ctx := context.Background()
	_ = s.UpsertContact(ctx, cache.Contact{
		JID: "111@s.whatsapp.net", PushName: "Alice Lastname",
		FullName: "Alice Anderson", FirstName: "Alice",
	})
	_ = s.UpsertContact(ctx, cache.Contact{
		JID: "222@s.whatsapp.net", FullName: "Bob Builder",
		BusinessName: "Bob's Hardware",
	})
	_ = s.UpsertContact(ctx, cache.Contact{
		JID: "333@s.whatsapp.net", PushName: "Carol", FirstName: "Carol",
	})
	_ = s.UpsertNickname(ctx, cache.Nickname{JID: "111@s.whatsapp.net", Nickname: "Ali"})
}

func TestSearchContacts_ReturnsContactsFromCache(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, seedContacts, nil)

	res := callTool(t, h, "search_contacts", map[string]any{"query": "alice"})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	list, _ := s["contacts"].([]any)
	if len(list) != 1 {
		t.Fatalf("contacts count = %d, want 1 (got=%+v)", len(list), list)
	}
	first, _ := list[0].(map[string]any)
	if got := first["jid"]; got != "111@s.whatsapp.net" {
		t.Errorf("jid=%v, want 111@s.whatsapp.net", got)
	}
	if got := first["nickname"]; got != "Ali" {
		t.Errorf("nickname=%v, want Ali", got)
	}
	if got := first["name"]; got != "Ali" {
		t.Errorf("name=%v, want Ali (nickname should shadow the cascade)", got)
	}
	if got := first["phone_number"]; got != "111" {
		t.Errorf("phone_number=%v, want 111", got)
	}
}

func TestSearchContacts_RejectsEmptyQuery(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, seedContacts, nil)

	res := callTool(t, h, "search_contacts", map[string]any{"query": "   "})
	expectError(t, res, mcp.ErrInvalidArgument)
}

func TestListAllContacts_ReturnsAllNonGroups(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, seedContacts, nil)

	res := callTool(t, h, "list_all_contacts", nil)
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	contacts, _ := s["contacts"].([]any)
	if len(contacts) != 3 {
		t.Fatalf("len=%d, want 3 (got=%+v)", len(contacts), contacts)
	}
}

func TestGetContactDetails_MergesCacheAndUSync(t *testing.T) {
	t.Parallel()
	targetJID := types.NewJID("111", types.DefaultUserServer)
	mock := &mockWA{
		userInfo: map[types.JID]types.UserInfo{
			targetJID: {Status: "Available"},
		},
		profileURL: "https://cdn/example.jpg",
	}
	h := newHarness(t, true, seedContacts, mock)

	res := callTool(t, h, "get_contact_details", map[string]any{"jid": "111@s.whatsapp.net"})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	if got := s["jid"]; got != "111@s.whatsapp.net" {
		t.Errorf("jid=%v", got)
	}
	if got := s["full_name"]; got != "Alice Anderson" {
		t.Errorf("full_name=%v, want Alice Anderson", got)
	}
	if got := s["nickname"]; got != "Ali" {
		t.Errorf("nickname=%v, want Ali", got)
	}
	if got := s["phone"]; got != "111" {
		t.Errorf("phone=%v, want 111", got)
	}
	if got := s["status"]; got != "Available" {
		t.Errorf("status=%v, want Available", got)
	}
	if got := s["profile_picture_url"]; got != "https://cdn/example.jpg" {
		t.Errorf("profile_picture_url=%v", got)
	}
	if got, _ := s["is_on_whatsapp"].(bool); !got {
		t.Errorf("is_on_whatsapp=%v, want true", got)
	}
}

func TestGetContactDetails_USyncFallbackForUnknownJID(t *testing.T) {
	t.Parallel()
	target := types.NewJID("555", types.DefaultUserServer)
	mock := &mockWA{
		userInfo: map[types.JID]types.UserInfo{
			target: {Status: "hello"},
		},
	}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "get_contact_details", map[string]any{"jid": target.String()})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)
	if s["status"] != "hello" {
		t.Errorf("status=%v, want hello", s["status"])
	}
	if got, _ := s["is_on_whatsapp"].(bool); !got {
		t.Errorf("is_on_whatsapp=%v, want true", got)
	}
}

func TestGetContactDetails_NotFoundWhenMissingEverywhere(t *testing.T) {
	t.Parallel()
	// UserInfo returns empty map → USync had no row. Also no IsOnWhatsApp hit.
	h := newHarness(t, true, nil, &mockWA{})

	res := callTool(t, h, "get_contact_details", map[string]any{"jid": "nosuch@s.whatsapp.net"})
	expectError(t, res, mcp.ErrNotFound)
}

func TestGetContactDetails_RejectsInvalidJID(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, seedContacts, nil)

	res := callTool(t, h, "get_contact_details", map[string]any{"jid": ""})
	expectError(t, res, mcp.ErrInvalidArgument)

	// JID with no user part is also invalid.
	res = callTool(t, h, "get_contact_details", map[string]any{"jid": "@s.whatsapp.net"})
	expectError(t, res, mcp.ErrInvalidArgument)
}

func TestGetGroupInfo_ReturnsWhatsmeowShape(t *testing.T) {
	t.Parallel()
	groupJID := types.NewJID("chatid", types.GroupServer)
	ownerJID := types.NewJID("111", types.DefaultUserServer)
	created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	info := &types.GroupInfo{
		JID:          groupJID,
		OwnerJID:     ownerJID,
		GroupName:    types.GroupName{Name: "Weekend Plans"},
		GroupTopic:   types.GroupTopic{Topic: "planning"},
		GroupLocked:  types.GroupLocked{IsLocked: true},
		GroupCreated: created,
		GroupAnnounce: types.GroupAnnounce{IsAnnounce: true},
		Participants: []types.GroupParticipant{
			{JID: ownerJID, IsAdmin: true, IsSuperAdmin: true},
			{JID: types.NewJID("222", types.DefaultUserServer)},
		},
	}
	mock := &mockWA{
		groupInfo: map[string]*types.GroupInfo{groupJID.String(): info},
	}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "get_group_info", map[string]any{"group_jid": groupJID.String()})
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	s := structured(t, res)

	if s["jid"] != groupJID.String() {
		t.Errorf("jid=%v, want %s", s["jid"], groupJID.String())
	}
	if s["name"] != "Weekend Plans" {
		t.Errorf("name=%v", s["name"])
	}
	if s["topic"] != "planning" {
		t.Errorf("topic=%v", s["topic"])
	}
	if got, _ := s["created_ts"].(float64); int64(got) != created.Unix() {
		t.Errorf("created_ts=%v, want %d", got, created.Unix())
	}
	if s["owner_jid"] != ownerJID.String() {
		t.Errorf("owner_jid=%v", s["owner_jid"])
	}
	if got, _ := s["is_announcement"].(bool); !got {
		t.Errorf("is_announcement=%v, want true", got)
	}
	if got, _ := s["is_locked"].(bool); !got {
		t.Errorf("is_locked=%v, want true", got)
	}
	parts, _ := s["participants"].([]any)
	if len(parts) != 2 {
		t.Fatalf("participants len=%d, want 2", len(parts))
	}
	p0, _ := parts[0].(map[string]any)
	if p0["jid"] != ownerJID.String() {
		t.Errorf("participants[0].jid=%v", p0["jid"])
	}
	if got, _ := p0["is_admin"].(bool); !got {
		t.Errorf("participants[0].is_admin=%v, want true", got)
	}
	if got, _ := p0["is_super_admin"].(bool); !got {
		t.Errorf("participants[0].is_super_admin=%v, want true", got)
	}
	p1, _ := parts[1].(map[string]any)
	if got, _ := p1["is_admin"].(bool); got {
		t.Errorf("participants[1].is_admin=%v, want false", got)
	}
}

func TestGetGroupInfo_NotFound(t *testing.T) {
	t.Parallel()
	mock := &mockWA{} // empty map → GroupInfo returns ErrGroupNotFound
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "get_group_info", map[string]any{"group_jid": "nobody@g.us"})
	expectError(t, res, mcp.ErrNotFound)
}

func TestGetGroupInfo_IQNotFoundMapsToNotFound(t *testing.T) {
	t.Parallel()
	groupJID := types.NewJID("chatid", types.GroupServer)
	mock := &mockWA{
		groupInfoErr: map[string]error{
			groupJID.String(): whatsmeow.ErrIQNotFound,
		},
	}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "get_group_info", map[string]any{"group_jid": groupJID.String()})
	expectError(t, res, mcp.ErrNotFound)
}

func TestGetGroupInfo_RejectsNonGroupJID(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil, nil)

	res := callTool(t, h, "get_group_info", map[string]any{"group_jid": "111@s.whatsapp.net"})
	expectError(t, res, mcp.ErrInvalidArgument)
}

func TestGetGroupInfo_NotLoggedInShortCircuitsToNotPaired(t *testing.T) {
	t.Parallel()
	groupJID := types.NewJID("chatid", types.GroupServer)
	mock := &mockWA{
		groupInfoErr: map[string]error{
			groupJID.String(): wa.ErrNotLoggedIn,
		},
	}
	h := newHarness(t, true, nil, mock)

	res := callTool(t, h, "get_group_info", map[string]any{"group_jid": groupJID.String()})
	expectError(t, res, mcp.ErrNotPaired)
}

func TestTools_NotPairedShortCircuits(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, seedContacts, &mockWA{})

	for _, name := range []string{"search_contacts", "list_all_contacts", "get_contact_details", "get_group_info"} {
		t.Run(name, func(t *testing.T) {
			var args map[string]any
			switch name {
			case "search_contacts":
				args = map[string]any{"query": "x"}
			case "get_contact_details":
				args = map[string]any{"jid": "111@s.whatsapp.net"}
			case "get_group_info":
				args = map[string]any{"group_jid": "xxx@g.us"}
			}
			res := callTool(t, h, name, args)
			if !res.IsError {
				t.Fatalf("expected not_paired short-circuit, got %+v", res)
			}
			s := structured(t, res)
			if s["code"] != string(mcp.ErrNotPaired) {
				t.Errorf("code=%v, want %q", s["code"], mcp.ErrNotPaired)
			}
		})
	}
}

func TestTools_ListRegisteredToolsAdvertisesSchemas(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := mcpgo.ListToolsRequest{}
	resp, err := h.client.ListTools(ctx, req)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	want := map[string]bool{
		"search_contacts":     false,
		"list_all_contacts":   false,
		"get_contact_details": false,
		"get_group_info":      false,
	}
	for _, tool := range resp.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
			if tool.InputSchema.Type == "" && len(tool.RawInputSchema) == 0 {
				t.Errorf("tool %s: missing input schema", tool.Name)
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing tool %s from list_tools output", name)
		}
	}
}

// -- Pipe-adapter helpers (match the pattern in internal/mcp/server_test.go)

type writeCloser struct{ *io.PipeWriter }

func (writeCloser) Close() error { return nil }

type readCloser struct{ io.ReadCloser }

// Ensure errors.Is is available for any future test cases that need it.
var _ = errors.Is

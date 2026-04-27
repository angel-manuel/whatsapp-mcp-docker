package mcptools_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpclienttransport "github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcptools"
)

// Canonical fixture timestamps. Spread across a whole day so ordering
// assertions are readable.
var (
	tsAlice1  = time.Date(2024, 6, 1, 9, 0, 0, 0, time.UTC)
	tsBob1    = time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	tsGroup1  = time.Date(2024, 6, 1, 11, 0, 0, 0, time.UTC)
	tsGroup2  = time.Date(2024, 6, 1, 11, 5, 0, 0, time.UTC)
	tsGroup3  = time.Date(2024, 6, 1, 11, 10, 0, 0, time.UTC)
	tsAlice2  = time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	tsGroupMe = time.Date(2024, 6, 1, 11, 15, 0, 0, time.UTC)
)

const (
	jidAlice = "111@s.whatsapp.net"
	jidBob   = "222@s.whatsapp.net"
	jidGroup = "777@g.us"
	jidSelf  = "999@s.whatsapp.net"
)

// seedFixtures writes a deterministic set of chats / messages to store.
// Kept as a package-level so each test can start from the same baseline.
func seedFixtures(t *testing.T, store *cache.Store) {
	t.Helper()
	ctx := context.Background()

	chats := []cache.Chat{
		{JID: jidAlice, Name: "Alice", LastMessageTS: tsAlice2},
		{JID: jidBob, Name: "Bob", LastMessageTS: tsBob1},
		{JID: jidGroup, Name: "Friends", IsGroup: true, LastMessageTS: tsGroupMe},
	}
	for _, c := range chats {
		if err := store.UpsertChat(ctx, c); err != nil {
			t.Fatalf("seed chat %s: %v", c.JID, err)
		}
	}

	msgs := []cache.Message{
		// Alice 1:1
		{ID: "m-alice-1", ChatJID: jidAlice, SenderJID: jidAlice, Timestamp: tsAlice1, Kind: cache.KindText, Body: "hello from alice"},
		{ID: "m-alice-2", ChatJID: jidAlice, SenderJID: jidSelf, Timestamp: tsAlice2, Kind: cache.KindText, Body: "hey alice, any news on the refactor?", IsFromMe: true},
		// Bob 1:1 — one image, one text; use this chat to test media_type output.
		{
			ID: "m-bob-1", ChatJID: jidBob, SenderJID: jidBob, Timestamp: tsBob1, Kind: cache.KindImage, Body: "look at this", Media: &cache.Media{
				Mime: "image/jpeg", Filename: "photo.jpg", Length: 12345,
			},
		},
		// Group chat — 3 messages by alice+bob, 1 by self. Used for context window.
		{ID: "m-g-1", ChatJID: jidGroup, SenderJID: jidAlice, Timestamp: tsGroup1, Kind: cache.KindText, Body: "morning team"},
		{ID: "m-g-2", ChatJID: jidGroup, SenderJID: jidBob, Timestamp: tsGroup2, Kind: cache.KindText, Body: "morning!"},
		{ID: "m-g-3", ChatJID: jidGroup, SenderJID: jidAlice, Timestamp: tsGroup3, Kind: cache.KindText, Body: "shall we ship today?"},
		{ID: "m-g-me", ChatJID: jidGroup, SenderJID: jidSelf, Timestamp: tsGroupMe, Kind: cache.KindText, Body: "ship it", IsFromMe: true},
	}
	for _, m := range msgs {
		if err := store.InsertMessage(ctx, m); err != nil {
			t.Fatalf("seed message %s: %v", m.ID, err)
		}
	}
}

// newServerAndClient spins up an in-memory cache, registers the read-
// side tools, and wires an stdio MCP client to the server via pipes.
// The returned cleanup must be invoked via t.Cleanup; it blocks until
// the server goroutine exits.
func newServerAndClient(t *testing.T) *mcpclient.Client {
	return newServerAndClientWithExtras(t, nil)
}

// newServerAndClientWithExtras is identical to newServerAndClient but invokes
// extras(store) after the standard seedFixtures, before tool registration.
// Useful for tests that need additional rows in the cache.
func newServerAndClientWithExtras(t *testing.T, extras func(*cache.Store)) *mcpclient.Client {
	t.Helper()

	store, err := cache.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedFixtures(t, store)
	if extras != nil {
		extras(store)
	}

	reg := mcp.NewRegistry()
	if err := mcptools.Register(reg, store); err != nil {
		t.Fatalf("Register: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv, err := mcp.New(mcp.Config{
		Transport: mcp.TransportStdio,
		Name:      "mcptools-test",
		Version:   "test",
	}, logger, reg, mcp.AlwaysPaired)
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}

	cliToSrvReader, cliToSrvWriter := io.Pipe()
	srvToCliReader, srvToCliWriter := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.ListenStdio(ctx, cliToSrvReader, srvToCliWriter)
	}()

	tr := mcpclienttransport.NewIO(
		srvToCliReader,
		pipeWriteCloser{cliToSrvWriter},
		pipeReadCloser{io.NopCloser(strings.NewReader(""))},
	)
	client := mcpclient.NewClient(tr)
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		_ = cliToSrvReader.Close()
		_ = cliToSrvWriter.Close()
		_ = srvToCliReader.Close()
		_ = srvToCliWriter.Close()
		wg.Wait()
	})

	initCtx, initCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer initCancel()
	if err := client.Start(initCtx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test-client", Version: "0"}
	if _, err := client.Initialize(initCtx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return client
}

type pipeWriteCloser struct{ *io.PipeWriter }

func (pipeWriteCloser) Close() error { return nil }

type pipeReadCloser struct{ io.ReadCloser }

// callTool invokes name with args and returns the decoded structured
// output. Fails the test if the tool returned an error result.
func callTool(t *testing.T, c *mcpclient.Client, name string, args map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := mcpgo.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := c.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("%s: CallTool err: %v", name, err)
	}
	if res.IsError {
		payload, _ := json.Marshal(res.StructuredContent)
		t.Fatalf("%s: tool returned error: %s", name, payload)
	}
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("%s: structured content not a map: %T", name, res.StructuredContent)
	}
	return m
}

// callToolError invokes name expecting an error and returns the
// structured error payload (code + message).
func callToolError(t *testing.T, c *mcpclient.Client, name string, args map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := mcpgo.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := c.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("%s: CallTool err: %v", name, err)
	}
	if !res.IsError {
		t.Fatalf("%s: expected IsError=true, got %+v", name, res.StructuredContent)
	}
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("%s: structured content not a map: %T", name, res.StructuredContent)
	}
	return m
}

func TestListChats_OrdersByLastActiveAndIncludesPreview(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	out := callTool(t, c, "list_chats", map[string]any{})
	chats, ok := out["chats"].([]any)
	if !ok {
		t.Fatalf("chats is not array: %T", out["chats"])
	}
	if len(chats) != 3 {
		t.Fatalf("len(chats) = %d, want 3", len(chats))
	}
	// Sort order: last_active desc -> alice (12:00), group (11:15), bob (10:00).
	wantOrder := []string{jidAlice, jidGroup, jidBob}
	for i, c := range chats {
		cm := c.(map[string]any)
		if cm["jid"] != wantOrder[i] {
			t.Errorf("chats[%d].jid = %v, want %v", i, cm["jid"], wantOrder[i])
		}
	}
	alice := chats[0].(map[string]any)
	if alice["is_group"].(bool) {
		t.Errorf("alice.is_group = true, want false")
	}
	if alice["last_message"] != "hey alice, any news on the refactor?" {
		t.Errorf("alice.last_message = %v", alice["last_message"])
	}
	if alice["last_sender"] != jidSelf {
		t.Errorf("alice.last_sender = %v", alice["last_sender"])
	}
	if alice["last_is_from_me"] != true {
		t.Errorf("alice.last_is_from_me = %v", alice["last_is_from_me"])
	}
}

func TestListChats_QueryFiltersByNameSubstring(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	out := callTool(t, c, "list_chats", map[string]any{"query": "fri"})
	chats := out["chats"].([]any)
	if len(chats) != 1 {
		t.Fatalf("len(chats) = %d, want 1", len(chats))
	}
	if chats[0].(map[string]any)["jid"] != jidGroup {
		t.Errorf("got jid %v", chats[0].(map[string]any)["jid"])
	}
}

func TestListChats_SortByName(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	out := callTool(t, c, "list_chats", map[string]any{"sort_by": "name"})
	chats := out["chats"].([]any)
	wantOrder := []string{"Alice", "Bob", "Friends"}
	for i, c := range chats {
		if c.(map[string]any)["name"] != wantOrder[i] {
			t.Errorf("chats[%d].name = %v, want %v", i, c.(map[string]any)["name"], wantOrder[i])
		}
	}
}

func TestListChats_IncludeLastMessageFalseNullsPreview(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	out := callTool(t, c, "list_chats", map[string]any{"include_last_message": false})
	chats := out["chats"].([]any)
	first := chats[0].(map[string]any)
	if first["last_message"] != nil {
		t.Errorf("last_message = %v, want nil", first["last_message"])
	}
	if first["last_is_from_me"] != nil {
		t.Errorf("last_is_from_me = %v, want nil", first["last_is_from_me"])
	}
}

func TestListChats_RejectsInvalidPagination(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	err := callToolError(t, c, "list_chats", map[string]any{"limit": -5})
	if err["code"] != string(mcp.ErrInvalidArgument) {
		t.Errorf("code = %v, want %s", err["code"], mcp.ErrInvalidArgument)
	}
}

func TestListChats_ExposesChatType(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	out := callTool(t, c, "list_chats", map[string]any{})
	chats := out["chats"].([]any)
	got := map[string]string{}
	for _, raw := range chats {
		m := raw.(map[string]any)
		got[m["jid"].(string)] = m["chat_type"].(string)
	}
	want := map[string]string{
		jidAlice: "direct",
		jidBob:   "direct",
		jidGroup: "group",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("chat_type for %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestListChats_FilterByChatType(t *testing.T) {
	t.Parallel()
	c := newServerAndClientWithExtras(t, func(store *cache.Store) {
		// Add a newsletter alongside the existing fixtures so we can prove
		// the filter selects on chat_type rather than is_group alone.
		if err := store.UpsertChat(context.Background(), cache.Chat{
			JID:  "120363999000000099@newsletter",
			Name: "Brief",
			Type: cache.ChatTypeNewsletter,
		}); err != nil {
			t.Fatalf("seed newsletter: %v", err)
		}
	})

	out := callTool(t, c, "list_chats", map[string]any{"chat_type": "newsletter"})
	chats := out["chats"].([]any)
	if len(chats) != 1 {
		t.Fatalf("len(chats) = %d, want 1", len(chats))
	}
	first := chats[0].(map[string]any)
	if first["jid"] != "120363999000000099@newsletter" || first["chat_type"] != "newsletter" {
		t.Errorf("got jid=%v chat_type=%v", first["jid"], first["chat_type"])
	}
}

func TestGetChat_ReturnsSingleChat(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	out := callTool(t, c, "get_chat", map[string]any{"chat_jid": jidGroup})
	if out["jid"] != jidGroup {
		t.Errorf("jid = %v", out["jid"])
	}
	if out["name"] != "Friends" {
		t.Errorf("name = %v", out["name"])
	}
	if out["is_group"] != true {
		t.Errorf("is_group = %v", out["is_group"])
	}
	if out["last_message"] != "ship it" {
		t.Errorf("last_message = %v", out["last_message"])
	}
}

func TestGetChat_NotFound(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	err := callToolError(t, c, "get_chat", map[string]any{"chat_jid": "nobody@s.whatsapp.net"})
	if err["code"] != string(mcp.ErrNotFound) {
		t.Errorf("code = %v, want %s", err["code"], mcp.ErrNotFound)
	}
}

func TestListMessages_FiltersByChat(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	out := callTool(t, c, "list_messages", map[string]any{"chat_jid": jidGroup})
	msgs := out["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("len(messages) = %d, want 4", len(msgs))
	}
	// Ordered newest first.
	first := msgs[0].(map[string]any)
	if first["id"] != "m-g-me" {
		t.Errorf("messages[0].id = %v, want m-g-me", first["id"])
	}
	last := msgs[3].(map[string]any)
	if last["id"] != "m-g-1" {
		t.Errorf("messages[3].id = %v, want m-g-1", last["id"])
	}
}

func TestListMessages_FiltersBySender(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	out := callTool(t, c, "list_messages", map[string]any{"sender_jid": jidAlice})
	msgs := out["messages"].([]any)
	// Alice sent m-alice-1 and the two group messages m-g-1, m-g-3.
	if len(msgs) != 3 {
		t.Fatalf("len(messages) = %d, want 3", len(msgs))
	}
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["sender"] != jidAlice {
			t.Errorf("sender = %v, want %v", mm["sender"], jidAlice)
		}
	}
}

func TestListMessages_QueryUsesFullTextSearch(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	out := callTool(t, c, "list_messages", map[string]any{"query": "refactor"})
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(msgs))
	}
	got := msgs[0].(map[string]any)
	if got["id"] != "m-alice-2" {
		t.Errorf("id = %v, want m-alice-2", got["id"])
	}
}

func TestListMessages_MediaMetadataExposed(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	out := callTool(t, c, "list_messages", map[string]any{"chat_jid": jidBob})
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("len(messages) = %d", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if m["media_type"] != "image" {
		t.Errorf("media_type = %v, want image", m["media_type"])
	}
	if m["filename"] != "photo.jpg" {
		t.Errorf("filename = %v", m["filename"])
	}
	// JSON numbers decode as float64.
	if n, _ := m["file_length"].(float64); int64(n) != 12345 {
		t.Errorf("file_length = %v, want 12345", m["file_length"])
	}
}

func TestListMessages_AfterBeforeFilterByTimestamp(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	// After 11:00 excludes the earlier alice/bob messages.
	out := callTool(t, c, "list_messages", map[string]any{
		"after": tsGroup1.Format(time.RFC3339),
	})
	msgs := out["messages"].([]any)
	var ids []string
	for _, m := range msgs {
		ids = append(ids, m.(map[string]any)["id"].(string))
	}
	wantSet := map[string]bool{
		"m-g-2": true, "m-g-3": true, "m-g-me": true, "m-alice-2": true,
	}
	if len(ids) != len(wantSet) {
		t.Fatalf("got ids=%v, want %d entries", ids, len(wantSet))
	}
	for _, id := range ids {
		if !wantSet[id] {
			t.Errorf("unexpected id %s", id)
		}
	}
}

func TestListMessages_InvalidAfterRejected(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	err := callToolError(t, c, "list_messages", map[string]any{"after": "not-a-date"})
	if err["code"] != string(mcp.ErrInvalidArgument) {
		t.Errorf("code = %v, want %s", err["code"], mcp.ErrInvalidArgument)
	}
}

func TestGetMessageContext_ReturnsWindow(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	// Target m-g-2: one message before (m-g-1), two after (m-g-3, m-g-me).
	out := callTool(t, c, "get_message_context", map[string]any{
		"message_id": "m-g-2",
		"before":     5,
		"after":      5,
	})
	msg := out["message"].(map[string]any)
	if msg["id"] != "m-g-2" {
		t.Fatalf("message.id = %v", msg["id"])
	}
	before := out["before"].([]any)
	after := out["after"].([]any)
	if len(before) != 1 || before[0].(map[string]any)["id"] != "m-g-1" {
		t.Errorf("before = %v, want [m-g-1]", before)
	}
	afterIDs := []string{}
	for _, a := range after {
		afterIDs = append(afterIDs, a.(map[string]any)["id"].(string))
	}
	want := []string{"m-g-3", "m-g-me"}
	if strings.Join(afterIDs, ",") != strings.Join(want, ",") {
		t.Errorf("after = %v, want %v", afterIDs, want)
	}
}

func TestGetMessageContext_NotFound(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	err := callToolError(t, c, "get_message_context", map[string]any{"message_id": "does-not-exist"})
	if err["code"] != string(mcp.ErrNotFound) {
		t.Errorf("code = %v, want %s", err["code"], mcp.ErrNotFound)
	}
}

func TestGetDirectChatByContact_ExactJID(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	out := callTool(t, c, "get_direct_chat_by_contact", map[string]any{"contact_jid": jidAlice})
	if out["jid"] != jidAlice {
		t.Errorf("jid = %v", out["jid"])
	}
	if out["is_group"] != false {
		t.Errorf("is_group = %v", out["is_group"])
	}
}

func TestGetDirectChatByContact_FallsBackToPhoneSubstring(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	out := callTool(t, c, "get_direct_chat_by_contact", map[string]any{"contact_jid": "111"})
	if out["jid"] != jidAlice {
		t.Errorf("jid = %v, want %v", out["jid"], jidAlice)
	}
}

func TestGetDirectChatByContact_RejectsGroupJID(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	err := callToolError(t, c, "get_direct_chat_by_contact", map[string]any{"contact_jid": jidGroup})
	if err["code"] != string(mcp.ErrInvalidArgument) {
		t.Errorf("code = %v, want %s", err["code"], mcp.ErrInvalidArgument)
	}
}

func TestGetContactChats_IncludesDirectAndGroups(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	out := callTool(t, c, "get_contact_chats", map[string]any{"contact_jid": jidAlice})
	chats := out["chats"].([]any)
	ids := []string{}
	for _, ch := range chats {
		ids = append(ids, ch.(map[string]any)["jid"].(string))
	}
	// Alice is the direct chat jid AND a sender in the group.
	wantSet := map[string]bool{jidAlice: true, jidGroup: true}
	if len(ids) != len(wantSet) {
		t.Fatalf("ids = %v, want 2", ids)
	}
	for _, id := range ids {
		if !wantSet[id] {
			t.Errorf("unexpected chat %s", id)
		}
	}
}

func TestGetContactChats_BobHasOnlyDirectChat(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	out := callTool(t, c, "get_contact_chats", map[string]any{"contact_jid": jidBob})
	chats := out["chats"].([]any)
	ids := []string{}
	for _, ch := range chats {
		ids = append(ids, ch.(map[string]any)["jid"].(string))
	}
	// Bob: direct chat (jid matches) AND group (sender of m-g-2).
	wantSet := map[string]bool{jidBob: true, jidGroup: true}
	if len(ids) != len(wantSet) {
		t.Fatalf("ids = %v", ids)
	}
}

func TestGetLastInteraction_ReturnsMostRecentMessage(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	// Alice's latest interaction is the self-sent message in her 1:1
	// chat (m-alice-2 at 12:00) — more recent than her group messages.
	out := callTool(t, c, "get_last_interaction", map[string]any{"contact_jid": jidAlice})
	if out["id"] != "m-alice-2" {
		t.Errorf("id = %v, want m-alice-2", out["id"])
	}
}

func TestGetLastInteraction_NotFound(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	err := callToolError(t, c, "get_last_interaction", map[string]any{"contact_jid": "unknown@s.whatsapp.net"})
	if err["code"] != string(mcp.ErrNotFound) {
		t.Errorf("code = %v, want %s", err["code"], mcp.ErrNotFound)
	}
}

func TestNotPairedGatesAllCacheTools(t *testing.T) {
	t.Parallel()

	store, err := cache.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedFixtures(t, store)

	reg := mcp.NewRegistry()
	if err := mcptools.Register(reg, store); err != nil {
		t.Fatalf("Register: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv, err := mcp.New(mcp.Config{Transport: mcp.TransportStdio, Name: "x", Version: "t"}, logger, reg, mcp.NeverPaired)
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}

	cliToSrvReader, cliToSrvWriter := io.Pipe()
	srvToCliReader, srvToCliWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.ListenStdio(ctx, cliToSrvReader, srvToCliWriter) }()
	defer func() {
		cancel()
		_ = cliToSrvReader.Close()
		_ = cliToSrvWriter.Close()
		_ = srvToCliReader.Close()
		_ = srvToCliWriter.Close()
		wg.Wait()
	}()

	tr := mcpclienttransport.NewIO(srvToCliReader, pipeWriteCloser{cliToSrvWriter}, pipeReadCloser{io.NopCloser(strings.NewReader(""))})
	client := mcpclient.NewClient(tr)
	defer client.Close()

	startCtx, startCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer startCancel()
	if err := client.Start(startCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test-client", Version: "0"}
	if _, err := client.Initialize(startCtx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	names := []string{
		"list_chats", "get_chat", "list_messages", "get_message_context",
		"get_direct_chat_by_contact", "get_contact_chats", "get_last_interaction",
	}
	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Name = name
			// Pass dummy required args so we aren't rejected on schema.
			req.Params.Arguments = map[string]any{
				"chat_jid":    "x@s.whatsapp.net",
				"contact_jid": "x@s.whatsapp.net",
				"message_id":  "x",
			}
			res, err := client.CallTool(startCtx, req)
			if err != nil {
				t.Fatalf("CallTool %s: %v", name, err)
			}
			if !res.IsError {
				t.Fatalf("%s: expected IsError=true", name)
			}
			m := res.StructuredContent.(map[string]any)
			if m["code"] != string(mcp.ErrNotPaired) {
				t.Errorf("%s: code = %v, want %s", name, m["code"], mcp.ErrNotPaired)
			}
		})
	}
}

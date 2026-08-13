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

func TestListMessages_EnrichedFields(t *testing.T) {
	t.Parallel()
	// Seed a contact row for Alice so her sender JID resolves to a display
	// name; Bob has no contact row, so his sender_name must stay null.
	c := newServerAndClientWithExtras(t, func(s *cache.Store) {
		if err := s.UpsertContact(context.Background(), cache.Contact{
			JID:      jidAlice,
			FullName: "Alice Anderson",
		}); err != nil {
			t.Fatalf("seed contact: %v", err)
		}
	})

	// Alice's 1:1 chat: m-alice-2 is from self (outgoing), m-alice-1 from Alice (incoming).
	out := callTool(t, c, "list_messages", map[string]any{"chat_jid": jidAlice})
	msgs := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(msgs))
	}

	// Newest-first: m-alice-2 (self) then m-alice-1 (Alice).
	outgoing := msgs[0].(map[string]any)
	if outgoing["id"] != "m-alice-2" {
		t.Fatalf("messages[0].id = %v, want m-alice-2", outgoing["id"])
	}
	if outgoing["direction"] != "outgoing" {
		t.Errorf("direction = %v, want outgoing (is_from_me message)", outgoing["direction"])
	}
	if outgoing["delivery_status"] != "unknown" {
		t.Errorf("delivery_status = %v, want unknown (no receipt data cached)", outgoing["delivery_status"])
	}

	incoming := msgs[1].(map[string]any)
	if incoming["id"] != "m-alice-1" {
		t.Fatalf("messages[1].id = %v, want m-alice-1", incoming["id"])
	}
	if incoming["direction"] != "incoming" {
		t.Errorf("direction = %v, want incoming", incoming["direction"])
	}
	if incoming["sender_name"] != "Alice Anderson" {
		t.Errorf("sender_name = %v, want Alice Anderson", incoming["sender_name"])
	}

	// Bob has no contact row → sender_name is null.
	bobOut := callTool(t, c, "list_messages", map[string]any{"chat_jid": jidBob})
	bobMsgs := bobOut["messages"].([]any)
	if len(bobMsgs) != 1 {
		t.Fatalf("len(bob messages) = %d, want 1", len(bobMsgs))
	}
	bob := bobMsgs[0].(map[string]any)
	if v, ok := bob["sender_name"]; !ok || v != nil {
		t.Errorf("sender_name = %v (present=%v), want null", v, ok)
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

// seedCrossJIDContact adds a contact ("Carol") whose messages are split
// across a phone-number JID 1:1 chat and a separate privacy-LID 1:1 chat,
// linked through jid_aliases. The LID's user component deliberately shares no
// digits with the phone number, so the merge can only happen via the alias
// table (not by accidental substring matching).
const (
	jidCarolPN  = "333@s.whatsapp.net"
	jidCarolLID = "888777@lid"
)

var (
	tsCarolPN1  = time.Date(2024, 6, 2, 10, 0, 0, 0, time.UTC)
	tsCarolLID1 = time.Date(2024, 6, 2, 10, 5, 0, 0, time.UTC)
	tsCarolPN2  = time.Date(2024, 6, 2, 10, 10, 0, 0, time.UTC)
	tsCarolLID2 = time.Date(2024, 6, 2, 10, 20, 0, 0, time.UTC)
)

func seedCrossJIDContact(t *testing.T, store *cache.Store) {
	t.Helper()
	ctx := context.Background()

	for _, c := range []cache.Chat{
		{JID: jidCarolPN, Name: "Carol", LastMessageTS: tsCarolPN2},
		{JID: jidCarolLID, Name: "Carol", LastMessageTS: tsCarolLID2},
	} {
		if err := store.UpsertChat(ctx, c); err != nil {
			t.Fatalf("seed cross-jid chat %s: %v", c.JID, err)
		}
	}
	// Resolve Carol's phone JID to a display name; the LID has no contact row.
	if err := store.UpsertContact(ctx, cache.Contact{JID: jidCarolPN, FullName: "Carol Carter"}); err != nil {
		t.Fatalf("seed carol contact: %v", err)
	}
	if err := store.UpsertJIDAlias(ctx, jidCarolLID, jidCarolPN); err != nil {
		t.Fatalf("seed carol alias: %v", err)
	}

	msgs := []cache.Message{
		{ID: "c-pn-1", ChatJID: jidCarolPN, SenderJID: jidCarolPN, Timestamp: tsCarolPN1, Kind: cache.KindText, Body: "hi from my phone"},
		{ID: "c-lid-1", ChatJID: jidCarolLID, SenderJID: jidCarolLID, Timestamp: tsCarolLID1, Kind: cache.KindText, Body: "and from my lid"},
		{ID: "c-pn-2", ChatJID: jidCarolPN, SenderJID: jidSelf, Timestamp: tsCarolPN2, Kind: cache.KindText, Body: "got it (phone)", IsFromMe: true},
		{ID: "c-lid-2", ChatJID: jidCarolLID, SenderJID: jidSelf, Timestamp: tsCarolLID2, Kind: cache.KindText, Body: "got it (lid)", IsFromMe: true},
	}
	for _, m := range msgs {
		if err := store.InsertMessage(ctx, m); err != nil {
			t.Fatalf("seed cross-jid message %s: %v", m.ID, err)
		}
	}
}

func TestGetConversation_MergesAcrossPhoneAndLID(t *testing.T) {
	t.Parallel()
	c := newServerAndClientWithExtras(t, func(s *cache.Store) { seedCrossJIDContact(t, s) })

	// Pass the bare phone number: only the phone JID matches directly; the LID
	// thread is pulled in solely through the alias link.
	out := callTool(t, c, "get_conversation", map[string]any{"contact": "333"})

	// jids surfaces both merged identities.
	rawJIDs, _ := out["jids"].([]any)
	gotJIDs := map[string]bool{}
	for _, j := range rawJIDs {
		gotJIDs[j.(string)] = true
	}
	if !gotJIDs[jidCarolPN] || !gotJIDs[jidCarolLID] {
		t.Fatalf("jids = %v, want both %s and %s", rawJIDs, jidCarolPN, jidCarolLID)
	}

	msgs := out["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("len(messages) = %d, want 4 (both threads merged)", len(msgs))
	}

	// Strict newest-first interleave across the two chats.
	wantOrder := []string{"c-lid-2", "c-pn-2", "c-lid-1", "c-pn-1"}
	seen := map[string]bool{}
	for i, m := range msgs {
		mm := m.(map[string]any)
		id := mm["id"].(string)
		if id != wantOrder[i] {
			t.Errorf("messages[%d].id = %s, want %s", i, id, wantOrder[i])
		}
		if seen[id] {
			t.Errorf("duplicate message %s in merged timeline", id)
		}
		seen[id] = true
	}

	// Enriched fields survive the merge.
	last := msgs[len(msgs)-1].(map[string]any) // c-pn-1, incoming from Carol's phone JID
	if last["direction"] != "incoming" {
		t.Errorf("c-pn-1 direction = %v, want incoming", last["direction"])
	}
	if last["sender_name"] != "Carol Carter" {
		t.Errorf("c-pn-1 sender_name = %v, want Carol Carter", last["sender_name"])
	}
	first := msgs[0].(map[string]any) // c-lid-2, outgoing
	if first["direction"] != "outgoing" {
		t.Errorf("c-lid-2 direction = %v, want outgoing", first["direction"])
	}
	if first["delivery_status"] != "unknown" {
		t.Errorf("c-lid-2 delivery_status = %v, want unknown", first["delivery_status"])
	}
}

func TestGetConversation_SingleIdentityNoAlias(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	// Bob has no alias: the conversation is still resolved from his single JID,
	// covering both his 1:1 thread (m-bob-1) and his line in the group (m-g-2).
	out := callTool(t, c, "get_conversation", map[string]any{"contact": jidBob})
	if jids := out["jids"].([]any); len(jids) != 1 || jids[0] != jidBob {
		t.Fatalf("jids = %v, want [%s]", out["jids"], jidBob)
	}
	ids := map[string]bool{}
	for _, m := range out["messages"].([]any) {
		ids[m.(map[string]any)["id"].(string)] = true
	}
	if !ids["m-bob-1"] || !ids["m-g-2"] {
		t.Errorf("messages = %v, want both m-bob-1 and m-g-2", ids)
	}
}

func TestGetConversation_RejectsEmptyContact(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	err := callToolError(t, c, "get_conversation", map[string]any{"contact": "   "})
	if err["code"] != string(mcp.ErrInvalidArgument) {
		t.Errorf("code = %v, want %s", err["code"], mcp.ErrInvalidArgument)
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
		"list_chats", "list_conversations", "get_chat", "list_messages", "get_message_context",
		"get_direct_chat_by_contact", "get_contact_chats", "get_last_interaction",
		"get_conversation",
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
				"contact":     "x@s.whatsapp.net",
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

// conversationsByJID indexes a list_conversations result by its representative
// jid for convenient lookup, and fails if the same jid appears twice (a merge
// regression would surface a contact under both identities).
func conversationsByJID(t *testing.T, out map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := out["conversations"].([]any)
	if !ok {
		t.Fatalf("conversations not an array: %T", out["conversations"])
	}
	byJID := map[string]map[string]any{}
	for _, c := range raw {
		cm := c.(map[string]any)
		jid := cm["jid"].(string)
		if _, dup := byJID[jid]; dup {
			t.Fatalf("conversation jid %s appears twice — merge regression", jid)
		}
		byJID[jid] = cm
	}
	return byJID
}

func jidSet(t *testing.T, conv map[string]any) map[string]bool {
	t.Helper()
	raw, ok := conv["jids"].([]any)
	if !ok {
		t.Fatalf("jids not an array: %T", conv["jids"])
	}
	set := map[string]bool{}
	for _, j := range raw {
		set[j.(string)] = true
	}
	return set
}

func TestListConversations_MergesLinkedIdentities(t *testing.T) {
	t.Parallel()
	c := newServerAndClientWithExtras(t, func(s *cache.Store) { seedCrossJIDContact(t, s) })

	out := callTool(t, c, "list_conversations", map[string]any{})
	byJID := conversationsByJID(t, out)

	// Carol surfaces ONCE, keyed on her phone JID, with both identities in jids.
	carol, ok := byJID[jidCarolPN]
	if !ok {
		t.Fatalf("Carol not found under phone JID %s; conversations=%v", jidCarolPN, out["conversations"])
	}
	if _, dup := byJID[jidCarolLID]; dup {
		t.Fatalf("Carol's LID %s leaked as a separate conversation", jidCarolLID)
	}
	ids := jidSet(t, carol)
	if !ids[jidCarolPN] || !ids[jidCarolLID] {
		t.Errorf("Carol jids = %v, want both %s and %s", carol["jids"], jidCarolPN, jidCarolLID)
	}
	// Preview is the newest message across BOTH threads (the LID thread, 10:20).
	if carol["last_message"] != "got it (lid)" {
		t.Errorf("Carol last_message = %v, want 'got it (lid)'", carol["last_message"])
	}

	// The un-aliased fixtures pass through as their own single-identity rows.
	for _, jid := range []string{jidAlice, jidBob, jidGroup} {
		conv, ok := byJID[jid]
		if !ok {
			t.Fatalf("%s missing from conversations", jid)
		}
		if ids := jidSet(t, conv); len(ids) != 1 || !ids[jid] {
			t.Errorf("%s jids = %v, want [%s]", jid, conv["jids"], jid)
		}
	}
}

func TestListConversations_SumsUnreadAcrossIdentities(t *testing.T) {
	t.Parallel()
	c := newServerAndClientWithExtras(t, func(s *cache.Store) {
		seedCrossJIDContact(t, s)
		ctx := context.Background()
		// Re-upsert both of Carol's chats with unread counts (same timestamps so
		// the merge ordering is unchanged); the merged row should sum them.
		if err := s.UpsertChat(ctx, cache.Chat{JID: jidCarolPN, Name: "Carol", LastMessageTS: tsCarolPN2, UnreadCount: 2}); err != nil {
			t.Fatalf("unread pn: %v", err)
		}
		if err := s.UpsertChat(ctx, cache.Chat{JID: jidCarolLID, Name: "Carol", LastMessageTS: tsCarolLID2, UnreadCount: 3}); err != nil {
			t.Fatalf("unread lid: %v", err)
		}
	})

	out := callTool(t, c, "list_conversations", map[string]any{})
	carol := conversationsByJID(t, out)[jidCarolPN]
	if carol == nil {
		t.Fatalf("Carol not found; conversations=%v", out["conversations"])
	}
	// JSON numbers decode as float64.
	if got := carol["unread_count"].(float64); got != 5 {
		t.Errorf("Carol unread_count = %v, want 5 (2+3 merged)", got)
	}
}

func TestListConversations_PaginationCountsMergedRowAsOne(t *testing.T) {
	t.Parallel()
	c := newServerAndClientWithExtras(t, func(s *cache.Store) { seedCrossJIDContact(t, s) })

	// Carol's last activity (Jun 2) is the most recent, so limit=1 returns her
	// single merged row — proving the merge happens before the page slice (a
	// pre-merge LIMIT 1 would return one half of the split instead).
	out := callTool(t, c, "list_conversations", map[string]any{"limit": 1, "page": 0})
	conv := out["conversations"].([]any)
	if len(conv) != 1 {
		t.Fatalf("len(conversations) = %d, want 1", len(conv))
	}
	first := conv[0].(map[string]any)
	if first["jid"] != jidCarolPN {
		t.Errorf("page0 jid = %v, want %s", first["jid"], jidCarolPN)
	}
	if ids := jidSet(t, first); !ids[jidCarolPN] || !ids[jidCarolLID] {
		t.Errorf("page0 jids = %v, want both Carol identities", first["jids"])
	}
}

func TestListConversations_SortByName(t *testing.T) {
	t.Parallel()
	c := newServerAndClientWithExtras(t, func(s *cache.Store) { seedCrossJIDContact(t, s) })

	out := callTool(t, c, "list_conversations", map[string]any{"sort_by": "name"})
	conv := out["conversations"].([]any)
	var names []string
	for _, x := range conv {
		if n, ok := x.(map[string]any)["name"].(string); ok {
			names = append(names, n)
		}
	}
	want := []string{"Alice", "Bob", "Carol", "Friends"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("name order = %v, want %v", names, want)
	}
}

func TestListConversations_QueryMatchesNameAndMergedJID(t *testing.T) {
	t.Parallel()
	c := newServerAndClientWithExtras(t, func(s *cache.Store) { seedCrossJIDContact(t, s) })

	// Match by name.
	byName := conversationsByJID(t, callTool(t, c, "list_conversations", map[string]any{"query": "carol"}))
	if len(byName) != 1 || byName[jidCarolPN] == nil {
		t.Errorf("query 'carol' = %v, want only merged Carol row", byName)
	}

	// Match by a substring of the *LID* identity — proving the filter sees the
	// full merged identity set, not just the representative phone JID.
	byLID := conversationsByJID(t, callTool(t, c, "list_conversations", map[string]any{"query": "888777"}))
	if len(byLID) != 1 || byLID[jidCarolPN] == nil {
		t.Errorf("query '888777' = %v, want merged Carol row matched via LID", byLID)
	}
}

func TestListConversations_FilterByChatType(t *testing.T) {
	t.Parallel()
	c := newServerAndClientWithExtras(t, func(s *cache.Store) { seedCrossJIDContact(t, s) })

	byJID := conversationsByJID(t, callTool(t, c, "list_conversations", map[string]any{"chat_type": "direct"}))
	if _, ok := byJID[jidGroup]; ok {
		t.Errorf("chat_type=direct returned the group %s", jidGroup)
	}
	for _, jid := range []string{jidAlice, jidBob, jidCarolPN} {
		if _, ok := byJID[jid]; !ok {
			t.Errorf("chat_type=direct missing %s", jid)
		}
	}
}

func TestListConversations_RejectsInvalidSortBy(t *testing.T) {
	t.Parallel()
	c := newServerAndClient(t)

	err := callToolError(t, c, "list_conversations", map[string]any{"sort_by": "bogus"})
	if err["code"] != string(mcp.ErrInvalidArgument) {
		t.Errorf("code = %v, want %s", err["code"], mcp.ErrInvalidArgument)
	}
}

// Untitled direct chats (chats.name == ”) fall back to the contact's display
// name so 1:1 conversations aren't surfaced with name=null when contact info is
// cached. The fallback is scoped to chat_type=='direct'; groups keep null.
func TestListConversations_UntitledDirectFallsBackToContactName(t *testing.T) {
	t.Parallel()
	const (
		jidDanPN  = "445@s.whatsapp.net" // untitled phone-JID chat, has a contact row
		jidDanLID = "778899@lid"         // untitled LID chat, NO contact row, newer (winner)
		jidErin   = "556@s.whatsapp.net" // untitled direct chat, no contact → stays null
		jidNoName = "667@g.us"           // untitled group, even with a stray contact row → null
	)
	tsDanPN := time.Date(2024, 6, 3, 10, 0, 0, 0, time.UTC)
	tsDanLID := time.Date(2024, 6, 3, 11, 0, 0, 0, time.UTC) // newer → LID row wins
	tsErin := time.Date(2024, 6, 3, 9, 0, 0, 0, time.UTC)
	tsGrp := time.Date(2024, 6, 3, 8, 0, 0, 0, time.UTC)

	c := newServerAndClientWithExtras(t, func(s *cache.Store) {
		ctx := context.Background()
		chats := []cache.Chat{
			{JID: jidDanPN, LastMessageTS: tsDanPN},   // name=""
			{JID: jidDanLID, LastMessageTS: tsDanLID}, // name=""
			{JID: jidErin, LastMessageTS: tsErin},     // name=""
			{JID: jidNoName, IsGroup: true, LastMessageTS: tsGrp},
		}
		for _, ch := range chats {
			if err := s.UpsertChat(ctx, ch); err != nil {
				t.Fatalf("seed chat %s: %v", ch.JID, err)
			}
		}
		// Contact name lives on the phone JID, but the *winner* row is the LID —
		// proving the merge picks the name up from any member, not just the winner.
		if err := s.UpsertContact(ctx, cache.Contact{JID: jidDanPN, PushName: "Dan Dawson"}); err != nil {
			t.Fatalf("seed dan contact: %v", err)
		}
		// A stray contact row keyed on the group JID must NOT leak into the title.
		if err := s.UpsertContact(ctx, cache.Contact{JID: jidNoName, PushName: "ShouldNotShow"}); err != nil {
			t.Fatalf("seed group contact: %v", err)
		}
		if err := s.UpsertJIDAlias(ctx, jidDanLID, jidDanPN); err != nil {
			t.Fatalf("seed dan alias: %v", err)
		}
	})

	byJID := conversationsByJID(t, callTool(t, c, "list_conversations", map[string]any{}))

	dan, ok := byJID[jidDanPN]
	if !ok {
		t.Fatalf("Dan not found under phone JID %s", jidDanPN)
	}
	if dan["name"] != "Dan Dawson" {
		t.Errorf("Dan name = %v, want 'Dan Dawson' (resolved from contact on the PN member)", dan["name"])
	}

	if erin := byJID[jidErin]; erin == nil {
		t.Fatalf("Erin not found")
	} else if erin["name"] != nil {
		t.Errorf("Erin name = %v, want null (untitled direct, no contact)", erin["name"])
	}

	if grp := byJID[jidNoName]; grp == nil {
		t.Fatalf("untitled group not found")
	} else if grp["name"] != nil {
		t.Errorf("group name = %v, want null (fallback must not apply to groups)", grp["name"])
	}

	// The fallback name participates in search and sort, fixing the documented
	// "1:1 chats have no title so search misses them" limitation.
	found := conversationsByJID(t, callTool(t, c, "list_conversations", map[string]any{"query": "dawson"}))
	if len(found) != 1 || found[jidDanPN] == nil {
		t.Errorf("query 'dawson' = %v, want only Dan's merged row", found)
	}
}

// get_contact_chats applies the same untitled-direct fallback so the canonical
// "conversation with this person" entry point labels the chat.
func TestGetContactChats_UntitledDirectFallsBackToContactName(t *testing.T) {
	t.Parallel()
	const (
		jidGus = "557@s.whatsapp.net" // untitled direct chat with a contact row
		jidHal = "558@s.whatsapp.net" // untitled direct chat, no contact → null
	)
	c := newServerAndClientWithExtras(t, func(s *cache.Store) {
		ctx := context.Background()
		for _, ch := range []cache.Chat{
			{JID: jidGus, LastMessageTS: time.Date(2024, 6, 4, 10, 0, 0, 0, time.UTC)},
			{JID: jidHal, LastMessageTS: time.Date(2024, 6, 4, 11, 0, 0, 0, time.UTC)},
		} {
			if err := s.UpsertChat(ctx, ch); err != nil {
				t.Fatalf("seed chat %s: %v", ch.JID, err)
			}
		}
		if err := s.UpsertContact(ctx, cache.Contact{JID: jidGus, FullName: "Gus Green"}); err != nil {
			t.Fatalf("seed gus contact: %v", err)
		}
	})

	gus := callTool(t, c, "get_contact_chats", map[string]any{"contact_jid": jidGus})
	gusChats := gus["chats"].([]any)
	if len(gusChats) != 1 || gusChats[0].(map[string]any)["name"] != "Gus Green" {
		t.Errorf("Gus chats = %v, want one chat named 'Gus Green'", gusChats)
	}

	hal := callTool(t, c, "get_contact_chats", map[string]any{"contact_jid": jidHal})
	halChats := hal["chats"].([]any)
	if len(halChats) != 1 || halChats[0].(map[string]any)["name"] != nil {
		t.Errorf("Hal chats = %v, want one chat with name=null", halChats)
	}
}

// seedReactions adds reactions to the group fixture: two on m-g-1 (one from
// Alice, one of ours) and none anywhere else, so tests can assert both the
// populated and the omitted case.
func seedReactions(t *testing.T, s *cache.Store) {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertContact(ctx, cache.Contact{JID: jidAlice, FullName: "Alice Anderson"}); err != nil {
		t.Fatalf("seed reaction contact: %v", err)
	}
	rs := []cache.Reaction{
		{ChatJID: jidGroup, TargetID: "m-g-1", SenderJID: jidAlice, Emoji: "👍", Timestamp: tsGroup2},
		{ChatJID: jidGroup, TargetID: "m-g-1", Emoji: "🎉", Timestamp: tsGroup3, IsFromMe: true},
		{ChatJID: jidGroup, TargetID: "m-g-2", SenderJID: jidBob, Emoji: "😂", Timestamp: tsGroup3},
	}
	for _, r := range rs {
		if err := s.UpsertReaction(ctx, r); err != nil {
			t.Fatalf("seed reaction %s/%s: %v", r.TargetID, r.Emoji, err)
		}
	}
}

// messageByID picks one message out of a list_messages-style array.
func messageByID(t *testing.T, msgs []any, id string) map[string]any {
	t.Helper()
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["id"] == id {
			return mm
		}
	}
	t.Fatalf("message %q not in result", id)
	return nil
}

func TestListMessages_SurfacesReactions(t *testing.T) {
	t.Parallel()
	c := newServerAndClientWithExtras(t, func(s *cache.Store) { seedReactions(t, s) })

	out := callTool(t, c, "list_messages", map[string]any{"chat_jid": jidGroup})
	msgs := out["messages"].([]any)

	target := messageByID(t, msgs, "m-g-1")
	reactions, ok := target["reactions"].([]any)
	if !ok {
		t.Fatalf("m-g-1 has no reactions array: %+v", target)
	}
	if len(reactions) != 2 {
		t.Fatalf("len(reactions) = %d, want 2", len(reactions))
	}
	// Ordered by reaction timestamp: Alice's 👍 then our own 🎉.
	alice := reactions[0].(map[string]any)
	if alice["emoji"] != "👍" {
		t.Errorf("reactions[0].emoji = %v, want 👍", alice["emoji"])
	}
	if alice["sender"] != jidAlice {
		t.Errorf("reactions[0].sender = %v, want %v", alice["sender"], jidAlice)
	}
	if alice["sender_name"] != "Alice Anderson" {
		t.Errorf("reactions[0].sender_name = %v, want Alice Anderson", alice["sender_name"])
	}
	if alice["is_from_me"] != false {
		t.Errorf("reactions[0].is_from_me = %v, want false", alice["is_from_me"])
	}

	mine := reactions[1].(map[string]any)
	if mine["emoji"] != "🎉" {
		t.Errorf("reactions[1].emoji = %v, want 🎉", mine["emoji"])
	}
	if mine["is_from_me"] != true {
		t.Errorf("reactions[1].is_from_me = %v, want true", mine["is_from_me"])
	}
	// Our own reaction is stored under the canonical empty sender.
	if mine["sender"] != "" {
		t.Errorf("reactions[1].sender = %v, want empty", mine["sender"])
	}
	if mine["sender_name"] != nil {
		t.Errorf("reactions[1].sender_name = %v, want null", mine["sender_name"])
	}
}

func TestListMessages_OmitsReactionsWhenNone(t *testing.T) {
	t.Parallel()
	c := newServerAndClientWithExtras(t, func(s *cache.Store) { seedReactions(t, s) })

	out := callTool(t, c, "list_messages", map[string]any{"chat_jid": jidGroup})
	msgs := out["messages"].([]any)

	unreacted := messageByID(t, msgs, "m-g-3")
	if _, present := unreacted["reactions"]; present {
		t.Errorf("m-g-3 carries a reactions key; want it omitted entirely: %+v", unreacted)
	}
}

func TestGetMessageContext_SurfacesReactionsAcrossWindow(t *testing.T) {
	t.Parallel()
	c := newServerAndClientWithExtras(t, func(s *cache.Store) { seedReactions(t, s) })

	// Target m-g-2 (reacted by Bob); m-g-1 sits in the before-window and has
	// two reactions of its own.
	out := callTool(t, c, "get_message_context", map[string]any{"message_id": "m-g-2"})

	target := out["message"].(map[string]any)
	targetReactions, ok := target["reactions"].([]any)
	if !ok || len(targetReactions) != 1 {
		t.Fatalf("target reactions = %+v, want 1", target["reactions"])
	}
	if targetReactions[0].(map[string]any)["emoji"] != "😂" {
		t.Errorf("target reaction emoji = %v, want 😂", targetReactions[0].(map[string]any)["emoji"])
	}

	before := out["before"].([]any)
	prev := messageByID(t, before, "m-g-1")
	if got, ok := prev["reactions"].([]any); !ok || len(got) != 2 {
		t.Errorf("before m-g-1 reactions = %+v, want 2", prev["reactions"])
	}

	after := out["after"].([]any)
	next := messageByID(t, after, "m-g-3")
	if _, present := next["reactions"]; present {
		t.Errorf("after m-g-3 carries reactions; want omitted: %+v", next)
	}
}

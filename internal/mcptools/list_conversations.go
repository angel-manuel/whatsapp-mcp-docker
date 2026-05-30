package mcptools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

const listConversationsDefaultLimit = 20

var listConversationsInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query":                {"type": ["string","null"], "description": "Case-insensitive substring match on the conversation's title or any of its merged JIDs. Like list_chats, 1:1 chats usually have no title, so searching a person's name here may miss their direct conversation — use search_contacts → get_conversation instead. query is for named groups."},
    "limit":                {"type": "integer", "minimum": 1, "maximum": 200, "default": 20},
    "page":                 {"type": "integer", "minimum": 0, "default": 0, "description": "0-based page index. Pagination is applied AFTER linked identities are merged, so a split contact counts as one item."},
    "include_last_message": {"type": "boolean", "default": true},
    "sort_by":              {"type": "string", "enum": ["last_active","name"], "default": "last_active", "description": "Result ordering. Default 'last_active' => most-recently-active-first; 'name' => A-Z by title (untitled conversations last)."},
    "chat_type":            {"type": ["string","null"], "enum": ["direct","group","newsletter","community",null], "description": "Filter to a single chat type."}
  },
  "additionalProperties": false
}`)

var listConversationsOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "conversations": {
      "type": "array",
      "items": ` + conversationSchemaFragment + `
    }
  },
  "required": ["conversations"]
}`)

type listConversationsInput struct {
	Query              *string `json:"query"`
	Limit              int     `json:"limit"`
	Page               int     `json:"page"`
	IncludeLastMessage *bool   `json:"include_last_message"`
	SortBy             string  `json:"sort_by"`
	ChatType           *string `json:"chat_type"`
}

type listConversationsOutput struct {
	Conversations []ConversationDTO `json:"conversations"`
}

// list_conversations is the list-level analog of get_conversation. list_chats
// returns one row per chat row, so a contact split across a phone JID
// (…@s.whatsapp.net) and a privacy LID (…@lid) shows up as two separate
// rows — "show me my recent conversations" double-counts them. This tool
// collapses those linked direct chats into one logical conversation (merged
// jids, summed unread_count, newest last-message preview across the merge),
// while groups/newsletters — which already have a single JID — pass through
// unchanged. list_chats is left intact for power users who want raw rows.
//
// Because a merged page cannot be expressed with SQL LIMIT/OFFSET (two rows
// collapse to one), the handler loads the full filtered chat set, merges, then
// paginates in Go. The chat table is a bounded local cache, so this is cheap;
// only two reads are issued (chats + the small jid_aliases table), avoiding an
// N-per-chat ResolveLinkedJIDs fan-out.
func registerListConversations(reg *mcp.Registry, store *cache.Store) error {
	return reg.Register(mcp.Tool{
		Name: "list_conversations",
		Description: "List WhatsApp conversations cached locally — the de-duplicated overview and " +
			"the front door for \"show me my recent conversations\". Like list_chats but collapses a " +
			"contact's linked phone JID (…@s.whatsapp.net) and privacy LID (…@lid) direct chats into " +
			"ONE row (list_chats double-counts these as two), surfacing the merged identities as `jids`, " +
			"a summed `unread_count`, and the newest last-message preview across the merge. Groups and " +
			"newsletters pass through unchanged. Supports substring search (`query`, not `search`), " +
			"pagination (applied AFTER merging, so a split contact counts as one item), and sort_by " +
			"last_active (default) or name. To drill into one conversation's full timeline use " +
			"get_conversation. " +
			"Served from local cache, which may lag live WhatsApp. An empty result can mean " +
			"'not yet synced,' not 'no message.' For a guaranteed-fresh read: cache_sync → " +
			"poll cache_sync_status until finished → read.",
		InputSchema:  listConversationsInputSchema,
		OutputSchema: listConversationsOutputSchema,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in listConversationsInput
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return mcp.InvalidArgumentError(fmt.Sprintf("decode arguments: %v", err)), nil
				}
			}
			return handleListConversations(ctx, store, in)
		},
	})
}

// chatRow is one scanned chats row, carried through the merge before it
// becomes a ConversationDTO.
type chatRow struct {
	jid        string
	name       string
	isGroup    bool
	chatType   string
	ts         int64
	unread     int
	hasMessage bool
	body       string
	id         string
	sender     string
	isFromMe   bool
}

// conversationGroup accumulates every chat row that shares a canonical
// identity (phone JID). The winner — the member with the greatest
// last_message_ts — supplies the name and last-message preview.
type conversationGroup struct {
	canonical string
	winner    chatRow
	unread    int
	members   map[string]bool
}

func handleListConversations(ctx context.Context, store *cache.Store, in listConversationsInput) (any, error) {
	limit, offset, perr := validatePagination(in.Limit, in.Page, listConversationsDefaultLimit)
	if perr != nil {
		return mcp.InvalidArgumentError(perr.Message), nil
	}

	include := true
	if in.IncludeLastMessage != nil {
		include = *in.IncludeLastMessage
	}
	sortBy := strings.TrimSpace(in.SortBy)
	if sortBy == "" {
		sortBy = "last_active"
	}
	if sortBy != "last_active" && sortBy != "name" {
		return mcp.InvalidArgumentError(fmt.Sprintf("sort_by must be 'last_active' or 'name', got %q", sortBy)), nil
	}
	if in.ChatType != nil && *in.ChatType != "" {
		switch *in.ChatType {
		case "direct", "group", "newsletter", "community":
		default:
			return mcp.InvalidArgumentError(fmt.Sprintf("chat_type must be direct|group|newsletter|community, got %q", *in.ChatType)), nil
		}
	}

	// Load the alias table once. canonicalOf maps any LID to its phone JID
	// (and leaves un-aliased JIDs mapping to themselves); identitiesOf gives
	// the full identity set per phone JID so `jids` lists every linked
	// identity even when only one side has a chat row — matching the set
	// get_conversation derives from cache.ResolveLinkedJIDs.
	canonicalOf, identitiesOf, err := loadAliasMaps(ctx, store)
	if err != nil {
		return mcp.InternalError(fmt.Sprintf("list_conversations aliases: %v", err)), nil
	}

	rows, err := loadConversationChats(ctx, store, include, in.ChatType)
	if err != nil {
		return mcp.InternalError(fmt.Sprintf("list_conversations: %v", err)), nil
	}

	// Group rows by canonical identity, newest member winning the preview.
	groups := map[string]*conversationGroup{}
	var order []string
	for _, r := range rows {
		key := r.jid
		if c, ok := canonicalOf[r.jid]; ok {
			key = c
		}
		g, ok := groups[key]
		if !ok {
			g = &conversationGroup{canonical: key, members: map[string]bool{}}
			groups[key] = g
			order = append(order, key)
		}
		g.unread += r.unread
		g.members[r.jid] = true
		// The query orders by last_message_ts DESC, so the first row seen for
		// a group is its newest; only overwrite the winner if a later row is
		// strictly newer (defensive — keeps the merge order-independent).
		if g.winner.jid == "" || r.ts > g.winner.ts {
			g.winner = r
		} else if r.ts == g.winner.ts && g.winner.name == "" && r.name != "" {
			// Tie on ts: prefer a member that actually carries a name.
			g.winner.name = r.name
		}
	}

	conversations := make([]ConversationDTO, 0, len(order))
	for _, key := range order {
		g := groups[key]
		dto := ConversationDTO{
			ChatDTO: buildChatDTO(g.canonical, g.winner.name, g.winner.isGroup, g.winner.chatType,
				g.winner.ts, include && g.winner.hasMessage, g.winner.body, g.winner.id,
				g.winner.sender, g.winner.isFromMe),
			JIDs:        mergedIdentities(g, identitiesOf),
			UnreadCount: g.unread,
		}
		conversations = append(conversations, dto)
	}

	conversations = filterConversations(conversations, in.Query)
	sortConversations(conversations, sortBy)
	conversations = paginate(conversations, offset, limit)

	return listConversationsOutput{Conversations: conversations}, nil
}

// loadAliasMaps reads jid_aliases once and returns (canonicalOf, identitiesOf).
// canonicalOf[lid] = pn for every recorded pair; identitiesOf[pn] = {pn, …lids}.
func loadAliasMaps(ctx context.Context, store *cache.Store) (map[string]string, map[string][]string, error) {
	rows, err := store.DB().QueryContext(ctx, `SELECT lid_jid, pn_jid FROM jid_aliases`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	canonicalOf := map[string]string{}
	idSet := map[string]map[string]bool{}
	for rows.Next() {
		var lid, pn string
		if err := rows.Scan(&lid, &pn); err != nil {
			return nil, nil, err
		}
		if lid == "" || pn == "" {
			continue
		}
		canonicalOf[lid] = pn
		if idSet[pn] == nil {
			idSet[pn] = map[string]bool{pn: true}
		}
		idSet[pn][lid] = true
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	identitiesOf := make(map[string][]string, len(idSet))
	for pn, set := range idSet {
		ids := make([]string, 0, len(set))
		for j := range set {
			if j != pn {
				ids = append(ids, j)
			}
		}
		sort.Strings(ids)
		identitiesOf[pn] = append([]string{pn}, ids...) // phone JID first
	}
	return canonicalOf, identitiesOf, nil
}

// loadConversationChats issues the list_chats SELECT shape (plus unread_count),
// optionally filtered by chat_type, with no LIMIT/OFFSET so the full set is
// available for merging. The chat_type filter is safe pre-merge: linked direct
// chats all share chat_type='direct', so it never splits a group.
func loadConversationChats(ctx context.Context, store *cache.Store, include bool, chatType *string) ([]chatRow, error) {
	var (
		where  string
		params []any
	)
	if chatType != nil && *chatType != "" {
		where = "WHERE c.chat_type = ?"
		params = append(params, *chatType)
	}

	var query string
	if include {
		query = fmt.Sprintf(`
SELECT c.jid, c.name, c.is_group, c.chat_type, c.last_message_ts, c.unread_count,
       COALESCE(m.body, ''), COALESCE(m.id, ''), COALESCE(m.sender_jid, ''),
       COALESCE(m.is_from_me, 0), CASE WHEN m.id IS NULL THEN 0 ELSE 1 END
FROM chats c
LEFT JOIN messages m
       ON m.chat_jid = c.jid
      AND m.ts = c.last_message_ts
%s
ORDER BY c.last_message_ts DESC, c.jid ASC`, where)
	} else {
		query = fmt.Sprintf(`
SELECT c.jid, c.name, c.is_group, c.chat_type, c.last_message_ts, c.unread_count,
       '', '', '', 0, 0
FROM chats c
%s
ORDER BY c.last_message_ts DESC, c.jid ASC`, where)
	}

	rows, err := store.DB().QueryContext(ctx, query, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []chatRow
	for rows.Next() {
		var (
			r                 chatRow
			isGroup, isFromMe int
			hasMessage        int
		)
		if err := rows.Scan(&r.jid, &r.name, &isGroup, &r.chatType, &r.ts, &r.unread,
			&r.body, &r.id, &r.sender, &isFromMe, &hasMessage); err != nil {
			return nil, err
		}
		r.isGroup = isGroup != 0
		r.isFromMe = isFromMe != 0
		r.hasMessage = include && hasMessage == 1
		out = append(out, r)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return out, nil
}

// mergedIdentities returns the conversation's full identity set. When the
// canonical JID has recorded aliases, identitiesOf already holds the complete
// set (phone JID first). Otherwise it falls back to the group's actual member
// JIDs, sorted for determinism.
func mergedIdentities(g *conversationGroup, identitiesOf map[string][]string) []string {
	if ids, ok := identitiesOf[g.canonical]; ok {
		return ids
	}
	ids := make([]string, 0, len(g.members))
	for j := range g.members {
		ids = append(ids, j)
	}
	sort.Strings(ids)
	return ids
}

// filterConversations applies the post-merge query filter: a conversation
// survives if its merged name or any of its JIDs contains the substring
// (case-insensitive). Filtering after the merge means a named phone-JID
// conversation is not dropped because its LID row happens to be untitled.
func filterConversations(conversations []ConversationDTO, query *string) []ConversationDTO {
	if query == nil || *query == "" {
		return conversations
	}
	needle := strings.ToLower(*query)
	out := conversations[:0]
	for _, c := range conversations {
		match := false
		if name, ok := c.Name.(string); ok && strings.Contains(strings.ToLower(name), needle) {
			match = true
		}
		if !match {
			for _, j := range c.JIDs {
				if strings.Contains(strings.ToLower(j), needle) {
					match = true
					break
				}
			}
		}
		if match {
			out = append(out, c)
		}
	}
	return out
}

// sortConversations mirrors list_chats' ordering. last_active: newest first,
// JID as a stable tiebreak. name: A→Z, untitled conversations last.
func sortConversations(conversations []ConversationDTO, sortBy string) {
	if sortBy == "name" {
		sort.SliceStable(conversations, func(i, j int) bool {
			ni, _ := conversations[i].Name.(string)
			nj, _ := conversations[j].Name.(string)
			if (ni == "") != (nj == "") {
				return ni != "" // named before untitled
			}
			if ni != nj {
				return ni < nj
			}
			return conversations[i].JID < conversations[j].JID
		})
		return
	}
	sort.SliceStable(conversations, func(i, j int) bool {
		ti := timeKey(conversations[i].LastMessageTime)
		tj := timeKey(conversations[j].LastMessageTime)
		if ti != tj {
			return ti > tj // most-recently-active first
		}
		return conversations[i].JID < conversations[j].JID
	})
}

// timeKey extracts a sortable key from the ISO-8601 LastMessageTime (string)
// or "" when null. Lexicographic order on RFC3339 matches chronological order.
func timeKey(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// paginate slices the merged, sorted set by the validated offset/limit.
func paginate(conversations []ConversationDTO, offset, limit int) []ConversationDTO {
	if offset >= len(conversations) {
		return []ConversationDTO{}
	}
	end := offset + limit
	if end > len(conversations) {
		end = len(conversations)
	}
	return conversations[offset:end]
}

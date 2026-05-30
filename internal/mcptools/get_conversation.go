package mcptools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

const getConversationDefaultLimit = 20

var getConversationInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "contact": {"type": "string", "minLength": 1, "description": "Full JID (…@s.whatsapp.net or …@lid) or bare phone number of the contact."},
    "limit":   {"type": "integer", "minimum": 1, "maximum": 200, "default": 20},
    "page":    {"type": "integer", "minimum": 0, "default": 0},
    "before":  {"type": ["string","null"], "description": "ISO-8601 timestamp; only messages strictly before this are returned."},
    "after":   {"type": ["string","null"], "description": "ISO-8601 timestamp; only messages strictly after this are returned."}
  },
  "required": ["contact"],
  "additionalProperties": false
}`)

var getConversationOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "messages": {"type": "array", "items": ` + messageSchemaFragment + `},
    "jids":     {"type": "array", "items": {"type": "string"}}
  },
  "required": ["messages","jids"]
}`)

type getConversationInput struct {
	Contact string  `json:"contact"`
	Limit   int     `json:"limit"`
	Page    int     `json:"page"`
	Before  *string `json:"before"`
	After   *string `json:"after"`
}

type getConversationOutput struct {
	Messages []MessageDTO `json:"messages"`
	JIDs     []string     `json:"jids"`
}

// get_conversation(contact): the canonical front door for "what's the latest with this person?".
// Merges every chat for a contact across their phone JID (…@s.whatsapp.net) and privacy LID
// (…@lid) into one newest-first timeline, de-duplicated, with resolved sender names and
// delivered/read status on each message. Use this instead of get_direct_chat_by_contact when you
// just want the full conversation; the lower-level per-JID tools remain available for power use.
func registerGetConversation(reg *mcp.Registry, store *cache.Store) error {
	return reg.Register(mcp.Tool{
		Name: "get_conversation",
		Description: "get_conversation(contact): the canonical front door for \"what's the latest with " +
			"this person?\". Merges every chat for a contact across their phone JID (…@s.whatsapp.net) " +
			"and privacy LID (…@lid) into one newest-first timeline, de-duplicated, with resolved " +
			"sender names and delivered/read status on each message. Use this instead of " +
			"get_direct_chat_by_contact when you just want the full conversation; the lower-level " +
			"per-JID tools remain available for power use. " +
			"Served from local cache, which may lag live WhatsApp. An empty result can mean " +
			"'not yet synced,' not 'no message.' For a guaranteed-fresh read: cache_sync → " +
			"poll cache_sync_status until finished → read.",
		InputSchema:  getConversationInputSchema,
		OutputSchema: getConversationOutputSchema,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in getConversationInput
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return mcp.InvalidArgumentError(fmt.Sprintf("decode arguments: %v", err)), nil
				}
			}
			return handleGetConversation(ctx, store, in)
		},
	})
}

func handleGetConversation(ctx context.Context, store *cache.Store, in getConversationInput) (any, error) {
	contact := strings.TrimSpace(in.Contact)
	if contact == "" {
		return mcp.InvalidArgumentError("contact must not be empty"), nil
	}
	limit, offset, perr := validatePagination(in.Limit, in.Page, getConversationDefaultLimit)
	if perr != nil {
		return mcp.InvalidArgumentError(perr.Message), nil
	}

	// Resolve the contact identifier to all of its identities: first to the
	// concrete JID(s) it names (exact JID, or phone-number lookup mirroring
	// get_direct_chat_by_contact), then expand each across the phone↔LID
	// alias so a contact split over two addresses merges into one set.
	seeds, err := resolveConversationSeeds(ctx, store, contact)
	if err != nil {
		return mcp.InternalError(fmt.Sprintf("get_conversation resolve: %v", err)), nil
	}
	jids := make([]string, 0, len(seeds))
	seenJID := map[string]bool{}
	for _, seed := range seeds {
		linked, err := store.ResolveLinkedJIDs(ctx, seed)
		if err != nil {
			return mcp.InternalError(fmt.Sprintf("get_conversation linked jids: %v", err)), nil
		}
		for _, j := range linked {
			if seenJID[j] {
				continue
			}
			seenJID[j] = true
			jids = append(jids, j)
		}
	}

	out := getConversationOutput{Messages: []MessageDTO{}, JIDs: jids}
	if len(jids) == 0 {
		return out, nil
	}

	// Merge query mirrors the get_contact_chats matching shape at message
	// granularity: a message belongs to the conversation when it lives in a
	// 1:1 chat keyed on one of the identities (m.chat_jid IN …) OR was sent by
	// one of them anywhere, including groups (m.sender_jid IN …).
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(jids)), ",")
	var (
		params []any
		wheres []string
	)
	idSet := func() {
		for _, j := range jids {
			params = append(params, j)
		}
	}
	idSet() // for m.chat_jid IN (...)
	idSet() // for m.sender_jid IN (...)
	wheres = append(wheres, fmt.Sprintf("(m.chat_jid IN (%s) OR m.sender_jid IN (%s))", placeholders, placeholders))

	if in.After != nil && *in.After != "" {
		t, err := parseISOTime(*in.After)
		if err != nil {
			return mcp.InvalidArgumentError(fmt.Sprintf("after: invalid ISO-8601 timestamp %q", *in.After)), nil
		}
		wheres = append(wheres, "m.ts > ?")
		params = append(params, t.Unix())
	}
	if in.Before != nil && *in.Before != "" {
		t, err := parseISOTime(*in.Before)
		if err != nil {
			return mcp.InvalidArgumentError(fmt.Sprintf("before: invalid ISO-8601 timestamp %q", *in.Before)), nil
		}
		wheres = append(wheres, "m.ts < ?")
		params = append(params, t.Unix())
	}

	query := fmt.Sprintf(`
SELECT m.id, m.chat_jid, c.name, m.sender_jid, m.body, m.ts, m.is_from_me,
       m.kind, m.media_filename, m.media_length,
       %s
FROM messages m
LEFT JOIN chats c ON c.jid = m.chat_jid`+messageContactJoins+`
WHERE %s
ORDER BY m.ts DESC, m.id DESC
LIMIT ? OFFSET ?`, messageSenderNameColumns, strings.Join(wheres, " AND "))
	params = append(params, limit, offset)

	rows, err := store.DB().QueryContext(ctx, query, params...)
	if err != nil {
		return mcp.InternalError(fmt.Sprintf("get_conversation: %v", err)), nil
	}
	defer rows.Close()

	// De-dup guard keyed on (chat_jid, id): a single OR query never repeats a
	// row, but the explicit set keeps the contract honest if the resolution
	// ever fans out into multiple unioned reads.
	seenMsg := map[string]bool{}
	for rows.Next() {
		dto, err := scanMessageRow(rows)
		if err != nil {
			return mcp.InternalError(fmt.Sprintf("get_conversation scan: %v", err)), nil
		}
		key := dto.ChatJID + "\x00" + dto.ID
		if seenMsg[key] {
			continue
		}
		seenMsg[key] = true
		out.Messages = append(out.Messages, dto)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return mcp.InternalError(fmt.Sprintf("get_conversation rows: %v", err)), nil
	}
	return out, nil
}

// resolveConversationSeeds turns a caller-supplied contact identifier into the
// concrete JID(s) it names, before phone↔LID alias expansion. A full JID
// (anything containing '@') is taken verbatim. A bare phone number / fragment
// is matched against known chat keys and message senders (non-group),
// mirroring get_direct_chat_by_contact's LIKE fallback; when nothing matches
// it falls back to the canonical phone JID form so a not-yet-synced contact
// still resolves deterministically.
func resolveConversationSeeds(ctx context.Context, store *cache.Store, contact string) ([]string, error) {
	if strings.Contains(contact, "@") {
		return []string{contact}, nil
	}
	likeQuery := strings.ReplaceAll(strings.ReplaceAll(contact, "%", `\%`), "_", `\_`)
	pattern := "%" + likeQuery + "%"
	rows, err := store.DB().QueryContext(ctx, `
SELECT jid FROM chats    WHERE jid        LIKE ? ESCAPE '\' AND jid        NOT LIKE '%@g.us'
UNION
SELECT sender_jid FROM messages WHERE sender_jid LIKE ? ESCAPE '\' AND sender_jid NOT LIKE '%@g.us'
ORDER BY jid ASC`, pattern, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seeds []string
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			return nil, err
		}
		if jid != "" {
			seeds = append(seeds, jid)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(seeds) == 0 {
		seeds = append(seeds, contact+"@s.whatsapp.net")
	}
	return seeds, nil
}

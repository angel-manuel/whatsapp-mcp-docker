package mcptools

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
)

// attachReactions fills in MessageDTO.Reactions for every message in msgs.
//
// It issues exactly one query for the whole batch, never one per message —
// same no-N+1 rule the canonical message SELECT follows with
// messageContactJoins. Messages with no reactions are left with a nil slice,
// which marshals away entirely (`omitempty`).
//
// msgs is mutated in place, so callers pass the slice they are about to
// return.
func attachReactions(ctx context.Context, store *cache.Store, msgs []MessageDTO) error {
	if store == nil || len(msgs) == 0 {
		return nil
	}

	// De-duplicate the (chat_jid, id) pairs: get_conversation merges several
	// chats and get_message_context returns a target that may also appear in
	// its own window.
	seen := make(map[[2]string]struct{}, len(msgs))
	params := make([]any, 0, len(msgs)*2)
	placeholders := make([]string, 0, len(msgs))
	for _, m := range msgs {
		key := [2]string{m.ChatJID, m.ID}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		params = append(params, m.ChatJID, m.ID)
		placeholders = append(placeholders, "(?,?)")
	}

	// Reaction sender names resolve exactly like message sender names — same
	// contacts/nicknames join, same ""→null rule via resolveSenderName.
	query := fmt.Sprintf(`
SELECT r.chat_jid, r.target_id, r.sender_jid, r.emoji, r.is_from_me,
       COALESCE(ct.push_name,''), COALESCE(ct.business_name,''),
       COALESCE(ct.first_name,''), COALESCE(ct.full_name,''), COALESCE(nk.nickname,'')
  FROM reactions r
  LEFT JOIN contacts  ct ON ct.jid = r.sender_jid
  LEFT JOIN nicknames nk ON nk.jid = r.sender_jid
 WHERE (r.chat_jid, r.target_id) IN (VALUES %s)
 ORDER BY r.ts ASC, r.sender_jid ASC`, strings.Join(placeholders, ","))

	rows, err := store.DB().QueryContext(ctx, query, params...)
	if err != nil {
		return err
	}
	defer rows.Close()

	byMessage := make(map[[2]string][]ReactionDTO)
	for rows.Next() {
		var (
			chatJID, targetID, sender, emoji string
			isFromMe                         int
			pushName, businessName           string
			firstName, fullName              string
			nickname                         string
		)
		if err := rows.Scan(&chatJID, &targetID, &sender, &emoji, &isFromMe,
			&pushName, &businessName, &firstName, &fullName, &nickname); err != nil {
			return err
		}
		name := resolveSenderName(cache.ContactRow{
			JID: sender, PushName: pushName, BusinessName: businessName,
			FirstName: firstName, FullName: fullName, Nickname: nickname,
		})
		key := [2]string{chatJID, targetID}
		byMessage[key] = append(byMessage[key], ReactionDTO{
			Emoji:      emoji,
			Sender:     sender,
			SenderName: stringOrNil(name),
			IsFromMe:   isFromMe != 0,
		})
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	for i := range msgs {
		if got := byMessage[[2]string{msgs[i].ChatJID, msgs[i].ID}]; len(got) > 0 {
			msgs[i].Reactions = got
		}
	}
	return nil
}

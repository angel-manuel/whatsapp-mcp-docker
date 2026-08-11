package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

// Values of ResolvedJID.Kind. Anything that is not a person, a group or a
// newsletter (broadcast lists, bots, …) is reported as jidKindUnknown
// rather than being forced into one of the other buckets.
const (
	jidKindUser       = "user"
	jidKindGroup      = "group"
	jidKindNewsletter = "newsletter"
	jidKindUnknown    = "unknown"
)

// ResolvedJID is the shape returned by resolve_jid: the readable identity
// behind any value send_message accepts as `recipient`.
//
// Every field but JID and Kind is best-effort. A JID nothing is known
// about resolves to a populated jid/kind with the rest empty instead of an
// error — the caller is typically rendering a human approval prompt and an
// honest "no name on file" is more useful there than a failed tool call.
type ResolvedJID struct {
	JID          string `json:"jid"`           // input, canonicalised via ToNonAD
	CanonicalJID string `json:"canonical_jid"` // phone JID when known, else JID
	Kind         string `json:"kind"`          // "user" | "group" | "newsletter" | "unknown"
	Name         string `json:"name"`          // display name; "" when unknown
	Phone        string `json:"phone"`         // E.164 with leading +; "" when not derivable
}

var resolveJIDSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "jid": {"type": "string", "minLength": 1, "description": "Anything send_message accepts as its recipient: a JID ('user@s.whatsapp.net', '…@lid', 'group@g.us', '…@newsletter') or a bare phone number with country code."}
  },
  "required": ["jid"],
  "additionalProperties": false
}`)

// resolveJID is the handler for the resolve_jid MCP tool. It answers from
// the local cache wherever it can: callers sit on the hot path of a send
// approval, so a live round trip is only made for a group whose name the
// cache has never learned.
func resolveJID(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			JID string `json:"jid"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}
		in.JID = strings.TrimSpace(in.JID)
		if in.JID == "" {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, "jid must not be empty"), nil
		}
		// resolveRecipient is the same normalisation send_message applies,
		// so every recipient it accepts resolves here.
		parsed, err := resolveRecipient(in.JID)
		if err != nil || parsed.User == "" {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, fmt.Sprintf("invalid jid %q", in.JID)), nil
		}
		canonical := parsed.ToNonAD()

		out := ResolvedJID{
			JID:          canonical.String(),
			CanonicalJID: canonical.String(),
			Kind:         jidKind(canonical),
		}

		switch out.Kind {
		case jidKindUser:
			// A @lid keys no contact row of its own — resolve it to the
			// contact's phone JID through jid_aliases first. When that
			// alias is unknown Phone stays empty: the LID's user part is
			// an opaque identifier, never a phone number.
			phoneJID, hasPhone, err := phoneJIDFor(ctx, deps.Cache, canonical)
			if err != nil {
				return mcp.ErrorResult(mcp.ErrInternal, err.Error()), nil
			}
			if hasPhone {
				out.CanonicalJID = phoneJID.String()
				out.Phone = "+" + phoneJID.User
			}
			row, _, err := lookupContact(ctx, deps.Cache, contactIdentities(canonical, phoneJID, hasPhone))
			if err != nil {
				return mcp.ErrorResult(mcp.ErrInternal, err.Error()), nil
			}
			out.Name = realDisplayName(row)
		case jidKindGroup:
			out.Name = groupName(ctx, deps, canonical)
		case jidKindNewsletter:
			name, err := deps.Cache.GetChatNameByJID(ctx, canonical.String())
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return mcp.ErrorResult(mcp.ErrInternal, err.Error()), nil
			}
			out.Name = name
		}
		return out, nil
	}
}

// jidKind classifies a JID by its server component. The user servers are
// the same set get_contact_details considers USync-able, so both tools
// agree on what counts as a person.
func jidKind(jid types.JID) string {
	switch jid.Server {
	case types.DefaultUserServer, types.LegacyUserServer, types.HiddenUserServer:
		return jidKindUser
	case types.GroupServer:
		return jidKindGroup
	case types.NewsletterServer:
		return jidKindNewsletter
	default:
		return jidKindUnknown
	}
}

// groupName resolves a group's display name, cache first: the ingestor
// keeps chats.name current, so the authoritative GetGroupInfo round trip
// only runs when the cache has never learned a name. Both lookups are
// best-effort — a group we cannot name still resolves, with Name empty.
func groupName(ctx context.Context, deps Deps, jid types.JID) string {
	if name, err := deps.Cache.GetChatNameByJID(ctx, jid.String()); err == nil && name != "" {
		return name
	}
	if info, err := deps.WA.GroupInfo(ctx, jid); err == nil && info != nil {
		return info.Name
	}
	return ""
}

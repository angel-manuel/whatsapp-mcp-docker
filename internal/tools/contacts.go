package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// ContactView is the dict-shape returned by search_contacts and
// list_all_contacts. It matches the Python reference field names plus a
// top-level `nickname` merged in from the local nicknames table. Empty
// string is used consistently for "not known" — the Python reference
// emits null in those cases, but JSON-schema-validated clients cope with
// both and keeping everything string-typed simplifies Go-side tests.
type ContactView struct {
	JID          string `json:"jid"`
	PhoneNumber  string `json:"phone_number"`
	Name         string `json:"name"`
	FirstName    string `json:"first_name"`
	FullName     string `json:"full_name"`
	PushName     string `json:"push_name"`
	BusinessName string `json:"business_name"`
	Nickname     string `json:"nickname"`
}

// ContactDetails is the shape for get_contact_details. See the task
// specification for the exact set of fields; in particular
// profile_picture_url is omitted when no picture is available and
// is_on_whatsapp is always present as a boolean.
type ContactDetails struct {
	JID string `json:"jid"`
	// Name is the display-name cascade (see realDisplayName), mirroring
	// ContactView.name so callers never have to re-derive it. Empty when
	// no name field is known — the JID is not echoed as a pseudo-name.
	Name string `json:"name"`
	// Phone is the contact's phone number (the user part of their
	// …@s.whatsapp.net JID). Empty when the real number is not known,
	// which is the case for a @lid with no recorded alias — LID digits
	// are an opaque identifier and must never be reported as a phone.
	Phone             string `json:"phone"`
	PushName          string `json:"push_name"`
	BusinessName      string `json:"business_name"`
	FullName          string `json:"full_name"`
	FirstName         string `json:"first_name"`
	Nickname          string `json:"nickname"`
	Status            string `json:"status"`
	ProfilePictureURL string `json:"profile_picture_url,omitempty"`
	IsOnWhatsApp      bool   `json:"is_on_whatsapp"`
}

var searchContactsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Substring matched case-insensitively against push_name / full_name / first_name / business_name / nickname / phone (the JID user portion)."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 200, "default": 50, "description": "Maximum number of contacts to return."},
    "page":  {"type": "integer", "minimum": 0, "default": 0, "description": "Zero-indexed pagination page."}
  },
  "required": ["query"],
  "additionalProperties": false
}`)

var listAllContactsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "limit": {"type": "integer", "minimum": 1, "maximum": 500, "default": 100, "description": "Maximum number of contacts to return."},
    "page":  {"type": "integer", "minimum": 0, "default": 0, "description": "Zero-indexed pagination page."}
  },
  "additionalProperties": false
}`)

var getContactDetailsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "jid": {"type": "string", "minLength": 1, "description": "WhatsApp JID to look up (e.g. 1234567890@s.whatsapp.net)."}
  },
  "required": ["jid"],
  "additionalProperties": false
}`)

// searchContacts is the handler for the MCP search_contacts tool.
func searchContacts(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
			Page  int    `json:"page"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}
		if strings.TrimSpace(in.Query) == "" {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, "query must not be empty"), nil
		}
		rows, err := deps.Cache.SearchContacts(ctx, in.Query, in.Limit, in.Page)
		if err != nil {
			return mcp.ErrorResult(mcp.ErrInternal, err.Error()), nil
		}
		return map[string]any{
			"contacts": toContactViews(rows),
			"count":    len(rows),
		}, nil
	}
}

// listAllContacts is the handler for list_all_contacts.
func listAllContacts(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			Limit int `json:"limit"`
			Page  int `json:"page"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}
		rows, err := deps.Cache.ListAllContacts(ctx, in.Limit, in.Page)
		if err != nil {
			return mcp.ErrorResult(mcp.ErrInternal, err.Error()), nil
		}
		return map[string]any{
			"contacts": toContactViews(rows),
			"count":    len(rows),
		}, nil
	}
}

// getContactDetails is the handler for get_contact_details. The JID is
// first resolved against the local cache — following jid_aliases so a
// @lid finds the contact row WhatsApp keys on the phone JID; if it is
// absent there, we fall back to a whatsmeow USync (GetUserInfo +
// IsOnWhatsApp) so callers can discover whether the JID exists at all.
// The profile picture URL is fetched opportunistically — a missing
// picture is not an error.
func getContactDetails(deps Deps) mcp.Handler {
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
		parsed, err := types.ParseJID(in.JID)
		if err != nil || parsed.User == "" {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, fmt.Sprintf("invalid jid %q", in.JID)), nil
		}

		canonical := parsed.ToNonAD()
		canonicalJID := canonical.String()

		details := ContactDetails{
			JID:          canonicalJID,
			IsOnWhatsApp: false,
		}

		// Resolve the phone JID behind the caller's address before touching
		// the contacts table: a @lid keys no contact row of its own, and its
		// user part is an opaque identifier rather than a phone number. When
		// the alias is unknown, Phone stays empty.
		phoneJID, hasPhone, err := phoneJIDFor(ctx, deps.Cache, canonical)
		if err != nil {
			return mcp.ErrorResult(mcp.ErrInternal, err.Error()), nil
		}
		if hasPhone {
			details.Phone = phoneJID.User
		}
		identities := contactIdentities(canonical, phoneJID, hasPhone)

		// Start from the cached row if any — we merge server data on top.
		cached, cacheHit, err := lookupContact(ctx, deps.Cache, identities)
		if err != nil {
			return mcp.ErrorResult(mcp.ErrInternal, err.Error()), nil
		}
		if cacheHit {
			details.PushName = cached.PushName
			details.BusinessName = cached.BusinessName
			details.FullName = cached.FullName
			details.FirstName = cached.FirstName
			details.Nickname = cached.Nickname
			details.IsOnWhatsApp = true
		} else {
			// Nicknames can exist without a backing contact row; surface
			// them even on the USync path.
			for _, id := range identities {
				if nick, nerr := deps.Cache.GetNicknameByJID(ctx, id.String()); nerr == nil && nick != "" {
					details.Nickname = nick
					break
				}
			}
		}

		// USync for status + LID + profile picture. Only user JIDs support
		// this; for group / broadcast / newsletter JIDs we short-circuit
		// the USync call.
		if jidKind(canonical) == jidKindUser {
			if ui, err := deps.WA.UserInfo(ctx, []types.JID{canonical}); err == nil {
				if entry, ok := ui[canonical]; ok {
					if entry.Status != "" {
						details.Status = entry.Status
					}
					if entry.VerifiedName != nil && entry.VerifiedName.Details != nil && details.BusinessName == "" {
						details.BusinessName = entry.VerifiedName.Details.GetVerifiedName()
					}
					details.IsOnWhatsApp = true
				}
			} else if errors.Is(err, wa.ErrNotLoggedIn) {
				// Middleware would normally have caught this; surface as
				// not_paired to keep the error taxonomy consistent.
				return mcp.NotPairedError(), nil
			}
			// Only attempt phone-number registration check when we know a
			// real phone number — hasPhone is false for an unaliased @lid,
			// whose user part would be rejected (or worse, silently match
			// someone else) as a phone number.
			if !details.IsOnWhatsApp && hasPhone {
				if checks, err := deps.WA.IsOnWhatsApp(ctx, []string{"+" + phoneJID.User}); err == nil {
					for _, r := range checks {
						if r.IsIn {
							details.IsOnWhatsApp = true
							break
						}
					}
				} else if errors.Is(err, wa.ErrNotLoggedIn) {
					return mcp.NotPairedError(), nil
				}
			}
			if url, err := deps.WA.ProfilePictureURL(ctx, canonical); err == nil && url != "" {
				details.ProfilePictureURL = url
			}
			// Profile-picture errors are non-fatal (401 "forbidden" is
			// common when the contact has restricted visibility) — we
			// intentionally swallow them.
		}

		if !cacheHit && !details.IsOnWhatsApp {
			return mcp.ErrorResult(mcp.ErrNotFound, fmt.Sprintf("no contact found for %q", in.JID)), nil
		}
		// Derived last so the cascade sees the USync-supplied business name
		// as well as the cached fields.
		details.Name = realDisplayName(cache.ContactRow{
			JID:          canonicalJID,
			PushName:     details.PushName,
			BusinessName: details.BusinessName,
			FirstName:    details.FirstName,
			FullName:     details.FullName,
			Nickname:     details.Nickname,
		})
		return details, nil
	}
}

// phoneJIDFor returns the phone-number JID (…@s.whatsapp.net) the given
// address belongs to. A phone JID is its own answer; a privacy LID is
// resolved through the cache's jid_aliases table, which the ingestor
// populates from the alternate address WhatsApp attaches to live messages.
//
// The bool is false when no phone JID is known — for a LID that is the
// common case on a cold cache. Callers must then leave the phone number
// empty: LID digits are an opaque identifier, and reporting them as a
// phone number is worse than reporting nothing.
func phoneJIDFor(ctx context.Context, store *cache.Store, jid types.JID) (types.JID, bool, error) {
	switch jid.Server {
	case types.DefaultUserServer, types.LegacyUserServer:
		return jid, true, nil
	case types.HiddenUserServer:
		// Fall through to the alias lookup below.
	default:
		// Groups, newsletters, broadcasts: no phone number to derive.
		return types.JID{}, false, nil
	}
	linked, err := store.ResolveLinkedJIDs(ctx, jid.String())
	if err != nil {
		return types.JID{}, false, err
	}
	for _, l := range linked {
		alias, err := types.ParseJID(l)
		if err != nil {
			continue
		}
		if alias.Server == types.DefaultUserServer || alias.Server == types.LegacyUserServer {
			return alias.ToNonAD(), true, nil
		}
	}
	return types.JID{}, false, nil
}

// contactIdentities lists the JIDs a contact's cached rows may be keyed
// on, phone JID first — that is where WhatsApp records contacts, so it is
// the identity worth trying before the address the caller supplied.
func contactIdentities(jid, phoneJID types.JID, hasPhone bool) []types.JID {
	if hasPhone && phoneJID.String() != jid.String() {
		return []types.JID{phoneJID, jid}
	}
	return []types.JID{jid}
}

// lookupContact returns the first cached contact row found across the
// candidate identities. A row missing from every identity is reported as
// ok=false with a nil error — only a real read failure returns one.
func lookupContact(ctx context.Context, store *cache.Store, identities []types.JID) (cache.ContactRow, bool, error) {
	for _, id := range identities {
		row, err := store.GetContactByJID(ctx, id.String())
		switch {
		case err == nil:
			return row, true, nil
		case errors.Is(err, sql.ErrNoRows):
			continue
		default:
			return cache.ContactRow{}, false, err
		}
	}
	return cache.ContactRow{}, false, nil
}

// realDisplayName is ContactRow.DisplayName with its JID fallback removed:
// it returns "" when no name field is populated, so a phone number or LID
// digits are never presented as if they were a name. Mirrors the same
// convention mcptools applies to MessageDTO.sender_name.
func realDisplayName(r cache.ContactRow) string {
	if r.Nickname == "" && r.FullName == "" && r.PushName == "" &&
		r.FirstName == "" && r.BusinessName == "" {
		return ""
	}
	return r.DisplayName()
}

func toContactViews(rows []cache.ContactRow) []ContactView {
	out := make([]ContactView, 0, len(rows))
	for _, r := range rows {
		out = append(out, ContactView{
			JID:          r.JID,
			PhoneNumber:  r.Phone(),
			Name:         displayName(r),
			FirstName:    r.FirstName,
			FullName:     r.FullName,
			PushName:     r.PushName,
			BusinessName: r.BusinessName,
			Nickname:     r.Nickname,
		})
	}
	return out
}

// displayName mirrors the Python reference cascade: nickname > full_name
// > push_name > first_name > business_name > phone number. It delegates to
// cache.ContactRow.DisplayName so the cascade lives in one place.
func displayName(r cache.ContactRow) string {
	return r.DisplayName()
}

// decodeArgs is a small wrapper around json.Unmarshal that tolerates an
// empty / missing argument object. Tools use it so that a caller who
// passes no arguments still gets their defaults rather than a decode
// error.
func decodeArgs(args json.RawMessage, into any) error {
	if len(args) == 0 || string(args) == "null" {
		return nil
	}
	if err := json.Unmarshal(args, into); err != nil {
		return fmt.Errorf("decode arguments: %w", err)
	}
	return nil
}

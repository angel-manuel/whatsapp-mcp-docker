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
	JID               string `json:"jid"`
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
// first resolved against the local cache; if it is absent there, we fall
// back to a whatsmeow USync (GetUserInfo + IsOnWhatsApp) so callers can
// discover whether the JID exists at all. The profile picture URL is
// fetched opportunistically — a missing picture is not an error.
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

		canonicalJID := parsed.ToNonAD().String()

		details := ContactDetails{
			JID:          canonicalJID,
			Phone:        parsed.User,
			IsOnWhatsApp: false,
		}

		// Start from the cached row if any — we merge server data on top.
		cached, err := deps.Cache.GetContactByJID(ctx, canonicalJID)
		cacheHit := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
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
			if nick, nerr := deps.Cache.GetNicknameByJID(ctx, canonicalJID); nerr == nil {
				details.Nickname = nick
			}
		}

		// USync for status + LID + profile picture. Only users on the
		// regular whatsapp server support this; for group / broadcast /
		// newsletter JIDs we short-circuit the USync call.
		if parsed.Server == types.DefaultUserServer || parsed.Server == types.LegacyUserServer || parsed.Server == types.HiddenUserServer {
			if ui, err := deps.WA.UserInfo(ctx, []types.JID{parsed.ToNonAD()}); err == nil {
				if entry, ok := ui[parsed.ToNonAD()]; ok {
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
			// Only attempt phone-number registration check when we have
			// a plausible phone number (regular user server).
			if !details.IsOnWhatsApp && parsed.Server == types.DefaultUserServer {
				if checks, err := deps.WA.IsOnWhatsApp(ctx, []string{"+" + parsed.User}); err == nil {
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
			if url, err := deps.WA.ProfilePictureURL(ctx, parsed.ToNonAD()); err == nil && url != "" {
				details.ProfilePictureURL = url
			}
			// Profile-picture errors are non-fatal (401 "forbidden" is
			// common when the contact has restricted visibility) — we
			// intentionally swallow them.
		}

		if !cacheHit && !details.IsOnWhatsApp {
			return mcp.ErrorResult(mcp.ErrNotFound, fmt.Sprintf("no contact found for %q", in.JID)), nil
		}
		return details, nil
	}
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
// > push_name > first_name > business_name > phone number.
func displayName(r cache.ContactRow) string {
	switch {
	case r.Nickname != "":
		return r.Nickname
	case r.FullName != "":
		return r.FullName
	case r.PushName != "":
		return r.PushName
	case r.FirstName != "":
		return r.FirstName
	case r.BusinessName != "":
		return r.BusinessName
	default:
		return r.Phone()
	}
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

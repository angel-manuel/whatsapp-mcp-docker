package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// GroupParticipant is the per-member projection inside a GroupInfoView.
type GroupParticipant struct {
	JID          string `json:"jid"`
	IsAdmin      bool   `json:"is_admin"`
	IsSuperAdmin bool   `json:"is_super_admin"`
}

// GroupInfoView is the dict-shape returned by get_group_info. `created_ts`
// is a UNIX-seconds timestamp (0 when the server did not emit one).
type GroupInfoView struct {
	JID            string             `json:"jid"`
	Name           string             `json:"name"`
	Topic          string             `json:"topic"`
	CreatedTS      int64              `json:"created_ts"`
	OwnerJID       string             `json:"owner_jid"`
	Participants   []GroupParticipant `json:"participants"`
	IsAnnouncement bool               `json:"is_announcement"`
	IsLocked       bool               `json:"is_locked"`
}

var getGroupInfoSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "group_jid": {"type": "string", "minLength": 1, "description": "Group JID (must end in @g.us)."}
  },
  "required": ["group_jid"],
  "additionalProperties": false
}`)

// getGroupInfo is the handler for get_group_info. The source of truth is
// whatsmeow.GetGroupInfo — the cache name, if any, is consulted only as a
// last-ditch fallback for the not-found path, not merged on top of the
// server response.
func getGroupInfo(deps Deps) mcp.Handler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			GroupJID string `json:"group_jid"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, err.Error()), nil
		}
		in.GroupJID = strings.TrimSpace(in.GroupJID)
		if in.GroupJID == "" {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, "group_jid must not be empty"), nil
		}

		parsed, err := types.ParseJID(in.GroupJID)
		if err != nil {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, fmt.Sprintf("invalid jid %q: %v", in.GroupJID, err)), nil
		}
		if parsed.Server != types.GroupServer {
			return mcp.ErrorResult(mcp.ErrInvalidArgument, fmt.Sprintf("jid %q is not a group JID (expected @%s)", in.GroupJID, types.GroupServer)), nil
		}

		info, err := deps.WA.GroupInfo(ctx, parsed)
		switch {
		case err == nil:
			return toGroupInfoView(info), nil
		case errors.Is(err, wa.ErrNotLoggedIn):
			return mcp.NotPairedError(), nil
		case errors.Is(err, whatsmeow.ErrIQNotFound), errors.Is(err, whatsmeow.ErrGroupNotFound):
			return mcp.ErrorResult(mcp.ErrNotFound, fmt.Sprintf("group %q not found", in.GroupJID)), nil
		default:
			return mcp.ErrorResult(mcp.ErrInternal, err.Error()), nil
		}
	}
}

func toGroupInfoView(info *types.GroupInfo) GroupInfoView {
	if info == nil {
		return GroupInfoView{}
	}
	parts := make([]GroupParticipant, 0, len(info.Participants))
	for _, p := range info.Participants {
		parts = append(parts, GroupParticipant{
			JID:          p.JID.String(),
			IsAdmin:      p.IsAdmin,
			IsSuperAdmin: p.IsSuperAdmin,
		})
	}
	var createdTS int64
	if !info.GroupCreated.IsZero() {
		createdTS = info.GroupCreated.Unix()
	}
	var ownerJID string
	if !info.OwnerJID.IsEmpty() {
		ownerJID = info.OwnerJID.String()
	}
	return GroupInfoView{
		JID:            info.JID.String(),
		Name:           info.Name,
		Topic:          info.Topic,
		CreatedTS:      createdTS,
		OwnerJID:       ownerJID,
		Participants:   parts,
		IsAnnouncement: info.IsAnnounce,
		IsLocked:       info.IsLocked,
	}
}

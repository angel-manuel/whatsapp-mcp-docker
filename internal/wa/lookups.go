package wa

import (
	"context"
	"errors"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// ErrNotLoggedIn is returned by the read-side lookups when the underlying
// whatsmeow client is not currently logged in. Callers should typically
// translate this into a not_paired tool error.
var ErrNotLoggedIn = errors.New("wa: not logged in")

// GroupInfo fetches authoritative group metadata from the WhatsApp server
// via whatsmeow.GetGroupInfo. The caller is expected to pass a properly
// parsed group JID (server=g.us); non-group JIDs are accepted here but
// whatsmeow will surface a server error.
func (c *Client) GroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error) {
	wm := c.snapshotWM()
	if wm == nil || !wm.IsLoggedIn() {
		return nil, ErrNotLoggedIn
	}
	return wm.GetGroupInfo(ctx, jid)
}

// UserInfo fetches live user metadata (status, avatar id, LID, ...) for
// the given JIDs via a USync query. The returned map is keyed on the
// server's canonical form of the JID.
func (c *Client) UserInfo(ctx context.Context, jids []types.JID) (map[types.JID]types.UserInfo, error) {
	if len(jids) == 0 {
		return map[types.JID]types.UserInfo{}, nil
	}
	wm := c.snapshotWM()
	if wm == nil || !wm.IsLoggedIn() {
		return nil, ErrNotLoggedIn
	}
	return wm.GetUserInfo(ctx, jids)
}

// IsOnWhatsApp checks the given phone numbers via USync. Phones must be
// in international form including the leading `+` (whatsmeow asserts
// this). Callers get back the canonical JID + is_in flag per query.
func (c *Client) IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
	if len(phones) == 0 {
		return nil, nil
	}
	wm := c.snapshotWM()
	if wm == nil || !wm.IsLoggedIn() {
		return nil, ErrNotLoggedIn
	}
	return wm.IsOnWhatsApp(ctx, phones)
}

// ProfilePictureURL returns the full-res profile picture URL for jid, or
// ("", nil) when no picture is set (or the user has hidden it). Any other
// error (server-side 401, network) is surfaced verbatim.
func (c *Client) ProfilePictureURL(ctx context.Context, jid types.JID) (string, error) {
	wm := c.snapshotWM()
	if wm == nil || !wm.IsLoggedIn() {
		return "", ErrNotLoggedIn
	}
	info, err := wm.GetProfilePictureInfo(ctx, jid, &whatsmeow.GetProfilePictureParams{})
	if err != nil {
		return "", err
	}
	if info == nil {
		return "", nil
	}
	return info.URL, nil
}

// snapshotWM returns the underlying whatsmeow client under lock. Nil is
// returned when the client has not been opened (or is mid-close).
func (c *Client) snapshotWM() *whatsmeow.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wm
}

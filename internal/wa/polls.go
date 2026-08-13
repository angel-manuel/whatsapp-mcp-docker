package wa

import (
	"context"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// BuildPollCreation builds the poll creation envelope for SendMessage.
// whatsmeow attaches a freshly generated message secret to it, which is what
// every later vote on this poll is encrypted against — see StoreMessageSecret
// for why the caller has to persist it.
//
// selectableCount is how many options a voter may pick; whatsmeow clamps an
// out-of-range value to 0 ("no limit").
func (c *Client) BuildPollCreation(name string, options []string, selectableCount int) (*waE2E.Message, error) {
	wm := c.snapshotWM()
	if wm == nil || !wm.IsLoggedIn() {
		return nil, ErrNotLoggedIn
	}
	return wm.BuildPollCreation(name, options, selectableCount), nil
}

// BuildPollVote builds an encrypted vote for the poll described by pollInfo.
// The option names must match the poll's ballot exactly — they are hashed, and
// a name that differs by so much as a space votes for nothing.
//
// It fails with whatsmeow's ErrOriginalMessageSecretNotFound when this device
// never held the poll's secret, which is the normal outcome for a poll created
// before the device was linked.
func (c *Client) BuildPollVote(ctx context.Context, pollInfo *types.MessageInfo, optionNames []string) (*waE2E.Message, error) {
	wm := c.snapshotWM()
	if wm == nil || !wm.IsLoggedIn() {
		return nil, ErrNotLoggedIn
	}
	return wm.BuildPollVote(ctx, pollInfo, optionNames)
}

// DecryptPollVote unwraps the encrypted selection inside a poll update event.
// It satisfies cache.PollDecrypter, which is how the ingestor reads votes
// without importing whatsmeow's client.
func (c *Client) DecryptPollVote(ctx context.Context, vote *events.Message) (*waE2E.PollVoteMessage, error) {
	wm := c.snapshotWM()
	if wm == nil {
		return nil, ErrNotLoggedIn
	}
	return wm.DecryptPollVote(ctx, vote)
}

// StoreMessageSecret persists the secret of a message we sent, so that later
// updates referring back to it (poll votes, reactions) can be decrypted.
//
// whatsmeow does this automatically for *received* messages only: SendMessage
// never records the secret it just put on the wire. Without this call, votes
// on a poll created through send_poll would arrive and fail to decrypt with
// ErrOriginalMessageSecretNotFound — permanently, since WhatsApp does not
// resend them.
func (c *Client) StoreMessageSecret(ctx context.Context, chat, sender types.JID, id types.MessageID, secret []byte) error {
	if len(secret) == 0 {
		return nil
	}
	wm := c.snapshotWM()
	if wm == nil || wm.Store == nil || wm.Store.MsgSecrets == nil {
		return ErrNotLoggedIn
	}
	return wm.Store.MsgSecrets.PutMessageSecret(ctx, chat, sender, id, secret)
}

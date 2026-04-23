package wa

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.mau.fi/whatsmeow"
)

// pairSession tracks the lifetime of a /admin/pair/start flow. Only one can
// be active at a time, guarded by Client.adminMu.
type pairSession struct {
	cancel context.CancelFunc
	out    chan PairEvent
	done   chan struct{}
}

// StartPairing opens a whatsmeow QR channel, connects the client, and returns
// a channel of typed PairEvents. The caller must cancel ctx (or drain the
// channel until it closes) to release resources. Only one pair flow may be
// active at a time.
func (c *Client) StartPairing(ctx context.Context) (<-chan PairEvent, error) {
	c.adminMu.Lock()
	defer c.adminMu.Unlock()

	if c.pairing != nil {
		return nil, ErrPairInProgress
	}

	c.mu.Lock()
	device := c.device
	wm := c.wm
	c.mu.Unlock()

	if device != nil && device.ID != nil {
		return nil, ErrAlreadyPaired
	}

	// GetQRChannel must be called before Connect and requires that the
	// store contain no user ID (both guaranteed above).
	pairCtx, cancel := context.WithCancel(ctx)
	raw, err := wm.GetQRChannel(pairCtx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("wa: GetQRChannel: %w", err)
	}

	c.stopReconnect.Store(false)
	c.mu.Lock()
	c.state = StateConnecting
	c.mu.Unlock()

	if err := wm.Connect(); err != nil {
		cancel()
		return nil, fmt.Errorf("wa: connect for pairing: %w", err)
	}

	out := make(chan PairEvent, 8)
	session := &pairSession{
		cancel: cancel,
		out:    out,
		done:   make(chan struct{}),
	}
	c.pairing = session

	go func() {
		defer close(out)
		defer close(session.done)
		defer c.clearPairing(session)

		for {
			select {
			case <-pairCtx.Done():
				// Drain remaining items so the underlying goroutine can exit.
				for range raw {
				}
				return
			case item, ok := <-raw:
				if !ok {
					return
				}
				evt := toPairEvent(item)
				select {
				case out <- evt:
				case <-pairCtx.Done():
					return
				}
				if evt.IsTerminal() {
					return
				}
			}
		}
	}()

	return out, nil
}

// clearPairing removes the active pair session pointer if it still matches
// session. Called from the pairing goroutine on exit.
func (c *Client) clearPairing(session *pairSession) {
	c.adminMu.Lock()
	if c.pairing == session {
		c.pairing = nil
	}
	c.adminMu.Unlock()
}

// PairPhone requests a phone-code pairing. An active pair flow must already
// be open (StartPairing handles the prerequisite Connect + QR subscription);
// success or failure of the overall pair flow is observed on the pair channel
// returned by StartPairing, not here.
func (c *Client) PairPhone(ctx context.Context, phone string) (string, error) {
	c.adminMu.Lock()
	pairing := c.pairing
	c.adminMu.Unlock()

	if pairing == nil {
		return "", ErrNotPairing
	}

	c.mu.Lock()
	device := c.device
	wm := c.wm
	displayName := c.cfg.PairClientDisplay
	c.mu.Unlock()

	if device != nil && device.ID != nil {
		return "", ErrAlreadyPaired
	}

	code, err := wm.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, displayName)
	if err != nil {
		return "", err
	}
	return code, nil
}

// Unpair performs a clean logout (best-effort), tears down the on-disk
// session database, and reinitializes the whatsmeow client around an empty
// device so the caller can pair again.
func (c *Client) Unpair(ctx context.Context) error {
	c.adminMu.Lock()
	defer c.adminMu.Unlock()

	if c.pairing != nil {
		// An active pair flow is still attached to the old wm; cancel it so
		// it can exit before we swap the client out from under it.
		c.pairing.cancel()
		select {
		case <-c.pairing.done:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		c.pairing = nil
	}

	c.mu.Lock()
	oldWM := c.wm
	oldContainer := c.container
	oldHandlerID := c.handlerID
	device := c.device
	c.mu.Unlock()

	// Best-effort server-side logout. Ignore ErrNotLoggedIn (remote logout
	// already happened) and any other logout error — the local teardown
	// below runs regardless.
	if device != nil && device.ID != nil && oldWM != nil && oldWM.IsLoggedIn() {
		if err := oldWM.Logout(ctx); err != nil {
			c.log.Warnf("wa: unpair logout: %v", err)
		}
	}

	c.stopReconnect.Store(true)
	if oldWM != nil {
		oldWM.RemoveEventHandler(oldHandlerID)
		oldWM.Disconnect()
	}
	if oldContainer != nil {
		if err := oldContainer.Close(); err != nil {
			c.log.Warnf("wa: unpair close sqlstore: %v", err)
		}
	}

	// Remove session.db and the WAL/SHM sidecar files. Any error other than
	// "not exist" is fatal: we cannot safely reopen if the FS is wedged.
	base := filepath.Join(c.cfg.DataDir, "session.db")
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(base + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("wa: remove %s: %w", base+suffix, err)
		}
	}

	// Reopen sqlstore + fresh empty device + new whatsmeow client.
	if err := c.openWhatsmeow(ctx); err != nil {
		return err
	}

	c.stopReconnect.Store(false)
	c.mu.Lock()
	c.state = StateNotPaired
	c.lastEvent = Event{}
	c.lastEventSet = false
	c.mu.Unlock()

	// Emit a synthetic disconnected event so any subscribers observe the
	// transition rather than silently seeing a stale status.
	c.fanout(Event{Type: EventDisconnected, Timestamp: time.Now().UTC()})

	return nil
}

// toPairEvent maps a whatsmeow QRChannelItem to an admin-facing PairEvent.
// Upstream event names like "err-client-outdated" lose the "err-" prefix.
func toPairEvent(item whatsmeow.QRChannelItem) PairEvent {
	switch item.Event {
	case whatsmeow.QRChannelEventCode:
		return PairEvent{
			Type:      PairEventCode,
			Code:      item.Code,
			TimeoutMs: item.Timeout.Milliseconds(),
		}
	case "success":
		return PairEvent{Type: PairEventSuccess}
	case "timeout":
		return PairEvent{Type: PairEventTimeout}
	case "err-client-outdated":
		return PairEvent{Type: PairEventClientOutdated}
	case "err-scanned-without-multidevice":
		return PairEvent{Type: PairEventScannedWithoutMultidevice}
	case whatsmeow.QRChannelEventError:
		msg := ""
		if item.Error != nil {
			msg = item.Error.Error()
		}
		return PairEvent{Type: PairEventError, Error: msg}
	default:
		// err-unexpected-state and any future variants fall through here.
		msg := item.Event
		if item.Error != nil {
			msg = item.Error.Error()
		}
		return PairEvent{Type: PairEventError, Error: msg}
	}
}


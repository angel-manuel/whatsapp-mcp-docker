package wa

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
)

// pairSession tracks the lifetime of a /admin/pair/start flow. Only one can
// be active at a time, guarded by Client.adminMu. After the flow goroutine
// exits the session is *not* nil-ed out — its observation fields preserve
// the terminal outcome so callers polling via PairLatest / PairWaitNext
// after the flow ended still see the result. The next StartPairing or
// Unpair replaces or clears the pointer.
type pairSession struct {
	cancel context.CancelFunc
	out    chan PairEvent
	done   chan struct{}

	// Observation state, written by the StartPairing goroutine and read by
	// PairLatest / PairWaitNext. Independent from `out`, which is owned by
	// the SSE consumer.
	obsMu     sync.Mutex
	obsCond   *sync.Cond
	obsLatest PairEvent
	obsSeq    uint64 // 0 means no event yet; bumps on every publish
	obsDone   bool   // goroutine has exited; no more events will arrive
}

// isDone reports whether the pair flow goroutine has exited. Once true the
// session is observation-only.
func (s *pairSession) isDone() bool {
	s.obsMu.Lock()
	defer s.obsMu.Unlock()
	return s.obsDone
}

// publish records a new pair event and wakes any waiters. Called from the
// StartPairing goroutine.
func (s *pairSession) publish(evt PairEvent) {
	s.obsMu.Lock()
	s.obsLatest = evt
	s.obsSeq++
	s.obsCond.Broadcast()
	s.obsMu.Unlock()
}

// markDone records that the goroutine is exiting and wakes any waiters.
// Called from a deferred closure in the StartPairing goroutine.
func (s *pairSession) markDone() {
	s.obsMu.Lock()
	s.obsDone = true
	s.obsCond.Broadcast()
	s.obsMu.Unlock()
}

// StartPairing opens a whatsmeow QR channel, connects the client, and returns
// a channel of typed PairEvents. The caller must cancel ctx (or drain the
// channel until it closes) to release resources. Only one pair flow may be
// active at a time. Once the flow's goroutine exits, the session pointer is
// kept on c.pairing so post-terminal observers (PairLatest / PairWaitNext)
// still see the outcome; it is replaced on the next StartPairing or cleared
// by Unpair.
func (c *Client) StartPairing(ctx context.Context) (<-chan PairEvent, error) {
	c.adminMu.Lock()
	defer c.adminMu.Unlock()

	if c.pairing != nil && !c.pairing.isDone() {
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
	session.obsCond = sync.NewCond(&session.obsMu)
	c.pairing = session

	go func() {
		defer close(out)
		defer close(session.done)
		defer session.markDone()
		defer cancel()

		for {
			select {
			case <-pairCtx.Done():
				// whatsmeow's QR emitter observes the same context and will
				// close its output; any buffered items are GC'd with the
				// channel, no explicit drain required.
				return
			case item, ok := <-raw:
				if !ok {
					return
				}
				evt := toPairEvent(item)
				// Publish before forwarding to the SSE consumer so an
				// observer waking on the broadcast can read the new event
				// regardless of whether anything is draining `out`.
				session.publish(evt)
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

// PairPhone requests a phone-code pairing. An active pair flow must already
// be open (StartPairing handles the prerequisite Connect + QR subscription);
// success or failure of the overall pair flow is observed on the pair channel
// returned by StartPairing, not here.
func (c *Client) PairPhone(ctx context.Context, phone string) (string, error) {
	c.adminMu.Lock()
	pairing := c.pairing
	c.adminMu.Unlock()

	if pairing == nil || pairing.isDone() {
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

package wa

import (
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

type subscriber struct {
	ch     chan Event
	closed bool
}

// Subscribe returns a channel that receives lifecycle events plus an
// unsubscribe function. The returned channel has a small buffer; if a
// consumer falls behind, newer events are dropped with a warning rather than
// stalling the dispatcher. Call the returned function exactly once to close
// the channel and remove the subscription.
func (c *Client) Subscribe() (<-chan Event, func()) {
	s := &subscriber{ch: make(chan Event, 16)}
	c.mu.Lock()
	c.subs = append(c.subs, s)
	c.mu.Unlock()

	var once bool
	return s.ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if once {
			return
		}
		once = true
		for i, existing := range c.subs {
			if existing == s {
				c.subs = append(c.subs[:i], c.subs[i+1:]...)
				break
			}
		}
		if !s.closed {
			close(s.ch)
			s.closed = true
		}
	}
}

// State returns the current lifecycle state.
func (c *Client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// dispatch runs in its own goroutine and is the sole consumer of rawCh.
func (c *Client) dispatch() {
	defer close(c.done)
	for {
		select {
		case <-c.quit:
			c.closeAllSubs()
			return
		case raw := <-c.rawCh:
			c.handle(raw)
			if hook := c.cfg.EventHook; hook != nil {
				hook(raw)
			}
		}
	}
}

func (c *Client) handle(raw any) {
	now := time.Now().UTC()
	switch v := raw.(type) {
	case *events.Connected:
		c.setState(StateConnected)
		c.fanout(Event{Type: EventConnected, Timestamp: now})

	case *events.Disconnected:
		c.mu.Lock()
		// A graceful transport-level disconnect should not clobber a terminal
		// state set by LoggedOut / StreamReplaced.
		if c.state != StateLoggedOut && c.state != StateStreamReplaced {
			c.state = StateDisconnected
		}
		c.mu.Unlock()
		c.fanout(Event{Type: EventDisconnected, Timestamp: now})

	case *events.LoggedOut:
		c.stopReconnect.Store(true)
		c.currentWM().Disconnect()
		c.setState(StateLoggedOut)
		c.fanout(Event{
			Type:          EventLoggedOut,
			Timestamp:     now,
			FailureReason: int(v.Reason),
		})

	case *events.StreamReplaced:
		c.stopReconnect.Store(true)
		c.currentWM().Disconnect()
		c.setState(StateStreamReplaced)
		c.fanout(Event{Type: EventStreamReplaced, Timestamp: now})

	case *events.TemporaryBan:
		c.fanout(Event{
			Type:      EventTemporaryBan,
			Timestamp: now,
			BanExpire: v.Expire,
			BanCode:   int(v.Code),
		})

	case *events.ConnectFailure:
		c.fanout(Event{
			Type:           EventConnectionFailure,
			Timestamp:      now,
			FailureReason:  int(v.Reason),
			FailureMessage: v.Message,
		})
	}
}

func (c *Client) fanout(evt Event) {
	c.mu.Lock()
	c.lastEvent = evt
	c.lastEventSet = true
	subs := append([]*subscriber(nil), c.subs...)
	c.mu.Unlock()
	for _, s := range subs {
		select {
		case s.ch <- evt:
		default:
			c.log.Warnf("wa: dropping %s event for slow subscriber", evt.Type)
		}
	}
}

func (c *Client) setState(s State) {
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()
}

// currentWM returns a live pointer to the whatsmeow client under c.mu.
// Callers must not hold c.mu.
func (c *Client) currentWM() *whatsmeow.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wm
}

func (c *Client) closeAllSubs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.subs {
		if !s.closed {
			close(s.ch)
			s.closed = true
		}
	}
	c.subs = nil
}

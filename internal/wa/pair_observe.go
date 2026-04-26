package wa

import "context"

// PairLatest returns the most recent pair event observed on the current or
// most recently completed pair flow. The bool is false when no pair flow
// has ever been started in this process (or after Unpair has cleared it),
// in which case the returned PairEvent is the zero value. When the bool is
// true but no event has been emitted yet (a flow is starting and we are
// still waiting on whatsmeow), the returned PairEvent is also the zero
// value (Type == "") — callers can detect that with evt.Type == "".
func (c *Client) PairLatest() (PairEvent, bool) {
	c.adminMu.Lock()
	session := c.pairing
	c.adminMu.Unlock()
	if session == nil {
		return PairEvent{}, false
	}
	session.obsMu.Lock()
	defer session.obsMu.Unlock()
	return session.obsLatest, true
}

// PairWaitNext blocks until a new pair event arrives or ctx is cancelled.
// "New" is relative to the moment of the call: if an event is published
// after entry, it is returned; if ctx expires first, the latest event
// already observed is returned along with ctx.Err(). When no pair flow
// has ever been started the bool is false and the call returns
// immediately. When the flow goroutine has already exited and no new
// events will arrive, the latest event (typically a terminal one) is
// returned with err == nil.
func (c *Client) PairWaitNext(ctx context.Context) (PairEvent, bool, error) {
	c.adminMu.Lock()
	session := c.pairing
	c.adminMu.Unlock()
	if session == nil {
		return PairEvent{}, false, nil
	}

	session.obsMu.Lock()
	startSeq := session.obsSeq
	// If the goroutine already exited (terminal observed earlier, or
	// flow cancelled), return immediately so callers polling after a
	// terminal still see the outcome instead of blocking forever.
	if session.obsDone {
		evt := session.obsLatest
		session.obsMu.Unlock()
		return evt, true, nil
	}
	session.obsMu.Unlock()

	// Spawn a watchdog goroutine that broadcasts on the cond when ctx
	// fires. sync.Cond has no native context support; this is the
	// idiomatic workaround. It exits as soon as the wait below
	// returns, either via ctx done or because the producer broadcast.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			session.obsCond.Broadcast()
		case <-done:
		}
	}()

	session.obsMu.Lock()
	for session.obsSeq == startSeq && !session.obsDone && ctx.Err() == nil {
		session.obsCond.Wait()
	}
	evt := session.obsLatest
	session.obsMu.Unlock()
	return evt, true, ctx.Err()
}

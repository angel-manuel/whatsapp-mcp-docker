package wa

import (
	"context"
	"sync"
	"testing"
	"time"
)

// These tests target the observation half of pairSession (publish /
// markDone / obsCond) and the read methods (PairLatest / PairWaitNext)
// without standing up a whatsmeow client. We hand-build a *Client with
// just the adminMu and pairing fields populated; the rest of Client is
// untouched by the observation code path.

func newObsClient(session *pairSession) *Client {
	c := &Client{}
	c.pairing = session
	return c
}

func newObsSession() *pairSession {
	s := &pairSession{
		out:  make(chan PairEvent, 8),
		done: make(chan struct{}),
	}
	s.obsCond = sync.NewCond(&s.obsMu)
	return s
}

func TestPairLatest_NoSession(t *testing.T) {
	c := &Client{}
	evt, ok := c.PairLatest()
	if ok {
		t.Errorf("ok=true, want false on absent session; evt=%+v", evt)
	}
}

func TestPairLatest_NoEventsYet(t *testing.T) {
	s := newObsSession()
	c := newObsClient(s)
	evt, ok := c.PairLatest()
	if !ok {
		t.Fatal("ok=false, want true (session exists)")
	}
	if evt.Type != "" {
		t.Errorf("evt.Type=%q, want empty (no events published yet)", evt.Type)
	}
}

func TestPairLatest_ReturnsLatestPublished(t *testing.T) {
	s := newObsSession()
	c := newObsClient(s)
	s.publish(PairEvent{Type: PairEventCode, Code: "rot-1", TimeoutMs: 15000})
	s.publish(PairEvent{Type: PairEventCode, Code: "rot-2", TimeoutMs: 15000})
	evt, ok := c.PairLatest()
	if !ok || evt.Code != "rot-2" {
		t.Errorf("got (%+v, %v), want rot-2/true", evt, ok)
	}
}

func TestPairWaitNext_BlocksUntilPublish(t *testing.T) {
	s := newObsSession()
	c := newObsClient(s)

	type result struct {
		evt    PairEvent
		active bool
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		evt, active, err := c.PairWaitNext(ctx)
		resCh <- result{evt, active, err}
	}()

	// Give the waiter time to enter cond.Wait, then publish.
	time.Sleep(20 * time.Millisecond)
	s.publish(PairEvent{Type: PairEventCode, Code: "first", TimeoutMs: 15000})

	select {
	case r := <-resCh:
		if !r.active {
			t.Errorf("active=false, want true")
		}
		if r.evt.Code != "first" {
			t.Errorf("code=%q, want first", r.evt.Code)
		}
		if r.err != nil {
			t.Errorf("err=%v, want nil", r.err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("PairWaitNext did not return after publish")
	}
}

func TestPairWaitNext_DeadlineReturnsLatestPlusErr(t *testing.T) {
	s := newObsSession()
	c := newObsClient(s)
	s.publish(PairEvent{Type: PairEventCode, Code: "rot-1", TimeoutMs: 15000})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// No new event arrives. PairWaitNext should observe the deadline,
	// return the existing latest, and report ctx.Err().
	evt, active, err := c.PairWaitNext(ctx)
	if !active {
		t.Errorf("active=false, want true")
	}
	if evt.Code != "rot-1" {
		t.Errorf("code=%q, want rot-1", evt.Code)
	}
	if err == nil {
		t.Errorf("err=nil, want context deadline")
	}
}

func TestPairWaitNext_DoneSessionReturnsImmediately(t *testing.T) {
	s := newObsSession()
	c := newObsClient(s)
	s.publish(PairEvent{Type: PairEventSuccess})
	s.markDone()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	evt, active, err := c.PairWaitNext(ctx)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("PairWaitNext blocked for %s on a done session", elapsed)
	}
	if !active {
		t.Errorf("active=false, want true (terminal preserved)")
	}
	if evt.Type != PairEventSuccess {
		t.Errorf("evt.Type=%q, want success", evt.Type)
	}
	if err != nil {
		t.Errorf("err=%v, want nil", err)
	}
}

func TestPairWaitNext_NoSession(t *testing.T) {
	c := &Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	evt, active, err := c.PairWaitNext(ctx)
	if active {
		t.Errorf("active=true, want false")
	}
	if evt.Type != "" {
		t.Errorf("evt.Type=%q, want empty", evt.Type)
	}
	if err != nil {
		t.Errorf("err=%v, want nil", err)
	}
}

func TestSessionIsDone(t *testing.T) {
	s := newObsSession()
	if s.isDone() {
		t.Error("isDone=true on fresh session, want false")
	}
	s.markDone()
	if !s.isDone() {
		t.Error("isDone=false after markDone, want true")
	}
}

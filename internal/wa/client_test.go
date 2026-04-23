package wa

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

func newTestClient(t *testing.T) *Client {
	t.Helper()
	c, err := Open(context.Background(), Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Logf("Close: %v", err)
		}
	})
	return c
}

func waitEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case evt, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		return evt
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	return Event{}
}

func TestOpenRejectsSecondInstanceSameDir(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(context.Background(), Config{DataDir: dir})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	_, err = Open(context.Background(), Config{DataDir: dir})
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second Open: want ErrLockHeld, got %v", err)
	}
}

func TestOpenStartsNotPairedWhenNoDevice(t *testing.T) {
	c := newTestClient(t)
	if c.IsPaired() {
		t.Fatal("fresh data dir should not be paired")
	}
	if got := c.State(); got != StateNotPaired {
		t.Fatalf("state = %q, want %q", got, StateNotPaired)
	}
}

func TestDispatcherFansOutConnected(t *testing.T) {
	c := newTestClient(t)

	chA, unsubA := c.Subscribe()
	chB, unsubB := c.Subscribe()
	t.Cleanup(unsubA)
	t.Cleanup(unsubB)

	c.rawCh <- &events.Connected{}

	for i, ch := range []<-chan Event{chA, chB} {
		evt := waitEvent(t, ch)
		if evt.Type != EventConnected {
			t.Fatalf("sub %d: type = %q, want %q", i, evt.Type, EventConnected)
		}
	}
	if got := c.State(); got != StateConnected {
		t.Fatalf("state = %q, want %q", got, StateConnected)
	}
}

func TestLoggedOutStopsReconnectAndSetsState(t *testing.T) {
	c := newTestClient(t)
	ch, unsub := c.Subscribe()
	t.Cleanup(unsub)

	c.rawCh <- &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}

	evt := waitEvent(t, ch)
	if evt.Type != EventLoggedOut {
		t.Fatalf("type = %q, want %q", evt.Type, EventLoggedOut)
	}
	if evt.FailureReason != int(events.ConnectFailureLoggedOut) {
		t.Fatalf("FailureReason = %d, want %d", evt.FailureReason, events.ConnectFailureLoggedOut)
	}
	if !c.stopReconnect.Load() {
		t.Fatal("stopReconnect should be set after LoggedOut")
	}
	if got := c.State(); got != StateLoggedOut {
		t.Fatalf("state = %q, want %q", got, StateLoggedOut)
	}
	if ok := c.wm.AutoReconnectHook(errors.New("probe")); ok {
		t.Fatal("AutoReconnectHook should return false after logout")
	}
}

func TestStreamReplacedStopsReconnect(t *testing.T) {
	c := newTestClient(t)
	ch, unsub := c.Subscribe()
	t.Cleanup(unsub)

	c.rawCh <- &events.StreamReplaced{}

	evt := waitEvent(t, ch)
	if evt.Type != EventStreamReplaced {
		t.Fatalf("type = %q, want %q", evt.Type, EventStreamReplaced)
	}
	if !c.stopReconnect.Load() {
		t.Fatal("stopReconnect should be set after StreamReplaced")
	}
	if got := c.State(); got != StateStreamReplaced {
		t.Fatalf("state = %q, want %q", got, StateStreamReplaced)
	}
}

func TestDisconnectedPreservesTerminalState(t *testing.T) {
	c := newTestClient(t)
	ch, unsub := c.Subscribe()
	t.Cleanup(unsub)

	c.rawCh <- &events.LoggedOut{}
	_ = waitEvent(t, ch)

	c.rawCh <- &events.Disconnected{}
	evt := waitEvent(t, ch)
	if evt.Type != EventDisconnected {
		t.Fatalf("type = %q, want %q", evt.Type, EventDisconnected)
	}
	if got := c.State(); got != StateLoggedOut {
		t.Fatalf("state = %q, want %q (Disconnected should not overwrite LoggedOut)", got, StateLoggedOut)
	}
}

func TestTemporaryBanCarriesExpireAndCode(t *testing.T) {
	c := newTestClient(t)
	ch, unsub := c.Subscribe()
	t.Cleanup(unsub)

	c.rawCh <- &events.TemporaryBan{
		Code:   events.TempBanSentToTooManyPeople,
		Expire: 12 * time.Hour,
	}

	evt := waitEvent(t, ch)
	if evt.Type != EventTemporaryBan {
		t.Fatalf("type = %q, want %q", evt.Type, EventTemporaryBan)
	}
	if evt.BanExpire != 12*time.Hour {
		t.Fatalf("BanExpire = %v, want 12h", evt.BanExpire)
	}
	if evt.BanCode != int(events.TempBanSentToTooManyPeople) {
		t.Fatalf("BanCode = %d, want %d", evt.BanCode, events.TempBanSentToTooManyPeople)
	}
}

func TestConnectionFailureCarriesReason(t *testing.T) {
	c := newTestClient(t)
	ch, unsub := c.Subscribe()
	t.Cleanup(unsub)

	c.rawCh <- &events.ConnectFailure{
		Reason:  events.ConnectFailureServiceUnavailable,
		Message: "try again later",
	}

	evt := waitEvent(t, ch)
	if evt.Type != EventConnectionFailure {
		t.Fatalf("type = %q, want %q", evt.Type, EventConnectionFailure)
	}
	if evt.FailureReason != int(events.ConnectFailureServiceUnavailable) {
		t.Fatalf("FailureReason = %d, want %d", evt.FailureReason, events.ConnectFailureServiceUnavailable)
	}
	if evt.FailureMessage != "try again later" {
		t.Fatalf("FailureMessage = %q", evt.FailureMessage)
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	c := newTestClient(t)
	ch, unsub := c.Subscribe()
	unsub()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected closed channel, got value")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for close")
	}

	unsub() // second call must be a no-op
}

func TestConnectReturnsErrNotPairedWhenUnpaired(t *testing.T) {
	c := newTestClient(t)
	if err := c.Connect(context.Background()); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("Connect on fresh dir: want ErrNotPaired, got %v", err)
	}
}

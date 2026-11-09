package sessions

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeInstance is an Instance with no WhatsApp behind it.
type fakeInstance struct {
	id        string
	status    Status
	logoutErr error

	mu       sync.Mutex
	closed   bool
	loggedOn bool
}

func (f *fakeInstance) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, f.id)
	})
}
func (f *fakeInstance) Status() Status { return f.status }
func (f *fakeInstance) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loggedOn = true
	return f.logoutErr
}
func (f *fakeInstance) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

type fakeOpener struct {
	mu      sync.Mutex
	opened  map[string]*fakeInstance
	failIDs map[string]bool
}

func newFakeOpener() *fakeOpener {
	return &fakeOpener{opened: map[string]*fakeInstance{}, failIDs: map[string]bool{}}
}

func (o *fakeOpener) open(id, dir string) (Instance, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failIDs[id] {
		return nil, errors.New("boom")
	}
	// Mimic a real session writing into its directory.
	if err := os.WriteFile(filepath.Join(dir, "session.db"), []byte("x"), 0o600); err != nil {
		return nil, err
	}
	inst := &fakeInstance{id: id, status: StatusUnpaired}
	o.opened[id] = inst
	return inst, nil
}

func (o *fakeOpener) get(id string) *fakeInstance {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opened[id]
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type harness struct {
	root    string
	regPath string
	reg     *Registry
	opener  *fakeOpener
	mgr     *Manager
}

func newHarness(t *testing.T, maxSessions int) *harness {
	t.Helper()
	dataDir := t.TempDir()
	h := &harness{
		root:    filepath.Join(dataDir, "sessions"),
		regPath: filepath.Join(dataDir, "sessions.db"),
		opener:  newFakeOpener(),
	}
	var err error
	h.reg, err = OpenRegistry(context.Background(), h.regPath)
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	t.Cleanup(func() { _ = h.reg.Close() })
	h.mgr = NewManager(h.reg, h.root, maxSessions, h.opener.open, quietLogger())
	if err := h.mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return h
}

func TestToken_ShapeAndUniqueness(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 50 {
		tok, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if !ValidTokenShape(tok) {
			t.Fatalf("token %q fails its own shape check", tok)
		}
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 42), strings.Repeat("a", 44), strings.Repeat("+", 43)} {
		if ValidTokenShape(bad) {
			t.Errorf("ValidTokenShape(%q) = true", bad)
		}
	}
}

func TestManager_CreateLookupList(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 5)
	ctx := context.Background()

	a, err := h.mgr.Create(ctx, "alice")
	if err != nil {
		t.Fatalf("Create alice: %v", err)
	}
	b, err := h.mgr.Create(ctx, "bob")
	if err != nil {
		t.Fatalf("Create bob: %v", err)
	}
	if a.Adopted || b.Adopted {
		t.Errorf("fresh sessions reported adopted: %+v %+v", a, b)
	}
	if a.Token == b.Token {
		t.Fatal("two sessions share a token")
	}

	for _, tc := range []struct{ token, want string }{{a.Token, "alice"}, {b.Token, "bob"}} {
		id, inst, found := h.mgr.Lookup(tc.token)
		if !found || id != tc.want || inst == nil {
			t.Errorf("Lookup -> (%q, %v, %v), want %q", id, inst, found, tc.want)
		}
	}
	if _, _, found := h.mgr.Lookup("x" + a.Token[1:]); found {
		t.Error("a mutated token resolved to a session")
	}

	list := h.mgr.List()
	if len(list) != 2 || list[0].ID != "alice" || list[1].ID != "bob" {
		t.Fatalf("List = %+v", list)
	}
	if list[0].Status != StatusUnpaired {
		t.Errorf("status = %q, want unpaired", list[0].Status)
	}

	if _, err := h.mgr.Create(ctx, "alice"); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate Create: err = %v, want ErrExists", err)
	}
}

func TestManager_RejectsInvalidIDs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 5)
	for _, id := range []string{"", "..", "../x", "a/b", "A", ".hidden", "-x"} {
		if _, err := h.mgr.Create(context.Background(), id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Create(%q): err = %v, want ErrInvalidID", id, err)
		}
	}
	ents, _ := os.ReadDir(h.root)
	if len(ents) != 0 {
		t.Errorf("invalid ids left directories behind: %v", ents)
	}
}

func TestManager_EnforcesLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if _, err := h.mgr.Create(ctx, id); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	if _, err := h.mgr.Create(ctx, "c"); !errors.Is(err, ErrLimit) {
		t.Fatalf("third Create: err = %v, want ErrLimit", err)
	}
	if _, err := h.mgr.Delete(ctx, "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := h.mgr.Create(ctx, "c"); err != nil {
		t.Fatalf("Create after Delete: %v", err)
	}
}

func TestManager_DeleteInvalidatesTokenAndWipesDir(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 5)
	ctx := context.Background()
	c, err := h.mgr.Create(ctx, "alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inst := h.opener.get("alice")
	inst.status = StatusConnected

	res, err := h.mgr.Delete(ctx, "alice")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !res.RemoteLogout || !inst.loggedOn || !inst.closed {
		t.Errorf("Delete did not log out and close: res=%+v inst=%+v", res, inst)
	}
	if _, _, found := h.mgr.Lookup(c.Token); found {
		t.Error("token still resolves after Delete")
	}
	if _, err := os.Stat(filepath.Join(h.root, "alice")); !os.IsNotExist(err) {
		t.Errorf("session dir survived Delete: %v", err)
	}
	recs, _ := h.reg.All(ctx)
	if len(recs) != 0 {
		t.Errorf("registry still has %v", recs)
	}
	if _, err := h.mgr.Delete(ctx, "alice"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete: err = %v, want ErrNotFound", err)
	}
}

// An offline session cannot be unlinked remotely; its data must be wiped
// anyway, and the response must say the phone may still list the device.
func TestManager_DeleteWipesEvenWhenLogoutFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 5)
	ctx := context.Background()
	if _, err := h.mgr.Create(ctx, "alice"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	inst := h.opener.get("alice")
	inst.status = StatusConnected
	inst.logoutErr = errors.New("socket dropped mid-logout")

	res, err := h.mgr.Delete(ctx, "alice")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if res.RemoteLogout {
		t.Error("RemoteLogout = true despite logout failing")
	}
	if _, err := os.Stat(filepath.Join(h.root, "alice")); !os.IsNotExist(err) {
		t.Errorf("session dir survived Delete: %v", err)
	}
}

// The registry must never hold a token, only its hash: a copy of the
// volume must not be enough to impersonate a session.
func TestRegistry_StoresOnlyTokenHashes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 5)
	c, err := h.mgr.Create(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.reg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	dir := filepath.Dir(h.regPath)
	files, _ := filepath.Glob(filepath.Join(dir, "sessions.db*"))
	if len(files) == 0 {
		t.Fatal("no registry files found")
	}
	hash := HashToken(c.Token)
	var sawHash bool
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if bytes.Contains(raw, []byte(c.Token)) {
			t.Fatalf("%s contains the raw token", f)
		}
		if bytes.Contains(raw, hash[:]) {
			sawHash = true
		}
	}
	if !sawHash {
		t.Error("token hash not found in registry files; is the test looking at the right database?")
	}
}

// Sessions survive a restart: a new Manager over the same registry and
// root reopens them and the original tokens still resolve.
func TestManager_StartReopensRegisteredSessions(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 5)
	ctx := context.Background()
	c, err := h.mgr.Create(ctx, "alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.mgr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	opener := newFakeOpener()
	opener.failIDs["broken"] = true
	if err := h.reg.Insert(ctx, Record{ID: "broken", Hash: HashToken("unused")}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	mgr := NewManager(h.reg, h.root, 5, opener.open, quietLogger())
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	id, inst, found := mgr.Lookup(c.Token)
	if !found || id != "alice" || inst == nil {
		t.Fatalf("Lookup after restart = (%q, %v, %v)", id, inst, found)
	}
	// One session failing to open must not take the others down.
	list := mgr.List()
	if len(list) != 2 || list[1].ID != "broken" || list[1].Status != StatusError {
		t.Fatalf("List = %+v, want broken session in error state", list)
	}
}

// Creating a session whose directory already exists adopts it — the 0.7
// migration path — and a failed open must never delete adopted data.
func TestManager_AdoptsExistingDirectory(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 5)
	ctx := context.Background()
	dir := filepath.Join(h.root, "default")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "cache.db")
	if err := os.WriteFile(keep, []byte("history"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.opener.failIDs["default"] = true
	if _, err := h.mgr.Create(ctx, "default"); err == nil {
		t.Fatal("Create with failing opener succeeded")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("failed Create deleted adopted data: %v", err)
	}

	h.opener.failIDs["default"] = false
	c, err := h.mgr.Create(ctx, "default")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !c.Adopted {
		t.Error("Adopted = false for a pre-existing directory")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("adopted data missing: %v", err)
	}
}

// Unpaired sessions have nothing to unlink: Delete must not even try.
func TestManager_DeleteSkipsLogoutWhenNotConnected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 5)
	ctx := context.Background()
	for _, st := range []Status{StatusUnpaired, StatusDisconnected, StatusLoggedOut} {
		id := strings.ReplaceAll(string(st), "_", "-")
		if _, err := h.mgr.Create(ctx, id); err != nil {
			t.Fatalf("Create: %v", err)
		}
		inst := h.opener.get(id)
		inst.status = st
		res, err := h.mgr.Delete(ctx, id)
		if err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if inst.loggedOn || res.RemoteLogout {
			t.Errorf("%s: Logout attempted (res=%+v)", st, res)
		}
		if !inst.closed {
			t.Errorf("%s: not closed", st)
		}
	}
}

func TestManager_FailedCreateCleansUpFreshDirectory(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 5)
	h.opener.failIDs["alice"] = true
	if _, err := h.mgr.Create(context.Background(), "alice"); err == nil {
		t.Fatal("Create with failing opener succeeded")
	}
	if _, err := os.Stat(filepath.Join(h.root, "alice")); !os.IsNotExist(err) {
		t.Errorf("failed Create left its directory: %v", err)
	}
	if h.mgr.Count() != 0 {
		t.Errorf("Count = %d after failed Create", h.mgr.Count())
	}
}

func TestManager_ConcurrentCreatesRespectLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 3)
	var (
		wg sync.WaitGroup
		mu sync.Mutex
		ok int
	)
	for i := range 10 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := h.mgr.Create(context.Background(), string(rune('a'+i))); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if ok != 3 || h.mgr.Count() != 3 {
		t.Fatalf("created %d (Count %d), want exactly 3", ok, h.mgr.Count())
	}
}

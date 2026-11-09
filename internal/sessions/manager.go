package sessions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/config"
)

// Instance is what the Manager needs from an open session. *Session
// implements it; tests substitute fakes.
type Instance interface {
	Handler() http.Handler
	Status() Status
	Logout(ctx context.Context) error
	Close() error
}

// Opener opens the session with the given id rooted at dir.
type Opener func(id, dir string) (Instance, error)

// Manager errors, mapped to HTTP status codes by the admin API.
var (
	ErrInvalidID = errors.New("sessions: invalid session id")
	ErrExists    = errors.New("sessions: session already exists")
	ErrLimit     = errors.New("sessions: session limit reached")
	ErrNotFound  = errors.New("sessions: no such session")
)

// logoutTimeout bounds the best-effort remote unlink during Delete.
const logoutTimeout = 10 * time.Second

// Info describes one session for GET /sessions. It never carries the token
// or its hash.
type Info struct {
	ID        string    `json:"id"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// Created is the result of Create. Token is shown exactly once.
type Created struct {
	ID    string `json:"id"`
	Token string `json:"token"`
	// Adopted is true when the session's directory already existed — the
	// 0.7 migration path — so an existing pairing and cache were picked up
	// rather than a fresh unpaired session created.
	Adopted bool `json:"adopted"`
}

// Deleted is the result of Delete.
type Deleted struct {
	ID string `json:"id"`
	// RemoteLogout reports whether the device was unlinked from the phone.
	// False when the session was offline or unpaired: local data is wiped
	// regardless, but the phone may still list the device until the user
	// removes it under Linked devices.
	RemoteLogout bool `json:"remote_logout"`
}

type entry struct {
	id        string
	hash      TokenHash
	createdAt time.Time
	// inst is nil when the session failed to open at startup.
	inst Instance
}

// Manager owns every hosted session: the registry, the open instances and
// the token-hash index requests are routed by.
type Manager struct {
	reg     *Registry
	root    string
	max     int
	open    Opener
	log     *slog.Logger
	storage Storage

	mu      sync.RWMutex
	byID    map[string]*entry
	byHash  map[TokenHash]*entry
	pending map[string]struct{} // ids mid-Create, counted against max
}

// NewManager builds a Manager hosting at most maxSessions sessions under root.
func NewManager(reg *Registry, root string, maxSessions int, open Opener, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		reg:     reg,
		root:    root,
		max:     maxSessions,
		open:    open,
		log:     log,
		storage: FSStorage{},
		byID:    map[string]*entry{},
		byHash:  map[TokenHash]*entry{},
		pending: map[string]struct{}{},
	}
}

// WithStorage sets where session state lives (FSStorage by default) and
// returns m. Call it before Start.
func (m *Manager) WithStorage(s Storage) *Manager {
	m.storage = s
	return m
}

// Start opens every registered session. A session that fails to open is
// logged and kept with StatusError rather than taking every other session
// down with it; its token is refused with 503 until a restart.
func (m *Manager) Start(ctx context.Context) error {
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return fmt.Errorf("sessions: create %s: %w", m.root, err)
	}
	recs, err := m.reg.All(ctx)
	if err != nil {
		return err
	}
	registered := make(map[string]struct{}, len(recs))
	for _, rec := range recs {
		registered[rec.ID] = struct{}{}
		e := &entry{id: rec.ID, hash: rec.Hash, createdAt: rec.CreatedAt}
		dir, derr := m.dirFor(rec.ID)
		if derr == nil {
			e.inst, derr = m.open(rec.ID, dir)
		}
		if derr != nil {
			m.log.Error("session failed to open; its token will get 503 until restart",
				slog.String("event_type", "sessions.open_failed"),
				slog.String("session", rec.ID),
				slog.String("err", derr.Error()))
		}
		m.mu.Lock()
		m.byID[rec.ID] = e
		m.byHash[rec.Hash] = e
		m.mu.Unlock()
	}
	m.warnOrphans(ctx, registered)
	m.log.Info("sessions started",
		slog.String("event_type", "sessions.start"),
		slog.Int("sessions", len(recs)))
	return nil
}

// warnOrphans logs stored session state with no registry row: data a POST
// /sessions with that id would adopt (the 0.7 migration path), or debris
// from an interrupted Delete.
func (m *Manager) warnOrphans(ctx context.Context, registered map[string]struct{}) {
	orphans, err := m.storage.Orphans(ctx, m.root, registered)
	if err != nil {
		return
	}
	for _, name := range orphans {
		m.log.Warn("session state has no registry entry; POST /sessions with this id adopts it",
			slog.String("event_type", "sessions.orphan_dir"),
			slog.String("session", name))
	}
}

// dirFor maps an id to its directory, refusing anything that would escape
// root. ValidSessionID already admits no separators or dots; this is the
// belt to that brace.
func (m *Manager) dirFor(id string) (string, error) {
	if !config.ValidSessionID(id) {
		return "", ErrInvalidID
	}
	dir := filepath.Join(m.root, id)
	rel, err := filepath.Rel(m.root, dir)
	if err != nil || rel != id || strings.HasPrefix(rel, "..") {
		return "", ErrInvalidID
	}
	return dir, nil
}

// Create registers and opens a new session and returns its token. If the
// session's directory already exists its contents are adopted.
func (m *Manager) Create(ctx context.Context, id string) (Created, error) {
	dir, err := m.dirFor(id)
	if err != nil {
		return Created{}, err
	}

	m.mu.Lock()
	if _, ok := m.byID[id]; ok {
		m.mu.Unlock()
		return Created{}, ErrExists
	}
	if _, ok := m.pending[id]; ok {
		m.mu.Unlock()
		return Created{}, ErrExists
	}
	if len(m.byID)+len(m.pending) >= m.max {
		m.mu.Unlock()
		return Created{}, ErrLimit
	}
	m.pending[id] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
	}()

	adopted, err := m.storage.Exists(ctx, id, dir)
	if err != nil {
		return Created{}, fmt.Errorf("sessions: check %s: %w", id, err)
	}
	// Only ever remove state this call created: adopted state holds
	// someone's pairing and history.
	cleanup := func() {
		if !adopted {
			_ = m.storage.Wipe(context.WithoutCancel(ctx), id, dir)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Created{}, fmt.Errorf("sessions: create %s: %w", dir, err)
	}

	token, err := NewToken()
	if err != nil {
		cleanup()
		return Created{}, err
	}
	rec := Record{ID: id, Hash: HashToken(token), CreatedAt: time.Now().UTC().Truncate(time.Second)}

	inst, err := m.open(id, dir)
	if err != nil {
		cleanup()
		return Created{}, fmt.Errorf("sessions: open %s: %w", id, err)
	}
	if err := m.reg.Insert(ctx, rec); err != nil {
		_ = inst.Close()
		cleanup()
		if errors.Is(err, ErrDuplicate) {
			return Created{}, ErrExists
		}
		return Created{}, err
	}

	e := &entry{id: id, hash: rec.Hash, createdAt: rec.CreatedAt, inst: inst}
	m.mu.Lock()
	m.byID[id] = e
	m.byHash[rec.Hash] = e
	m.mu.Unlock()

	m.log.Info("session created",
		slog.String("event_type", "sessions.create"),
		slog.String("session", id),
		slog.Bool("adopted", adopted))
	return Created{ID: id, Token: token, Adopted: adopted}, nil
}

// Delete unlinks the session's device (best-effort), closes it and wipes
// its directory. Its token stops working before any of that starts.
func (m *Manager) Delete(ctx context.Context, id string) (Deleted, error) {
	if !config.ValidSessionID(id) {
		return Deleted{}, ErrNotFound
	}
	m.mu.Lock()
	e, ok := m.byID[id]
	if !ok {
		m.mu.Unlock()
		return Deleted{}, ErrNotFound
	}
	delete(m.byID, id)
	delete(m.byHash, e.hash)
	m.mu.Unlock()

	// Unregister first: a crash after this leaves an orphan directory
	// (logged at the next start), never a registered session whose data
	// has already been wiped.
	if err := m.reg.Delete(ctx, id); err != nil {
		m.mu.Lock()
		m.byID[id] = e
		m.byHash[e.hash] = e
		m.mu.Unlock()
		return Deleted{}, err
	}

	res := Deleted{ID: id}
	if e.inst != nil {
		// Only a connected session can unlink itself from the phone. An
		// unpaired or logged-out one has nothing to unlink; a disconnected
		// one stays listed on the phone, which is worth a warning.
		switch e.inst.Status() {
		case StatusConnected:
			lctx, cancel := context.WithTimeout(ctx, logoutTimeout)
			if err := e.inst.Logout(lctx); err == nil {
				res.RemoteLogout = true
			} else {
				m.log.Warn("remote logout failed; wiping local data anyway",
					slog.String("event_type", "sessions.logout_failed"),
					slog.String("session", id),
					slog.String("err", err.Error()))
			}
			cancel()
		case StatusDisconnected:
			m.log.Warn("session is offline, so the phone still lists this device until removed under Linked devices",
				slog.String("event_type", "sessions.logout_skipped"),
				slog.String("session", id))
		}
		if err := e.inst.Close(); err != nil {
			m.log.Warn("session close",
				slog.String("session", id),
				slog.String("err", err.Error()))
		}
	}

	dir, err := m.dirFor(id)
	if err != nil {
		return res, err
	}
	if err := m.storage.Wipe(ctx, id, dir); err != nil {
		return res, err
	}
	m.log.Info("session deleted",
		slog.String("event_type", "sessions.delete"),
		slog.String("session", id),
		slog.Bool("remote_logout", res.RemoteLogout))
	return res, nil
}

// Lookup resolves a bearer token to its session. found reports whether the
// token is registered; inst is nil when it is but the session failed to
// open.
func (m *Manager) Lookup(token string) (id string, inst Instance, found bool) {
	if !ValidTokenShape(token) {
		return "", nil, false
	}
	h := HashToken(token)
	m.mu.RLock()
	e, ok := m.byHash[h]
	m.mu.RUnlock()
	if !ok {
		return "", nil, false
	}
	return e.id, e.inst, true
}

// List describes every session, ordered by id.
func (m *Manager) List() []Info {
	m.mu.RLock()
	entries := make([]*entry, 0, len(m.byID))
	for _, e := range m.byID {
		entries = append(entries, e)
	}
	m.mu.RUnlock()

	out := make([]Info, 0, len(entries))
	for _, e := range entries {
		st := StatusError
		if e.inst != nil {
			st = e.inst.Status()
		}
		out = append(out, Info{ID: e.id, Status: st, CreatedAt: e.createdAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Count returns the number of registered sessions.
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byID)
}

// Close closes every open session. The registry is left to its owner.
func (m *Manager) Close() error {
	m.mu.Lock()
	entries := make([]*entry, 0, len(m.byID))
	for _, e := range m.byID {
		entries = append(entries, e)
	}
	m.byID = map[string]*entry{}
	m.byHash = map[TokenHash]*entry{}
	m.mu.Unlock()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, e := range entries {
		if e.inst == nil {
			continue
		}
		wg.Add(1)
		go func(e *entry) {
			defer wg.Done()
			if err := e.inst.Close(); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("close %s: %w", e.id, err))
				mu.Unlock()
			}
		}(e)
	}
	wg.Wait()
	return errors.Join(errs...)
}

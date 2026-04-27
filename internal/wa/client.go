package wa

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"

	// Register the pure-Go SQLite driver under the name "sqlite" so that
	// whatsmeow's sqlstore can open it without a cgo runtime.
	_ "modernc.org/sqlite"
)

// DefaultDataDir is used when Config.DataDir is empty.
const DefaultDataDir = "/data"

// DefaultPairClientDisplay is the "Browser (OS)" string the WhatsApp server
// accepts when pairing via phone code; it appears on the user's phone.
const DefaultPairClientDisplay = "Chrome (Linux)"

// Errors returned by the admin ops on Client.
var (
	// ErrNotPaired is returned by Connect when no paired device is present.
	ErrNotPaired = errors.New("wa: no paired device present")
	// ErrAlreadyPaired is returned by StartPairing when a device is already
	// present on disk. Callers should call Unpair first.
	ErrAlreadyPaired = errors.New("wa: already paired")
	// ErrPairInProgress is returned by StartPairing when another pair flow
	// already holds the admin lock.
	ErrPairInProgress = errors.New("wa: pair flow already in progress")
	// ErrNotPairing is returned by PairPhone when no pair flow is active
	// (PairPhone shares the socket opened by StartPairing).
	ErrNotPairing = errors.New("wa: no active pair flow; call /admin/pair/start first")
)

// Config controls how the wa.Client is constructed.
type Config struct {
	// DataDir is the persistent volume root (default: /data). The sqlstore
	// lives at {DataDir}/session.db and the exclusive lock at {DataDir}/.lock.
	DataDir string
	// Logger receives whatsmeow's internal logs. Nil defaults to Noop.
	Logger waLog.Logger
	// PairDeviceName is the label shown on the user's phone after pairing.
	// Empty defaults to "whatsapp-mcp".
	PairDeviceName string
	// PairClientDisplay is the "Browser (OS)" string sent during phone-code
	// pairing. Empty defaults to DefaultPairClientDisplay.
	PairClientDisplay string
	// EventHook, if non-nil, is invoked synchronously by the dispatcher
	// for every raw whatsmeow event after the lifecycle handler runs.
	// Used to fan events to the cache ingestor without coupling wa to it.
	EventHook func(evt any)
}

// Client owns the whatsmeow client, the sqlstore container, and the lifecycle
// event dispatcher. Exactly one Client may run per DataDir at a time.
type Client struct {
	log waLog.Logger
	cfg Config

	lock *lockfile

	// adminMu serializes mutating admin operations (StartPairing, PairPhone,
	// Unpair) against each other. Holders must not also hold mu.
	adminMu sync.Mutex

	// pairing is non-nil while a /admin/pair/start flow is active. Protected
	// by adminMu.
	pairing *pairSession

	mu        sync.Mutex
	container *sqlstore.Container
	device    *store.Device
	wm        *whatsmeow.Client
	handlerID uint32

	stopReconnect atomic.Bool

	state        State
	subs         []*subscriber
	lastEvent    Event
	lastEventSet bool

	rawCh chan any
	quit  chan struct{}
	done  chan struct{}

	closeOnce sync.Once
}

// Open acquires the data-dir lock, opens the sqlstore, constructs the
// whatsmeow client, and starts the event dispatcher. When a paired device is
// present on disk the client attempts an initial connect; when no device
// exists the client stays disconnected in not_paired state until the caller
// drives the pairing flow.
func Open(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir
	}
	if cfg.Logger == nil {
		cfg.Logger = waLog.Noop
	}
	if cfg.PairDeviceName == "" {
		cfg.PairDeviceName = "whatsapp-mcp"
	}
	if cfg.PairClientDisplay == "" {
		cfg.PairClientDisplay = DefaultPairClientDisplay
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", cfg.DataDir, err)
	}

	lock, err := acquireLock(filepath.Join(cfg.DataDir, ".lock"))
	if err != nil {
		return nil, err
	}

	c := &Client{
		log:   cfg.Logger,
		cfg:   cfg,
		lock:  lock,
		rawCh: make(chan any, 64),
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
	}

	if err := c.openWhatsmeow(ctx); err != nil {
		_ = lock.release()
		return nil, err
	}

	if c.device.ID == nil {
		c.state = StateNotPaired
	} else {
		c.state = StateDisconnected
	}

	go c.dispatch()

	if c.device.ID != nil {
		c.mu.Lock()
		c.state = StateConnecting
		wm := c.wm
		c.mu.Unlock()
		if cerr := wm.Connect(); cerr != nil {
			cfg.Logger.Warnf("wa: initial connect failed; auto-reconnect will retry: %v", cerr)
		}
	}

	return c, nil
}

// openWhatsmeow opens the sqlstore, loads (or creates) the first device, and
// builds a whatsmeow client wired to this client's dispatch channel. It
// populates c.container/c.device/c.wm/c.handlerID. Callers must not hold
// c.mu; openWhatsmeow does not touch c.state or c.stopReconnect.
func (c *Client) openWhatsmeow(ctx context.Context) error {
	dsn := sessionDSN(c.cfg.DataDir)
	container, err := sqlstore.New(ctx, "sqlite", dsn, c.log)
	if err != nil {
		return fmt.Errorf("open sqlstore: %w", err)
	}

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		_ = container.Close()
		return fmt.Errorf("load device: %w", err)
	}

	wm := whatsmeow.NewClient(device, c.log)
	if wm == nil {
		_ = container.Close()
		return errors.New("wa: whatsmeow.NewClient returned nil")
	}

	wm.EnableAutoReconnect = true
	wm.AutoReconnectHook = func(err error) bool {
		if c.stopReconnect.Load() {
			c.log.Infof("wa: auto-reconnect suppressed after logout/stream_replaced (%v)", err)
			return false
		}
		return true
	}

	handlerID := wm.AddEventHandler(func(evt any) {
		select {
		case c.rawCh <- evt:
		case <-c.quit:
		}
	})

	c.mu.Lock()
	c.container = container
	c.device = device
	c.wm = wm
	c.handlerID = handlerID
	c.mu.Unlock()
	return nil
}

// Connect explicitly connects the underlying whatsmeow client. It is a no-op
// if already connected, and returns ErrNotPaired when no device is present
// (the pair flow must drive the pre-pair connection via the QR channel).
func (c *Client) Connect(_ context.Context) error {
	c.mu.Lock()
	wm, device := c.wm, c.device
	if device.ID == nil {
		c.mu.Unlock()
		return ErrNotPaired
	}
	if wm.IsConnected() {
		c.mu.Unlock()
		return nil
	}
	c.stopReconnect.Store(false)
	c.state = StateConnecting
	c.mu.Unlock()
	return wm.Connect()
}

// Disconnect tears down the WhatsApp socket without deleting the on-disk
// session. Auto-reconnect is suppressed until the next Connect.
func (c *Client) Disconnect() {
	c.stopReconnect.Store(true)
	c.mu.Lock()
	wm := c.wm
	c.mu.Unlock()
	wm.Disconnect()
}

// Close disconnects, stops the dispatcher, closes the sqlstore, and releases
// the data-dir lock. It is safe to call multiple times.
func (c *Client) Close() error {
	var result error
	c.closeOnce.Do(func() {
		c.stopReconnect.Store(true)
		c.mu.Lock()
		wm := c.wm
		container := c.container
		c.mu.Unlock()
		if wm != nil {
			wm.Disconnect()
		}
		close(c.quit)
		<-c.done

		var errs []error
		if container != nil {
			if err := container.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close sqlstore: %w", err))
			}
		}
		if err := c.lock.release(); err != nil {
			errs = append(errs, fmt.Errorf("release lock: %w", err))
		}
		result = errors.Join(errs...)
	})
	return result
}

// IsPaired reports whether a paired device is present on disk.
func (c *Client) IsPaired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.device != nil && c.device.ID != nil
}

// Whatsmeow returns the underlying whatsmeow client. Callers must treat it as
// read-mostly; event handling is already owned by this package.
func (c *Client) Whatsmeow() *whatsmeow.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wm
}

// Status returns a point-in-time snapshot of the client's lifecycle state.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{
		State:     c.state,
		Connected: c.wm != nil && c.wm.IsConnected(),
		LoggedIn:  c.wm != nil && c.wm.IsLoggedIn(),
	}
	if c.device != nil && c.device.ID != nil {
		st.JID = c.device.ID.String()
		st.Pushname = c.device.PushName
	}
	if c.lastEventSet {
		evt := c.lastEvent
		st.LastEvent = &evt
	}
	return st
}

func sessionDSN(dir string) string {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(on)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	return "file:" + filepath.Join(dir, "session.db") + "?" + q.Encode()
}

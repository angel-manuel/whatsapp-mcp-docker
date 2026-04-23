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

	// Register the CGO SQLite driver under the name "sqlite3" so that
	// whatsmeow's sqlstore can open it.
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// DefaultDataDir is used when Config.DataDir is empty.
const DefaultDataDir = "/data"

// ErrNotPaired is returned by Connect when no paired device is present.
var ErrNotPaired = errors.New("wa: no paired device present")

// Config controls how the wa.Client is constructed.
type Config struct {
	// DataDir is the persistent volume root (default: /data). The sqlstore
	// lives at {DataDir}/session.db and the exclusive lock at {DataDir}/.lock.
	DataDir string
	// Logger receives whatsmeow's internal logs. Nil defaults to Noop.
	Logger waLog.Logger
}

// Client owns the whatsmeow client, the sqlstore container, and the lifecycle
// event dispatcher. Exactly one Client may run per DataDir at a time.
type Client struct {
	log waLog.Logger
	cfg Config

	lock      *lockfile
	container *sqlstore.Container
	device    *store.Device
	wm        *whatsmeow.Client

	stopReconnect atomic.Bool

	mu    sync.Mutex
	state State
	subs  []*subscriber
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
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", cfg.DataDir, err)
	}

	lock, err := acquireLock(filepath.Join(cfg.DataDir, ".lock"))
	if err != nil {
		return nil, err
	}

	dsn := sessionDSN(cfg.DataDir)
	container, err := sqlstore.New(ctx, "sqlite3", dsn, cfg.Logger)
	if err != nil {
		_ = lock.release()
		return nil, fmt.Errorf("open sqlstore: %w", err)
	}

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		_ = container.Close()
		_ = lock.release()
		return nil, fmt.Errorf("load device: %w", err)
	}

	wm := whatsmeow.NewClient(device, cfg.Logger)
	if wm == nil {
		_ = container.Close()
		_ = lock.release()
		return nil, errors.New("wa: whatsmeow.NewClient returned nil")
	}

	c := &Client{
		log:       cfg.Logger,
		cfg:       cfg,
		lock:      lock,
		container: container,
		device:    device,
		wm:        wm,
		rawCh:     make(chan any, 64),
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	if device.ID == nil {
		c.state = StateNotPaired
	} else {
		c.state = StateDisconnected
	}

	wm.EnableAutoReconnect = true
	wm.AutoReconnectHook = func(err error) bool {
		if c.stopReconnect.Load() {
			cfg.Logger.Infof("wa: auto-reconnect suppressed after logout/stream_replaced (%v)", err)
			return false
		}
		return true
	}

	wm.AddEventHandler(func(evt any) {
		select {
		case c.rawCh <- evt:
		case <-c.quit:
		}
	})

	go c.dispatch()

	if device.ID != nil {
		c.mu.Lock()
		c.state = StateConnecting
		c.mu.Unlock()
		if cerr := wm.Connect(); cerr != nil {
			cfg.Logger.Warnf("wa: initial connect failed; auto-reconnect will retry: %v", cerr)
		}
	}

	return c, nil
}

// Connect explicitly connects the underlying whatsmeow client. It is a no-op
// if already connected, and returns ErrNotPaired when no device is present
// (the pair flow must drive the pre-pair connection via the QR channel).
func (c *Client) Connect(_ context.Context) error {
	c.mu.Lock()
	if c.device.ID == nil {
		c.mu.Unlock()
		return ErrNotPaired
	}
	if c.wm.IsConnected() {
		c.mu.Unlock()
		return nil
	}
	c.stopReconnect.Store(false)
	c.state = StateConnecting
	c.mu.Unlock()
	return c.wm.Connect()
}

// Disconnect tears down the WhatsApp socket without deleting the on-disk
// session. Auto-reconnect is suppressed until the next Connect.
func (c *Client) Disconnect() {
	c.stopReconnect.Store(true)
	c.wm.Disconnect()
}

// Close disconnects, stops the dispatcher, closes the sqlstore, and releases
// the data-dir lock. It is safe to call multiple times.
func (c *Client) Close() error {
	var result error
	c.closeOnce.Do(func() {
		c.stopReconnect.Store(true)
		c.wm.Disconnect()
		close(c.quit)
		<-c.done

		var errs []error
		if err := c.container.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close sqlstore: %w", err))
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
	return c.device.ID != nil
}

// Whatsmeow returns the underlying whatsmeow client. Callers must treat it as
// read-mostly; event handling is already owned by this package.
func (c *Client) Whatsmeow() *whatsmeow.Client {
	return c.wm
}

func sessionDSN(dir string) string {
	q := url.Values{}
	q.Set("_foreign_keys", "on")
	q.Set("_journal_mode", "WAL")
	q.Set("_busy_timeout", "5000")
	return "file:" + filepath.Join(dir, "session.db") + "?" + q.Encode()
}

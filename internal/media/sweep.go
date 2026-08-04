package media

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	applog "github.com/angel-manuel/whatsapp-mcp-docker/internal/log"
)

// DefaultSweepInterval is how often RunSweeper reclaims space when the
// caller does not specify an interval.
const DefaultSweepInterval = time.Hour

// SweepResult reports what a single Sweep pass reclaimed. RemainingBytes is
// the store size after eviction, which is what an operator wants to compare
// against MEDIA_MAX_BYTES.
type SweepResult struct {
	ScannedBlobs   int
	ExpiredBlobs   int
	EvictedBlobs   int
	FreedBytes     int64
	RemainingBytes int64
}

// entry is one stored blob as seen by the sweeper. The two timestamps come
// from different files on purpose: storedAt is the blob's own mtime (never
// rewritten), usedAt is the sidecar's (bumped by every Lookup/Open).
type entry struct {
	digest   string
	name     string
	size     int64
	storedAt time.Time
	usedAt   time.Time
}

// Sweep enforces the configured retention policy against the wall clock
// value now. It runs in two passes:
//
//  1. TTL: anything *downloaded* longer than Options.TTL ago is removed
//     outright. Age is the blob's own mtime, so re-reading a file does not
//     keep it alive past its expiry.
//  2. Size: while the store exceeds Options.MaxBytes, the least recently
//     *used* blob is removed. Recency is the sidecar's mtime, which Lookup
//     and Open bump on every hit — so a blob fetched every minute outlives
//     one downloaded later and never touched again.
//
// Orphans (a blob with no sidecar, a sidecar with no blob, an abandoned temp
// file) are cleaned up as they are encountered: they would otherwise consume
// space that no lookup can ever reach.
//
// A blob that cannot be removed is logged and skipped rather than aborting
// the pass; retention is best-effort by nature.
func (s *Store) Sweep(now time.Time) (SweepResult, error) {
	dirEntries, err := os.ReadDir(s.dir)
	if err != nil {
		return SweepResult{}, fmt.Errorf("media: read %s: %w", s.dir, err)
	}

	blobs := make(map[string]*entry, len(dirEntries))
	// digest -> sidecar mtime, i.e. when the object was last resolved.
	sidecars := make(map[string]time.Time, len(dirEntries))

	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if strings.HasPrefix(name, ".tmp-") {
			// A temp file left behind by a crashed write. Anything older
			// than an hour cannot belong to an in-flight Put.
			if info, err := de.Info(); err == nil && now.Sub(info.ModTime()) > time.Hour {
				s.remove(filepath.Join(s.dir, name))
			}
			continue
		}
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		digest, ok := NormalizeDigest(stem)
		if !ok {
			continue // not ours; leave it alone
		}
		if strings.HasSuffix(name, ".json") {
			used := time.Time{}
			if info, err := de.Info(); err == nil {
				used = info.ModTime()
			}
			sidecars[digest] = used
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		blobs[digest] = &entry{
			digest: digest, name: name, size: info.Size(),
			// usedAt is filled in from the sidecar below; until then, fall
			// back to the stored time so a blob missing its sidecar mtime
			// is never treated as infinitely fresh.
			storedAt: info.ModTime(), usedAt: info.ModTime(),
		}
	}

	var res SweepResult
	live := make([]*entry, 0, len(blobs))

	// Orphaned sidecars: no bytes to serve, so nothing can reference them.
	for digest := range sidecars {
		if _, ok := blobs[digest]; !ok {
			s.remove(filepath.Join(s.dir, digest+".json"))
		}
	}

	for digest, b := range blobs {
		res.ScannedBlobs++
		used, ok := sidecars[digest]
		if !ok {
			// Orphaned blob: unreachable through Lookup, so it is dead
			// weight regardless of age.
			s.remove(filepath.Join(s.dir, b.name))
			res.EvictedBlobs++
			res.FreedBytes += b.size
			continue
		}
		if !used.IsZero() {
			b.usedAt = used
		}
		if s.opts.TTL > 0 && now.Sub(b.storedAt) > s.opts.TTL {
			s.evict(b)
			res.ExpiredBlobs++
			res.FreedBytes += b.size
			continue
		}
		live = append(live, b)
		res.RemainingBytes += b.size
	}

	if s.opts.MaxBytes > 0 && res.RemainingBytes > s.opts.MaxBytes {
		// Least recently used first, digest as a tiebreak so the pass is
		// deterministic when several blobs share a timestamp.
		sort.Slice(live, func(i, j int) bool {
			if live[i].usedAt.Equal(live[j].usedAt) {
				return live[i].digest < live[j].digest
			}
			return live[i].usedAt.Before(live[j].usedAt)
		})
		for _, b := range live {
			if res.RemainingBytes <= s.opts.MaxBytes {
				break
			}
			s.evict(b)
			res.EvictedBlobs++
			res.FreedBytes += b.size
			res.RemainingBytes -= b.size
		}
	}

	return res, nil
}

// evict removes a blob and its sidecar.
func (s *Store) evict(b *entry) {
	s.remove(filepath.Join(s.dir, b.name))
	s.remove(filepath.Join(s.dir, b.digest+".json"))
}

func (s *Store) remove(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		applog.WithEvent(s.log, "media.sweep").Warn("remove failed",
			slog.String("path", path), slog.String("err", err.Error()))
	}
}

// RunSweeper sweeps once immediately and then every interval until ctx is
// cancelled. Startup is included because a container may have been down for
// longer than the TTL; waiting a full interval would leave expired bytes
// readable in the meantime.
//
// An interval of zero uses DefaultSweepInterval. Sweep errors are logged and
// the loop continues — a transient filesystem error must not take the
// process down.
func (s *Store) RunSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	log := applog.WithEvent(s.log, "media.sweep")
	sweep := func() {
		res, err := s.Sweep(time.Now())
		if err != nil {
			log.Warn("sweep failed", slog.String("err", err.Error()))
			return
		}
		if res.ExpiredBlobs == 0 && res.EvictedBlobs == 0 {
			log.Debug("sweep clean",
				slog.Int("blobs", res.ScannedBlobs),
				slog.Int64("bytes", res.RemainingBytes))
			return
		}
		log.Info("sweep reclaimed",
			slog.Int("scanned", res.ScannedBlobs),
			slog.Int("expired", res.ExpiredBlobs),
			slog.Int("evicted", res.EvictedBlobs),
			slog.Int64("freed_bytes", res.FreedBytes),
			slog.Int64("remaining_bytes", res.RemainingBytes))
	}

	sweep()
	if s.opts.MaxBytes == 0 && s.opts.TTL == 0 {
		// Nothing to enforce; the one startup pass above still cleared
		// orphans, but a periodic no-op loop is pure noise.
		log.Debug("retention disabled; sweeper not scheduled")
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

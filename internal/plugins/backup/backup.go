// Package backup takes periodic snapshots of the corpus.
//
// # Why this is in-process and there is no sidecar
//
// Merlin has a backup sidecar and it works, because `pg_dump` is a client asking a server for a
// consistent snapshot. bbolt has no equivalent, and `cp markov.db` is NOT a backup: the file is a
// single mmap updated by copy-on-write pages plus a meta-page flip at commit, so an external byte
// copy can capture a state between the page write and the flip, or mid-remap. The result usually
// APPEARS to work, which is the worst property a backup can have. A sidecar cannot do it either,
// because bbolt holds an exclusive flock on the file.
//
// The only correct mechanism is in-process: a read transaction calling tx.WriteTo, which is
// consistent by construction and does not block writers. storage.Store.Backup has done that since
// M6a; this row is the ticker and the retention around it.
//
// # The retention rules are each a way to lose everything
//
// Write to a temp name and rename, because a partial file with a real name is indistinguishable
// from a snapshot. Prune only files this service named, because a retention pass loose in a
// directory is a delete loop pointed at whatever else is in there. And NEVER PRUNE AFTER A FAILED
// BACKUP: pruning on a schedule while backups quietly fail removes every good copy, one tick at a
// time, and leaves the operator with a directory full of nothing at the moment they need it. That
// last one is the same reasoning as scoping the deploy's image prune to `until=168h` so a rollback
// still has an image to roll back to.
package backup

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/6586x57890143/peregrine/internal/core"
)

// Snapshotter writes a consistent copy of the corpus to a path. *storage.Store satisfies it.
type Snapshotter interface {
	Backup(path string) error
}

// Options are the dials.
type Options struct {
	// Dir is where snapshots go. Empty disables the feature entirely, which is the default:
	// there is no safe guess for a path, and writing megabytes somewhere the operator did not
	// choose is worse than not backing up.
	//
	// In a container this must be a WRITABLE MOUNT. The image runs read_only, so any path
	// outside a volume or bind fails on the first write, and a relative path resolves against
	// the distroless working directory. The production compose file binds one to /backups,
	// deliberately outside the corpus volume so that losing that volume does not take the
	// snapshots with it.
	Dir string

	// Every is how often a snapshot is taken.
	Every time.Duration

	// Keep is how many to retain. Each is a full copy of the corpus, so the disk cost is Keep
	// times the corpus size.
	Keep int
}

// prefix and suffix bracket the names this service creates. Retention matches on both, so a
// prune can only ever remove a file this service named.
const (
	prefix    = "markov-"
	suffix    = ".db"
	tempMark  = ".partial"
	timestamp = "20060102-150405"
)

// Service is the feature.
type Service struct {
	store  Snapshotter
	opts   Options
	logger *slog.Logger

	loops       sync.WaitGroup
	cancelLoops context.CancelFunc
}

// New builds the service.
func New(store Snapshotter, opts Options) *Service {
	return &Service{store: store, opts: opts}
}

func (s *Service) Name() string { return "backup" }

// Init reports whether backups are on, and refuses a directory it cannot write to.
//
// The check happens at startup rather than at the first tick, because a backup directory that
// turns out to be unwritable is exactly the thing an operator wants to find out about while they
// are still watching the logs, not six hours later. It is a WARNING rather than a startup error:
// one optional behaviour failing must never take the process down, and the corpus is more
// important than its backups.
func (s *Service) Init(deps core.Deps) error {
	s.logger = deps.Logger

	if !s.enabled() {
		s.logger.Info("corpus backups are off; set PEREGRINE_BACKUP_DIR to enable them")
		return nil
	}

	if err := os.MkdirAll(s.opts.Dir, 0o750); err != nil {
		s.logger.Warn("cannot create the backup directory, so backups will not run",
			"dir", s.opts.Dir, "err", err)
		s.opts.Dir = ""
		return nil
	}
	s.logger.Info("corpus backups are on", "dir", s.opts.Dir, "every", s.opts.Every, "keep", s.opts.Keep)
	return nil
}

// Start launches the ticker.
//
// NOT Immediate. A snapshot at startup would mean every restart writes one, so a crash loop
// would churn through the retention window and discard every older copy in minutes, which is
// exactly when the older copies matter most.
func (s *Service) Start(ctx context.Context) error {
	if !s.enabled() {
		return nil
	}
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancelLoops = cancel

	core.RunLoop(loopCtx, &s.loops, s.logger, core.Loop{
		Name:  "backup",
		Every: s.opts.Every,
		Fn:    func(context.Context) { s.Once() },
	})
	return nil
}

// Shutdown stops the ticker and waits for a snapshot in flight.
//
// It does NOT take a final snapshot. A backup is a read transaction against a corpus that is
// about to be closed, under a shutdown budget shared with every other service, and the value of
// one more copy is low while the cost of overrunning that budget is the corpus being closed by
// SIGKILL rather than by us.
func (s *Service) Shutdown(ctx context.Context) error {
	if s.cancelLoops != nil {
		s.cancelLoops()
	}
	done := make(chan struct{})
	go func() {
		s.loops.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.logger.Warn("shutdown deadline reached with a corpus snapshot still running")
	}
	return nil
}

// Once takes one snapshot and prunes, and reports whether the snapshot succeeded.
//
// Exported so an operator-facing command could take one on demand later without a second
// implementation of the temp-then-rename dance.
func (s *Service) Once() bool {
	if !s.enabled() {
		return false
	}

	start := time.Now()
	final := filepath.Join(s.opts.Dir, prefix+time.Now().UTC().Format(timestamp)+suffix)
	temp := final + tempMark

	// Written under a temp name and renamed, because a half-written file with a real name is
	// indistinguishable from a snapshot, and the one moment anybody looks in this directory is
	// the moment they need a file that is definitely whole.
	if err := s.store.Backup(temp); err != nil {
		s.logger.Error("corpus snapshot failed; NOT pruning older ones", "err", err)
		// The partial file goes, so a failed attempt does not leave debris that the next
		// operator has to identify. Its own failure is logged and ignored: there is nothing
		// useful to do about it and the snapshot failure is the news.
		if rmErr := os.Remove(temp); rmErr != nil && !os.IsNotExist(rmErr) {
			s.logger.Warn("could not remove a partial snapshot", "path", temp, "err", rmErr)
		}
		return false
	}
	if err := os.Rename(temp, final); err != nil {
		s.logger.Error("could not name a completed snapshot; NOT pruning older ones",
			"from", temp, "to", final, "err", err)
		return false
	}

	info, err := os.Stat(final)
	switch {
	case err != nil:
		s.logger.Info("corpus snapshot taken", "path", final, "took", time.Since(start))
	default:
		s.logger.Info("corpus snapshot taken", "path", final, "bytes", info.Size(),
			"took", time.Since(start))
	}

	// Pruning happens ONLY after a snapshot that worked. See the package comment: pruning on a
	// schedule while backups fail deletes every good copy one tick at a time.
	s.prune()
	return true
}

// prune keeps the newest Keep snapshots and removes the rest.
//
// Names are sorted rather than modification times compared, which is deliberate: the timestamp
// in the name is UTC and fixed-width, so lexical order is chronological order, and a name cannot
// be changed by a filesystem operation the way an mtime can.
func (s *Service) prune() {
	if s.opts.Keep <= 0 {
		return
	}

	entries, err := os.ReadDir(s.opts.Dir)
	if err != nil {
		s.logger.Warn("could not list the backup directory to prune it", "dir", s.opts.Dir, "err", err)
		return
	}

	var mine []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Both ends checked, and partials excluded. A retention pass that matched loosely would
		// be a delete loop pointed at whatever else is in this directory, which on a mounted
		// volume could be the corpus itself.
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) {
			mine = append(mine, name)
		}
	}
	if len(mine) <= s.opts.Keep {
		return
	}

	sort.Strings(mine)
	for _, name := range mine[:len(mine)-s.opts.Keep] {
		path := filepath.Join(s.opts.Dir, name)
		if err := os.Remove(path); err != nil {
			s.logger.Warn("could not remove an old snapshot", "path", path, "err", err)
			continue
		}
		s.logger.Info("removed an old corpus snapshot", "path", path)
	}
}

func (s *Service) enabled() bool { return s.opts.Dir != "" }

// Describe returns a one-line summary, for the status line and for tests.
func (s *Service) Describe() string {
	if !s.enabled() {
		return "off"
	}
	return fmt.Sprintf("%s every %s keeping %d", s.opts.Dir, s.opts.Every, s.opts.Keep)
}

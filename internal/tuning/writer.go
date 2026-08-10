package tuning

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// prefix and suffix bracket the names this package creates. Retention matches on BOTH, so
// a prune can only ever remove a file this writer named.
//
// The timestamp format is fixed-width UTC, which is what makes lexical order chronological
// order. internal/plugins/backup names its snapshots the same way and for the same reason:
// sorting names cannot be confused by a filesystem operation the way comparing mtimes can.
const (
	prefix    = "tuning-"
	suffix    = ".jsonl"
	timestamp = "20060102-150405"
)

// Options are the writer's dials.
type Options struct {
	// Dir is where the export goes. The caller decides that an empty Dir means the feature
	// is off; NewWriter on an empty Dir is an error, because a writer pointed at the
	// working directory is the CWD-relative path bug this repository spent M0 removing.
	Dir string

	// Rotate is how long one file stays open. Zero means never rotate, which is expressible
	// on purpose: a short-lived local run wants one file.
	Rotate time.Duration

	// Keep is how many completed files to retain. Zero or less disables pruning entirely,
	// which is the safe direction for a directory an operator may be keeping archives in.
	Keep int

	// Now is injectable so a test can rotate without sleeping. Nil means time.Now.
	Now func() time.Time
}

// Writer appends records to a rotating JSONL file.
//
// It holds a mutex and performs no I/O of its own on a timer: flushing and rotation happen
// when the caller asks. That keeps this package free of goroutines, which is what lets the
// whole thing be tested in a TempDir with a fake clock.
type Writer struct {
	opts Options
	now  func() time.Time

	mu       sync.Mutex
	file     *os.File
	buf      *bufio.Writer
	name     string
	openedAt time.Time

	// failed records that the last write or rotation did not succeed.
	//
	// It suppresses pruning, and that rule is the most important one here. Pruning on a
	// schedule while writing quietly fails removes every good file, one rotation at a time,
	// and leaves the operator with an empty directory at the moment they went looking. It
	// is the same reasoning as internal/plugins/backup never pruning after a failed
	// snapshot, and as the deploy scoping its image prune so a rollback still has an image.
	failed bool

	written uint64
}

// NewWriter opens the first file, creating the directory if it is missing.
//
// The directory is created and written to HERE rather than at the first record, because a
// directory that turns out to be unwritable is something an operator wants to find out
// about while they are still watching the startup logs. In a container this is the exact
// failure the backup bind has: a bind mount keeps its host ownership, so a directory owned
// by the deploy user is not writable by uid 65532 and every single write fails.
func NewWriter(opts Options) (*Writer, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, fmt.Errorf("tuning: no directory configured")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(opts.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("tuning: create %s: %w", opts.Dir, err)
	}

	w := &Writer{opts: opts, now: now}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// open starts a new file. The caller holds the lock, except in NewWriter where nothing
// else can see the writer yet.
func (w *Writer) open() error {
	name := prefix + w.now().UTC().Format(timestamp) + suffix
	path := filepath.Join(w.opts.Dir, name)

	// O_APPEND, so a restart inside the same second appends rather than truncating a file
	// that already has records in it. Two processes are impossible here (bbolt's flock
	// stops a second bot), but a restart landing on the same name is not.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		w.failed = true
		return fmt.Errorf("tuning: open %s: %w", path, err)
	}

	w.file = f
	w.buf = bufio.NewWriter(f)
	w.name = name
	w.openedAt = w.now()
	w.failed = false
	return nil
}

// Write appends one record, rotating first if the current file is due.
//
// Buffered, and NOT synced. A crash losing the last few records is acceptable; an fsync on
// the reply path is not, and the caller is a bot answering a human.
func (w *Writer) Write(rec Record) error {
	encoded, err := json.Marshal(rec)
	if err != nil {
		// Marshalling failure is the caller's bug rather than the disk's, so it does not
		// set failed: there is nothing wrong with the file and pruning is still safe.
		return fmt.Errorf("tuning: encode %s: %w", rec.recordKind(), err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.rotateIfDue(); err != nil {
		return err
	}
	if w.buf == nil {
		return fmt.Errorf("tuning: writer is closed")
	}

	encoded = append(encoded, '\n')
	if _, err := w.buf.Write(encoded); err != nil {
		w.failed = true
		return fmt.Errorf("tuning: write %s: %w", w.name, err)
	}
	w.written++
	return nil
}

// rotateIfDue closes the current file and starts a new one when Rotate has elapsed. The
// caller holds the lock.
func (w *Writer) rotateIfDue() error {
	if w.opts.Rotate <= 0 || w.file == nil {
		return nil
	}
	if w.now().Sub(w.openedAt) < w.opts.Rotate {
		return nil
	}

	if err := w.closeFile(); err != nil {
		return err
	}
	if err := w.open(); err != nil {
		return err
	}
	// Pruning happens on rotation rather than on a timer, so it can only ever run at the
	// moment a file has just been completed. And only when nothing has failed.
	w.prune()
	return nil
}

// Flush pushes buffered records to the file. Called from a ticker by the owner and at
// shutdown, which is what bounds how much a crash can lose.
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flush()
}

func (w *Writer) flush() error {
	if w.buf == nil {
		return nil
	}
	if err := w.buf.Flush(); err != nil {
		w.failed = true
		return fmt.Errorf("tuning: flush %s: %w", w.name, err)
	}
	return nil
}

// Close flushes and closes the current file. The writer refuses further writes afterwards.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	err := w.closeFile()
	w.buf, w.file = nil, nil
	return err
}

func (w *Writer) closeFile() error {
	if w.file == nil {
		return nil
	}
	flushErr := w.flush()
	closeErr := w.file.Close()
	w.file, w.buf = nil, nil
	if flushErr != nil {
		return flushErr
	}
	if closeErr != nil {
		w.failed = true
		return fmt.Errorf("tuning: close %s: %w", w.name, closeErr)
	}
	return nil
}

// Written reports how many records have been accepted, for the shutdown log line.
func (w *Writer) Written() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

// Name reports the file currently open, for logging.
func (w *Writer) Name() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.name
}

// prune keeps the newest Keep completed files and removes the rest. The caller holds the
// lock.
//
// Three rules, each of which is a way to lose the archive:
//
//   - Never after a failure. See the comment on the failed field.
//   - Only files this writer named, matched at BOTH ends. A retention pass that matched
//     loosely would be a delete loop pointed at whatever else is in the directory, which
//     on a mounted volume could be the corpus or the backups.
//   - Never the file currently open, which is why it is excluded by name rather than by
//     trusting that it sorts last. It does sort last today; a clock that went backwards
//     over a restart is exactly the kind of thing that makes "it sorts last" untrue at the
//     worst moment.
func (w *Writer) prune() {
	if w.opts.Keep <= 0 || w.failed {
		return
	}

	entries, err := os.ReadDir(w.opts.Dir)
	if err != nil {
		return
	}

	var mine []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == w.name {
			continue
		}
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) {
			mine = append(mine, name)
		}
	}
	if len(mine) <= w.opts.Keep {
		return
	}

	sort.Strings(mine)
	for _, name := range mine[:len(mine)-w.opts.Keep] {
		_ = os.Remove(filepath.Join(w.opts.Dir, name))
	}
}

package tuning

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeClock lets a test rotate a file without sleeping for a day.
type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time          { return c.at }
func (c *fakeClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func newClock() *fakeClock {
	return &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func mustWriter(t *testing.T, opts Options) *Writer {
	t.Helper()
	w, err := NewWriter(opts)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a path this test just created
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	trimmed := strings.TrimRight(string(b), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func exportFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), suffix) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// A record has to arrive as one JSON object per line, carrying its own kind. The whole
// format depends on that: a reader dispatches on kind and cannot guess from the field set.
func TestEveryRecordIsOneLineCarryingItsKind(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()
	w := mustWriter(t, Options{Dir: dir, Now: clock.now})

	records := []Record{
		Sample{Kind: KindSample, At: clock.now(), Trigger: "reply", Reply: "the server is doomed"},
		Engagement{Kind: KindEngagement, At: clock.now(), ID: "1", Reactions: 2},
		Snapshot{Kind: KindSnapshot, At: clock.now(), Version: "abc"},
	}
	for _, rec := range records {
		if err := w.Write(rec); err != nil {
			t.Fatalf("Write(%s): %v", rec.recordKind(), err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got := lines(t, filepath.Join(dir, w.Name()))
	if len(got) != 3 {
		t.Fatalf("wrote %d lines, want 3: %q", len(got), got)
	}

	wantKinds := []Kind{KindSample, KindEngagement, KindSnapshot}
	for i, line := range got {
		var envelope struct {
			Kind Kind `json:"kind"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("line %d is not one JSON object: %v (%q)", i, err, line)
		}
		if envelope.Kind != wantKinds[i] {
			t.Errorf("line %d kind = %q, want %q", i, envelope.Kind, wantKinds[i])
		}
	}
}

// A refused send must leave no text in the export. internal/safety never records the
// offending content anywhere and this file is not the exception, so the omitempty on Reply
// is load-bearing rather than cosmetic.
func TestARefusedSampleCarriesNoReplyText(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()
	w := mustWriter(t, Options{Dir: dir, Now: clock.now})

	if err := w.Write(Sample{
		Kind: KindSample, At: clock.now(), Trigger: "reply",
		Sent: false, Outcome: "produced", Words: 4,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// The field, not the value: Trigger is the string "reply", so a bare substring check
	// matches the trigger and reports a failure that is not there.
	line := lines(t, filepath.Join(dir, w.Name()))[0]
	if strings.Contains(line, `"reply":`) {
		t.Errorf("a sample with Sent=false emitted a reply field: %s", line)
	}
}

// Rotation has to produce names whose lexical order is chronological order, because prune
// sorts names rather than comparing mtimes.
func TestRotationNamesSortChronologically(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()
	w := mustWriter(t, Options{Dir: dir, Rotate: time.Hour, Now: clock.now})

	first := w.Name()
	for range 3 {
		clock.advance(90 * time.Minute)
		if err := w.Write(Snapshot{Kind: KindSnapshot, At: clock.now()}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	files := exportFiles(t, dir)
	if len(files) != 4 {
		t.Fatalf("got %d files, want 4 (one initial plus three rotations): %q", len(files), files)
	}
	if files[0] != first {
		t.Errorf("the oldest file by name is %q, want the first one opened, %q", files[0], first)
	}
	if !sort.StringsAreSorted(files) {
		t.Errorf("names do not sort chronologically: %q", files)
	}
}

// Nothing may rotate when Rotate is zero. A short local run wants one file, and "no
// rotation" has to be expressible rather than being a very long duration.
func TestZeroRotateKeepsOneFile(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()
	w := mustWriter(t, Options{Dir: dir, Now: clock.now})

	for range 3 {
		clock.advance(48 * time.Hour)
		if err := w.Write(Snapshot{Kind: KindSnapshot, At: clock.now()}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if got := exportFiles(t, dir); len(got) != 1 {
		t.Errorf("got %d files with Rotate=0, want 1: %q", len(got), got)
	}
}

// Pruning keeps the newest Keep completed files and never removes the one currently open.
func TestPruneKeepsTheNewestAndNeverTheOpenFile(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()
	w := mustWriter(t, Options{Dir: dir, Rotate: time.Hour, Keep: 2, Now: clock.now})

	for range 5 {
		clock.advance(90 * time.Minute)
		if err := w.Write(Snapshot{Kind: KindSnapshot, At: clock.now()}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	files := exportFiles(t, dir)
	// Keep completed files plus the one still open.
	if len(files) != 3 {
		t.Fatalf("got %d files with Keep=2, want 3 (two kept plus the open one): %q", len(files), files)
	}
	open := w.Name()
	if files[len(files)-1] != open {
		t.Errorf("the open file %q is not present as the newest: %q", open, files)
	}
}

// A retention pass loose in a directory is a delete loop pointed at whatever else is in
// there, which on a mounted volume could be the corpus or the backups. Both ends of the
// name are matched for that reason.
func TestPruneOnlyTouchesFilesThisWriterNamed(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()

	strangers := []string{
		"markov.db",                   // the corpus, if somebody pointed this at /data
		"markov-20260301-120000.db",   // a backup snapshot
		"tuning-20260101-000000.txt",  // right prefix, wrong suffix
		"notes-20260101-000000.jsonl", // right suffix, wrong prefix
	}
	for _, name := range strangers {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep me"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	w := mustWriter(t, Options{Dir: dir, Rotate: time.Hour, Keep: 1, Now: clock.now})
	for range 4 {
		clock.advance(90 * time.Minute)
		if err := w.Write(Snapshot{Kind: KindSnapshot, At: clock.now()}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	for _, name := range strangers {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("prune removed a file it did not create: %s (%v)", name, err)
		}
	}
}

// Pruning on a schedule while writing quietly fails removes every good file, one rotation
// at a time, and leaves an empty directory at the moment somebody goes looking. Same rule
// as internal/plugins/backup never pruning after a failed snapshot.
func TestAFailedWriteSuppressesThePrune(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()
	w := mustWriter(t, Options{Dir: dir, Rotate: time.Hour, Keep: 1, Now: clock.now})

	// Three completed files plus the open one, without pruning yet.
	w.opts.Keep = 0
	for range 3 {
		clock.advance(90 * time.Minute)
		if err := w.Write(Snapshot{Kind: KindSnapshot, At: clock.now()}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	before := len(exportFiles(t, dir))
	if before < 3 {
		t.Fatalf("setup produced %d files, want at least 3", before)
	}

	w.opts.Keep = 1
	w.mu.Lock()
	w.failed = true
	w.prune()
	w.mu.Unlock()

	if after := len(exportFiles(t, dir)); after != before {
		t.Errorf("prune removed %d files after a failure; it must remove none", before-after)
	}
}

// An empty directory is an error rather than a fall back to the working directory. Every
// runtime path in this repository used to be CWD-relative, which silently created fresh
// empty state that looked like it was working (M0), and in a container the read-only root
// filesystem turns that into a first-write failure instead.
func TestAnEmptyDirectoryIsRefused(t *testing.T) {
	for _, dir := range []string{"", "   "} {
		if _, err := NewWriter(Options{Dir: dir}); err == nil {
			t.Errorf("NewWriter(%q) succeeded, want an error", dir)
		}
	}
}

// A restart landing inside the same second must append rather than truncate a file that
// already has records in it.
func TestReopeningWithinTheSameSecondAppends(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()

	first := mustWriter(t, Options{Dir: dir, Now: clock.now})
	if err := first.Write(Snapshot{Kind: KindSnapshot, At: clock.now()}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	name := first.Name()
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := mustWriter(t, Options{Dir: dir, Now: clock.now})
	if second.Name() != name {
		t.Fatalf("second writer opened %q, want the same name %q for this test to mean anything",
			second.Name(), name)
	}
	if err := second.Write(Snapshot{Kind: KindSnapshot, At: clock.now()}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := second.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if got := lines(t, filepath.Join(dir, name)); len(got) != 2 {
		t.Errorf("file has %d lines after a same-second restart, want 2: %q", len(got), got)
	}
}

// Writing after Close must fail rather than panic on a nil buffer.
func TestWriteAfterCloseIsAnError(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(Options{Dir: dir})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Write(Snapshot{Kind: KindSnapshot}); err == nil {
		t.Error("Write after Close succeeded, want an error")
	}
}

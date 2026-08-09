package backup

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// fakeStore writes a file the way storage.Store.Backup does, or fails on demand.
type fakeStore struct {
	calls int
	fail  bool
	paths []string
}

func (f *fakeStore) Backup(path string) error {
	f.calls++
	f.paths = append(f.paths, path)
	if f.fail {
		// A real failure can leave a partial file behind, which is the case the temp name and
		// the cleanup exist for, so the fake leaves one too.
		_ = os.WriteFile(path, []byte("partial"), 0o600)
		return errors.New("snapshot failed")
	}
	return os.WriteFile(path, []byte("a whole corpus"), 0o600)
}

func logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fixture(t *testing.T, opts Options) (*Service, *fakeStore) {
	t.Helper()
	store := &fakeStore{}
	s := New(store, opts)
	if err := s.Init(core.Deps{Logger: logger()}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s, store
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// TestAnEmptyDirDisablesTheFeature. There is no safe guess for a path, and writing megabytes
// somewhere the operator did not choose is worse than not backing up.
func TestAnEmptyDirDisablesTheFeature(t *testing.T) {
	s, store := fixture(t, Options{Every: time.Hour, Keep: 7})

	if s.Once() {
		t.Error("Once reported a snapshot with no directory configured")
	}
	if store.calls != 0 {
		t.Errorf("called Backup %d times with the feature off", store.calls)
	}
	if got := s.Describe(); got != "off" {
		t.Errorf("Describe = %q, want off", got)
	}
}

// TestASnapshotIsWrittenAndNamedForTheTime.
func TestASnapshotIsWrittenAndNamedForTheTime(t *testing.T) {
	dir := t.TempDir()
	s, store := fixture(t, Options{Dir: dir, Every: time.Hour, Keep: 7})

	if !s.Once() {
		t.Fatal("Once reported failure for a working store")
	}
	if store.calls != 1 {
		t.Errorf("called Backup %d times, want 1", store.calls)
	}

	got := names(t, dir)
	if len(got) != 1 {
		t.Fatalf("directory holds %v, want one snapshot", got)
	}
	if !strings.HasPrefix(got[0], prefix) || !strings.HasSuffix(got[0], suffix) {
		t.Errorf("snapshot is named %q, which retention will not recognize", got[0])
	}
}

// TestTheSnapshotIsWrittenUnderATempNameAndRenamed.
//
// A half-written file with a real name is indistinguishable from a snapshot, and the one moment
// anybody looks in this directory is the moment they need a file that is definitely whole.
func TestTheSnapshotIsWrittenUnderATempNameAndRenamed(t *testing.T) {
	dir := t.TempDir()
	s, store := fixture(t, Options{Dir: dir, Every: time.Hour, Keep: 7})

	s.Once()

	if len(store.paths) != 1 {
		t.Fatalf("Backup was called with %v", store.paths)
	}
	if !strings.HasSuffix(store.paths[0], tempMark) {
		t.Errorf("Backup wrote directly to %q; it must write a temp name and rename", store.paths[0])
	}
	for _, name := range names(t, dir) {
		if strings.HasSuffix(name, tempMark) {
			t.Errorf("a temp file survived a successful snapshot: %q", name)
		}
	}
}

// TestAFailedSnapshotLeavesNoDebrisAndPrunesNothing.
//
// This is the rule that matters most in the package: pruning on a schedule while backups quietly
// fail removes every good copy, one tick at a time, and leaves the operator with a directory full
// of nothing at the moment they need it.
//
// Verified by reverting: move the prune call above the error return and the two older snapshots
// become one.
//
// The seeded snapshots are written by hand and there are MORE of them than Keep, deliberately. An
// earlier version of this test took one real snapshot with Keep at 1, which meant a prune had
// nothing to delete and the test passed with the fix reverted: it asserted the behaviour it was
// named for and could not observe it. That was caught by the revert check, which is the whole
// reason for doing them.
func TestAFailedSnapshotLeavesNoDebrisAndPrunesNothing(t *testing.T) {
	dir := t.TempDir()
	s, store := fixture(t, Options{Dir: dir, Every: time.Hour, Keep: 1})

	// Two snapshots against a Keep of one, so a prune has something to destroy.
	for _, stamp := range []string{"20260101-000001", "20260102-000002"} {
		if err := os.WriteFile(filepath.Join(dir, prefix+stamp+suffix), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := names(t, dir)

	store.fail = true
	if s.Once() {
		t.Error("Once reported success for a failing store")
	}

	after := names(t, dir)
	if len(after) != len(before) {
		t.Errorf("directory holds %v after a failed snapshot, want both earlier ones untouched "+
			"and no partial left behind: pruning while backups fail deletes every good copy one "+
			"tick at a time", after)
	}
	for i := range before {
		if i < len(after) && after[i] != before[i] {
			t.Errorf("snapshot %d changed from %q to %q", i, before[i], after[i])
		}
	}

	// And the control: a snapshot that WORKS does prune, so the assertion above is measuring the
	// failure path rather than a prune that never runs.
	store.fail = false
	if !s.Once() {
		t.Fatal("the recovery snapshot failed")
	}
	if got := names(t, dir); len(got) != 1 {
		t.Errorf("directory holds %v after a successful snapshot with Keep=1, want one", got)
	}
}

// TestRetentionKeepsTheNewest.
func TestRetentionKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	s, _ := fixture(t, Options{Dir: dir, Every: time.Hour, Keep: 3})

	// Written by hand rather than by taking five snapshots, because the name carries a
	// one-second-resolution timestamp and five real snapshots in the same second would collide.
	for _, stamp := range []string{"20260101-000001", "20260102-000002", "20260103-000003", "20260104-000004"} {
		path := filepath.Join(dir, prefix+stamp+suffix)
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	s.prune()

	got := names(t, dir)
	if len(got) != 3 {
		t.Fatalf("directory holds %v, want 3", got)
	}
	if got[0] != prefix+"20260102-000002"+suffix {
		t.Errorf("the oldest snapshot survived: %v", got)
	}
}

// TestRetentionOnlyTouchesFilesThisServiceNamed.
//
// A retention pass loose in a directory is a delete loop pointed at whatever else is in there, and
// on a mounted volume that could be the corpus itself.
func TestRetentionOnlyTouchesFilesThisServiceNamed(t *testing.T) {
	dir := t.TempDir()
	s, _ := fixture(t, Options{Dir: dir, Every: time.Hour, Keep: 1})

	strangers := []string{
		"markov.db",              // the corpus, if somebody points the backup dir at the data dir
		"markov.db.tmp",          // and its temp file
		"notes.txt",              // anything else
		prefix + "nope" + ".txt", // right prefix, wrong suffix
		prefix + "20260101-000001" + suffix + tempMark, // a partial from a failed run
	}
	for _, name := range strangers {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Plus two real snapshots, so the prune has something to do.
	for _, stamp := range []string{"20260101-000001", "20260102-000002"} {
		if err := os.WriteFile(filepath.Join(dir, prefix+stamp+suffix), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	s.prune()

	got := names(t, dir)
	for _, stranger := range strangers {
		found := false
		for _, name := range got {
			if name == stranger {
				found = true
			}
		}
		if !found {
			t.Errorf("retention deleted %q, which this service did not create", stranger)
		}
	}
	if len(got) != len(strangers)+1 {
		t.Errorf("directory holds %v; want every stranger plus the newest snapshot", got)
	}
}

// TestRetentionDoesNothingBelowTheLimit.
func TestRetentionDoesNothingBelowTheLimit(t *testing.T) {
	dir := t.TempDir()
	s, _ := fixture(t, Options{Dir: dir, Every: time.Hour, Keep: 5})

	for _, stamp := range []string{"20260101-000001", "20260102-000002"} {
		if err := os.WriteFile(filepath.Join(dir, prefix+stamp+suffix), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s.prune()

	if got := names(t, dir); len(got) != 2 {
		t.Errorf("directory holds %v, want both snapshots", got)
	}
}

// TestAnUnwritableDirIsAWarningNotAFatal, and it is found at startup rather than at the first
// tick, because that is when an operator is still watching the logs.
func TestAnUnwritableDirIsAWarningNotAFatal(t *testing.T) {
	// A path whose parent is a FILE, so MkdirAll cannot succeed on any platform.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := New(&fakeStore{}, Options{Dir: filepath.Join(file, "backups"), Every: time.Hour, Keep: 7})
	if err := s.Init(core.Deps{Logger: logger()}); err != nil {
		t.Errorf("Init returned an error for an unwritable directory: %v", err)
	}
	if s.Once() {
		t.Error("Once reported a snapshot after the directory check failed")
	}
	if got := s.Describe(); got != "off" {
		t.Errorf("Describe = %q, want off after the directory check failed", got)
	}
}

// TestStartTakesNoImmediateSnapshot.
//
// A snapshot at startup would mean every restart writes one, so a crash loop would churn through
// the retention window and discard every older copy in minutes, which is exactly when the older
// copies matter most.
func TestStartTakesNoImmediateSnapshot(t *testing.T) {
	dir := t.TempDir()
	s, store := fixture(t, Options{Dir: dir, Every: time.Hour, Keep: 7})

	if err := s.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if store.calls != 0 {
		t.Errorf("Start took %d snapshots immediately; a crash loop would burn the retention "+
			"window", store.calls)
	}
}

// TestShutdownTakesNoFinalSnapshot. A backup is a read transaction against a corpus that is about
// to close, under a budget shared with every other service, and losing an orderly close to it is
// the worse trade.
func TestShutdownTakesNoFinalSnapshot(t *testing.T) {
	dir := t.TempDir()
	s, store := fixture(t, Options{Dir: dir, Every: time.Hour, Keep: 7})

	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if store.calls != 0 {
		t.Errorf("Shutdown took %d snapshots", store.calls)
	}
}

// TestDescribeSaysWhatIsConfigured, for the status line.
func TestDescribeSaysWhatIsConfigured(t *testing.T) {
	s, _ := fixture(t, Options{Dir: "/data/backups", Every: 6 * time.Hour, Keep: 4})
	got := s.Describe()
	for _, want := range []string{"/data/backups", "6h", "4"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe = %q, want it to mention %q", got, want)
		}
	}
}

// TestARealSnapshotIsAReadableCorpus is the one test here that uses the actual store rather than
// a fake, and it is the one that matters most.
//
// Everything above proves the naming, the temp-then-rename and the retention. None of it proves
// that what lands on disk is a corpus, and "the backup file appears" is precisely the false
// confidence this package's comment warns about: an external `cp markov.db` also produces a file
// that usually appears to work. So this takes a real snapshot of a real corpus with data in it,
// opens the result, and reads the data back.
func TestARealSnapshotIsAReadableCorpus(t *testing.T) {
	source := dbtest.Store(t)
	if err := source.Update(func(w *storage.Writer) error {
		return w.LearnNgram("the bird", "flew", "author-1")
	}); err != nil {
		t.Fatalf("seed the corpus: %v", err)
	}

	dir := t.TempDir()
	s := New(source, Options{Dir: dir, Every: time.Hour, Keep: 7})
	if err := s.Init(core.Deps{Logger: logger()}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !s.Once() {
		t.Fatal("the snapshot reported failure")
	}

	got := names(t, dir)
	if len(got) != 1 {
		t.Fatalf("directory holds %v, want one snapshot", got)
	}

	// Opened as a corpus, which also means it passes the schema check: a snapshot that storage
	// refuses to open is not a backup.
	restored, err := storage.Open(filepath.Join(dir, got[0]))
	if err != nil {
		t.Fatalf("the snapshot will not open as a corpus: %v", err)
	}
	defer func() { _ = restored.Close() }()

	if err := restored.View(func(r *storage.Reader) error {
		succ, err := r.Successors("the bird")
		if err != nil {
			return err
		}
		if len(succ) != 1 || succ[0].Token != "flew" {
			t.Errorf("the restored corpus holds %v, want the n-gram that was written", succ)
		}
		return nil
	}); err != nil {
		t.Fatalf("reading the restored corpus: %v", err)
	}
}

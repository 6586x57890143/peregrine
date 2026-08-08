package maintenance_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/maintenance"
	"github.com/6586x57890143/peregrine/internal/safety"
	"github.com/6586x57890143/peregrine/internal/storage"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testGate(t *testing.T) *safety.Gate {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blocklist.txt")
	if err := os.WriteFile(path, []byte("slur \\bexampleslur\\b\n"), 0o600); err != nil {
		t.Fatalf("write blocklist: %v", err)
	}
	bl, err := safety.LoadBlocklist(path)
	if err != nil {
		t.Fatalf("LoadBlocklist: %v", err)
	}
	return safety.NewGate(bl, quietLogger(), false)
}

func ngrams(t *testing.T, s *storage.Store) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	if err := s.View(func(r *storage.Reader) error {
		return r.ForEachNgram(func(prefix, next string, sc corpus.Successor) error {
			out[prefix+" -> "+next] = sc.Count
			return nil
		})
	}); err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	return out
}

func TestCleanRemovesBlocklistedFragments(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t,
		s,
		dbtest.Learn{Prefix: "the bird", Next: "flew", Author: "u1"},
		dbtest.Learn{Prefix: "you are", Next: "exampleslur", Author: "u1"},
		dbtest.Learn{Prefix: "exampleslur here", Next: "again", Author: "u1"},
		// The built-in baseline applies too, not only the operator list.
		dbtest.Learn{Prefix: "absolute", Next: "wop", Author: "u1"},
	)

	res, err := maintenance.Clean(s, testGate(t), quietLogger())
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if res.Scanned != 4 {
		t.Errorf("Scanned = %d, want 4", res.Scanned)
	}
	if res.Removed != 3 {
		t.Errorf("Removed = %d, want 3", res.Removed)
	}

	got := ngrams(t, s)
	if len(got) != 1 {
		t.Errorf("corpus holds %v, want only the clean entry", got)
	}
	if _, ok := got["the bird -> flew"]; !ok {
		t.Error("the clean entry was removed")
	}
}

// TestCleanCatchesEvadedFragments is the reason Clean normalizes. It is cleaning a
// corpus written before the normalizer existed, so the entries in it are in whatever
// spelling they were learned in, including the evaded ones.
func TestCleanCatchesEvadedFragments(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t,
		s,
		dbtest.Learn{Prefix: "you are", Next: "3xampl3slur", Author: "u1"},
		dbtest.Learn{Prefix: "you are", Next: "exampl" + string(rune(0x0435)) + "slur", Author: "u1"},
		dbtest.Learn{Prefix: "the bird", Next: "flew", Author: "u1"},
	)

	if _, err := maintenance.Clean(s, testGate(t), quietLogger()); err != nil {
		t.Fatalf("Clean: %v", err)
	}

	got := ngrams(t, s)
	if len(got) != 1 {
		t.Errorf("evaded spellings survived the clean: %v", got)
	}
}

// TestCleanRebuildsKNIndexes is the consistency guard. Leaving the distinct counts
// describing n-grams that no longer exist would make the Kneser-Ney lambda term wrong
// for every prefix that lost a successor, which skews generation silently rather than
// failing.
func TestCleanRebuildsKNIndexes(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t,
		s,
		dbtest.Learn{Prefix: "i like", Next: "cheese", Author: "u1"},
		dbtest.Learn{Prefix: "i like", Next: "exampleslur", Author: "u1"},
		dbtest.Learn{Prefix: "we love", Next: "cheese", Author: "u1"},
	)

	if _, err := maintenance.Clean(s, testGate(t), quietLogger()); err != nil {
		t.Fatalf("Clean: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		st, err := r.KNStats("i like", "cheese")
		if err != nil {
			return err
		}
		// "i like" lost one of its two successors.
		if st.DistinctSuccessors != 1 {
			t.Errorf("DistinctSuccessors = %d after clean, want 1: the index still describes "+
				"a deleted n-gram", st.DistinctSuccessors)
		}
		if st.DistinctPredecessors != 2 {
			t.Errorf("DistinctPredecessors = %d, want 2", st.DistinctPredecessors)
		}
		if total := r.TotalDistinctPredecessors(); total != 2 {
			t.Errorf("TotalDistinctPredecessors = %d, want 2", total)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCleanDoesNotApplySpamShape covers a trap. The spam check tests length,
// character repetition and character-class ratio, all designed for whole messages; a
// two-word n-gram fragment fails it, so applying it here would delete most of the
// corpus.
func TestCleanDoesNotApplySpamShape(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t,
		s,
		dbtest.Learn{Prefix: "ok", Next: "no", Author: "u1"},
		dbtest.Learn{Prefix: "a", Next: "b", Author: "u1"},
		dbtest.Learn{Prefix: "\U0001F426", Next: "\U0001F602", Author: "u1"},
	)

	res, err := maintenance.Clean(s, testGate(t), quietLogger())
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("removed %d short or emoji fragments; the spam-shape check must not apply "+
			"to n-gram fragments", res.Removed)
	}
}

func TestCleanOnAnEmptyCorpus(t *testing.T) {
	s := dbtest.Store(t)
	res, err := maintenance.Clean(s, testGate(t), quietLogger())
	if err != nil {
		t.Fatalf("Clean on an empty corpus must succeed: %v", err)
	}
	if res.Scanned != 0 || res.Removed != 0 {
		t.Errorf("got %+v, want zero", res)
	}
}

func TestCleanWithNoBlocklistStillAppliesBaseline(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t,
		s,
		dbtest.Learn{Prefix: "absolute", Next: "wop", Author: "u1"},
		dbtest.Learn{Prefix: "the bird", Next: "flew", Author: "u1"},
	)

	// A gate with no operator list at all, which is the state a fresh deployment is in.
	gate := safety.NewGate(nil, quietLogger(), false)
	res, err := maintenance.Clean(s, gate, quietLogger())
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if res.Removed != 1 {
		t.Errorf("Removed = %d, want 1: the built-in baseline must apply with no operator list", res.Removed)
	}
}

func TestPurgeAuthor(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t,
		s,
		dbtest.Learn{Prefix: "say", Next: "thing", Author: "bad"},
		dbtest.Learn{Prefix: "say", Next: "thing", Author: "good"},
	)

	removed, err := maintenance.PurgeAuthor(s, "bad", quietLogger())
	if err != nil {
		t.Fatalf("PurgeAuthor: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	if err := s.View(func(r *storage.Reader) error {
		got, ok, err := r.Successor("say", "thing")
		if err != nil || !ok {
			t.Fatalf("successor missing: %v", err)
		}
		if got.Authors != 1 {
			t.Errorf("authors = %d after purge, want 1", got.Authors)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeAuthorRefusesEmpty(t *testing.T) {
	s := dbtest.Store(t)
	if _, err := maintenance.PurgeAuthor(s, "", quietLogger()); err == nil {
		t.Error("purging the empty author ID must be refused: it is the bot's own marker")
	}
}

func TestCompactProducesAUsableCorpus(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t, s, dbtest.Learn{Prefix: "the bird", Next: "flew", Author: "u1"})

	dest := dbtest.Path(t)
	if err := maintenance.Compact(s, dest, quietLogger()); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	out, err := storage.Open(dest)
	if err != nil {
		t.Fatalf("the compacted file is not a usable corpus: %v", err)
	}
	defer func() { _ = out.Close() }()

	if got := ngrams(t, out); len(got) != 1 {
		t.Errorf("compacted corpus holds %v, want the one entry", got)
	}
}

func TestCompactRefusesToOverwriteInPlace(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t, s, dbtest.Learn{Prefix: "the bird", Next: "flew", Author: "u1"})

	// Compacting onto the live path would mean bbolt opening a file it already holds,
	// which the flock refuses. Asserting the failure is clearer than asserting the
	// mechanism, and it documents why the operator moves the file by hand.
	err := maintenance.Compact(s, s.Path(), quietLogger())
	if err == nil {
		t.Error("compacting onto the source path must fail rather than corrupt the live corpus")
	}
	if err != nil && !strings.Contains(err.Error(), "compact") && !strings.Contains(err.Error(), "corpus") {
		t.Logf("failure message: %v", err)
	}
}

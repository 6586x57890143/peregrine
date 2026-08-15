package storage_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.etcd.io/bbolt"

	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// The report is a measuring instrument, so these tests are about it measuring the right
// thing rather than about it running. A report that walks the corpus successfully and
// reports the wrong magnitude is worse than one that fails, because every decision it
// informs is then made confidently on a wrong number.

const sentinel = "<end>"

// seedForReport builds a corpus whose every statistic is known by hand.
//
//	"the bird" -> "is"     three authors, count 3
//	"the bird" -> "left"   one author,    count 4
//	"a"        -> "bird"   two authors,   count 2
//	"lonely"   -> "prefix" one author,    count 1
//
// So: 4 edges, 3 prefixes, edge mass 10, and at k=2 exactly two edges (mass 5) survive.
func seedForReport(t *testing.T, s *storage.Store) {
	t.Helper()
	if err := s.Update(func(w *storage.Writer) error {
		for _, a := range []string{"alice", "bob", "carol"} {
			if err := w.LearnNgram("the bird", "is", a); err != nil {
				return err
			}
		}
		for range 4 {
			if err := w.LearnNgram("the bird", "left", "alice"); err != nil {
				return err
			}
		}
		for _, a := range []string{"alice", "bob"} {
			if err := w.LearnNgram("a", "bird", a); err != nil {
				return err
			}
		}
		return w.LearnNgram("lonely", "prefix", "dave")
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func statsOf(t *testing.T, s *storage.Store, minAuthors int) corpus.Stats {
	t.Helper()
	st, err := s.CorpusStats(minAuthors, sentinel)
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	return st
}

func TestCorpusStatsCountsScaleAndMass(t *testing.T) {
	s := dbtest.Store(t)
	seedForReport(t, s)

	st := statsOf(t, s, 2)
	if st.Edges != 4 {
		t.Errorf("Edges = %d, want 4", st.Edges)
	}
	if st.Prefixes != 3 {
		t.Errorf("Prefixes = %d, want 3", st.Prefixes)
	}
	if st.TotalEdgeMass != 10 {
		t.Errorf("TotalEdgeMass = %d, want 10 (3+4+2+1)", st.TotalEdgeMass)
	}
}

// The author histogram is the input to every gate decision, so an off-by-one in the
// bucketing would misinform the one thing this mode exists to inform.
func TestCorpusStatsBucketsAuthorsPerEdge(t *testing.T) {
	s := dbtest.Store(t)
	seedForReport(t, s)

	st := statsOf(t, s, 2)
	want := map[int]int{1: 2, 2: 1, 3: 1} // "left" and "prefix" have one author each.
	for authors, count := range want {
		if st.Authors[authors] != count {
			t.Errorf("Authors[%d] = %d, want %d", authors, st.Authors[authors], count)
		}
	}
	if st.Authors[0] != 0 {
		t.Errorf("Authors[0] = %d, want 0: nothing here was written with an empty author",
			st.Authors[0])
	}
}

// Index 0 is not "unattributed by accident". learnMessage passes an empty author for the
// bot's own output precisely so self-learning cannot bootstrap a phrase into eligibility,
// and an operator reading a nonzero bucket 0 is reading how much of the corpus is the bot
// talking to itself.
func TestCorpusStatsCountsTheBotsOwnEdgesInBucketZero(t *testing.T) {
	s := dbtest.Store(t)
	if err := s.Update(func(w *storage.Writer) error {
		return w.LearnNgram("the bot", "said", "")
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	st := statsOf(t, s, 2)
	if st.Authors[0] != 1 {
		t.Errorf("Authors[0] = %d, want 1", st.Authors[0])
	}
}

// Both curves, because they are different claims and only the second predicts behaviour:
// generation samples in proportion to probability, so refusing most of the EDGES and
// refusing most of the MASS are very different outcomes. On the real corpus they differ
// by a factor of four.
func TestCorpusStatsReportsAdmissionByEdgeAndByMass(t *testing.T) {
	s := dbtest.Store(t)
	seedForReport(t, s)

	st := statsOf(t, s, 2)
	byThreshold := map[int]corpus.Admission{}
	for _, a := range st.Admission {
		byThreshold[a.MinAuthors] = a
	}

	if got := byThreshold[0]; got.Edges != 4 || got.Mass != 10 {
		t.Errorf("k=0 admitted %d edges / mass %d, want 4 / 10", got.Edges, got.Mass)
	}
	// At k=2 only "is" (3 authors, count 3) and "bird" (2 authors, count 2) survive.
	if got := byThreshold[2]; got.Edges != 2 || got.Mass != 5 {
		t.Errorf("k=2 admitted %d edges / mass %d, want 2 / 5", got.Edges, got.Mass)
	}
	if got := byThreshold[2]; got.EdgeShare != 0.5 || got.MassShare != 0.5 {
		t.Errorf("k=2 shares = %.2f edge / %.2f mass, want 0.50 / 0.50",
			got.EdgeShare, got.MassShare)
	}
	// k=4 admits nothing here, and must say so rather than being absent.
	if got := byThreshold[4]; got.Edges != 0 || got.MassShare != 0 {
		t.Errorf("k=4 admitted %d edges / share %.2f, want 0 / 0", got.Edges, got.MassShare)
	}
}

// The branch factor is the number that settles whether low-order joins are a fixture-size
// artifact, and the gated column is the half that matters: a gated mean at or near 1 is a
// deterministic walk however hot the sampler is.
func TestCorpusStatsReportsBranchFactorPerOrder(t *testing.T) {
	s := dbtest.Store(t)
	seedForReport(t, s)

	st := statsOf(t, s, 2)
	byOrder := map[int]corpus.OrderStats{}
	for _, o := range st.Orders {
		byOrder[o.Order] = o
	}

	// Order 1: prefixes "a" and "lonely", one successor each, so a mean of 1.0. Only "a"
	// has an admissible one, so half its prefixes are dead.
	one := byOrder[1]
	if one.Prefixes != 2 || one.Edges != 2 {
		t.Errorf("order 1: %d prefixes / %d edges, want 2 / 2", one.Prefixes, one.Edges)
	}
	if one.MeanSuccessors != 1.0 || one.MeanGatedSucc != 0.5 {
		t.Errorf("order 1: mean %.2f, gated mean %.2f, want 1.00 / 0.50",
			one.MeanSuccessors, one.MeanGatedSucc)
	}
	if one.DeadPrefixRate != 0.5 {
		t.Errorf("order 1: dead prefix rate %.2f, want 0.50", one.DeadPrefixRate)
	}

	// Order 2: "the bird" alone, two successors, one of which survives the gate.
	two := byOrder[2]
	if two.Prefixes != 1 || two.MeanSuccessors != 2.0 || two.MeanGatedSucc != 1.0 {
		t.Errorf("order 2: %d prefixes, mean %.2f, gated %.2f, want 1 / 2.00 / 1.00",
			two.Prefixes, two.MeanSuccessors, two.MeanGatedSucc)
	}
	if two.DeadPrefixRate != 0 {
		t.Errorf("order 2: dead prefix rate %.2f, want 0", two.DeadPrefixRate)
	}
}

// The joint distribution is the number that exists nowhere else in the system, and the
// whole argument for a count-aware gate rests on it: an edge one person said once is a
// sparse corpus, and an edge one person said a hundred times is the poisoning shape.
func TestCorpusStatsSplitsSingleAuthorEdgesByCount(t *testing.T) {
	s := dbtest.Store(t)
	seedForReport(t, s)

	st := statsOf(t, s, 2)
	got := map[uint64]int{}
	for i, threshold := range corpus.CountThresholds {
		got[threshold] = st.SingleAuthorByCount[i]
	}

	// Two single-author edges: "left" at count 4 and "prefix" at count 1.
	if got[1] != 2 {
		t.Errorf("single-author edges with count >= 1 = %d, want 2", got[1])
	}
	if got[3] != 1 {
		t.Errorf("single-author edges with count >= 3 = %d, want 1 (only \"left\")", got[3])
	}
	if got[5] != 0 {
		t.Errorf("single-author edges with count >= 5 = %d, want 0", got[5])
	}
}

// Reported separately because it is not a word and it is the largest topic entry there
// is, so every distribution has it as the maximum. A reader who does not know that sees
// "max == messages learned" and goes looking for a double-count bug that is not there.
func TestCorpusStatsReportsTheSentinelSeparately(t *testing.T) {
	s := dbtest.Store(t)
	if err := s.Update(func(w *storage.Writer) error {
		for range 7 {
			if err := w.IncTopic(sentinel); err != nil {
				return err
			}
		}
		return w.IncTopic("bird")
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	st := statsOf(t, s, 2)
	if st.SentinelCount != 7 {
		t.Errorf("SentinelCount = %d, want 7", st.SentinelCount)
	}
	if st.TotalTokens != 8 {
		t.Errorf("TotalTokens = %d, want 8: the sentinel is reported separately but not "+
			"subtracted, because the report describes the file", st.TotalTokens)
	}
}

// Percentile indexes its input directly, so an unsorted distribution would make every
// quantile in the report a different arbitrary element each run.
func TestCorpusStatsReturnsSortedDistributions(t *testing.T) {
	s := dbtest.Store(t)
	if err := s.Update(func(w *storage.Writer) error {
		for i, word := range []string{"rare", "common", "middling"} {
			for range (3 - i) * 4 {
				if err := w.IncTopic(word); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	st := statsOf(t, s, 2)
	for i := 1; i < len(st.TopicCounts); i++ {
		if st.TopicCounts[i] < st.TopicCounts[i-1] {
			t.Fatalf("TopicCounts is not ascending at %d: %v", i, st.TopicCounts)
		}
	}
}

func TestCorpusStatsOnAnEmptyCorpusReportsZeroesRatherThanFailing(t *testing.T) {
	s := dbtest.Store(t)

	st := statsOf(t, s, 2)
	if st.Edges != 0 || st.TotalEdgeMass != 0 {
		t.Errorf("empty corpus reported %d edges / mass %d", st.Edges, st.TotalEdgeMass)
	}
	for _, a := range st.Admission {
		if a.EdgeShare != 0 || a.MassShare != 0 {
			t.Errorf("k=%d share on an empty corpus = %.2f / %.2f, want 0 (a share of "+
				"nothing must not be NaN)", a.MinAuthors, a.EdgeShare, a.MassShare)
		}
	}
}

// The one property that makes this mode safe to point at a corpus: it opens read-only,
// so unlike storage.Open it creates no bucket, stamps no version and runs no migration.
// A mode that mutates the corpus it was asked to measure has changed the thing it is
// describing, and upgradeToV2 would do it destructively by emptying the image cache.
func TestOpenReadOnlyWritesNothing(t *testing.T) {
	path := dbtest.Path(t)
	s, err := storage.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Update(func(w *storage.Writer) error {
		return w.LearnNgram("the bird", "is", "alice")
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}

	ro, err := storage.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if _, err := ro.CorpusStats(2, sentinel); err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if err := ro.Close(); err != nil {
		t.Fatalf("close read-only: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read corpus: %v", err)
	}
	if string(before) != string(after) {
		t.Error("OpenReadOnly plus CorpusStats changed the corpus file byte-for-byte")
	}
}

// A reader that tolerates a layout the writer refuses would walk a pre-M6 corpus and
// print confident nonsense about it, which is the worst outcome available to a mode whose
// entire output is numbers an operator is about to act on.
func TestOpenReadOnlyRefusesACorpusItCannotRead(t *testing.T) {
	t.Run("no meta bucket at all, which is the pre-M6 layout", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "old.db")
		db, err := bbolt.Open(path, 0600, nil)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := db.Update(func(tx *bbolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists([]byte("ngrams"))
			return err
		}); err != nil {
			t.Fatalf("write old-layout bucket: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		if _, err := storage.OpenReadOnly(path); !errors.Is(err, storage.ErrSchemaMismatch) {
			t.Errorf("OpenReadOnly on a pre-M6 corpus: %v, want ErrSchemaMismatch", err)
		}
	})

	t.Run("empty and unstamped, where stamping it would be a write", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.db")
		db, err := bbolt.Open(path, 0600, nil)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := db.Update(func(tx *bbolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists([]byte("meta"))
			return err
		}); err != nil {
			t.Fatalf("create meta: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		if _, err := storage.OpenReadOnly(path); !errors.Is(err, storage.ErrSchemaMismatch) {
			t.Errorf("OpenReadOnly on an empty unstamped corpus: %v, want ErrSchemaMismatch", err)
		}
	})
}

func TestOpenReadOnlyAcceptsACurrentCorpus(t *testing.T) {
	path := dbtest.Path(t)
	s, err := storage.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Update(func(w *storage.Writer) error {
		return w.LearnNgram("the bird", "is", "alice")
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ro, err := storage.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly on a current corpus: %v", err)
	}
	t.Cleanup(func() {
		if err := ro.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	st, err := ro.CorpusStats(2, sentinel)
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if st.Edges != 1 {
		t.Errorf("Edges = %d, want 1", st.Edges)
	}
}

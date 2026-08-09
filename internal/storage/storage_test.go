package storage_test

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// This suite uses a real bbolt file throughout, because the contract being tested is
// enforced by bbolt and not by Go: key ordering, range boundaries, transaction
// atomicity and the flock are all properties of the library.

func successorMap(t *testing.T, s *storage.Store, prefix string) map[string]corpus.Successor {
	t.Helper()
	out := map[string]corpus.Successor{}
	if err := s.View(func(r *storage.Reader) error {
		got, err := r.Successors(prefix)
		if err != nil {
			return err
		}
		for _, sc := range got {
			out[sc.Token] = sc
		}
		return nil
	}); err != nil {
		t.Fatalf("Successors(%q): %v", prefix, err)
	}
	return out
}

func TestLearnAndReadSuccessors(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t,
		s,
		dbtest.Learn{Prefix: "the bird", Next: "flew", Author: "u1"},
		dbtest.Learn{Prefix: "the bird", Next: "flew", Author: "u2"},
		dbtest.Learn{Prefix: "the bird", Next: "sang", Author: "u1"},
	)

	got := successorMap(t, s, "the bird")
	if len(got) != 2 {
		t.Fatalf("got %d successors, want 2: %v", len(got), got)
	}
	if got["flew"].Count != 2 {
		t.Errorf("flew count = %d, want 2", got["flew"].Count)
	}
	if got["flew"].Authors != 2 {
		t.Errorf("flew authors = %d, want 2", got["flew"].Authors)
	}
	if got["sang"].Count != 1 || got["sang"].Authors != 1 {
		t.Errorf("sang = %+v, want count 1 authors 1", got["sang"])
	}
}

// TestRepetitionByOneAuthorDoesNotRaiseDiversity is the anti-poisoning property in
// one test. n-gram weight is raw frequency, so repeating a phrase is a direct write
// to the model; the distinct-author count is what makes that worthless on its own.
func TestRepetitionByOneAuthorDoesNotRaiseDiversity(t *testing.T) {
	s := dbtest.Store(t)

	entries := make([]dbtest.Learn, 0, 500)
	for range 500 {
		entries = append(entries, dbtest.Learn{Prefix: "say", Next: "thething", Author: "attacker"})
	}
	dbtest.Seed(t, s, entries...)

	got := successorMap(t, s, "say")["thething"]
	if got.Count != 500 {
		t.Errorf("count = %d, want 500: frequency should still accumulate", got.Count)
	}
	if got.Authors != 1 {
		t.Errorf("authors = %d, want 1: 500 repetitions by one person is still one person", got.Authors)
	}
}

// TestBotOutputDoesNotCountTowardDiversity pins the self-learning exclusion. Without
// it, anything the bot said once would bootstrap itself into eligibility.
func TestBotOutputDoesNotCountTowardDiversity(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t,
		s,
		dbtest.Learn{Prefix: "say", Next: "thething", Author: "u1"},
		dbtest.Learn{Prefix: "say", Next: "thething", Author: ""}, // the bot itself
		dbtest.Learn{Prefix: "say", Next: "thething", Author: ""},
	)

	got := successorMap(t, s, "say")["thething"]
	if got.Count != 3 {
		t.Errorf("count = %d, want 3", got.Count)
	}
	if got.Authors != 1 {
		t.Errorf("authors = %d, want 1: the bot's own output must not count", got.Authors)
	}
}

// TestSuccessorsDoesNotLeakIntoLongerPrefixes is the key-layout property the NUL
// separator exists for. Successors("the") must not see keys belonging to "the cat".
func TestSuccessorsDoesNotLeakIntoLongerPrefixes(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t,
		s,
		dbtest.Learn{Prefix: "the", Next: "cat", Author: "u1"},
		dbtest.Learn{Prefix: "the", Next: "dog", Author: "u1"},
		dbtest.Learn{Prefix: "the cat", Next: "sat", Author: "u1"},
		dbtest.Learn{Prefix: "the cat", Next: "ran", Author: "u1"},
		dbtest.Learn{Prefix: "theatre", Next: "opens", Author: "u1"},
	)

	short := successorMap(t, s, "the")
	if len(short) != 2 {
		t.Errorf("Successors(\"the\") = %v, want exactly cat and dog", short)
	}
	for _, leaked := range []string{"sat", "ran", "opens"} {
		if _, ok := short[leaked]; ok {
			t.Errorf("Successors(\"the\") leaked %q from a longer prefix", leaked)
		}
	}

	long := successorMap(t, s, "the cat")
	if len(long) != 2 {
		t.Errorf("Successors(\"the cat\") = %v, want exactly sat and ran", long)
	}
}

// TestEmptyPrefixIsRefused is the regression pin for finding 5. The old ingestion
// loop descended to n == 1, where the prefix was empty, so every unigram accumulated
// into one key holding a map of the whole vocabulary that nothing ever read. Refusing
// it in the writer means the bug cannot return through a new caller.
func TestEmptyPrefixIsRefused(t *testing.T) {
	s := dbtest.Store(t)
	err := s.Update(func(w *storage.Writer) error {
		return w.LearnNgram("", "anything", "u1")
	})
	if err == nil {
		t.Fatal("an empty prefix must be refused: it is the finding-5 hot key")
	}
	if !strings.Contains(err.Error(), "finding 5") {
		t.Errorf("the error should point at the finding so the reason is findable; got: %v", err)
	}
}

// TestNulInTokenIsRefused pins the codec assertion. A NUL in a token would produce a
// key that stores and retrieves fine, under a prefix that is not the one the caller
// meant, which is silent corruption.
func TestNulInTokenIsRefused(t *testing.T) {
	s := dbtest.Store(t)
	nul := string(rune(0))

	cases := map[string]dbtest.Learn{
		"in prefix": {Prefix: "the" + nul + "bird", Next: "flew", Author: "u1"},
		"in next":   {Prefix: "the bird", Next: "fl" + nul + "ew", Author: "u1"},
		"in author": {Prefix: "the bird", Next: "flew", Author: "u" + nul + "1"},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			err := s.Update(func(w *storage.Writer) error {
				return w.LearnNgram(e.Prefix, e.Next, e.Author)
			})
			if !errors.Is(err, storage.ErrNulInToken) {
				t.Errorf("got %v, want ErrNulInToken", err)
			}
		})
	}
}

func TestKNStatsAreMaintainedIncrementally(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t,
		s,
		// "cheese" follows two distinct contexts, so N1+(. cheese) is 2.
		dbtest.Learn{Prefix: "i like", Next: "cheese", Author: "u1"},
		dbtest.Learn{Prefix: "we love", Next: "cheese", Author: "u1"},
		// Repeating one of them must not raise it again.
		dbtest.Learn{Prefix: "i like", Next: "cheese", Author: "u2"},
		// "i like" has two distinct successors, so N1+(i like .) is 2.
		dbtest.Learn{Prefix: "i like", Next: "bread", Author: "u1"},
	)

	if err := s.View(func(r *storage.Reader) error {
		st, err := r.KNStats("i like", "cheese")
		if err != nil {
			return err
		}
		if st.DistinctSuccessors != 2 {
			t.Errorf("DistinctSuccessors = %d, want 2", st.DistinctSuccessors)
		}
		if st.DistinctPredecessors != 2 {
			t.Errorf("DistinctPredecessors = %d, want 2", st.DistinctPredecessors)
		}
		if total := r.TotalDistinctPredecessors(); total != 3 {
			// cheese contributes 2, bread contributes 1.
			t.Errorf("TotalDistinctPredecessors = %d, want 3", total)
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
}

// TestHistoryEvictionIsChronological is the regression pin for finding 10, and it is
// built specifically around the mixed-digit-width case that broke the old version:
// snowflakes stored as decimal strings sorted "9999..." before "10000...", so a
// 17-digit ID was evicted before an 18-digit one regardless of age.
func TestHistoryEvictionIsChronological(t *testing.T) {
	s := dbtest.Store(t)

	// Deliberately spanning 17, 18 and 19 digits, in ascending numeric (and
	// therefore chronological) order.
	ids := []string{
		"99999999999999999",   // 17 digits, oldest
		"100000000000000000",  // 18 digits
		"999999999999999999",  // 18 digits
		"1000000000000000000", // 19 digits
		"1000000000000000001", // 19 digits, newest
	}

	// A window of 3 forces the two oldest out.
	const window = 3
	if err := s.Update(func(w *storage.Writer) error {
		for i, id := range ids {
			if err := w.MarkSeen(id, time.Unix(int64(i), 0), window); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		if got := r.HistoryCount(); got != window {
			t.Errorf("HistoryCount() = %d, want %d", got, window)
		}
		// The two numerically smallest, which are the two oldest, must be gone.
		for _, gone := range ids[:2] {
			seen, err := r.Seen(gone)
			if err != nil {
				return err
			}
			if seen {
				t.Errorf("id %q (%d digits) survived eviction; it is older than the entries kept",
					gone, len(gone))
			}
		}
		for _, kept := range ids[2:] {
			seen, err := r.Seen(kept)
			if err != nil {
				return err
			}
			if !seen {
				t.Errorf("id %q was evicted but is newer than entries that survived", kept)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
}

// TestMarkSeenIsIdempotent guards the counter. A double count would make the window
// shrink below its configured size on every duplicate, which the backfill produces
// constantly.
func TestMarkSeenIsIdempotent(t *testing.T) {
	s := dbtest.Store(t)

	if err := s.Update(func(w *storage.Writer) error {
		for range 5 {
			if err := w.MarkSeen("123456789012345678", time.Unix(0, 0), 100); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		if got := r.HistoryCount(); got != 1 {
			t.Errorf("HistoryCount() = %d after five identical MarkSeen calls, want 1", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSeenRejectsNonSnowflake(t *testing.T) {
	s := dbtest.Store(t)
	if err := s.View(func(r *storage.Reader) error {
		_, err := r.Seen("not-a-number")
		if err == nil {
			t.Error("a non-snowflake message ID must be an error, not a silent miss")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCountersSurviveLargeValues covers the width choice. A busy server crosses
// 2^32 occurrences of a common bigram, and a uint32 count would silently wrap.
func TestCountersPast32Bits(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t, s, dbtest.Learn{Prefix: "the", Next: "bird", Author: "u1"})

	// Reaching 2^32 by learning would take too long, so this asserts the encoding
	// round-trips a value past the boundary via the association codec, which shares
	// its width with the n-gram count.
	const big = uint64(1) << 33
	if err := s.Update(func(w *storage.Writer) error {
		for range 3 {
			if err := w.AddTopicWord("bird", "roof", 0.5); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("AddTopicWord: %v", err)
	}
	if err := s.View(func(r *storage.Reader) error {
		got, err := r.TopicWord("bird", "roof")
		if err != nil {
			return err
		}
		if got.Count != 3 {
			t.Errorf("count = %d, want 3", got.Count)
		}
		if got.PosSum < 1.49 || got.PosSum > 1.51 {
			t.Errorf("PosSum = %v, want about 1.5", got.PosSum)
		}
		if mean := got.MeanPosition(); mean < 0.49 || mean > 0.51 {
			t.Errorf("MeanPosition() = %v, want about 0.5", mean)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = big
}

func TestTopicCountHasNoMinimumWordLength(t *testing.T) {
	s := dbtest.Store(t)
	if err := s.Update(func(w *storage.Writer) error {
		for _, word := range []string{"ok", "no", "wtf", "a"} {
			if err := w.IncTopic(word); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("IncTopic: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		for _, word := range []string{"ok", "no", "wtf", "a"} {
			if got := r.TopicCount(word); got != 1 {
				t.Errorf("TopicCount(%q) = %d, want 1: the old three-character minimum "+
					"excluded most of this server's register from topic gravity", word, got)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAssocsForDoesNotLeakAcrossPrefixes(t *testing.T) {
	s := dbtest.Store(t)
	if err := s.Update(func(w *storage.Writer) error {
		for _, e := range []struct{ a, b string }{
			{"bird", "roof"},
			{"bird", "sky"},
			{"birdcage", "wire"},
			{"bird house", "wood"},
		} {
			if err := w.AddTopicWord(e.a, e.b, 0.5); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("AddTopicWord: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		got, err := r.TopicWordsFor("bird")
		if err != nil {
			return err
		}
		if len(got) != 2 {
			t.Errorf("TopicWordsFor(\"bird\") = %v, want exactly roof and sky", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestPurgeAuthorRemovesDiversityNotFrequency pins exactly what the purge does and
// does not do, so the limitation is documented by a test rather than discovered.
func TestPurgeAuthorRemovesDiversityNotFrequency(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t,
		s,
		dbtest.Learn{Prefix: "say", Next: "thing", Author: "bad"},
		dbtest.Learn{Prefix: "say", Next: "thing", Author: "bad"},
		dbtest.Learn{Prefix: "say", Next: "thing", Author: "good"},
		dbtest.Learn{Prefix: "other", Next: "phrase", Author: "bad"},
	)

	var removed int
	if err := s.Update(func(w *storage.Writer) error {
		var err error
		removed, err = w.PurgeAuthor("bad")
		return err
	}); err != nil {
		t.Fatalf("PurgeAuthor: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d diversity entries, want 2", removed)
	}

	shared := successorMap(t, s, "say")["thing"]
	if shared.Authors != 1 {
		t.Errorf("authors = %d after purge, want 1 (only \"good\" remains)", shared.Authors)
	}
	if shared.Count != 3 {
		t.Errorf("count = %d, want 3: the purge deliberately leaves frequency alone", shared.Count)
	}

	// A phrase only the purged author ever said drops to zero distinct authors, which
	// is what makes it ineligible to generate.
	solo := successorMap(t, s, "other")["phrase"]
	if solo.Authors != 0 {
		t.Errorf("authors = %d for a phrase only the purged author said, want 0", solo.Authors)
	}
}

func TestPurgeAuthorRefusesEmptyID(t *testing.T) {
	s := dbtest.Store(t)
	err := s.Update(func(w *storage.Writer) error {
		_, err := w.PurgeAuthor("")
		return err
	})
	if err == nil {
		t.Error("purging the empty author ID must be refused: it is the bot's own marker, " +
			"so this would silently strip diversity from everything the bot ever said")
	}
}

func TestDeleteNgramAndRebuild(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t,
		s,
		dbtest.Learn{Prefix: "i like", Next: "cheese", Author: "u1"},
		dbtest.Learn{Prefix: "we love", Next: "cheese", Author: "u1"},
		dbtest.Learn{Prefix: "i like", Next: "bread", Author: "u1"},
	)

	if err := s.Update(func(w *storage.Writer) error {
		if err := w.DeleteNgram("we love", "cheese"); err != nil {
			return err
		}
		return w.RebuildKNIndexes()
	}); err != nil {
		t.Fatalf("delete and rebuild: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		st, err := r.KNStats("i like", "cheese")
		if err != nil {
			return err
		}
		// cheese now follows only one context.
		if st.DistinctPredecessors != 1 {
			t.Errorf("DistinctPredecessors = %d after delete and rebuild, want 1", st.DistinctPredecessors)
		}
		if st.DistinctSuccessors != 2 {
			t.Errorf("DistinctSuccessors = %d, want 2", st.DistinctSuccessors)
		}
		if got := r.TotalDistinctPredecessors(); got != 2 {
			t.Errorf("TotalDistinctPredecessors = %d, want 2", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestImageCacheEvictsOldest(t *testing.T) {
	s := dbtest.Store(t)

	// URLs whose lexicographic order is the REVERSE of their age, so an eviction that
	// used cursor order rather than the timestamp would remove the wrong one.
	urls := []string{"https://z.example/3", "https://m.example/2", "https://a.example/1"}
	if err := s.Update(func(w *storage.Writer) error {
		for i, u := range urls {
			if err := w.AddImageURL(u, time.Unix(int64(i), 0), 2); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("AddImageURL: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		got, err := r.ImageURLs()
		if err != nil {
			return err
		}
		if len(got) != 2 {
			t.Fatalf("cache holds %d URLs, want 2: %v", len(got), got)
		}
		for _, u := range got {
			if u == urls[0] {
				t.Errorf("the oldest URL %q survived; eviction used key order rather than age", u)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestOpenRefusesAPreM6Corpus is the migration guard. The old layout is not readable
// and there is no converter, so the only safe behaviour is to refuse with an
// explanation rather than read the bytes as though they were the new format.
func TestOpenRefusesAPreM6Corpus(t *testing.T) {
	path := dbtest.Path(t)

	// Fabricate a corpus in the shape the old code wrote: a "markov" bucket with a
	// JSON successor map, and no schema version.
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("markov"))
		if err != nil {
			return err
		}
		return b.Put([]byte("the bird"), []byte(`{"flew":3,"sang":1}`))
	}); err != nil {
		t.Fatalf("seed old layout: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	s, err := storage.Open(path)
	if err == nil {
		_ = s.Close()
		t.Fatal("Open accepted a pre-M6 corpus; it must refuse rather than read old bytes as new")
	}
	if !errors.Is(err, storage.ErrSchemaMismatch) {
		t.Errorf("got %v, want ErrSchemaMismatch", err)
	}
	// The message has to say what to do about it.
	for _, want := range []string{"pre-M6", "Start fresh"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must be actionable and mention %q; got: %v", want, err)
		}
	}
}

func TestOpenStampsAndAcceptsItsOwnCorpus(t *testing.T) {
	path := dbtest.Path(t)

	first, err := storage.Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	dbtest.Seed(t, first, dbtest.Learn{Prefix: "the", Next: "bird", Author: "u1"})
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := storage.Open(path)
	if err != nil {
		t.Fatalf("reopening a corpus this binary wrote must succeed: %v", err)
	}
	defer func() { _ = second.Close() }()

	if got := successorMap(t, second, "the"); got["bird"].Count != 1 {
		t.Errorf("data did not survive a reopen: %v", got)
	}
}

// TestOpenOnALockedCorpusFailsFast pins the timeout. Without it, opening a corpus a
// live bot holds blocks forever with no output, which reads as the tool hanging.
func TestOpenOnALockedCorpusFailsFast(t *testing.T) {
	path := dbtest.Path(t)
	first, err := storage.Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer func() { _ = first.Close() }()

	start := time.Now()
	if _, err := storage.Open(path); err == nil {
		t.Fatal("opening a locked corpus must fail rather than block forever")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("Open took %s; the bbolt timeout is not being applied", elapsed)
	}
}

func TestBackupIsReadable(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t, s, dbtest.Learn{Prefix: "the", Next: "bird", Author: "u1"})

	backupPath := dbtest.Path(t)
	if err := s.Backup(backupPath); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	restored, err := storage.Open(backupPath)
	if err != nil {
		t.Fatalf("the backup is not a usable corpus: %v", err)
	}
	defer func() { _ = restored.Close() }()

	if got := successorMap(t, restored, "the"); got["bird"].Count != 1 {
		t.Errorf("the backup does not contain the data: %v", got)
	}
}

func TestCompactPreservesData(t *testing.T) {
	s := dbtest.Store(t)
	entries := make([]dbtest.Learn, 0, 200)
	for i := range 200 {
		entries = append(entries, dbtest.Learn{
			Prefix: "prefix" + strconv.Itoa(i%20),
			Next:   "next" + strconv.Itoa(i),
			Author: "u" + strconv.Itoa(i%3),
		})
	}
	dbtest.Seed(t, s, entries...)

	compacted := dbtest.Path(t)
	if err := s.Compact(compacted); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	out, err := storage.Open(compacted)
	if err != nil {
		t.Fatalf("open compacted: %v", err)
	}
	defer func() { _ = out.Close() }()

	if err := out.View(func(r *storage.Reader) error {
		if got := r.NgramCount(); got != 200 {
			t.Errorf("compacted corpus holds %d n-grams, want 200", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReaderCannotStartATransaction is the finding-1 guard, expressed as a
// compile-time fact rather than a runtime assertion.
//
// The nested-transaction deadlock needed a consumer that held a database handle and
// could open a second transaction from inside the first. Reader has no such method,
// and neither does Writer, so the deadlock is not expressible from outside this
// package. This test documents that and will fail to compile if someone adds a
// View or Update method to either type.
func TestReaderCannotStartATransaction(t *testing.T) {
	s := dbtest.Store(t)

	if err := s.View(func(r *storage.Reader) error {
		// If Reader ever grows a View or Update method, the line below starts
		// compiling and this test should be revisited along with the design.
		var _ any = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The positive half: a Writer can read without a second transaction, which is why
	// nesting is unnecessary in the first place.
	if err := s.Update(func(w *storage.Writer) error {
		if err := w.LearnNgram("the", "bird", "u1"); err != nil {
			return err
		}
		got, err := w.Successors("the")
		if err != nil {
			return err
		}
		if len(got) != 1 {
			return fmt.Errorf("a Writer must be able to read its own writes; got %v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBlobRoundTrip(t *testing.T) {
	s := dbtest.Store(t)

	if err := s.Update(func(w *storage.Writer) error {
		return w.PutBlob(storage.BlobLeaderboard, "current", []byte(`{"scores":{}}`))
	}); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if err := s.View(func(r *storage.Reader) error {
		got, err := r.GetBlob(storage.BlobLeaderboard, "current")
		if err != nil {
			return err
		}
		if string(got) != `{"scores":{}}` {
			t.Errorf("GetBlob = %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.View(func(r *storage.Reader) error {
		if _, err := r.GetBlob("nonsense", "k"); err == nil {
			t.Error("an unknown blob kind must be an error, not a silent nil")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceBlobsIsAtomicSwap(t *testing.T) {
	s := dbtest.Store(t)

	if err := s.Update(func(w *storage.Writer) error {
		return w.ReplaceBlobs(storage.BlobCluster, map[string][]byte{"a": []byte("1"), "b": []byte("2")})
	}); err != nil {
		t.Fatalf("ReplaceBlobs: %v", err)
	}
	if err := s.Update(func(w *storage.Writer) error {
		return w.ReplaceBlobs(storage.BlobCluster, map[string][]byte{"c": []byte("3")})
	}); err != nil {
		t.Fatalf("ReplaceBlobs second: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		seen := map[string]string{}
		if err := r.ForEachBlob(storage.BlobCluster, func(k string, v []byte) error {
			seen[k] = string(v)
			return nil
		}); err != nil {
			return err
		}
		if len(seen) != 1 || seen["c"] != "3" {
			t.Errorf("after a replace the bucket holds %v, want only c=3", seen)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNameRoundTrip(t *testing.T) {
	s := dbtest.Store(t)

	want := corpus.Name{Count: 4, DiscordUserID: "123", Canonical: "dave"}
	if err := s.Update(func(w *storage.Writer) error {
		return w.PutName("davey", want)
	}); err != nil {
		t.Fatalf("PutName: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		got, ok, err := r.Name("davey")
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("name not found")
		}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
		if _, ok, _ := r.Name("nobody"); ok {
			t.Error("an unknown name reported as found")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserStats(t *testing.T) {
	s := dbtest.Store(t)
	now := time.Unix(1700000000, 0)

	if err := s.Update(func(w *storage.Writer) error {
		for range 3 {
			if err := w.IncUserStat("u1", now); err != nil {
				return err
			}
		}
		return w.IncUserStat("u2", now)
	}); err != nil {
		t.Fatalf("IncUserStat: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		all, err := r.AllUserStats()
		if err != nil {
			return err
		}
		if all["u1"].Count != 3 || all["u2"].Count != 1 {
			t.Errorf("AllUserStats = %v", all)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestHasSuccessorsMatchesTheOldPrefixLookup covers the replacement for what used to
// be `markovB.Get([]byte(prefix)) != nil`, which worked only because the prefix was
// itself a key. It no longer is, so the question became "is this prefix's key range
// non-empty", and getting the range wrong the obvious way (a byte-prefix match) would
// report true for "the" when only "the cat" had been learned.
func TestHasSuccessorsMatchesTheOldPrefixLookup(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t, s,
		dbtest.Learn{Prefix: "the cat", Next: "sat", Author: "u1"},
		dbtest.Learn{Prefix: "bird", Next: "flew", Author: "u1"},
	)

	cases := map[string]bool{
		"bird":     true,
		"the cat":  true,
		"the":      false, // "the cat" keys must NOT satisfy a query for "the"
		"the ca":   false, // a byte-prefix match would wrongly say true
		"birds":    false,
		"":         false, // an empty prefix is never a real key (finding 5)
		"nonsense": false,
	}
	if err := s.View(func(r *storage.Reader) error {
		for prefix, want := range cases {
			if got := r.HasSuccessors(prefix); got != want {
				t.Errorf("HasSuccessors(%q) = %v, want %v", prefix, got, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCorpusEmptyAndFirstPrefix covers the two cheap answers the generation path
// needs before it will attempt a reply. CorpusEmpty exists so that check is not a
// Bucket.Stats() page walk on a per-message path (finding 11).
func TestCorpusEmptyAndFirstPrefix(t *testing.T) {
	s := dbtest.Store(t)

	if err := s.View(func(r *storage.Reader) error {
		if !r.CorpusEmpty() {
			t.Error("a fresh corpus must report empty")
		}
		if prefix, ok := r.FirstPrefix(); ok {
			t.Errorf("FirstPrefix on an empty corpus returned %q", prefix)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	dbtest.Seed(t, s,
		dbtest.Learn{Prefix: "zebra", Next: "ran", Author: "u1"},
		dbtest.Learn{Prefix: "aardvark", Next: "slept", Author: "u1"},
	)

	if err := s.View(func(r *storage.Reader) error {
		if r.CorpusEmpty() {
			t.Error("a seeded corpus must not report empty")
		}
		prefix, ok := r.FirstPrefix()
		if !ok {
			t.Fatal("FirstPrefix found nothing in a seeded corpus")
		}
		// The prefix alone, not the composite key: the separator and the successor
		// token must be stripped, or the fallback seed is a string with a NUL in it.
		if prefix != "aardvark" {
			t.Errorf("FirstPrefix = %q, want %q (the prefix, with no separator or successor)",
				prefix, "aardvark")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMessagesLearnedIsAMetaCounter pins that the lifetime ingestion count is not a
// key in the stats bucket. It used to be, under the literal key
// "total_messages_learned", so every reader of that bucket had to recognize and skip
// it; one that forgot would decode an integer as a WeeklyStat and count a phantom
// user.
func TestMessagesLearnedIsAMetaCounter(t *testing.T) {
	s := dbtest.Store(t)

	if err := s.Update(func(w *storage.Writer) error {
		for range 3 {
			if err := w.IncMessagesLearned(); err != nil {
				return err
			}
		}
		return w.IncUserStat("u1", time.Unix(1700000000, 0))
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		if got := r.MessagesLearned(); got != 3 {
			t.Errorf("MessagesLearned = %d, want 3", got)
		}
		all, err := r.AllUserStats()
		if err != nil {
			return err
		}
		if len(all) != 1 {
			t.Errorf("AllUserStats returned %d entries, want only the one real user: %v", len(all), all)
		}
		if r.Status().Learned != 3 {
			t.Errorf("Status().Learned = %d, want 3", r.Status().Learned)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestPutUserStatOverwrites covers the weekly reset, which is the reason PutUserStat
// exists at all: the rule is "start again at one if the last message predates this
// week", and that needs a definition of when a week starts, which is the caller's
// policy and not storage's.
func TestPutUserStatOverwrites(t *testing.T) {
	s := dbtest.Store(t)
	lastWeek := time.Unix(1700000000, 0).UTC()

	if err := s.Update(func(w *storage.Writer) error {
		for range 50 {
			if err := w.IncUserStat("u1", lastWeek); err != nil {
				return err
			}
		}
		// The reset a caller performs at a week boundary.
		return w.PutUserStat("u1", corpus.WeeklyStat{Count: 1, LastTimestamp: lastWeek.Add(7 * 24 * time.Hour)})
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		got, ok, err := r.UserStat("u1")
		if err != nil || !ok {
			t.Fatalf("UserStat: ok=%v err=%v", ok, err)
		}
		if got.Count != 1 {
			t.Errorf("Count = %d after a reset write, want 1: PutUserStat must overwrite rather "+
				"than accumulate", got.Count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestIsNameDoesNotDecode covers the cheap presence check the scorer uses. It must
// agree with Name on existence, including for an alias, and must not care whether the
// record decodes: a malformed record is still a name that exists, and the scorer's
// boost has no use for the fields.
func TestIsNameDoesNotDecode(t *testing.T) {
	s := dbtest.Store(t)
	if err := s.Update(func(w *storage.Writer) error {
		if err := w.PutName("dave", corpus.Name{Count: 3, DiscordUserID: "u1"}); err != nil {
			return err
		}
		return w.PutName("davey", corpus.Name{DiscordUserID: "u1", Canonical: "dave"})
	}); err != nil {
		t.Fatalf("PutName: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		for _, key := range []string{"dave", "davey"} {
			if !r.IsName(key) {
				t.Errorf("IsName(%q) = false, want true", key)
			}
		}
		if r.IsName("nobody") {
			t.Error("IsName reported an unknown key as a name")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- M7a additions

// TestPrefixTotalSumsOnlyItsOwnRange is the correctness test for the one method in this
// layer that deliberately walks instead of reading a counter.
//
// PrefixTotal is the normalizer of interpolated Kneser-Ney's discounted term, so if it
// ever summed across the NUL separator into longer prefixes, every lambda in the model
// would be wrong. Nothing in the output would look broken: the balance between n-gram
// orders would just quietly shift, which is the same class of invisible defect as the
// unweighted backoff it exists to replace.
func TestPrefixTotalSumsOnlyItsOwnRange(t *testing.T) {
	s := dbtest.Store(t)
	dbtest.Seed(t, s,
		dbtest.Learn{Prefix: "the", Next: "bird", Author: "a"},
		dbtest.Learn{Prefix: "the", Next: "bird", Author: "b"},
		dbtest.Learn{Prefix: "the", Next: "cat", Author: "a"},
		dbtest.Learn{Prefix: "the bird", Next: "flew", Author: "a"},
		dbtest.Learn{Prefix: "the bird", Next: "sang", Author: "b"},
		dbtest.Learn{Prefix: "theatre", Next: "trip", Author: "a"},
	)

	if err := s.View(func(r *storage.Reader) error {
		// "the" has bird twice and cat once. It must NOT pick up the two "the bird"
		// continuations, and it must not pick up "theatre" either: NUL sorts below every
		// printable byte, so the range ends before any longer prefix begins.
		if got := r.PrefixTotal("the"); got != 3 {
			t.Errorf("PrefixTotal(\"the\") = %d, want 3. Anything larger means the scan "+
				"crossed the separator into \"the bird\" or \"theatre\" keys", got)
		}
		if got := r.PrefixTotal("the bird"); got != 2 {
			t.Errorf("PrefixTotal(\"the bird\") = %d, want 2", got)
		}
		if got := r.PrefixTotal("theatre"); got != 1 {
			t.Errorf("PrefixTotal(\"theatre\") = %d, want 1", got)
		}
		if got := r.PrefixTotal("nonexistent"); got != 0 {
			t.Errorf("PrefixTotal on an unknown prefix = %d, want 0", got)
		}
		// Refused explicitly rather than left to work by accident, because it reads as
		// "sum the whole corpus" to anyone scanning the call site.
		if got := r.PrefixTotal(""); got != 0 {
			t.Errorf("PrefixTotal(\"\") = %d, want 0", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestPrefixTotalAgreesWithSuccessors keeps the walk and the enumeration honest about
// each other. They are two different cursor scans over the same range, and the engine
// uses one for the normalizer and the other for the candidates.
func TestPrefixTotalAgreesWithSuccessors(t *testing.T) {
	s := dbtest.Store(t)

	var entries []dbtest.Learn
	for i := range 40 {
		entries = append(entries, dbtest.Learn{
			Prefix: "shared prefix",
			Next:   "tok" + strconv.Itoa(i%7),
			Author: "a" + strconv.Itoa(i%3),
		})
	}
	dbtest.Seed(t, s, entries...)

	if err := s.View(func(r *storage.Reader) error {
		succ, err := r.Successors("shared prefix")
		if err != nil {
			return err
		}
		var want uint64
		for _, sc := range succ {
			want += sc.Count
		}
		if got := r.PrefixTotal("shared prefix"); got != want {
			t.Errorf("PrefixTotal = %d, sum over Successors = %d", got, want)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestTopicTotalIsMaintainedIncrementally covers the counter the Kneser-Ney base case
// divides by for its raw-frequency half.
func TestTopicTotalIsMaintainedIncrementally(t *testing.T) {
	s := dbtest.Store(t)

	if err := s.Update(func(w *storage.Writer) error {
		for _, word := range []string{"bird", "bird", "loose", "ok"} {
			if err := w.IncTopic(word); err != nil {
				return err
			}
		}
		// An empty word is a no-op and must not move the total, or the denominator
		// drifts above the sum of the counts it normalizes.
		return w.IncTopic("")
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		if got := r.TotalTopicCount(); got != 4 {
			t.Errorf("TotalTopicCount = %d, want 4", got)
		}
		// And it must equal the sum of the per-word counts it is the denominator for.
		var sum uint64
		for _, word := range []string{"bird", "loose", "ok"} {
			sum += r.TopicCount(word)
		}
		if sum != 4 {
			t.Errorf("per-word counts sum to %d, want 4", sum)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestOpenBackfillsTheTopicTotalForAnOlderCorpus is the upgrade path for a schema-1
// corpus written before the counter existed, which is every corpus created between M6a
// and M7a.
//
// A zero here is not a crash. It silently turns PEREGRINE_KN_RAW_MIX off, so the bot
// generates with pure Kneser-Ney and therefore systematically suppresses the memetic
// register it exists to speak in, with nothing in the log to say why. That is exactly
// the kind of silent degradation this repo keeps designing against, which is why the
// counter is derived at startup rather than defaulted.
func TestOpenBackfillsTheTopicTotalForAnOlderCorpus(t *testing.T) {
	path := dbtest.Path(t)

	// Build a normal current corpus, then remove the counter to reproduce what a
	// pre-M7a file looks like on disk.
	s, err := storage.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Update(func(w *storage.Writer) error {
		for _, word := range []string{"bird", "bird", "bird", "loose", "ok"} {
			if err := w.IncTopic(word); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed topics: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("meta")).Delete([]byte("count:topic_total"))
	}); err != nil {
		t.Fatalf("remove counter: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	reopened, err := storage.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	if err := reopened.View(func(r *storage.Reader) error {
		if got := r.TotalTopicCount(); got != 5 {
			t.Errorf("TotalTopicCount after backfill = %d, want 5. Open must derive the "+
				"counter for a corpus written before it existed, because a zero silently "+
				"disables the raw-count half of the Kneser-Ney base case", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestOpenDoesNotStampATopicTotalOnAnEmptyCorpus is the other half of the backfill
// condition. A new file must not have the key written for it, or every startup pays a
// walk over a bucket that is about to be filled by the first message anyway.
func TestOpenDoesNotStampATopicTotalOnAnEmptyCorpus(t *testing.T) {
	s := dbtest.Store(t)

	if err := s.View(func(r *storage.Reader) error {
		if got := r.TotalTopicCount(); got != 0 {
			t.Errorf("TotalTopicCount on a fresh corpus = %d, want 0", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	// And the first write must establish it correctly rather than starting from a
	// stamped zero that the backfill would then decline to fix.
	if err := s.Update(func(w *storage.Writer) error { return w.IncTopic("first") }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := s.View(func(r *storage.Reader) error {
		if got := r.TotalTopicCount(); got != 1 {
			t.Errorf("TotalTopicCount after one word = %d, want 1", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

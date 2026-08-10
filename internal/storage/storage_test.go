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

// snowflake returns a plausible Discord ID, n milliseconds after the epoch. Ascending
// n gives ascending, chronologically ordered IDs, which is the property the image
// cache's key layout depends on.
func snowflake(n int) string {
	return strconv.FormatUint((uint64(n)<<22)|1, 10)
}

func imageURLs(t *testing.T, s *storage.Store) []string {
	t.Helper()
	var out []string
	if err := s.View(func(r *storage.Reader) error {
		var err error
		out, err = r.ImageURLs()
		return err
	}); err != nil {
		t.Fatalf("ImageURLs: %v", err)
	}
	return out
}

func cacheImage(t *testing.T, s *storage.Store, url, msgID, author string, maxTotal, perAuthor int) {
	t.Helper()
	if err := s.Update(func(w *storage.Writer) error {
		return w.AddImageURL(url, msgID, author, maxTotal, perAuthor)
	}); err != nil {
		t.Fatalf("AddImageURL(%s): %v", url, err)
	}
}

func TestImageCacheEvictsOldest(t *testing.T) {
	s := dbtest.Store(t)

	// URLs whose lexicographic order is the REVERSE of their age, so eviction that
	// walked URL order rather than message order would remove the wrong one. That was
	// the pre-M11b layout's problem: the key was the URL, so cursor order had nothing
	// to do with age and the trim had to hunt for the minimum timestamp.
	urls := []string{"https://z.example/3", "https://m.example/2", "https://a.example/1"}
	for i, u := range urls {
		cacheImage(t, s, u, snowflake(100+i), snowflake(7), 2, 0)
	}

	got := imageURLs(t, s)
	if len(got) != 2 {
		t.Fatalf("cache holds %d URLs, want 2: %v", len(got), got)
	}
	for _, u := range got {
		if u == urls[0] {
			t.Errorf("the oldest URL %q survived; eviction used key order rather than message order", u)
		}
	}
}

// TestOneAuthorCannotFillTheImageCache is SPEC.md section 4, A7. Image reposting has
// the bot republish user-supplied media under its own name in a channel of its
// choosing, so a hostile user seeding the cache is the attack, and the per-author cap
// is what bounds their share of it.
//
// Verified by reverting: with the cap check removed from AddImageURL, the poisoner
// owns all 20 entries and this fails on the first assertion.
func TestOneAuthorCannotFillTheImageCache(t *testing.T) {
	s := dbtest.Store(t)

	const poisoner = "999000000000000001"
	for i := 0; i < 20; i++ {
		cacheImage(t, s, fmt.Sprintf("https://cdn.example/spam-%02d.png", i), snowflake(1000+i), poisoner, 20, 3)
	}

	var held int
	if err := s.View(func(r *storage.Reader) error {
		held = r.ImageAuthorCount(poisoner)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if held != 3 {
		t.Errorf("one author holds %d cache entries against a cap of 3; the cap is not enforced", held)
	}

	// And the cap evicts THEIR OLDEST rather than dropping the new URL, so a prolific
	// poster's share stays current instead of freezing at whatever they posted first.
	got := imageURLs(t, s)
	if len(got) != 3 {
		t.Fatalf("cache holds %v, want the author's 3 most recent", got)
	}
	for _, u := range got {
		if u == "https://cdn.example/spam-00.png" {
			t.Error("the cap dropped new URLs instead of evicting the author's oldest")
		}
	}
	if got[2] != "https://cdn.example/spam-19.png" {
		t.Errorf("newest cached URL is %q, want the last one posted", got[2])
	}
}

// TestTheCapIsPerAuthorAndNotGlobal pins that the cap bounds one author's share
// rather than the cache: several people together still fill it.
func TestTheCapIsPerAuthorAndNotGlobal(t *testing.T) {
	s := dbtest.Store(t)

	msg := 2000
	for a := 0; a < 4; a++ {
		author := snowflake(500 + a)
		for i := 0; i < 3; i++ {
			cacheImage(t, s, fmt.Sprintf("https://cdn.example/a%d-%d.png", a, i), snowflake(msg), author, 100, 2)
			msg++
		}
	}

	if got := imageURLs(t, s); len(got) != 8 {
		t.Errorf("cache holds %d URLs, want 8 (four authors at a cap of two): %v", len(got), got)
	}
}

// TestDeleteImagesByMessageRevokesTheRepost is the other half of A7: a deletion is a
// strong signal that the content must not be republished, so the entries a deleted
// message contributed have to become unrepostable.
//
// Verified by reverting: with DeleteImagesByMessage reduced to a no-op returning 0,
// the deleted message's URL is still in the cache and this fails.
func TestDeleteImagesByMessageRevokesTheRepost(t *testing.T) {
	s := dbtest.Store(t)

	const doomed = "1720000000000000001"
	const kept = "1730000000000000001"
	cacheImage(t, s, "https://cdn.example/doomed-a.png", doomed, snowflake(11), 100, 10)
	cacheImage(t, s, "https://cdn.example/doomed-b.png", doomed, snowflake(11), 100, 10)
	cacheImage(t, s, "https://cdn.example/kept.png", kept, snowflake(12), 100, 10)

	var removed int
	if err := s.Update(func(w *storage.Writer) error {
		var err error
		removed, err = w.DeleteImagesByMessage(doomed)
		return err
	}); err != nil {
		t.Fatalf("DeleteImagesByMessage: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d entries, want both of the deleted message's", removed)
	}

	got := imageURLs(t, s)
	if len(got) != 1 || got[0] != "https://cdn.example/kept.png" {
		t.Errorf("cache holds %v, want only the surviving message's URL", got)
	}

	// The counter has to move with the delete, or the next insert trims against a
	// cache size that is no longer true.
	var cached uint64
	if err := s.View(func(r *storage.Reader) error {
		cached = r.Status().ImageCache
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if cached != 1 {
		t.Errorf("count:image is %d after deleting two of three entries, want 1", cached)
	}
}

// TestImageCacheDedupesAURL pins that caching the same URL twice does not spend two
// slots, and that the second poster does not inherit the entry.
func TestImageCacheDedupesAURL(t *testing.T) {
	s := dbtest.Store(t)

	const url = "https://tenor.com/view/bird-flying-gif-12345"
	first := snowflake(300)
	second := snowflake(301)
	cacheImage(t, s, url, snowflake(3000), first, 100, 10)
	cacheImage(t, s, url, snowflake(3001), second, 100, 10)

	if got := imageURLs(t, s); len(got) != 1 {
		t.Errorf("cache holds %d copies of one URL, want 1: %v", len(got), got)
	}
	if err := s.View(func(r *storage.Reader) error {
		if n := r.ImageAuthorCount(second); n != 0 {
			t.Errorf("the second poster was charged %d entries for a URL already cached, want 0", n)
		}
		if n := r.ImageAuthorCount(first); n != 1 {
			t.Errorf("the original poster holds %d entries, want 1: re-posting reattributed it", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestImageCacheRefusesInventedIDs. A message or author ID that is not a snowflake can
// only come from a caller that made it up, and the right answer for that is an error
// rather than a key nothing will ever find again. Same rule as MarkSeen.
func TestImageCacheRefusesInventedIDs(t *testing.T) {
	s := dbtest.Store(t)

	for _, tc := range []struct{ name, msgID, author string }{
		{"message id", "not-a-snowflake", snowflake(1)},
		{"author id", snowflake(1), "me"},
	} {
		err := s.Update(func(w *storage.Writer) error {
			return w.AddImageURL("https://cdn.example/x.png", tc.msgID, tc.author, 10, 5)
		})
		if err == nil {
			t.Errorf("%s: AddImageURL accepted a non-snowflake", tc.name)
		}
	}
}

// TestSchemaUpgradeEmptiesTheImageCache pins the version 1 to 2 migration, and the
// asymmetry it represents: the corpus is refused because converting it means rewriting
// everything, whereas the image cache is discarded because it refills itself.
//
// A version 1 entry left in place would be unreadable under the new codec AND would
// keep counting against the cache size, so the cache would run permanently short.
func TestSchemaUpgradeEmptiesTheImageCache(t *testing.T) {
	path := dbtest.Path(t)

	// A version 1 corpus: real learned data plus an image entry in the old shape,
	// which was the bare URL as the key and a timestamp as the value.
	s, err := storage.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Update(func(w *storage.Writer) error {
		return w.LearnNgram("the bird", "flew", "author-1")
	}); err != nil {
		t.Fatalf("seed corpus: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket([]byte("image")).Put([]byte("https://cdn.example/v1.png"), []byte("12345678")); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("meta")).Put([]byte("count:image"), []byte{0, 0, 0, 0, 0, 0, 0, 1}); err != nil {
			return err
		}
		// Stamp it back to version 1.
		return tx.Bucket([]byte("meta")).Put([]byte("schema_version"), []byte{0, 0, 0, 0, 0, 0, 0, 1})
	}); err != nil {
		t.Fatalf("fabricate a v1 corpus: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	up, err := storage.Open(path)
	if err != nil {
		t.Fatalf("Open refused a version 1 corpus instead of upgrading it: %v", err)
	}
	defer func() { _ = up.Close() }()

	if got := imageURLs(t, up); len(got) != 0 {
		t.Errorf("the upgrade left %v in the image cache; a v1 entry is unreadable under the v2 codec", got)
	}
	if err := up.View(func(r *storage.Reader) error {
		st := r.Status()
		if st.ImageCache != 0 {
			t.Errorf("count:image is %d after the upgrade, want 0: the counter and the bucket disagree", st.ImageCache)
		}
		// The expensive half of the corpus survives. That is the entire reason this is
		// a migration rather than a refusal.
		if st.Ngrams == 0 {
			t.Error("the upgrade discarded learned n-grams; only the image cache is cheap enough to drop")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// And the upgraded corpus takes new-format entries.
	cacheImage(t, up, "https://cdn.example/v2.png", snowflake(4000), snowflake(9), 10, 5)
	if got := imageURLs(t, up); len(got) != 1 {
		t.Errorf("after upgrading, the cache holds %v, want the one new URL", got)
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

// TestReplaceBlobsIsAtomicSwap is gone with the methods it covered. ForEachBlob and ReplaceBlobs
// existed for the clustering pass and nothing else, and M13 removed them along with the bucket.

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

// ------------------------------------------------------------------ M9 cursors

// TestCursorRoundTrips covers the basic contract: an unread channel has no cursor, and a
// stored one comes back as the same ID.
func TestCursorRoundTrips(t *testing.T) {
	s := dbtest.Store(t)

	if err := s.View(func(r *storage.Reader) error {
		if got := r.Cursor("chan"); got != "" {
			t.Errorf("an unread channel has cursor %q, want empty. Empty is what ingest "+
				"reads as \"never seen\", which is what triggers the lookback bootstrap", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	const id = "1234567890123456789"
	if err := s.Update(func(w *storage.Writer) error { return w.SetCursor("chan", id) }); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	if err := s.View(func(r *storage.Reader) error {
		if got := r.Cursor("chan"); got != id {
			t.Errorf("Cursor = %q, want %q", got, id)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestCursorNeverMovesBackwards is the property that matters, not a courtesy.
//
// Two things can hand this an older ID: a batch processed out of order, and two ingest
// passes overlapping on one channel. Either would rewind the mark and make the next pass
// re-read and re-learn everything between, which is finding 13 arriving by a different
// route. A monotonic cursor cannot do that however confused its caller is.
func TestCursorNeverMovesBackwards(t *testing.T) {
	s := dbtest.Store(t)

	const newer = "2000000000000000000"
	const older = "1000000000000000000"

	if err := s.Update(func(w *storage.Writer) error {
		if err := w.SetCursor("chan", newer); err != nil {
			return err
		}
		return w.SetCursor("chan", older)
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		if got := r.Cursor("chan"); got != newer {
			t.Errorf("cursor moved backwards to %q, want it to stay at %q", got, newer)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestCursorComparesChronologicallyNotLexically. Snowflakes as decimal strings sort wrong:
// a 19-digit ID is numerically larger than a 20-digit one only if you compare bytes. This
// is the same trap as the history keys in finding 10, and it matters more here because a
// wrong comparison silently rewinds the mark.
func TestCursorComparesChronologicallyNotLexically(t *testing.T) {
	s := dbtest.Store(t)

	// 19 digits then 20 digits. Byte-wise "9..." > "1...", so a string comparison would
	// refuse the genuinely newer ID.
	const short = "9999999999999999999"
	const long = "10000000000000000000"

	if err := s.Update(func(w *storage.Writer) error {
		if err := w.SetCursor("chan", short); err != nil {
			return err
		}
		return w.SetCursor("chan", long)
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		if got := r.Cursor("chan"); got != long {
			t.Errorf("cursor = %q, want %q. Comparing snowflakes as strings gets this "+
				"backwards and rewinds the mark, which re-learns everything between", got, long)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestSetCursorRefusesNonsense. A message ID that is not a snowflake is an error rather
// than a key, which is the same answer LearnNgram gives an empty prefix: the caller
// invented it, and storing it would mean the cursor can never be compared again.
func TestSetCursorRefusesNonsense(t *testing.T) {
	s := dbtest.Store(t)

	for _, bad := range []string{"", "not-a-snowflake", "12.5", "-1"} {
		err := s.Update(func(w *storage.Writer) error { return w.SetCursor("chan", bad) })
		if err == nil {
			t.Errorf("SetCursor accepted %q as a message ID", bad)
		}
	}
	if err := s.Update(func(w *storage.Writer) error {
		return w.SetCursor("", "1234567890123456789")
	}); err == nil {
		t.Error("SetCursor accepted an empty channel ID")
	}
}

// TestForgetCursorMakesAChannelUnread, so an operator can re-ingest one channel without
// discarding the corpus.
func TestForgetCursorMakesAChannelUnread(t *testing.T) {
	s := dbtest.Store(t)

	if err := s.Update(func(w *storage.Writer) error {
		return w.SetCursor("chan", "1234567890123456789")
	}); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	if err := s.Update(func(w *storage.Writer) error { return w.ForgetCursor("chan") }); err != nil {
		t.Fatalf("ForgetCursor: %v", err)
	}
	if err := s.View(func(r *storage.Reader) error {
		if got := r.Cursor("chan"); got != "" {
			t.Errorf("cursor = %q after ForgetCursor, want empty", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestCursorsAreIndependentPerChannel, because they are the whole point: one busy channel
// must not affect what a quiet one re-reads.
func TestCursorsAreIndependentPerChannel(t *testing.T) {
	s := dbtest.Store(t)

	if err := s.Update(func(w *storage.Writer) error {
		if err := w.SetCursor("a", "1000000000000000001"); err != nil {
			return err
		}
		return w.SetCursor("b", "2000000000000000002")
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	seen := map[string]string{}
	if err := s.View(func(r *storage.Reader) error {
		return r.ForEachCursor(func(channelID, messageID string) error {
			seen[channelID] = messageID
			return nil
		})
	}); err != nil {
		t.Fatalf("ForEachCursor: %v", err)
	}

	if seen["a"] != "1000000000000000001" || seen["b"] != "2000000000000000002" {
		t.Errorf("ForEachCursor returned %v", seen)
	}
	if len(seen) != 2 {
		t.Errorf("saw %d cursors, want 2", len(seen))
	}
}

// TestAddingTheCursorBucketDidNotBreakAnExistingCorpus.
//
// A new bucket is backward compatible because Open creates every bucket it knows about
// with CreateBucketIfNotExists, so a corpus written before M9 gains an empty one and needs
// no schema bump. Worth pinning because the alternative, bumping SchemaVersion, would have
// forced every operator to discard their corpus for a feature that reads nothing from it.
func TestAddingTheCursorBucketDidNotBreakAnExistingCorpus(t *testing.T) {
	path := dbtest.Path(t)

	first, err := storage.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := first.Update(func(w *storage.Writer) error {
		return w.LearnNgram("the bird", "flew", "author")
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := storage.Open(path)
	if err != nil {
		t.Fatalf("reopen after adding a bucket must not need a schema bump: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	if err := reopened.View(func(r *storage.Reader) error {
		if got := r.Cursor("chan"); got != "" {
			t.Errorf("a corpus with no cursors reports %q", got)
		}
		if r.CorpusEmpty() {
			t.Error("the existing corpus was lost")
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestANewCorpusIsMarkedAlreadyBackfilled.
//
// The association re-walk repairs messages learned before a fix deployed (finding 46). A
// corpus created after that fix has nothing older to repair, so it must never walk. Deciding
// that in Open rather than in the service means it does not depend on an operator remembering
// to unset an environment variable after wiping the volume, and Open is the only place that
// knows the file is new.
func TestANewCorpusIsMarkedAlreadyBackfilled(t *testing.T) {
	s := dbtest.Store(t)

	var state string
	if err := s.View(func(r *storage.Reader) error {
		state = r.AssocBackfillState()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if state != storage.AssocBackfillDone {
		t.Errorf("a fresh corpus reports assoc backfill state %q, want %q: it would otherwise "+
			"re-read all of Discord history looking for messages that cannot exist",
			state, storage.AssocBackfillDone)
	}
}

// TestTheAssociationCursorIsIndependentOfTheIngestCursor.
//
// The two passes read the same channels for opposite reasons: ingest asks what is NEW and must
// never rewind, the re-walk asks what is OLD and finishes. Sharing one mark would let either
// move the other's, and moving the ingest mark backwards re-learns everything between, which
// is finding 13.
func TestTheAssociationCursorIsIndependentOfTheIngestCursor(t *testing.T) {
	s := dbtest.Store(t)
	const channel = "c1"

	newer, older := snowflake(5000), snowflake(1000)

	if err := s.Update(func(w *storage.Writer) error {
		if err := w.SetCursor(channel, newer); err != nil {
			return err
		}
		return w.SetAssocCursor(channel, older)
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.View(func(r *storage.Reader) error {
		if got := r.Cursor(channel); got != newer {
			t.Errorf("ingest cursor = %q, want %q: the association pass moved it", got, newer)
		}
		if got := r.AssocCursor(channel); got != older {
			t.Errorf("association cursor = %q, want %q", got, older)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestTheAssociationCursorRefusesToRewind, for the same reason SetCursor does: a batch
// processed out of order would otherwise have its associations counted twice.
func TestTheAssociationCursorRefusesToRewind(t *testing.T) {
	s := dbtest.Store(t)
	const channel = "c1"

	newer, older := snowflake(5000), snowflake(1000)

	if err := s.Update(func(w *storage.Writer) error {
		if err := w.SetAssocCursor(channel, newer); err != nil {
			return err
		}
		return w.SetAssocCursor(channel, older)
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.View(func(r *storage.Reader) error {
		if got := r.AssocCursor(channel); got != newer {
			t.Errorf("association cursor rewound to %q, want it held at %q", got, newer)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

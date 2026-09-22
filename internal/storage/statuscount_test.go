package storage_test

import (
	"fmt"
	"testing"

	"go.etcd.io/bbolt"

	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// Reader.Status answers from meta counters rather than walking the buckets, because a
// 26-second read transaction on the status ticker was stopping bbolt reclaiming freed
// pages and growing the file it was measuring (finding 58). The counters can only be
// trusted if every insert and delete moves them, and a counter that is merely
// self-consistent can still be consistently wrong, so these tests compare the counters
// against the thing they replaced: an actual walk of the same file.

// walkCounts is what Status used to do. The bucket names are spelled out because they
// are unexported, which is the same thing the schema-upgrade test does.
func walkCounts(t *testing.T, path string) map[string]int {
	t.Helper()
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = db.Close() }()

	out := map[string]int{}
	if err := db.View(func(tx *bbolt.Tx) error {
		for _, name := range []string{"ngram", "ngram_auth", "topic", "topic_word", "name_topic", "name"} {
			b := tx.Bucket([]byte(name))
			if b == nil {
				continue
			}
			out[name] = b.Stats().KeyN
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

func statusCounts(t *testing.T, s *storage.Store) map[string]int {
	t.Helper()
	out := map[string]int{}
	if err := s.View(func(r *storage.Reader) error {
		st := r.Status()
		out["ngram"] = st.Ngrams
		out["ngram_auth"] = st.AuthorEntries
		out["topic"] = st.Topics
		out["topic_word"] = st.TopicWords
		out["name_topic"] = st.NameTopics
		out["name"] = st.Names
		return nil
	}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	return out
}

func compareCounts(t *testing.T, stage string, counters, walked map[string]int) {
	t.Helper()
	for _, name := range []string{"ngram", "ngram_auth", "topic", "topic_word", "name_topic", "name"} {
		if counters[name] != walked[name] {
			t.Errorf("%s: %s counter says %d, a walk of the bucket finds %d",
				stage, name, counters[name], walked[name])
		}
	}
}

// exercise writes every path that touches a counted bucket, TWICE over identical keys.
//
// The second pass is the point of the fixture rather than padding. These counters track
// DISTINCT KEYS, so a repeat write must move nothing, and that asymmetry is the whole
// reason each write site had to find an existing-key signal instead of incrementing on
// every Put. An earlier version of this fixture varied its keys by two coprime moduli
// and accidentally never repeated an association pair at all, so a deliberately broken
// addAssoc that incremented unconditionally passed it. A second identical pass cannot
// have that hole, and it is obvious to a reader in a way that arithmetic is not.
func exercise(t *testing.T, s *storage.Store) {
	t.Helper()
	for pass := range 2 {
		if err := s.Update(func(w *storage.Writer) error {
			for i := range 40 {
				word := fmt.Sprintf("word%d", i%17)
				if err := w.IncTopic(word); err != nil {
					return err
				}
				if err := w.AddTopicWord(word, fmt.Sprintf("assoc%d", i%11), 0.5); err != nil {
					return err
				}
				if err := w.AddNameTopic(fmt.Sprintf("name%d", i%5), word, 0.5); err != nil {
					return err
				}
				if err := w.LearnNgram(
					fmt.Sprintf("prefix%d", i%13), word, fmt.Sprintf("author%d", i%3),
				); err != nil {
					return err
				}
				if err := w.PutName(fmt.Sprintf("name%d", i%5), corpus.Name{Count: uint64(pass*40 + i)}); err != nil {
					return err
				}
			}
			// One edge said by three different people. This is the only shape where
			// the two n-gram counters disagree: the key is new once, but the presence
			// set gains three entries, so a counter incremented in the firstSighting
			// branch instead of inside the presence branch undercounts by two. An
			// earlier fixture gave every edge a single author and missed exactly that.
			for _, author := range []string{"alice", "bob", "carol"} {
				if err := w.LearnNgram("a shared", "edge", author); err != nil {
					return err
				}
			}

			// The bot's own output passes an empty author, which writes no ngram_auth
			// key at all: the opposite direction, where the n-gram counter moves and
			// the presence counter must not.
			return w.LearnNgram("prefix0", "word0", "")
		}); err != nil {
			t.Fatalf("seed pass %d: %v", pass, err)
		}
	}
}

func TestStatusCountersMatchAWalk(t *testing.T) {
	path := dbtest.Path(t)
	s, err := storage.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	exercise(t, s)

	// Closed between each stage because walkCounts takes the flock itself, and an
	// exclusive lock held by a live store is exactly what it cannot share.
	check := func(stage string) {
		t.Helper()
		counters := statusCounts(t, s)
		if err := s.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		compareCounts(t, stage, counters, walkCounts(t, path))
		if s, err = storage.Open(path); err != nil {
			t.Fatalf("reopen: %v", err)
		}
	}
	check("after inserts")

	// Deletes are the half a naive counter gets wrong, because bbolt's Delete is a
	// silent no-op on a key that is not there.
	if err := s.Update(func(w *storage.Writer) error {
		if err := w.DeleteNgram("prefix3", "word3"); err != nil {
			return err
		}
		// Twice, plus a pair that never existed. All three must move nothing.
		if err := w.DeleteNgram("prefix3", "word3"); err != nil {
			return err
		}
		return w.DeleteNgram("prefix-that-was-never-learned", "nor-this")
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	check("after deletes")

	if err := s.Update(func(w *storage.Writer) error {
		_, err := w.PurgeAuthor("author1")
		return err
	}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	check("after a purge")

	if err := s.Close(); err != nil {
		t.Fatalf("final close: %v", err)
	}
}

// A corpus written before the counters existed has none of these keys, and Status
// would report zeroes forever without a backfill. Same shape as backfillTopicTotal:
// pay one walk on the startup after the upgrade, never again.
func TestStatusCountersBackfillAnOlderCorpus(t *testing.T) {
	path := dbtest.Path(t)
	s, err := storage.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	exercise(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Strip the counters, which is what a corpus written by any earlier binary looks
	// like. Everything else about the file stays exactly as it was.
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		for _, k := range []string{
			"count:ngram", "count:ngram_auth", "count:topic_keys",
			"count:topic_word", "count:name_topic", "count:name",
		} {
			if err := meta.Delete([]byte(k)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("strip counters: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	back, err := storage.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	counters := statusCounts(t, back)
	if err := back.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	walked := walkCounts(t, path)
	compareCounts(t, "after backfill", counters, walked)
	if walked["ngram"] == 0 {
		t.Fatal("the fixture wrote no n-grams, so this proved nothing")
	}
}

// An empty corpus must not be walked on every startup, so the backfill leaves the keys
// absent rather than storing zeroes. Status still has to read 0 rather than anything
// else, and the first insert has to create the counter from nothing.
func TestStatusCountersOnAnEmptyCorpus(t *testing.T) {
	s := dbtest.Store(t)

	for name, got := range statusCounts(t, s) {
		if got != 0 {
			t.Errorf("%s = %d on a fresh corpus, want 0", name, got)
		}
	}

	if err := s.Update(func(w *storage.Writer) error {
		return w.LearnNgram("the bird", "flew", "u1")
	}); err != nil {
		t.Fatalf("learn: %v", err)
	}
	got := statusCounts(t, s)
	if got["ngram"] != 1 || got["ngram_auth"] != 1 {
		t.Errorf("after one n-gram: ngram=%d ngram_auth=%d, want 1 and 1; "+
			"an addCounter onto an absent key has to start from zero",
			got["ngram"], got["ngram_auth"])
	}
}

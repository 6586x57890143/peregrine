package legacy

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"go.etcd.io/bbolt"
)

// TestCleanDatabaseNeedsNoConfig is the reason this test exists, more than the
// cleaning itself.
//
// cfg is a package-level variable that only Run assigns, and the -clean-db path
// never calls Run: it goes straight to CleanDatabase. So the moment anything in
// this call graph (isSpammyContent, containsSlur, or the pass itself) starts
// reading cfg, the maintenance mode nil-panics, and it does so only for the
// operator trying to clean a poisoned corpus, which is the worst audience for a
// crash. The compiler cannot see it and the bot's own startup would never hit it.
//
// cfg is deliberately left nil here rather than populated. A test that assigned a
// config would pass while the bug was present.
func TestCleanDatabaseNeedsNoConfig(t *testing.T) {
	if cfg != nil {
		t.Fatal("cfg must be nil for this test to mean anything; something in this package assigned it at init")
	}

	path := filepath.Join(t.TempDir(), "markov.db")
	seedCorpus(t, path, map[string]map[string]int{
		// A clean key with a clean continuation. Must survive untouched.
		"the bird": {"is": 3, "flew": 1},
		// A clean key whose continuations include a spammy one. The key survives,
		// the bad continuation does not.
		"look at": {"this": 2, "٩͡ل͜٩͡ل͜٩͡ل͜": 1},
		// A key that is itself spammy by the repeated-character rule (more than 20
		// consecutive identical non-space runes). The whole key goes.
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": {"anything": 1},
	})

	if err := CleanDatabase(path); err != nil {
		t.Fatalf("CleanDatabase: %v", err)
	}

	got := readCorpus(t, path)

	if _, ok := got["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]; ok {
		t.Error("a key caught by the repeated-character rule should have been deleted")
	}
	if m, ok := got["the bird"]; !ok {
		t.Error("a clean key must survive the pass")
	} else if m["is"] != 3 || m["flew"] != 1 {
		t.Errorf("clean counts must be preserved exactly, got %v", m)
	}
	if m, ok := got["look at"]; !ok {
		t.Error("a key with one bad continuation must survive; only the continuation goes")
	} else if len(m) != 1 || m["this"] != 2 {
		t.Errorf("expected only the clean continuation to remain, got %v", m)
	}
}

// TestCleanDatabaseOnALockedFileFailsFast pins the bbolt.Open timeout. Without it
// running -clean-db against a live bot blocked forever with no output at all,
// which reads as the tool hanging rather than as the database being in use.
func TestCleanDatabaseOnALockedFileFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "markov.db")
	seedCorpus(t, path, map[string]map[string]int{"a b": {"c": 1}})

	// Hold the exclusive flock, the way a running bot would.
	held, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("open to hold the lock: %v", err)
	}
	defer func() { _ = held.Close() }()

	if err := CleanDatabase(path); err == nil {
		t.Fatal("cleaning a locked corpus must fail rather than hang or silently succeed")
	}
}

func seedCorpus(t *testing.T, path string, keys map[string]map[string]int) {
	t.Helper()
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(MarkovBucket))
		if err != nil {
			return err
		}
		for k, v := range keys {
			encoded, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(k), encoded); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func readCorpus(t *testing.T, path string) map[string]map[string]int {
	t.Helper()
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db.Close() }()

	out := map[string]map[string]int{}
	if err := db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(MarkovBucket)).ForEach(func(k, v []byte) error {
			var m map[string]int
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			out[string(k)] = m
			return nil
		})
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	return out
}

package legacy

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"go.etcd.io/bbolt"
)

// dbPath resolves the corpus location. PEREGRINE_DB_PATH exists because every
// runtime path in this bot used to be relative to the working directory, which
// silently created a fresh empty corpus whenever the bot was started from
// anywhere but the repo root. It is mandatory in a container: the image runs
// with a read-only root filesystem, so the bare relative default resolves
// against the distroless working directory and bbolt.Open fails outright. In
// production this points inside the mounted volume, /data/markov.db.
func dbPath() string {
	if p := os.Getenv("PEREGRINE_DB_PATH"); p != "" {
		return p
	}
	return DBFile
}

// CleanDatabase iterates through the Markov bucket and removes spammy keys and
// values. It is the -clean-db maintenance mode and never touches Discord.
//
// It returns an error instead of calling log.Fatalf so the deferred db.Close
// runs. That mattered more here than on the startup path: this mode's whole
// purpose is to hold a write transaction over the entire corpus, and exiting
// mid-pass without closing left the flock held, so the retry that an operator
// naturally attempts next failed on the timeout rather than on the original
// problem. M6 replaces this pass wholesale, against the composite-key layout
// where the value is eight bytes and no read-modify-write is needed.
func CleanDatabase() error {
	log.Println("[CLEANUP] Opening database for cleaning...")
	// The 5s timeout matters: bbolt takes an exclusive flock, so running this
	// against a live bot used to block forever with no output. Now it fails and
	// says why.
	db, err := bbolt.Open(dbPath(), 0600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("open corpus at %s for cleaning (is the bot still running?): %w", dbPath(), err)
	}
	defer func() { _ = db.Close() }()

	log.Println("[CLEANUP] Starting database cleaning transaction...")
	start := time.Now()
	keysCleaned := 0
	valuesCleaned := 0

	err = db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(MarkovBucket))
		if b == nil {
			return fmt.Errorf("MarkovBucket not found")
		}

		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			keyStr := string(k)
			// Check the key itself for spam or slurs
			if isSpammyContent(keyStr) || containsSlur(keyStr) {
				if err := c.Delete(); err != nil {
					return fmt.Errorf("deleting key %q: %w", keyStr, err)
				}
				keysCleaned++
				continue // Move to the next key
			}

			// Check the values (next words) for spam or slurs
			var nextMap map[string]int
			if err := json.Unmarshal(v, &nextMap); err != nil {
				continue // Skip malformed data
			}

			originalValueCount := len(nextMap)
			cleanedMap := make(map[string]int)
			for word, count := range nextMap {
				if !isSpammyContent(word) && !containsSlur(word) {
					cleanedMap[word] = count
				}
			}

			// If we removed any spammy or slur words, update the record
			if len(cleanedMap) < originalValueCount {
				valuesCleaned += (originalValueCount - len(cleanedMap))
				if len(cleanedMap) == 0 {
					// If no valid next words are left, delete the whole key
					if err := c.Delete(); err != nil {
						return fmt.Errorf("deleting emptied key %q: %w", keyStr, err)
					}
				} else {
					// Otherwise, save the cleaned map. Errors here are fatal to
					// the pass rather than skipped: silently failing to write a
					// cleaned value leaves the slur in place while the summary
					// still reports it as cleaned, which is the one outcome that
					// makes this tool worse than useless.
					cleanedJSON, err := json.Marshal(cleanedMap)
					if err != nil {
						return fmt.Errorf("marshaling cleaned value for %q: %w", keyStr, err)
					}
					// b.Put on the key the cursor is currently positioned at
					// replaces a value in place. bbolt disallows structural
					// mutation during cursor iteration, and this is the one
					// mutation that is not structural. M6 replaces this whole
					// pass with the composite-key layout, where the value is
					// eight bytes and no read-modify-write is needed at all.
					if err := b.Put(k, cleanedJSON); err != nil {
						return fmt.Errorf("writing cleaned value for %q: %w", keyStr, err)
					}
				}
			}
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("cleaning pass failed after %s: %w", time.Since(start), err)
	}

	log.Printf("[CLEANUP] Finished in %s. Cleaned %d keys and %d values from the Markov bucket.", time.Since(start), keysCleaned, valuesCleaned)
	return nil
}

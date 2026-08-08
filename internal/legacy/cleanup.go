package legacy

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"go.etcd.io/bbolt"
)

// CleanDatabase iterates through the Markov bucket and removes spammy keys and
// values. It is the -clean-db maintenance mode and never touches Discord.
//
// It takes the corpus path rather than reading PEREGRINE_DB_PATH itself. There
// used to be a dbPath() helper here that resolved the environment on its own,
// which meant two code paths independently decided where the corpus lives, and
// nothing guaranteed they agreed. Cleaning a database that is not the one the bot
// uses succeeds, reports a tidy summary, and changes nothing an operator cares
// about, which is the most misleading outcome available. config.Load is now the
// single resolver and this takes its answer.
//
// It returns an error instead of calling log.Fatalf so the deferred db.Close
// runs. That mattered more here than on the startup path: this mode's whole
// purpose is to hold a write transaction over the entire corpus, and exiting
// mid-pass without closing left the flock held, so the retry that an operator
// naturally attempts next failed on the timeout rather than on the original
// problem. M6 replaces this pass wholesale, against the composite-key layout
// where the value is eight bytes and no read-modify-write is needed.
func CleanDatabase(path string) error {
	log.Printf("[CLEANUP] Opening database for cleaning: %s", path)
	// The 5s timeout matters: bbolt takes an exclusive flock, so running this
	// against a live bot used to block forever with no output. Now it fails and
	// says why.
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("open corpus at %s for cleaning (is the bot still running?): %w", path, err)
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

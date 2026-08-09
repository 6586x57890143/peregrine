// Package storage is the only package that knows a bucket exists.
//
// Everything above it works through Reader and Writer, which are bound to a single
// bbolt transaction and handed to a callback. That is not encapsulation for its own
// sake: it is what makes the worst bug in the review UNWRITABLE rather than fixed
// once.
//
// The bug (SPEC.md section 8, finding 1) was a nested transaction. Generation ran
// inside a db.View and, from there, called helpers that each opened their own
// db.View. bbolt holds mmaplock.RLock for a read transaction's entire life and takes
// the write lock to grow the mmap, and Go's RWMutex queues new readers behind a
// waiting writer. So an outer read, plus a writer waiting to remap, plus an inner
// read is a deadlock with no timeout and no recovery, and it gets likelier as the
// file grows.
//
// Nothing outside this package can reach a *bbolt.DB. A consumer holds a Reader,
// which has no method that starts a transaction, so it cannot nest one even by
// accident. That is the difference between fixing the deadlock and making it
// impossible to express.
package storage

import (
	"errors"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

// Bucket names. Unexported, so no other package can name one.
//
// The clustering package used to own three of these as exported constants, which
// legacy then aliased, pointing the dependency the wrong way round: an algorithm
// package owning storage-layer names (SPEC.md section 2). They live here now.
const (
	bucketNgram      = "ngram"       // <prefix> NUL <next>            -> count u64 | authors u32
	bucketNgramAuth  = "ngram_auth"  // <prefix> NUL <next> NUL <uid>  -> presence
	bucketKNSucc     = "kn_succ"     // <prefix>                       -> N1+(prefix .)
	bucketKNPre      = "kn_pre"      // <token> NUL <context>          -> presence
	bucketKNPreCount = "kn_pre_n"    // <token>                        -> N1+(. token)
	bucketTopic      = "topic"       // <word>                         -> count u64
	bucketTopicWord  = "topic_word"  // <word> NUL <assoc>             -> count u64 | posSum f64
	bucketNameTopic  = "name_topic"  // <name> NUL <topic>             -> count u64 | posSum f64
	bucketName       = "name"        // <name key>                     -> JSON corpus.Name
	bucketHistory    = "history"     // <snowflake be64>               -> unix nano
	bucketImage      = "image"       // <url>                          -> unix nano
	bucketStats      = "stats"       // <user id>                      -> JSON corpus.WeeklyStat
	bucketLeaderfoo  = "leaderboard" // fixed key                      -> JSON
	bucketCluster    = "cluster"     // <cluster id>                   -> JSON
	bucketCursor     = "cursor"      // <channel id>                   -> snowflake be64
	bucketMeta       = "meta"        // schema_version, counters
)

// allBuckets is the set Open creates. Listed once so adding a bucket cannot forget
// the creation step, which used to be a hand-maintained slice in main().
var allBuckets = []string{
	bucketNgram, bucketNgramAuth, bucketKNSucc, bucketKNPre, bucketKNPreCount,
	bucketTopic, bucketTopicWord, bucketNameTopic, bucketName,
	bucketHistory, bucketImage, bucketStats, bucketLeaderfoo, bucketCluster,
	bucketCursor, bucketMeta,
}

// Meta keys.
const (
	metaSchemaVersion   = "schema_version"
	metaHistoryCount    = "count:history"
	metaImageCount      = "count:image"
	metaMessagesLearned = "count:messages_learned"
	metaTopicTotal      = "count:topic_total"
)

// SchemaVersion is the on-disk layout this binary understands.
//
// Version 1 is the composite-key layout. There is no version 0 migration and there
// will not be one: the previous layout stored a JSON map of every successor as the
// value of each prefix key, so converting it means reading and rewriting the entire
// corpus, and the corpus is re-derivable from Discord history anyway. Open refuses a
// corpus it does not recognize rather than silently reading garbage.
const SchemaVersion = 1

// ErrSchemaMismatch is returned by Open when the file on disk was written by a
// different layout.
var ErrSchemaMismatch = errors.New("corpus schema version mismatch")

// Store owns the database handle. It is the only thing that does.
type Store struct {
	db *bbolt.DB
}

// Open opens or creates the corpus, verifies its schema version, and ensures every
// bucket exists.
//
// The five-second timeout is not a nicety. bbolt takes an exclusive flock, so
// opening a corpus a live bot already holds used to block forever with no output at
// all, which reads as the tool hanging rather than as the file being in use.
func Open(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open corpus at %s (is another process holding it?): %w", path, err)
	}

	s := &Store{db: db}
	if err := s.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) initialize() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("create bucket %s: %w", name, err)
			}
		}

		meta := tx.Bucket([]byte(bucketMeta))
		stored := meta.Get([]byte(metaSchemaVersion))
		switch {
		case stored == nil:
			// Either a brand new file or one written before versioning existed. Tell
			// those apart by looking for data: an empty corpus is new and gets
			// stamped, a populated one is the old layout and is refused.
			if populated(tx) {
				return fmt.Errorf("%w: this corpus holds data but carries no schema version, "+
					"so it was written by the pre-M6 layout. That layout stored a JSON successor "+
					"map per key and is not readable by this binary. Start fresh: stop the bot, "+
					"remove the corpus file (or `docker volume rm peregrine_corpus`), and let it "+
					"relearn from Discord history", ErrSchemaMismatch)
			}
			if err := meta.Put([]byte(metaSchemaVersion), encodeUint64(SchemaVersion)); err != nil {
				return err
			}

		case decodeUint64(stored) == SchemaVersion:

		default:
			return fmt.Errorf("%w: corpus is version %d, this binary speaks version %d",
				ErrSchemaMismatch, decodeUint64(stored), SchemaVersion)
		}

		return backfillTopicTotal(tx)
	})
}

// backfillTopicTotal derives count:topic_total once for a corpus written before that
// counter existed, which is every schema-1 corpus created before M7a.
//
// The counter is the denominator of the raw-count half of the Kneser-Ney base case
// (PEREGRINE_KN_RAW_MIX), so a zero here does not fail loudly: it silently turns the
// raw mix off and makes generation behave as though mu were 0, which is a change in
// output register with nothing in the log to explain it. Deriving it costs one cursor
// scan over the topic bucket, one key per distinct word, on the startup after an
// upgrade and never again.
//
// The condition is "counter absent, bucket populated" rather than "counter is zero",
// because a genuinely empty corpus must not be walked on every single startup.
func backfillTopicTotal(tx *bbolt.Tx) error {
	meta := tx.Bucket([]byte(bucketMeta))
	if meta.Get([]byte(metaTopicTotal)) != nil {
		return nil
	}

	topics := tx.Bucket([]byte(bucketTopic))
	if topics == nil {
		return nil
	}

	c := topics.Cursor()
	k, v := c.First()
	if k == nil {
		// A new corpus. Leave the key absent so the next startup does not walk an
		// empty bucket either; the first IncTopic creates it.
		return nil
	}

	var total uint64
	for ; k != nil; k, v = c.Next() {
		total += decodeUint64(v)
	}
	return meta.Put([]byte(metaTopicTotal), encodeUint64(total))
}

// populated reports whether any bucket holds a key, which is how a new file is told
// apart from a pre-versioning one.
func populated(tx *bbolt.Tx) bool {
	for _, name := range allBuckets {
		if name == bucketMeta {
			continue
		}
		b := tx.Bucket([]byte(name))
		if b == nil {
			continue
		}
		if k, _ := b.Cursor().First(); k != nil {
			return true
		}
	}
	// The pre-M6 layout used different bucket names, so check those too rather than
	// declaring an old corpus empty and stamping it as current.
	for _, legacyName := range []string{"markov", "topics", "names", "TopicWordBucket"} {
		if b := tx.Bucket([]byte(legacyName)); b != nil {
			if k, _ := b.Cursor().First(); k != nil {
				return true
			}
		}
	}
	return false
}

// Close releases the flock.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the file the store was opened from, for log lines.
func (s *Store) Path() string { return s.db.Path() }

// View runs fn inside a read transaction.
//
// fn receives a Reader, which cannot open a transaction. That is the entire point:
// the nested-transaction deadlock in finding 1 is not reachable from here, because
// there is no method on Reader that could start one.
func (s *Store) View(fn func(*Reader) error) error {
	return s.db.View(func(tx *bbolt.Tx) error {
		return fn(&Reader{tx: tx})
	})
}

// Update runs fn inside a write transaction.
//
// One writer at a time, process-wide, which is a property of bbolt rather than a
// choice here. Anything slow inside fn blocks all ingestion, so an O(n^2) loop in
// here is a correctness-adjacent problem and not merely slow.
func (s *Store) Update(fn func(*Writer) error) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		w := &Writer{Reader: Reader{tx: tx}}
		return fn(w)
	})
}

// Backup writes a consistent snapshot to path.
//
// In-process via tx.WriteTo, and that is the only correct way to back up bbolt. An
// external `cp markov.db` is NOT a backup: the file is a single mmap updated by
// copy-on-write pages plus a meta-page flip at commit, so a byte copy can capture a
// state between the page write and the flip, or mid-remap. The result usually
// APPEARS to work, which is the worst property a backup can have. A sidecar cannot
// do it either, because of the exclusive flock.
//
// Taken inside a read transaction, so it is consistent by construction and does not
// block writers. M13 puts it on a ticker with retention.
func (s *Store) Backup(path string) error {
	f, err := createFile(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	if err := s.db.View(func(tx *bbolt.Tx) error {
		_, err := tx.WriteTo(f)
		return err
	}); err != nil {
		return fmt.Errorf("snapshot corpus to %s: %w", path, err)
	}
	return f.Sync()
}

// Compact copies the corpus into a fresh file at path, reclaiming free pages.
//
// It exists because bbolt's file NEVER SHRINKS. Deleting keys frees pages for reuse
// but does not return them to the filesystem, so a corpus that grew to 128 MB
// through the old layout's write amplification stays 128 MB after -clean-db removes
// most of it. This is the only way to get the space back.
func (s *Store) Compact(path string) error {
	dst, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("create compacted corpus at %s: %w", path, err)
	}
	defer func() { _ = dst.Close() }()

	if err := bbolt.Compact(dst, s.db, 0); err != nil {
		return fmt.Errorf("compact into %s: %w", path, err)
	}
	return nil
}

// Reader reads. It holds a transaction and cannot start one.
type Reader struct {
	tx *bbolt.Tx
}

// Writer reads and writes. It embeds Reader so a write path can read without a
// second transaction, which is the other half of why nesting is unnecessary here.
type Writer struct {
	Reader
}

func (r *Reader) bucket(name string) *bbolt.Bucket {
	return r.tx.Bucket([]byte(name))
}

// counter reads a meta counter.
//
// Counters exist so the trims do not call Bucket.Stats(). Stats() walks every page
// in the bucket, and the old trim loops called it in their LOOP CONDITION, making
// eviction quadratic in pages: trimming a thousand keys from a large bucket walked
// the whole bucket a thousand times (SPEC.md section 8, finding 11). A counter
// maintained in the same transaction as the insert cannot drift, because bbolt
// transactions are atomic.
func (r *Reader) counter(key string) uint64 {
	b := r.bucket(bucketMeta)
	if b == nil {
		return 0
	}
	return decodeUint64(b.Get([]byte(key)))
}

func (w *Writer) setCounter(key string, n uint64) error {
	return w.bucket(bucketMeta).Put([]byte(key), encodeUint64(n))
}

func (w *Writer) addCounter(key string, delta int64) error {
	cur := w.counter(key)
	next := uint64(int64(cur) + delta)
	if delta < 0 && uint64(-delta) > cur {
		next = 0
	}
	return w.setCounter(key, next)
}

// presentMarker is the value written into a presence set.
//
// A single byte rather than a zero-length value, and this was a real bug rather than
// a style choice. bbolt's Bucket.Get returns nil both for a key that does not exist
// and for a key stored with an empty value, so `Get(k) == nil` could not tell "not
// present" from "present with no value". Every presence check therefore reported
// absent, every time, and the counts that depend on them were wrong in the same
// direction: 500 repetitions by one author reported 500 distinct authors, and the
// Kneser-Ney predecessor count counted occurrences instead of distinct contexts.
//
// The three tests that caught it are the ones worth keeping in mind here: repetition
// by one author must not raise diversity, KN stats must count distinct things, and a
// purge must decrement by one per continuation.
var presentMarker = []byte{1}

// putPresence records a key in a presence set. Named so the intent is visible at the
// call site: the key is the datum, the value only has to be distinguishable from
// absent.
func putPresence(b *bbolt.Bucket, key []byte) error {
	return b.Put(key, presentMarker)
}

// isPresent reports whether a presence key exists. Uses length rather than a nil
// comparison for the reason above.
func isPresent(b *bbolt.Bucket, key []byte) bool {
	return len(b.Get(key)) > 0
}

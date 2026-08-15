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
	bucketNgram      = "ngram"         // <prefix> NUL <next>            -> count u64 | authors u32
	bucketNgramAuth  = "ngram_auth"    // <prefix> NUL <next> NUL <uid>  -> presence
	bucketKNSucc     = "kn_succ"       // <prefix>                       -> N1+(prefix .)
	bucketKNPre      = "kn_pre"        // <token> NUL <context>          -> presence
	bucketKNPreCount = "kn_pre_n"      // <token>                        -> N1+(. token)
	bucketTopic      = "topic"         // <word>                         -> count u64
	bucketTopicWord  = "topic_word"    // <word> NUL <assoc>             -> count u64 | posSum f64
	bucketNameTopic  = "name_topic"    // <name> NUL <topic>             -> count u64 | posSum f64
	bucketName       = "name"          // <name key>                     -> JSON corpus.Name
	bucketHistory    = "history"       // <snowflake be64>               -> unix nano
	bucketImage      = "image"         // <msgID be64> NUL <url>         -> author be64
	bucketStats      = "stats"         // <user id>                      -> JSON corpus.WeeklyStat
	bucketLeaderfoo  = "leaderboard"   // fixed key                      -> JSON
	bucketCursor     = "cursor"        // <channel id>                   -> snowflake be64
	bucketRepairCurs = "repair_cursor" // <job> NUL <channel id>        -> snowflake be64
	bucketLearnGen   = "learn_gen"     // <generation be64>              -> unix nano
	bucketMeta       = "meta"          // schema_version, counters
)

// allBuckets is the set Open creates. Listed once so adding a bucket cannot forget
// the creation step, which used to be a hand-maintained slice in main().
//
// "cluster" was here until M13, a milestone after clustering was deleted: Open kept creating it
// and Reader.Status kept counting it, so a structure nothing could write still had a row in the
// status line. An existing corpus keeps the empty bucket, which costs one page and is not worth a
// migration; a new one never gets it. That is the shape worth noticing rather than the bucket:
// deleting a feature is not finished until the layout stops making room for it.
var allBuckets = []string{
	bucketNgram, bucketNgramAuth, bucketKNSucc, bucketKNPre, bucketKNPreCount,
	bucketTopic, bucketTopicWord, bucketNameTopic, bucketName,
	bucketHistory, bucketImage, bucketStats, bucketLeaderfoo,
	bucketCursor, bucketRepairCurs, bucketLearnGen, bucketMeta,
}

// Meta keys.
const (
	metaSchemaVersion   = "schema_version"
	metaHistoryCount    = "count:history"
	metaImageCount      = "count:image"
	metaMessagesLearned = "count:messages_learned"
	metaTopicTotal      = "count:topic_total"

	// metaRepairPrefix names one repair job's completion marker, as
	// "repair:<job name>". Per job rather than one shared key, because two repairs finish
	// at different times and a shared marker would let one declare the other done.
	//
	// See finding 46: the association indexes are the one thing in this layout that cannot
	// be rebuilt from the corpus, because associations need original word sequences with
	// positions and the corpus stores n-grams and counts. Any future index built from
	// message STRUCTURE rather than message content has the same property, which is why
	// this is a prefix and not a single key.
	metaRepairPrefix = "repair:"
)

// RepairState is how far one repair job has got.
//
// A NAMED TYPE rather than a bare string, because the M17 version took any string and wrote
// it raw: a typo became a state no reader recognized, and the job then silently never
// completed. An invalid value should not be spellable.
type RepairState string

const (
	RepairPending RepairState = ""
	RepairRunning RepairState = "running"
	RepairDone    RepairState = "done"
)

// Valid reports whether a state is one this package writes.
func (s RepairState) Valid() bool {
	return s == RepairPending || s == RepairRunning || s == RepairDone
}

// SchemaVersion is the on-disk layout this binary understands.
//
// Version 1 is the composite-key layout. There is no version 0 migration and there
// will not be one: the previous layout stored a JSON map of every successor as the
// value of each prefix key, so converting it means reading and rewriting the entire
// corpus, and the corpus is re-derivable from Discord history anyway. Open refuses a
// corpus it does not recognize rather than silently reading garbage.
//
// Version 2 rekeys the image cache from <url> to <message snowflake> NUL <url> so
// that reposting can be attributed, capped per author, and revoked when the source
// message is deleted (SPEC.md section 4, A7).
//
// Version 2 DOES have a migration, and the asymmetry with version 1 is the point
// rather than an inconsistency. The corpus is expensive to lose and expensive to
// convert, so it is refused. The image cache is a hundred-entry cache of URLs that
// refills itself within minutes of live traffic, so upgradeToV2 simply empties it.
// The rule this sets is: migrate what is cheap to discard, refuse what is not.
const SchemaVersion = 2

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

// OpenReadOnly opens a corpus for inspection without writing a single byte to it.
//
// # Why Open cannot be used instead
//
// Open calls initialize, which creates every missing bucket, stamps a schema version
// onto a file that has none, and can run upgradeToV2, which EMPTIES THE IMAGE CACHE.
// Every one of those is correct for a bot starting up and wrong for a report: a mode
// that mutates the corpus it is measuring has changed the thing it was asked to
// describe, and the image-cache migration would do it destructively.
//
// bbolt's ReadOnly takes a SHARED flock, so this deliberately cannot run against a
// corpus a live bot is holding: that fails on the same five-second timeout with the
// same message, which is the honest outcome. The intended input is a snapshot, and
// internal/plugins/backup already produces consistent ones via tx.WriteTo. An external
// `cp markov.db` is still not a backup and is still not a valid input here, for the
// reasons on Store.Backup.
//
// The schema check is a READ of the same key initialize writes, and it refuses the same
// two cases with the same explanations, because a reader that tolerates a layout the
// writer refuses would walk a pre-M6 corpus and print confident nonsense about it. The
// version 1 case is refused rather than migrated: migrating is a write.
func OpenReadOnly(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open corpus read-only at %s (is another process holding it?): %w", path, err)
	}

	s := &Store{db: db}
	if err := s.checkSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// checkSchema is initialize's version check with every write removed.
func (s *Store) checkSchema() error {
	return s.db.View(func(tx *bbolt.Tx) error {
		meta := tx.Bucket([]byte(bucketMeta))
		if meta == nil {
			// No meta bucket at all. On a writable open this would simply be created; here
			// it means the file was written by a layout that had no meta bucket, which is
			// the pre-M6 one.
			return fmt.Errorf("%w: this corpus has no meta bucket, so it was written by the "+
				"pre-M6 layout, which stored a JSON successor map per key and is not readable "+
				"by this binary", ErrSchemaMismatch)
		}

		stored := meta.Get([]byte(metaSchemaVersion))
		switch {
		case stored == nil:
			if populated(tx) {
				return fmt.Errorf("%w: this corpus holds data but carries no schema version, "+
					"so it was written by the pre-M6 layout and is not readable by this binary",
					ErrSchemaMismatch)
			}
			// An empty, unstamped file. Nothing to report on, and stamping it would be a
			// write, so say so rather than printing a page of zeroes.
			return fmt.Errorf("%w: this corpus is empty and unstamped, so there is nothing "+
				"to report on", ErrSchemaMismatch)

		case decodeUint64(stored) == SchemaVersion:
			return nil

		default:
			// Includes version 1, which Open migrates and this cannot: the migration is a
			// write. Compacting or copying it through a normal Open first is the answer.
			return fmt.Errorf("%w: corpus is version %d, this binary speaks version %d. A "+
				"read-only open cannot migrate it, because migrating is a write",
				ErrSchemaMismatch, decodeUint64(stored), SchemaVersion)
		}
	})
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
			// NO SPECIAL CASE FOR A FRESH CORPUS ANY MORE, and its deletion is the point
			// of the learn-generation stamp rather than a regression.
			//
			// M17 stamped a new file as "association backfill already done" right here, so a
			// fresh deploy would not walk all of Discord looking for messages that cannot
			// exist. That worked and it was a second mechanism answering a question the
			// first one now answers: cmd/bot records the current learn generation on the
			// first open, so a repair bounded at "when generation N began" gets a boundary
			// equal to this file's own birth and finds nothing. One mechanism, no special
			// case, and it generalizes to the next repair instead of only this one.

		case decodeUint64(stored) == SchemaVersion:

		case decodeUint64(stored) == 1:
			if err := upgradeToV2(tx); err != nil {
				return err
			}
			if err := meta.Put([]byte(metaSchemaVersion), encodeUint64(SchemaVersion)); err != nil {
				return err
			}

		default:
			return fmt.Errorf("%w: corpus is version %d, this binary speaks version %d",
				ErrSchemaMismatch, decodeUint64(stored), SchemaVersion)
		}

		return backfillTopicTotal(tx)
	})
}

// upgradeToV2 empties the image cache, which is the whole of the version 1 to 2
// migration.
//
// Version 2 rekeys that bucket from <url> to <message snowflake> NUL <url>, and a
// version 1 entry read under the new codec would fail splitImageKey and be skipped
// forever while still counting against the cache size. Rather than translate them,
// which is impossible anyway because the old layout recorded neither the message nor
// the author, the cache is emptied: it is a hundred URLs that refill themselves from
// live traffic within minutes.
//
// The counter is reset in the same transaction as the delete, so the two cannot
// disagree about how full the cache is. That mattering is the reason the trims are
// counter-driven rather than Stats()-driven in the first place (finding 11).
func upgradeToV2(tx *bbolt.Tx) error {
	// The bucket is known to exist: initialize creates every bucket in allBuckets
	// before it looks at the version, so there is no not-found case to tolerate here.
	if err := tx.DeleteBucket([]byte(bucketImage)); err != nil {
		return fmt.Errorf("upgrade to v2, drop image cache: %w", err)
	}
	if _, err := tx.CreateBucket([]byte(bucketImage)); err != nil {
		return fmt.Errorf("upgrade to v2, recreate image cache: %w", err)
	}
	if err := tx.Bucket([]byte(bucketMeta)).Delete([]byte(metaImageCount)); err != nil {
		return fmt.Errorf("upgrade to v2, reset image counter: %w", err)
	}
	return nil
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

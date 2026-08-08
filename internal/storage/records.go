package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/6586x57890143/peregrine/internal/corpus"
)

// ---------------------------------------------------------------- history

// Seen reports whether a message has already been learned from.
//
// The dedup window. It exists because the backfill re-reads recent history, so
// without it every pass would re-learn the same messages and double-count their
// n-grams.
func (r *Reader) Seen(messageID string) (bool, error) {
	key, err := encodeSnowflake(messageID)
	if err != nil {
		return false, err
	}
	b := r.bucket(bucketHistory)
	if b == nil {
		return false, nil
	}
	return b.Get(key) != nil, nil
}

// MarkSeen records a message as learned from, at the given time, and trims the
// window to max entries.
//
// The VALUE is a timestamp and nothing else. It used to be the message text
// verbatim, which meant the operator's database held a durable copy of ten thousand
// users' messages that no code ever read: every reader tested only whether the key
// existed (SPEC.md section 8, finding 18).
func (w *Writer) MarkSeen(messageID string, at time.Time, max int) error {
	key, err := encodeSnowflake(messageID)
	if err != nil {
		return err
	}
	b := w.bucket(bucketHistory)
	if b.Get(key) != nil {
		return nil // already counted; do not double the counter
	}
	if err := b.Put(key, encodeTime(at.UnixNano())); err != nil {
		return err
	}
	if err := w.addCounter(metaHistoryCount, 1); err != nil {
		return err
	}
	return w.trimHistory(max)
}

// trimHistory evicts the oldest entries until the window fits.
//
// Two findings meet here. The eviction order is now correct because the keys are
// fixed-width big-endian snowflakes, so a cursor's First() is genuinely the oldest
// message; storing them as decimal strings meant a 17-digit ID sorted before an
// 18-digit one and eviction was effectively arbitrary (finding 10). And the loop
// bound is a counter rather than Bucket.Stats(), which walks every page in the
// bucket and was previously called in the loop CONDITION, making eviction quadratic
// in pages (finding 11).
func (w *Writer) trimHistory(max int) error {
	if max <= 0 {
		return nil
	}
	b := w.bucket(bucketHistory)
	count := w.counter(metaHistoryCount)
	removed := int64(0)

	for count > uint64(max) {
		c := b.Cursor()
		k, _ := c.First()
		if k == nil {
			break
		}
		if err := b.Delete(bytes.Clone(k)); err != nil {
			return err
		}
		count--
		removed++
	}
	if removed > 0 {
		return w.addCounter(metaHistoryCount, -removed)
	}
	return nil
}

// HistoryCount reports the dedup window size, for the status line.
func (r *Reader) HistoryCount() uint64 { return r.counter(metaHistoryCount) }

// OldestSeen returns the timestamp of the oldest entry in the dedup window, which
// tells an operator how far back the window actually reaches. Zero if empty.
func (r *Reader) OldestSeen() time.Time {
	b := r.bucket(bucketHistory)
	if b == nil {
		return time.Time{}
	}
	_, v := b.Cursor().First()
	if v == nil {
		return time.Time{}
	}
	return time.Unix(0, decodeTime(v))
}

// ---------------------------------------------------------------- topics

// TopicCount returns how often a word has been seen as a topic. This is where
// unigram frequency lives: one key per word, which is where it always belonged
// rather than in a single map-valued n-gram key.
func (r *Reader) TopicCount(word string) uint64 {
	b := r.bucket(bucketTopic)
	if b == nil {
		return 0
	}
	return decodeUint64(b.Get([]byte(word)))
}

// IncTopic increments a word's topic count.
//
// No minimum length. The old version silently dropped words shorter than three
// characters, so the topic count for "ok", "no" and "wtf" was permanently zero and
// the significance term scored them log(1) = 0 (SPEC.md section 8, G10). In a server
// whose register is short interjections, that excluded much of the vocabulary from
// topic gravity for no stated reason.
func (w *Writer) IncTopic(word string) error {
	if word == "" {
		return nil
	}
	b := w.bucket(bucketTopic)
	return b.Put([]byte(word), encodeUint64(decodeUint64(b.Get([]byte(word)))+1))
}

// TopicWord returns a word-to-association co-occurrence record.
func (r *Reader) TopicWord(word, assoc string) (corpus.TopicAssoc, error) {
	return r.assoc(bucketTopicWord, word, assoc)
}

// AddTopicWord accumulates a co-occurrence with its relative position.
func (w *Writer) AddTopicWord(word, assoc string, position float64) error {
	return w.addAssoc(bucketTopicWord, word, assoc, position)
}

// TopicWordsFor returns every association recorded for a word.
func (r *Reader) TopicWordsFor(word string) (map[string]corpus.TopicAssoc, error) {
	return r.assocsFor(bucketTopicWord, word)
}

// NameTopic returns a name-to-topic co-occurrence record.
func (r *Reader) NameTopic(name, topic string) (corpus.TopicAssoc, error) {
	return r.assoc(bucketNameTopic, name, topic)
}

// AddNameTopic accumulates a name-to-topic co-occurrence.
func (w *Writer) AddNameTopic(name, topic string, position float64) error {
	return w.addAssoc(bucketNameTopic, name, topic, position)
}

// NameTopicsFor returns every topic recorded against a name.
func (r *Reader) NameTopicsFor(name string) (map[string]corpus.TopicAssoc, error) {
	return r.assocsFor(bucketNameTopic, name)
}

// assoc, addAssoc and assocsFor are shared by both association buckets, which have
// identical shape. They used to be two copies of a read-modify-write over a JSON
// map keyed by the outer term, with the same write-amplification problem as the
// n-gram bucket: adding one co-occurrence rewrote every association that term had.
func (r *Reader) assoc(bucket, a, b string) (corpus.TopicAssoc, error) {
	bkt := r.bucket(bucket)
	if bkt == nil {
		return corpus.TopicAssoc{}, nil
	}
	key, err := pairKey(a, b)
	if err != nil {
		return corpus.TopicAssoc{}, err
	}
	count, posSum := decodeAssoc(bkt.Get(key))
	return corpus.TopicAssoc{Count: count, PosSum: posSum}, nil
}

func (w *Writer) addAssoc(bucket, a, b string, position float64) error {
	bkt := w.bucket(bucket)
	key, err := pairKey(a, b)
	if err != nil {
		return err
	}
	count, posSum := decodeAssoc(bkt.Get(key))
	return bkt.Put(key, encodeAssoc(count+1, posSum+position))
}

func (r *Reader) assocsFor(bucket, a string) (map[string]corpus.TopicAssoc, error) {
	bkt := r.bucket(bucket)
	if bkt == nil {
		return nil, nil
	}
	seek, limit, err := ngramPrefixRange(a)
	if err != nil {
		return nil, err
	}

	out := map[string]corpus.TopicAssoc{}
	c := bkt.Cursor()
	for k, v := c.Seek(seek); k != nil && bytes.Compare(k, limit) < 0; k, v = c.Next() {
		name, ok := splitNgramKey(k, len(a))
		if !ok || name == "" {
			continue
		}
		count, posSum := decodeAssoc(v)
		out[name] = corpus.TopicAssoc{Count: count, PosSum: posSum}
	}
	return out, nil
}

// ---------------------------------------------------------------- names

// Name returns a learned name record.
//
// Still JSON, unlike everything above, and deliberately so. This bucket holds one
// entry per distinct display name in the server, so it is thousands of keys rather
// than millions, it is written once per new alias rather than once per token, and
// its shape is likely to gain fields. Fixed-width encoding buys nothing here and
// costs flexibility. The rule is that the hot path gets bytes and the cold path gets
// JSON.
func (r *Reader) Name(key string) (corpus.Name, bool, error) {
	b := r.bucket(bucketName)
	if b == nil {
		return corpus.Name{}, false, nil
	}
	v := b.Get([]byte(key))
	if v == nil {
		return corpus.Name{}, false, nil
	}
	var n corpus.Name
	if err := json.Unmarshal(v, &n); err != nil {
		return corpus.Name{}, false, fmt.Errorf("decode name %q: %w", key, err)
	}
	return n, true, nil
}

// PutName writes a name record.
func (w *Writer) PutName(key string, n corpus.Name) error {
	data, err := json.Marshal(n)
	if err != nil {
		return err
	}
	return w.bucket(bucketName).Put([]byte(key), data)
}

// ForEachName visits every name record.
func (r *Reader) ForEachName(fn func(key string, n corpus.Name) error) error {
	b := r.bucket(bucketName)
	if b == nil {
		return nil
	}
	return b.ForEach(func(k, v []byte) error {
		var n corpus.Name
		if err := json.Unmarshal(v, &n); err != nil {
			// One malformed record must not stop the walk: this feeds name
			// recognition, and skipping one alias is better than failing generation.
			return nil
		}
		return fn(string(k), n)
	})
}

// ---------------------------------------------------------------- images

// AddImageURL caches a URL for later reposting and trims the cache to max.
//
// Keyed by URL with a timestamp value, so a duplicate does not grow the cache and
// eviction is by insertion order via the counter.
func (w *Writer) AddImageURL(url string, at time.Time, max int) error {
	b := w.bucket(bucketImage)
	if b.Get([]byte(url)) != nil {
		return nil
	}
	if err := b.Put([]byte(url), encodeTime(at.UnixNano())); err != nil {
		return err
	}
	if err := w.addCounter(metaImageCount, 1); err != nil {
		return err
	}

	count := w.counter(metaImageCount)
	removed := int64(0)
	for count > uint64(max) && max > 0 {
		// Evict the oldest by timestamp rather than the lexicographically first URL.
		// Cursor order here is URL order, which has nothing to do with age, so this
		// finds the minimum explicitly.
		var oldestKey []byte
		var oldest int64
		if err := b.ForEach(func(k, v []byte) error {
			ts := decodeTime(v)
			if oldestKey == nil || ts < oldest {
				oldestKey = bytes.Clone(k)
				oldest = ts
			}
			return nil
		}); err != nil {
			return err
		}
		if oldestKey == nil {
			break
		}
		if err := b.Delete(oldestKey); err != nil {
			return err
		}
		count--
		removed++
	}
	if removed > 0 {
		return w.addCounter(metaImageCount, -removed)
	}
	return nil
}

// ImageURLs returns every cached URL.
func (r *Reader) ImageURLs() ([]string, error) {
	b := r.bucket(bucketImage)
	if b == nil {
		return nil, nil
	}
	var out []string
	if err := b.ForEach(func(k, _ []byte) error {
		out = append(out, string(k))
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteImageURL removes one cached URL, for when the source message was deleted.
func (w *Writer) DeleteImageURL(url string) error {
	b := w.bucket(bucketImage)
	if b.Get([]byte(url)) == nil {
		return nil
	}
	if err := b.Delete([]byte(url)); err != nil {
		return err
	}
	return w.addCounter(metaImageCount, -1)
}

// ---------------------------------------------------------------- stats

// UserStat returns a user's weekly message count.
func (r *Reader) UserStat(userID string) (corpus.WeeklyStat, bool, error) {
	b := r.bucket(bucketStats)
	if b == nil {
		return corpus.WeeklyStat{}, false, nil
	}
	v := b.Get([]byte(userID))
	if v == nil {
		return corpus.WeeklyStat{}, false, nil
	}
	var s corpus.WeeklyStat
	if err := json.Unmarshal(v, &s); err != nil {
		return corpus.WeeklyStat{}, false, err
	}
	return s, true, nil
}

// IncUserStat bumps a user's weekly counter.
func (w *Writer) IncUserStat(userID string, at time.Time) error {
	cur, _, err := w.UserStat(userID)
	if err != nil {
		return err
	}
	cur.Count++
	cur.LastTimestamp = at
	data, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	return w.bucket(bucketStats).Put([]byte(userID), data)
}

// AllUserStats returns every user's counter.
func (r *Reader) AllUserStats() (map[string]corpus.WeeklyStat, error) {
	b := r.bucket(bucketStats)
	if b == nil {
		return nil, nil
	}
	out := map[string]corpus.WeeklyStat{}
	if err := b.ForEach(func(k, v []byte) error {
		var s corpus.WeeklyStat
		if err := json.Unmarshal(v, &s); err != nil {
			return nil
		}
		out[string(k)] = s
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------- opaque blobs

// GetBlob and PutBlob carry the values whose shape belongs to their owner rather
// than to storage: the leaderboard, the aggro state, and the concept clusters.
//
// Deliberately opaque. Storage should not need a type definition for every piece of
// state some feature persists, and the alternative was clustering exporting its
// bucket names so another package could reach in, which pointed the dependency the
// wrong way (SPEC.md section 2).
func (r *Reader) GetBlob(kind, key string) ([]byte, error) {
	bucket, err := blobBucket(kind)
	if err != nil {
		return nil, err
	}
	b := r.bucket(bucket)
	if b == nil {
		return nil, nil
	}
	v := b.Get([]byte(key))
	if v == nil {
		return nil, nil
	}
	return bytes.Clone(v), nil
}

// PutBlob stores an opaque value.
func (w *Writer) PutBlob(kind, key string, value []byte) error {
	bucket, err := blobBucket(kind)
	if err != nil {
		return err
	}
	return w.bucket(bucket).Put([]byte(key), value)
}

// ForEachBlob visits every value of a kind.
func (r *Reader) ForEachBlob(kind string, fn func(key string, value []byte) error) error {
	bucket, err := blobBucket(kind)
	if err != nil {
		return err
	}
	b := r.bucket(bucket)
	if b == nil {
		return nil
	}
	return b.ForEach(func(k, v []byte) error { return fn(string(k), v) })
}

// ReplaceBlobs atomically swaps every value of a kind for a new set.
//
// Exists for the clustering pass, which produces a whole new set of clusters each
// run. It does a delete-and-recreate of the bucket, which is a full destructive
// rebuild, and that is recorded as a defect rather than endorsed: M8 makes clustering
// diff-based so this becomes unnecessary for its main caller (SPEC.md section 8,
// finding 15).
func (w *Writer) ReplaceBlobs(kind string, values map[string][]byte) error {
	bucket, err := blobBucket(kind)
	if err != nil {
		return err
	}
	if w.tx.Bucket([]byte(bucket)) != nil {
		if err := w.tx.DeleteBucket([]byte(bucket)); err != nil {
			return err
		}
	}
	b, err := w.tx.CreateBucket([]byte(bucket))
	if err != nil {
		return err
	}
	for k, v := range values {
		if err := b.Put([]byte(k), v); err != nil {
			return err
		}
	}
	return nil
}

// Blob kinds. Named constants rather than free strings so a typo is a compile error
// at the call site instead of a silent read from a bucket that does not exist.
const (
	BlobConfig      = "config"
	BlobLeaderboard = "leaderboard"
	BlobCluster     = "cluster"
)

func blobBucket(kind string) (string, error) {
	switch kind {
	case BlobConfig:
		return bucketMeta, nil
	case BlobLeaderboard:
		return bucketLeaderfoo, nil
	case BlobCluster:
		return bucketCluster, nil
	default:
		return "", fmt.Errorf("unknown blob kind %q", kind)
	}
}

// ---------------------------------------------------------------- status

// Status is the per-bucket key count for the status line.
type Status struct {
	Ngrams        int
	AuthorEntries int
	Topics        int
	TopicWords    int
	NameTopics    int
	Names         int
	Clusters      int
	HistoryWindow uint64
	ImageCache    uint64
}

// Status collects the counts.
//
// Bucket.Stats() walks every page, so this is genuinely expensive and is called on a
// ticker rather than per message. It used to be called once per message purely to
// fill a log field (SPEC.md section 8, finding 11).
func (r *Reader) Status() Status {
	keyN := func(name string) int {
		b := r.bucket(name)
		if b == nil {
			return 0
		}
		return b.Stats().KeyN
	}
	return Status{
		Ngrams:        keyN(bucketNgram),
		AuthorEntries: keyN(bucketNgramAuth),
		Topics:        keyN(bucketTopic),
		TopicWords:    keyN(bucketTopicWord),
		NameTopics:    keyN(bucketNameTopic),
		Names:         keyN(bucketName),
		Clusters:      keyN(bucketCluster),
		HistoryWindow: r.counter(metaHistoryCount),
		ImageCache:    r.counter(metaImageCount),
	}
}

// createFile is here rather than in storage.go so that file is free of os.
func createFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	return f, nil
}

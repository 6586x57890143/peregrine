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
	if err := b.Put([]byte(word), encodeUint64(decodeUint64(b.Get([]byte(word)))+1)); err != nil {
		return err
	}
	return w.addCounter(metaTopicTotal, 1)
}

// TotalTopicCount returns the sum of every topic count, which is total unigram
// occurrences and therefore the denominator of raw unigram probability.
//
// Kneser-Ney's base case needs it for the raw half of PEREGRINE_KN_RAW_MIX, and it
// is a counter for the same reason every other total here is: the alternative is a
// cursor scan over the whole vocabulary, and the base case is evaluated for every
// candidate at every step of every generated sentence.
func (r *Reader) TotalTopicCount() uint64 { return r.counter(metaTopicTotal) }

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

// IsName reports whether a key is a known name or alias, without decoding the
// record.
//
// Separate from Name because of where it is called: the scorer applies a small
// recognized-name boost to every candidate at every step of every generated sentence,
// and it only needs the yes or no. Going through Name would put a JSON unmarshal in
// that innermost loop, where the old code did a bare key lookup.
func (r *Reader) IsName(key string) bool {
	b := r.bucket(bucketName)
	if b == nil {
		return false
	}
	return b.Get([]byte(key)) != nil
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

// AddImageURL caches a URL for later reposting, attributed to the message and the
// author it came from, and enforces both caps.
//
// The caps are enforced HERE rather than at the caller, and that placement is the
// same argument as CheckLearn living inside learnMessage: image reposting is the
// bot republishing user-supplied media under its own name (SPEC.md section 4, A7),
// so "one author may not own more than their share of the cache" has to be a
// property of the store rather than a rule the current caller happens to follow. A
// second caller cannot skip it.
//
// At the per-author cap the author's OWN OLDEST entry is evicted rather than the new
// URL dropped. Dropping would freeze a prolific poster's contribution at whatever
// they happened to post first; evicting keeps their share current without letting it
// grow. Either way the cap holds, which is the part that matters.
//
// One bounded pass answers both questions the caps need (is this URL already cached,
// and how many entries does this author hold) plus finds the eviction candidate. The
// bucket is bounded by maxTotal, which is PEREGRINE_IMAGE_CACHE_SIZE and defaults to
// 100, with 8-byte values: this is the same trade as Reader.PrefixTotal, a bounded
// sequential scan in preference to a counter that would need maintaining per author
// on every insert.
func (w *Writer) AddImageURL(url, msgID, authorID string, maxTotal, maxPerAuthor int) error {
	key, err := imageKey(msgID, url)
	if err != nil {
		return err
	}
	author, err := encodeSnowflake(authorID)
	if err != nil {
		return fmt.Errorf("image author: %w", err)
	}

	b := w.bucket(bucketImage)

	var (
		authorCount   int
		authorsOldest []byte
	)
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		cached, ok := splitImageKey(k)
		if !ok {
			continue
		}
		if cached == url {
			// Already cached, from this message or another. Keep the existing
			// attribution: re-attributing would let a second poster of the same URL
			// spend someone else's quota, and it would reset the entry's age, which
			// is what decides when it leaves.
			return nil
		}
		if bytes.Equal(v, author) {
			authorCount++
			if authorsOldest == nil {
				// Keys are ordered by snowflake, so the first match in cursor order
				// is this author's oldest entry. No timestamp comparison needed.
				authorsOldest = bytes.Clone(k)
			}
		}
	}

	evicted := int64(0)
	if maxPerAuthor > 0 && authorCount >= maxPerAuthor && authorsOldest != nil {
		if err := b.Delete(authorsOldest); err != nil {
			return err
		}
		evicted++
	}

	if err := b.Put(key, author); err != nil {
		return err
	}
	if err := w.addCounter(metaImageCount, 1-evicted); err != nil {
		return err
	}

	return w.trimImageCache(maxTotal)
}

// trimImageCache evicts oldest-first down to max.
//
// Oldest-first is free here because the key leads with the source message's
// snowflake, so cursor order is chronological order. The old layout keyed by URL and
// searched the whole bucket for the minimum timestamp on every single eviction.
func (w *Writer) trimImageCache(max int) error {
	if max <= 0 {
		return nil
	}
	count := w.counter(metaImageCount)
	if count <= uint64(max) {
		return nil
	}

	drop := int(count - uint64(max))
	victims := make([][]byte, 0, drop)
	c := w.bucket(bucketImage).Cursor()
	for k, _ := c.First(); k != nil && len(victims) < drop; k, _ = c.Next() {
		victims = append(victims, bytes.Clone(k))
	}

	// Collected before deleting rather than deleted during the walk, because bbolt
	// does not define cursor behaviour across a mutation of the bucket being walked.
	b := w.bucket(bucketImage)
	for _, k := range victims {
		if err := b.Delete(k); err != nil {
			return err
		}
	}
	return w.addCounter(metaImageCount, -int64(len(victims)))
}

// ImageURLs returns every cached URL, oldest first.
func (r *Reader) ImageURLs() ([]string, error) {
	b := r.bucket(bucketImage)
	if b == nil {
		return nil, nil
	}
	var out []string
	if err := b.ForEach(func(k, _ []byte) error {
		if url, ok := splitImageKey(k); ok {
			out = append(out, url)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteImagesByMessage removes every URL cached from one message and reports how
// many went.
//
// This is SPEC.md section 4.2's deleted-message rule: a deletion is a strong signal
// that the content should not be republished, and the bot reposting something a
// moderator or its author has just removed is the exact failure A7 describes. The
// rule was specified a milestone ago and could not be implemented, because with the
// URL as the key there was no way to ask which entries a message had contributed.
//
// A message ID that is not a snowflake is an error rather than a silent no-op: the
// only way to get one here is a caller that invented it.
func (w *Writer) DeleteImagesByMessage(msgID string) (int, error) {
	seek, limit, err := imageMessageRange(msgID)
	if err != nil {
		return 0, err
	}

	b := w.bucket(bucketImage)
	var victims [][]byte
	c := b.Cursor()
	for k, _ := c.Seek(seek); k != nil && bytes.Compare(k, limit) < 0; k, _ = c.Next() {
		victims = append(victims, bytes.Clone(k))
	}
	if len(victims) == 0 {
		return 0, nil
	}
	for _, k := range victims {
		if err := b.Delete(k); err != nil {
			return 0, err
		}
	}
	if err := w.addCounter(metaImageCount, -int64(len(victims))); err != nil {
		return 0, err
	}
	return len(victims), nil
}

// ImageAuthorCount reports how many cached entries an author holds. Exported for the
// cap's test, which otherwise could only observe the cap through its effect on the
// URL list and could not tell a working cap from a full cache.
func (r *Reader) ImageAuthorCount(authorID string) int {
	author, err := encodeSnowflake(authorID)
	if err != nil {
		return 0
	}
	b := r.bucket(bucketImage)
	if b == nil {
		return 0
	}
	n := 0
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if bytes.Equal(v, author) {
			n++
		}
	}
	return n
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

// PutUserStat writes a user's counter outright, rather than incrementing it.
//
// It exists because the weekly reset is a POLICY decision and does not belong here.
// The rule is "if this user's last message predates the start of the current week,
// their count starts again at one", which needs a definition of when a week starts,
// and storage has no business holding one. The caller reads, decides, and writes.
func (w *Writer) PutUserStat(userID string, s corpus.WeeklyStat) error {
	data, err := json.Marshal(s)
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

// MessagesLearned returns the lifetime count of messages ingested.
//
// A meta counter rather than a key in the stats bucket, which is where it used to
// live under the literal key "total_messages_learned". That put a scalar in a bucket
// whose every other key is a Discord user ID holding a JSON WeeklyStat, so every
// reader of that bucket had to recognize and skip it: the leaderboard filtered it out
// with a strconv.ParseInt on the key, and anything that forgot to would have decoded
// an integer as a stat and silently counted a phantom user.
func (r *Reader) MessagesLearned() uint64 { return r.counter(metaMessagesLearned) }

// IncMessagesLearned bumps the lifetime ingestion counter.
func (w *Writer) IncMessagesLearned() error { return w.addCounter(metaMessagesLearned, 1) }

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

// ForEachBlob and ReplaceBlobs are gone as of M13, along with the BlobCluster kind and the
// bucket behind it.
//
// Both existed for one caller: the clustering pass, which rebuilt its whole bucket every 24 hours
// with a DeleteBucket plus CreateBucket, and which M7b deleted for the reasons in finding 29.
// ReplaceBlobs was the atomic version of that destructive rebuild, so it had no other possible
// user. The linter reporting them unreachable is how that was confirmed rather than assumed, which
// is the same way M5's and M6b's leftover wrappers were retired.

// Blob kinds. Named constants rather than free strings so a typo is a compile error
// at the call site instead of a silent read from a bucket that does not exist.
const (
	BlobConfig      = "config"
	BlobLeaderboard = "leaderboard"
)

// BlobCluster is gone as of M13, along with the bucket behind it. Clustering was deleted in M7b
// (finding 29) and this kind outlived it by six milestones, which is the same shape as the bucket
// itself: a deletion is not finished while the layout still has a name for the thing.
func blobBucket(kind string) (string, error) {
	switch kind {
	case BlobConfig:
		return bucketMeta, nil
	case BlobLeaderboard:
		return bucketLeaderfoo, nil
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
	HistoryWindow uint64
	ImageCache    uint64
	Learned       uint64
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
		HistoryWindow: r.counter(metaHistoryCount),
		ImageCache:    r.counter(metaImageCount),
		Learned:       r.counter(metaMessagesLearned),
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

// ---------------------------------------------------------------- ingest cursors

// Cursor returns the ID of the newest message ingested from a channel, or "" if the
// channel has never been read.
//
// This is the high-water mark that replaces re-reading a time window. The old ingest
// loop always re-scanned the trailing PEREGRINE_INGEST_LOOKBACK, relying on the history
// bucket to recognise what it had already learned, and the history bucket is capped at
// PEREGRINE_MAX_HISTORY entries. On a busy guild the older half of that window had
// already been evicted from history by the time the next pass came round, so those
// messages were learned AGAIN and their n-grams counted twice (SPEC.md section 8,
// finding 13). A cursor makes the question "what is new" instead of "what have I
// forgotten", and the answer does not depend on how much the bot remembers.
//
// Stored as a fixed-width big-endian snowflake for the same reason history keys are:
// Discord IDs are integers whose high bits are a timestamp, so byte order is
// chronological order, and comparing them as decimal strings makes a 17-digit ID sort
// before an 18-digit one.
func (r *Reader) Cursor(channelID string) string {
	b := r.bucket(bucketCursor)
	if b == nil {
		return ""
	}
	v := b.Get([]byte(channelID))
	if len(v) == 0 {
		return ""
	}
	return decodeSnowflake(v)
}

// SetCursor advances a channel's high-water mark.
//
// It refuses to move BACKWARDS, and that is the property worth having rather than a
// courtesy. Two things can hand this an older ID: a paging loop that processes a batch
// out of order, and two ingest passes overlapping on the same channel. Either would
// rewind the mark and cause the next pass to re-read and re-learn everything between,
// which is finding 13 arriving by a different route. A monotonic cursor cannot do that
// however confused its caller is.
func (w *Writer) SetCursor(channelID, messageID string) error {
	if channelID == "" {
		return fmt.Errorf("refusing to store a cursor for an empty channel ID")
	}
	next, err := encodeSnowflake(messageID)
	if err != nil {
		return fmt.Errorf("cursor for channel %s: %w", channelID, err)
	}

	b := w.bucket(bucketCursor)
	if current := b.Get([]byte(channelID)); len(current) > 0 && bytes.Compare(next, current) <= 0 {
		return nil
	}
	return b.Put([]byte(channelID), next)
}

// ForgetCursor drops a channel's mark, so the next pass treats it as never read.
//
// For an operator who needs one channel re-ingested without discarding the corpus, and
// for tests. Deliberately per channel rather than a wholesale reset: "re-read
// everything" is a decision with a cost, since re-learning a message the history bucket
// has since evicted double-counts it.
func (w *Writer) ForgetCursor(channelID string) error {
	return w.bucket(bucketCursor).Delete([]byte(channelID))
}

// ForEachCursor visits every stored cursor, for the status line and for maintenance.
func (r *Reader) ForEachCursor(fn func(channelID, messageID string) error) error {
	b := r.bucket(bucketCursor)
	if b == nil {
		return nil
	}
	return b.ForEach(func(k, v []byte) error {
		if len(v) == 0 {
			return nil
		}
		return fn(string(k), decodeSnowflake(v))
	})
}

// AssocCursor and SetAssocCursor are the association re-walk's own high-water marks.
//
// A SEPARATE BUCKET from the ingest cursor, and that separation is the whole reason the
// re-walk is safe to run at all. The two passes read the same channels for different
// reasons and at different speeds: ingest is asking "what is new" and must never rewind,
// while the re-walk is asking "what is old" and finishes. Sharing one mark would mean
// either pass moving the other's, and moving the ingest mark backwards is finding 13.
func (r *Reader) AssocCursor(channelID string) string {
	b := r.bucket(bucketAssocCursor)
	if b == nil {
		return ""
	}
	v := b.Get([]byte(channelID))
	if len(v) == 0 {
		return ""
	}
	return decodeSnowflake(v)
}

// SetAssocCursor advances the re-walk's mark, refusing to move backwards for the same
// reason SetCursor does: a batch processed out of order would otherwise cause the same
// messages to have their associations counted twice.
func (w *Writer) SetAssocCursor(channelID, messageID string) error {
	if channelID == "" {
		return fmt.Errorf("refusing to store an association cursor for an empty channel ID")
	}
	next, err := encodeSnowflake(messageID)
	if err != nil {
		return fmt.Errorf("association cursor for channel %s: %w", channelID, err)
	}

	b := w.bucket(bucketAssocCursor)
	if current := b.Get([]byte(channelID)); len(current) > 0 && bytes.Compare(next, current) <= 0 {
		return nil
	}
	return b.Put([]byte(channelID), next)
}

// AssocBackfillState reports whether the association re-walk has run. See finding 46.
func (r *Reader) AssocBackfillState() string {
	b := r.bucket(bucketMeta)
	if b == nil {
		return AssocBackfillPending
	}
	return string(b.Get([]byte(metaAssocBackfill)))
}

// SetAssocBackfillState records progress through the re-walk.
//
// Persisted rather than held in memory because the walk spans hours and restarts, and the
// marker is what stops a completed pass from running again on every boot.
func (w *Writer) SetAssocBackfillState(state string) error {
	return w.bucket(bucketMeta).Put([]byte(metaAssocBackfill), []byte(state))
}

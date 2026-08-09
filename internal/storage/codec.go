package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// sep separates the components of a composite key.
//
// NUL is safe because of what the tokenizer can produce: URLs, mentions, channel
// references, emotes, shortcodes, and runs of letters, digits, symbols and
// apostrophes. None of those can contain a NUL byte. The codec asserts it anyway
// rather than trusting that, because writing an ambiguous key is silent corruption:
// the key would still store and retrieve, just under a prefix that is not the one
// the caller meant.
//
// It also sorts below space, which is the property the range scans depend on.
// Successors("the") seeks "the\x00" and stops at "the\x01", and because no
// printable character sorts below NUL, that range cannot wander into "the cat\x00"
// keys. With a space or a pipe as the separator it could.
const sep = 0x00

// ErrNulInToken is returned when a token contains the separator byte. It is a
// programming error or a tokenizer change, never user input reaching this far, so
// callers should treat it as a bug rather than filtering it.
var ErrNulInToken = errors.New("token contains a NUL byte, which is the composite key separator")

// The value widths. Fixed-width big-endian throughout, for three reasons: the
// values are the same size for every key so bbolt never has to move a value on
// update, big-endian byte order equals numeric order so a cursor scan is a sorted
// scan, and there is no allocation or parse on read.
//
// What this replaces is the actual cost problem in the old layout. There, the key
// was the prefix alone and the value was a JSON map[string]int of every token that
// had ever followed it, so learning one occurrence of "the cat" meant reading,
// unmarshalling, mutating, marshalling and rewriting the entire successor map of
// "the". On a common prefix that map has thousands of entries, and it was rewritten
// once per occurrence (SPEC.md section 3.1).
const (
	countWidth   = 8 // uint64 occurrence count
	authorWidth  = 4 // uint32 distinct-author count
	ngramValue   = countWidth + authorWidth
	assocWidth   = countWidth + 8 // uint64 count + float64 position sum
	timestampLen = 8              // int64 unix nanoseconds
	snowflakeLen = 8              // uint64 Discord ID
)

// ngramKey builds <prefix> NUL <next>.
//
// prefix is the joined n-gram context, already space-separated and lowercased by
// the caller. It is not re-tokenized here: storage does not get to decide what a
// token is.
func ngramKey(prefix, next string) ([]byte, error) {
	if err := assertNoNul(prefix, next); err != nil {
		return nil, err
	}
	key := make([]byte, 0, len(prefix)+1+len(next))
	key = append(key, prefix...)
	key = append(key, sep)
	key = append(key, next...)
	return key, nil
}

// ngramPrefixRange returns the seek target and the exclusive upper bound for every
// successor of prefix.
//
// The upper bound is prefix followed by 0x01 rather than an incremented last byte,
// which is what makes this correct for prefixes that are extensions of each other.
// Everything under "the\x00..." sorts before "the\x01", and "the cat\x00..." sorts
// after it because space (0x20) is above 0x01. So the scan sees exactly the
// successors of "the" and none of "the cat".
func ngramPrefixRange(prefix string) (seek, limit []byte, err error) {
	if err := assertNoNul(prefix); err != nil {
		return nil, nil, err
	}
	seek = append([]byte(prefix), sep)
	limit = append([]byte(prefix), sep+1)
	return seek, limit, nil
}

// splitNgramKey recovers the successor token from a composite key. The prefix is
// known by the caller doing the scan, so only the tail is returned.
func splitNgramKey(key []byte, prefixLen int) (string, bool) {
	if len(key) < prefixLen+1 || key[prefixLen] != sep {
		return "", false
	}
	return string(key[prefixLen+1:]), true
}

// authorKey builds <prefix> NUL <next> NUL <authorID>, a presence entry.
//
// A presence SET rather than a count, because a distinct-author count cannot be
// maintained without knowing whether this particular author was already counted for
// this particular continuation. The value is zero bytes, which bbolt stores without
// a page of its own, so the cost is the key.
//
// This is the same shape as the Kneser-Ney predecessor index below, deliberately:
// two different questions ("how many distinct people said this" and "how many
// distinct contexts precede this") have the same answer shape, so they share their
// implementation and their tests.
func authorKey(prefix, next, authorID string) ([]byte, error) {
	if err := assertNoNul(prefix, next, authorID); err != nil {
		return nil, err
	}
	key := make([]byte, 0, len(prefix)+len(next)+len(authorID)+2)
	key = append(key, prefix...)
	key = append(key, sep)
	key = append(key, next...)
	key = append(key, sep)
	key = append(key, authorID...)
	return key, nil
}

// imageKey builds <message snowflake be64> NUL <url>.
//
// The message ID leads, and that ordering is the whole design of this bucket rather
// than a preference. It buys three things that the previous key, the bare URL, could
// not:
//
//   - Eviction order. A snowflake's high bits are a millisecond timestamp, so byte
//     order is numeric order is chronological order and a cursor's First() IS the
//     oldest cached image. The old layout stored a timestamp in the value and hunted
//     for the minimum with a full bucket scan INSIDE the eviction loop, which made
//     trimming k entries cost O(n*k). Same shape as finding 11, one bucket over.
//   - Deletion by message, which is what SPEC.md section 4.2's deleted-message rule
//     needs: a message being deleted is a strong signal that the bot must not
//     republish what was in it. With the URL as the key there was no way to find the
//     entries a message had contributed, which is why Writer.DeleteImageURL sat here
//     for a milestone with a comment about deleted messages and no caller.
//   - Attribution, in the value, which is what makes a per-author cap possible.
//
// The separator sits at a FIXED offset because the ID is fixed-width, so a NUL byte
// inside the snowflake itself cannot make the key ambiguous. That is not true of the
// variable-width composite keys above, which is why they assert instead.
func imageKey(msgID, url string) ([]byte, error) {
	id, err := encodeSnowflake(msgID)
	if err != nil {
		return nil, err
	}
	if err := assertNoNul(url); err != nil {
		return nil, err
	}
	key := make([]byte, 0, snowflakeLen+1+len(url))
	key = append(key, id...)
	key = append(key, sep)
	key = append(key, url...)
	return key, nil
}

// imageMessageRange returns the seek target and exclusive upper bound covering every
// entry cached from one message.
func imageMessageRange(msgID string) (seek, limit []byte, err error) {
	id, err := encodeSnowflake(msgID)
	if err != nil {
		return nil, nil, err
	}
	seek = append(append([]byte(nil), id...), sep)
	limit = append(append([]byte(nil), id...), sep+1)
	return seek, limit, nil
}

// splitImageKey recovers the URL from an image key.
//
// A key that is too short or missing its separator returns false rather than a
// truncated URL, because the caller's next move is to hand the result to Discord as
// something to post. Half a URL is worse than no URL.
func splitImageKey(key []byte) (string, bool) {
	if len(key) < snowflakeLen+1 || key[snowflakeLen] != sep {
		return "", false
	}
	return string(key[snowflakeLen+1:]), true
}

// pairKey builds <a> NUL <b>, used for the KN predecessor set and the two
// association buckets.
func pairKey(a, b string) ([]byte, error) {
	if err := assertNoNul(a, b); err != nil {
		return nil, err
	}
	key := make([]byte, 0, len(a)+1+len(b))
	key = append(key, a...)
	key = append(key, sep)
	key = append(key, b...)
	return key, nil
}

// encodeNgram packs a count and a distinct-author count into a fixed 12 bytes.
func encodeNgram(count uint64, authors uint32) []byte {
	buf := make([]byte, ngramValue)
	binary.BigEndian.PutUint64(buf[:countWidth], count)
	binary.BigEndian.PutUint32(buf[countWidth:], authors)
	return buf
}

// decodeNgram unpacks a stored n-gram value.
//
// A short or absent value decodes as zero rather than an error. That is deliberate
// for one case only: a value written by an older layout. Open refuses to run
// against a corpus whose schema_version it does not recognize, so in practice this
// is defensive rather than load-bearing, and returning zero means a corrupt entry
// degrades to "never generated" instead of taking down a read.
func decodeNgram(v []byte) (count uint64, authors uint32) {
	if len(v) >= countWidth {
		count = binary.BigEndian.Uint64(v[:countWidth])
	}
	if len(v) >= ngramValue {
		authors = binary.BigEndian.Uint32(v[countWidth:ngramValue])
	}
	return count, authors
}

// encodeUint64 and decodeUint64 carry the standalone counters: the KN distinct
// counts and the topic frequencies.
func encodeUint64(n uint64) []byte {
	buf := make([]byte, countWidth)
	binary.BigEndian.PutUint64(buf, n)
	return buf
}

func decodeUint64(v []byte) uint64 {
	if len(v) < countWidth {
		return 0
	}
	return binary.BigEndian.Uint64(v[:countWidth])
}

// encodeAssoc and decodeAssoc carry a count plus a position sum in 16 bytes.
//
// The float is stored as its IEEE-754 bit pattern rather than a formatted string,
// so it round-trips exactly. A position sum accumulated over hundreds of thousands
// of observations and rewritten as text each time would drift.
func encodeAssoc(count uint64, posSum float64) []byte {
	buf := make([]byte, assocWidth)
	binary.BigEndian.PutUint64(buf[:countWidth], count)
	binary.BigEndian.PutUint64(buf[countWidth:], math.Float64bits(posSum))
	return buf
}

func decodeAssoc(v []byte) (count uint64, posSum float64) {
	if len(v) >= countWidth {
		count = binary.BigEndian.Uint64(v[:countWidth])
	}
	if len(v) >= assocWidth {
		posSum = math.Float64frombits(binary.BigEndian.Uint64(v[countWidth:assocWidth]))
	}
	return count, posSum
}

// encodeSnowflake turns a Discord ID into a FIXED-WIDTH big-endian 8 bytes, and
// this is the whole fix for one of the review findings.
//
// Discord IDs are snowflakes: 64-bit integers whose high bits are a millisecond
// timestamp, so a numerically larger ID is always a chronologically later message.
// The old code stored them as decimal STRINGS, and bbolt orders keys as bytes, so
// "9999999999999999" (16 digits) sorted before "10000000000000000" (17 digits).
// History eviction removed the lexicographically smallest key, which meant it
// evicted a 17-digit ID before an 18-digit one regardless of age, so the dedup
// window was not the last N messages at all (SPEC.md section 8, finding 10).
//
// Fixed-width big-endian makes byte order equal numeric order equal chronological
// order, so a cursor's First() IS the oldest message and eviction is correct by
// construction rather than by a comparison function someone has to remember.
func encodeSnowflake(id string) ([]byte, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(id), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("message ID %q is not a snowflake: %w", id, err)
	}
	buf := make([]byte, snowflakeLen)
	binary.BigEndian.PutUint64(buf, n)
	return buf, nil
}

// decodeSnowflake reverses encodeSnowflake.
//
// It only exists because ingest cursors are the first thing to READ a snowflake back
// out: history keys are only ever tested for existence, so nothing needed the reverse
// direction until now. A short or empty value returns "" rather than a bogus ID, since
// the caller's next move is to treat "" as "never read", which is the safe answer for a
// value that cannot be understood.
func decodeSnowflake(v []byte) string {
	if len(v) < snowflakeLen {
		return ""
	}
	return strconv.FormatUint(binary.BigEndian.Uint64(v), 10)
}

// encodeTime and decodeTime store an instant as unix nanoseconds.
//
// The history bucket stores one of these as its VALUE and nothing else. It used to
// store the message text verbatim, giving the operator a durable copy of ten
// thousand users' messages that no code ever read: every reader only tested whether
// the key existed (SPEC.md section 8, finding 18). A timestamp answers the one
// question anyone might actually ask of this bucket, at eight bytes instead of a
// message.
func encodeTime(unixNano int64) []byte {
	buf := make([]byte, timestampLen)
	binary.BigEndian.PutUint64(buf, uint64(unixNano))
	return buf
}

func decodeTime(v []byte) int64 {
	if len(v) < timestampLen {
		return 0
	}
	return int64(binary.BigEndian.Uint64(v[:timestampLen]))
}

func assertNoNul(parts ...string) error {
	for _, p := range parts {
		if strings.IndexByte(p, sep) >= 0 {
			return fmt.Errorf("%w: %q", ErrNulInToken, p)
		}
	}
	return nil
}

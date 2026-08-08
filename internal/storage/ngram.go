package storage

import (
	"bytes"
	"fmt"

	"github.com/6586x57890143/peregrine/internal/corpus"
)

// Successors returns every continuation of prefix.
//
// One cursor scan over a contiguous key range, which is what the composite key
// layout buys. The old layout read a single key whose value was a JSON map of every
// successor, so this operation was one Get plus unmarshalling a map that could hold
// thousands of entries; now it is a range scan that stops as soon as the prefix
// changes, and the caller can stop early.
//
// Returns an empty slice rather than nil for an unknown prefix, so callers do not
// need to distinguish "no successors" from "not found". Generation treats both the
// same way: back off to a shorter prefix.
func (r *Reader) Successors(prefix string) ([]corpus.Successor, error) {
	b := r.bucket(bucketNgram)
	if b == nil {
		return nil, nil
	}
	seek, limit, err := ngramPrefixRange(prefix)
	if err != nil {
		return nil, err
	}

	var out []corpus.Successor
	c := b.Cursor()
	for k, v := c.Seek(seek); k != nil && bytes.Compare(k, limit) < 0; k, v = c.Next() {
		token, ok := splitNgramKey(k, len(prefix))
		if !ok || token == "" {
			continue
		}
		count, authors := decodeNgram(v)
		out = append(out, corpus.Successor{Token: token, Count: count, Authors: authors})
	}
	return out, nil
}

// HasSuccessors reports whether a prefix has any continuation at all.
//
// One cursor Seek and one comparison, no allocation of the successor list. It
// exists because the seed selector asks this question dozens of times per reply,
// once per candidate n-gram from the prompt, and it only needs the yes or no.
//
// Under the old layout this was `markovB.Get([]byte(prefix)) != nil`, which worked
// because the prefix WAS a key. It no longer is: keys are <prefix> NUL <next>, so
// the equivalent question is whether the prefix's key range is non-empty.
func (r *Reader) HasSuccessors(prefix string) bool {
	if prefix == "" {
		return false
	}
	b := r.bucket(bucketNgram)
	if b == nil {
		return false
	}
	seek, limit, err := ngramPrefixRange(prefix)
	if err != nil {
		return false
	}
	k, _ := b.Cursor().Seek(seek)
	return k != nil && bytes.Compare(k, limit) < 0
}

// CorpusEmpty reports whether anything has been learned yet.
//
// Deliberately NOT NgramCount() == 0. NgramCount goes through Bucket.Stats(),
// which walks every page in the largest bucket in the database, and the caller
// asking this question is the reply path: it runs on every message the bot answers,
// purely to decide whether to give up early. That is the shape of finding 11, where
// a page walk sat on a per-message path to fill a log field.
func (r *Reader) CorpusEmpty() bool {
	b := r.bucket(bucketNgram)
	if b == nil {
		return true
	}
	k, _ := b.Cursor().First()
	return k == nil
}

// FirstPrefix returns the lowest-sorting prefix in the corpus.
//
// The absolute fallback for seed selection: when nothing in the prompt or the
// recent context matches anything learned, generation still needs somewhere to
// start, and any real prefix beats a sentinel that has no continuations.
func (r *Reader) FirstPrefix() (string, bool) {
	b := r.bucket(bucketNgram)
	if b == nil {
		return "", false
	}
	k, _ := b.Cursor().First()
	if k == nil {
		return "", false
	}
	i := bytes.IndexByte(k, sep)
	if i <= 0 {
		return "", false
	}
	return string(k[:i]), true
}

// Successor returns one continuation, and whether it exists.
func (r *Reader) Successor(prefix, next string) (corpus.Successor, bool, error) {
	b := r.bucket(bucketNgram)
	if b == nil {
		return corpus.Successor{}, false, nil
	}
	key, err := ngramKey(prefix, next)
	if err != nil {
		return corpus.Successor{}, false, err
	}
	v := b.Get(key)
	if v == nil {
		return corpus.Successor{}, false, nil
	}
	count, authors := decodeNgram(v)
	return corpus.Successor{Token: next, Count: count, Authors: authors}, true, nil
}

// KNStats returns the two distinct counts interpolated Kneser-Ney needs.
//
// Both are read rather than computed. Counting distinct successors with a cursor is
// O(successors), and this is called for every candidate at every step of every
// generated sentence, so deriving it would put a range scan inside the innermost
// loop of the hot path.
func (r *Reader) KNStats(prefix, token string) (corpus.KNStats, error) {
	var out corpus.KNStats
	if b := r.bucket(bucketKNSucc); b != nil {
		out.DistinctSuccessors = decodeUint64(b.Get([]byte(prefix)))
	}
	if b := r.bucket(bucketKNPreCount); b != nil {
		out.DistinctPredecessors = decodeUint64(b.Get([]byte(token)))
	}
	return out, nil
}

// TotalDistinctPredecessors returns the sum over all tokens of N1+(. token), which
// is the denominator of the Kneser-Ney unigram distribution.
//
// Maintained as a counter for the same reason as everything else here: computing it
// means walking the whole predecessor-count bucket, and the unigram term is
// evaluated at every backoff step.
func (r *Reader) TotalDistinctPredecessors() uint64 {
	return r.counter("count:kn_pre_total")
}

// LearnNgram records one occurrence of prefix -> next by authorID, maintaining every
// index that depends on it.
//
// This is the write that the whole layout exists to make cheap, and it is worth
// being explicit about what it costs, because Kneser-Ney is not free (SPEC.md
// section 3.2). Per n-gram per message:
//
//	1 read + 1 write   the n-gram count itself
//	1 read + 1 write   the author presence key, only when the author is new to it
//	0 or 1 write       kn_succ, only when the successor is new to this prefix
//	1 read + 1 write   kn_pre presence, only when the context is new to this token
//	0 or 1 write       kn_pre_n and its total, only alongside a new kn_pre entry
//
// The conditionals matter: on a corpus that has seen anything, most n-grams are
// repeats, so the steady-state cost is close to one read and one write. The
// first-sighting cost is higher and that is the correct place to pay it.
//
// authorID is the Discord user ID, or empty for the bot's own output. Empty is
// deliberately NOT counted toward author diversity: self-learning feeds the bot's
// own text back in, and if that counted, anything the bot said once would bootstrap
// itself into eligibility (SPEC.md section 4, A6).
func (w *Writer) LearnNgram(prefix, next, authorID string) error {
	if prefix == "" {
		// An empty prefix is the finding-5 bug in a single condition. The old
		// ingestion loop ran n from MaxNGram down to 1, and at n == 1 the prefix
		// slice was empty, so every unigram in the corpus accumulated into ONE key
		// whose value was a map of the entire vocabulary, unmarshalled and
		// re-marshalled once per word per message. Nothing ever read it: every
		// reader built a prefix of at least one word. It was pure write
		// amplification and the dominant reason the file reached 128 MB.
		//
		// Refused here rather than only avoided by the caller, so the bug cannot
		// return through a new caller. Unigram frequency lives in the topic bucket,
		// one key per word, which is where it always belonged.
		return fmt.Errorf("refusing to write an empty n-gram prefix: unigram frequency belongs in the topic bucket (SPEC.md section 8, finding 5)")
	}

	ngramB := w.bucket(bucketNgram)
	key, err := ngramKey(prefix, next)
	if err != nil {
		return err
	}

	existing := ngramB.Get(key)
	count, authors := decodeNgram(existing)
	firstSighting := existing == nil

	// Author diversity. Only a genuinely new (prefix, next, author) triple moves the
	// count, which is what makes repetition by one person worthless.
	if authorID != "" {
		aKey, err := authorKey(prefix, next, authorID)
		if err != nil {
			return err
		}
		authB := w.bucket(bucketNgramAuth)
		if !isPresent(authB, aKey) {
			if err := putPresence(authB, aKey); err != nil {
				return err
			}
			authors++
		}
	}

	if err := ngramB.Put(key, encodeNgram(count+1, authors)); err != nil {
		return err
	}

	// kn_succ: N1+(prefix .), incremented only when this successor is new to this
	// prefix. That is exactly what firstSighting tells us.
	if firstSighting {
		succB := w.bucket(bucketKNSucc)
		if err := succB.Put([]byte(prefix), encodeUint64(decodeUint64(succB.Get([]byte(prefix)))+1)); err != nil {
			return err
		}
	}

	// kn_pre: the presence set whose only purpose is to make kn_pre_n correct. A
	// distinct-predecessor count cannot be maintained without knowing whether this
	// context was already seen for this token.
	preKey, err := pairKey(next, prefix)
	if err != nil {
		return err
	}
	preB := w.bucket(bucketKNPre)
	if !isPresent(preB, preKey) {
		if err := putPresence(preB, preKey); err != nil {
			return err
		}
		preCountB := w.bucket(bucketKNPreCount)
		if err := preCountB.Put([]byte(next), encodeUint64(decodeUint64(preCountB.Get([]byte(next)))+1)); err != nil {
			return err
		}
		if err := w.addCounter("count:kn_pre_total", 1); err != nil {
			return err
		}
	}

	return nil
}

// PurgeAuthor removes one author's contribution to author-diversity counts.
//
// This is the surgical remedy for poisoning: it lets an operator undo one bad
// actor without discarding a corpus the whole server contributed to. The
// alternative, which was the only option before, is deleting the database.
//
// What it does NOT do is remove that author's occurrence counts, and the reason is
// that the counts do not record who produced them. Storing per-author counts would
// mean a key per (prefix, next, author) with a value, which is the presence set plus
// a count on every entry, and the presence set is already the fastest-growing
// structure in the database. So this reduces the author diversity of everything the
// author touched, which is what the eligibility gate reads, and leaves frequency
// alone. In practice that is the effective half: a phrase only one person ever said
// drops to zero distinct authors and becomes ineligible to generate.
//
// Returns how many diversity entries were removed.
func (w *Writer) PurgeAuthor(authorID string) (int, error) {
	if authorID == "" {
		return 0, fmt.Errorf("refusing to purge an empty author ID: that is the marker for the bot's own output")
	}

	authB := w.bucket(bucketNgramAuth)
	ngramB := w.bucket(bucketNgram)
	suffix := append([]byte{sep}, authorID...)

	// Collect first, delete after. bbolt forbids structural mutation of a bucket
	// while a cursor over it is live, and Delete during iteration is only safe for
	// the key the cursor sits on. Two passes is the honest way.
	var victims [][]byte
	if err := authB.ForEach(func(k, _ []byte) error {
		if bytes.HasSuffix(k, suffix) {
			victims = append(victims, bytes.Clone(k))
		}
		return nil
	}); err != nil {
		return 0, err
	}

	removed := 0
	for _, k := range victims {
		// The n-gram key is the author key with the trailing NUL and author ID cut.
		ngKey := k[:len(k)-len(suffix)]
		if v := ngramB.Get(ngKey); v != nil {
			count, authors := decodeNgram(v)
			if authors > 0 {
				authors--
			}
			if err := ngramB.Put(ngKey, encodeNgram(count, authors)); err != nil {
				return removed, err
			}
		}
		if err := authB.Delete(k); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// DeleteNgram removes a continuation and every index entry that depends on it.
//
// Used by the maintenance pass. It deliberately does not try to decrement
// kn_pre_n: doing that correctly requires knowing whether any OTHER prefix still
// precedes the token, which is a scan of the predecessor set for that token. The
// pass that calls this rebuilds the KN indexes afterwards instead, which is both
// cheaper and provably consistent.
func (w *Writer) DeleteNgram(prefix, next string) error {
	key, err := ngramKey(prefix, next)
	if err != nil {
		return err
	}
	if err := w.bucket(bucketNgram).Delete(key); err != nil {
		return err
	}

	// Drop this continuation's author presence entries.
	authPrefix := append(key, sep)
	authB := w.bucket(bucketNgramAuth)
	var victims [][]byte
	c := authB.Cursor()
	for k, _ := c.Seek(authPrefix); k != nil && bytes.HasPrefix(k, authPrefix); k, _ = c.Next() {
		victims = append(victims, bytes.Clone(k))
	}
	for _, k := range victims {
		if err := authB.Delete(k); err != nil {
			return err
		}
	}

	preKey, err := pairKey(next, prefix)
	if err != nil {
		return err
	}
	return w.bucket(bucketKNPre).Delete(preKey)
}

// RebuildKNIndexes recomputes kn_succ, kn_pre_n and the predecessor total from the
// n-gram bucket.
//
// Needed after a bulk delete, because maintaining the distinct counts incrementally
// is correct for inserts and expensive for deletes: decrementing N1+(. token)
// requires knowing whether any other context still precedes that token. Rebuilding
// is O(n-grams) once, rather than O(predecessors) per deleted key.
//
// It runs inside the caller's write transaction, which means it blocks all
// ingestion for its duration. That is acceptable for a maintenance mode and would
// not be on the hot path.
func (w *Writer) RebuildKNIndexes() error {
	succ := map[string]uint64{}
	pre := map[string]uint64{}

	if err := w.bucket(bucketNgram).ForEach(func(k, _ []byte) error {
		i := bytes.LastIndexByte(k, sep)
		if i <= 0 {
			return nil
		}
		prefix := string(k[:i])
		next := string(k[i+1:])
		succ[prefix]++
		pre[next]++
		return nil
	}); err != nil {
		return err
	}

	if err := w.tx.DeleteBucket([]byte(bucketKNSucc)); err != nil {
		return err
	}
	succB, err := w.tx.CreateBucket([]byte(bucketKNSucc))
	if err != nil {
		return err
	}
	for prefix, n := range succ {
		if err := succB.Put([]byte(prefix), encodeUint64(n)); err != nil {
			return err
		}
	}

	if err := w.tx.DeleteBucket([]byte(bucketKNPreCount)); err != nil {
		return err
	}
	preB, err := w.tx.CreateBucket([]byte(bucketKNPreCount))
	if err != nil {
		return err
	}
	var total uint64
	for token, n := range pre {
		if err := preB.Put([]byte(token), encodeUint64(n)); err != nil {
			return err
		}
		total += n
	}
	return w.setCounter("count:kn_pre_total", total)
}

// NgramCount reports how many continuations are stored, for the status line.
func (r *Reader) NgramCount() int {
	b := r.bucket(bucketNgram)
	if b == nil {
		return 0
	}
	return b.Stats().KeyN
}

// ForEachNgram visits every continuation. For the maintenance pass and nothing on
// the hot path: it walks the largest bucket in the database.
func (r *Reader) ForEachNgram(fn func(prefix, next string, s corpus.Successor) error) error {
	b := r.bucket(bucketNgram)
	if b == nil {
		return nil
	}
	return b.ForEach(func(k, v []byte) error {
		i := bytes.LastIndexByte(k, sep)
		if i <= 0 {
			return nil
		}
		count, authors := decodeNgram(v)
		return fn(string(k[:i]), string(k[i+1:]), corpus.Successor{
			Token:   string(k[i+1:]),
			Count:   count,
			Authors: authors,
		})
	})
}

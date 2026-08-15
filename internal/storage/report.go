package storage

import (
	"bytes"
	"fmt"
	"strings"

	"go.etcd.io/bbolt"

	"github.com/6586x57890143/peregrine/internal/corpus"
)

// CorpusStats walks the whole corpus and returns its distributions.
//
// # Why this is a Store method and not a Reader method
//
// These are UNBOUNDED cursor walks, and ScanTopics's comment says exactly why that
// matters: a bounded scan "is the only reason a scan is acceptable here at all". A
// Reader is the type the generation hot path holds, inside the one read transaction a
// reply gets, and hanging an unbounded whole-bucket walk off it is a footgun aimed at
// the innermost loop in the bot. Only the finished aggregate escapes this file.
//
// It is affordable here because the caller is a maintenance mode running once against a
// snapshot, not a reply with a human waiting on it.
//
// # One pass answers three questions
//
// The ngram bucket is keyed <prefix> NUL <next> with a fixed 12-byte value of count and
// distinct-author count. bbolt cursors walk in key order, so every edge sharing a prefix
// arrives contiguously: the author histogram, the probability mass and the per-prefix
// branch factor all fall out of a single sequential scan in constant memory, with no
// grouping structure held in RAM.
// The sentinel is a PARAMETER rather than a constant here, and that is the same rule
// ScanTopics states: storage is the bottom layer and must not learn what a word means.
// internal/learn and internal/markov each define the sentinel with a test pinning that
// they agree, and a third definition down here would be a third thing to keep in step.
// The caller says which key is structural; this package just reads it.
func (s *Store) CorpusStats(minAuthors int, sentinel string) (corpus.Stats, error) {
	var st corpus.Stats
	st.Authors = make([]int, corpus.AuthorHistogramMax+1)
	st.SingleAuthorByCount = make([]int, len(corpus.CountThresholds))

	// admitted counts edges and mass surviving each candidate threshold, so the caller
	// gets the whole curve rather than the one value it happens to run.
	const maxThreshold = 6
	admittedEdges := make([]int, maxThreshold)
	admittedMass := make([]uint64, maxThreshold)

	// Per order. Index is prefix word count, which is at most MaxNGram-1; the slice grows
	// on demand rather than assuming a configured maximum, because the corpus was written
	// by whatever MaxNGram was set at the time and a report must describe the file rather
	// than the current configuration.
	type orderAcc struct {
		prefixes, edges, gatedPrefixes int
		successors, gatedSuccessors    int
		succCounts                     []int
	}
	orders := map[int]*orderAcc{}

	err := s.db.View(func(tx *bbolt.Tx) error {
		if err := s.walkNgrams(tx, minAuthors, &st, admittedEdges, admittedMass,
			func(order, succ, gated int) {
				acc, ok := orders[order]
				if !ok {
					acc = &orderAcc{}
					orders[order] = acc
				}
				acc.prefixes++
				acc.edges += succ
				acc.successors += succ
				acc.gatedSuccessors += gated
				acc.succCounts = append(acc.succCounts, succ)
				if gated > 0 {
					acc.gatedPrefixes++
				}
			}); err != nil {
			return err
		}

		// Topic counts: one key per word, value is a bare uint64.
		if b := tx.Bucket([]byte(bucketTopic)); b != nil {
			if err := b.ForEach(func(_, v []byte) error {
				n := decodeUint64(v)
				st.TopicCounts = append(st.TopicCounts, n)
				st.TotalTokens += n
				return nil
			}); err != nil {
				return fmt.Errorf("walk topics: %w", err)
			}
		}

		// Associations. Keyed <word> NUL <assoc>, so the same contiguity trick gives
		// associates-per-word without a map.
		var (
			currentWord string
			perWord     int
			started     bool
		)
		if b := tx.Bucket([]byte(bucketTopicWord)); b != nil {
			if err := b.ForEach(func(k, v []byte) error {
				count, _ := decodeAssoc(v)
				st.AssocCounts = append(st.AssocCounts, count)

				word, _, ok := splitCompositeKey(k)
				if !ok {
					return nil
				}
				if !started {
					currentWord, started = word, true
				}
				if word != currentWord {
					st.AssocPerWord = append(st.AssocPerWord, perWord)
					currentWord, perWord = word, 0
				}
				perWord++
				return nil
			}); err != nil {
				return fmt.Errorf("walk topic words: %w", err)
			}
		}
		if started {
			st.AssocPerWord = append(st.AssocPerWord, perWord)
		}

		st.Edges = keyCount(tx, bucketNgram)
		st.Vocabulary = keyCount(tx, bucketTopic)
		st.TopicWords = keyCount(tx, bucketTopicWord)
		st.NameTopics = keyCount(tx, bucketNameTopic)
		st.Names = keyCount(tx, bucketName)
		if meta := tx.Bucket([]byte(bucketMeta)); meta != nil {
			st.Learned = decodeUint64(meta.Get([]byte(metaMessagesLearned)))
		}
		if b := tx.Bucket([]byte(bucketTopic)); b != nil && sentinel != "" {
			if v := b.Get([]byte(sentinel)); v != nil {
				st.SentinelCount = decodeUint64(v)
			}
		}
		return nil
	})
	if err != nil {
		return corpus.Stats{}, err
	}

	for i := range maxThreshold {
		st.Admission = append(st.Admission, corpus.Admission{
			MinAuthors: i,
			Edges:      admittedEdges[i],
			EdgeShare:  share(admittedEdges[i], st.Edges),
			Mass:       admittedMass[i],
			MassShare:  shareU64(admittedMass[i], st.TotalEdgeMass),
		})
	}

	for order := 1; order <= len(orders); order++ {
		acc, ok := orders[order]
		if !ok {
			continue
		}
		corpus.SortInt(acc.succCounts)
		st.Prefixes += acc.prefixes
		st.SuccessorCounts = append(st.SuccessorCounts, acc.succCounts...)
		st.Orders = append(st.Orders, corpus.OrderStats{
			Order:          order,
			Prefixes:       acc.prefixes,
			Edges:          acc.edges,
			MeanSuccessors: share(acc.successors, acc.prefixes),
			MedianSucc:     corpus.Percentile(acc.succCounts, 0.5),
			GatedPrefixes:  acc.gatedPrefixes,
			MeanGatedSucc:  share(acc.gatedSuccessors, acc.prefixes),
			DeadPrefixRate: share(acc.prefixes-acc.gatedPrefixes, acc.prefixes),
		})
	}

	corpus.SortUint64(st.TopicCounts)
	corpus.SortUint64(st.AssocCounts)
	corpus.SortInt(st.AssocPerWord)
	corpus.SortInt(st.SuccessorCounts)
	return st, nil
}

// walkNgrams is the single sequential pass, factored out to keep CorpusStats readable.
//
// onPrefix fires once per distinct prefix with its total and admissible successor counts,
// which is what makes the per-order branch factor a streaming computation rather than a
// map from every prefix in the corpus to its successors.
func (s *Store) walkNgrams(
	tx *bbolt.Tx,
	minAuthors int,
	st *corpus.Stats,
	admittedEdges []int,
	admittedMass []uint64,
	onPrefix func(order, successors, gated int),
) error {
	b := tx.Bucket([]byte(bucketNgram))
	if b == nil {
		return nil
	}

	var (
		currentPrefix []byte
		succ, gated   int
		started       bool
	)
	flush := func() {
		if !started {
			return
		}
		onPrefix(strings.Count(string(currentPrefix), " ")+1, succ, gated)
	}

	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		count, authors := decodeNgram(v)
		st.TotalEdgeMass += count

		st.Authors[min(int(authors), corpus.AuthorHistogramMax)]++

		// The joint distribution, which is the number that sizes a concentration gate.
		// A single-authored edge seen once is a sparse corpus; a single-authored edge
		// seen a hundred times is the poisoning shape SPEC.md section 4 A6 describes,
		// and today's gate cannot tell them apart.
		if authors <= 1 {
			for i, threshold := range corpus.CountThresholds {
				if count >= threshold {
					st.SingleAuthorByCount[i]++
				}
			}
		}

		for i := range admittedEdges {
			if int(authors) >= i {
				admittedEdges[i]++
				admittedMass[i] += count
			}
		}

		prefix, _, ok := splitCompositeKeyBytes(k)
		if !ok {
			continue
		}
		if !started || !bytes.Equal(prefix, currentPrefix) {
			flush()
			// The key is only valid for the life of the transaction, so the prefix is
			// copied rather than retained. Every other accessor here follows that rule.
			currentPrefix = append(currentPrefix[:0], prefix...)
			succ, gated, started = 0, 0, true
		}
		succ++
		if minAuthors <= 0 || int(authors) >= minAuthors {
			gated++
		}
	}
	flush()
	return nil
}

// keyCount is Bucket.Stats().KeyN with a nil guard, matching what Reader.Status reports
// so the two can be cross-checked against each other.
func keyCount(tx *bbolt.Tx, name string) int {
	b := tx.Bucket([]byte(name))
	if b == nil {
		return 0
	}
	return b.Stats().KeyN
}

// splitCompositeKeyBytes splits <a> NUL <b> without allocating, for the hot walk.
func splitCompositeKeyBytes(key []byte) (a, bPart []byte, ok bool) {
	return bytes.Cut(key, []byte{0})
}

// splitCompositeKey is the string form, for the walks where the allocation does not
// matter.
func splitCompositeKey(key []byte) (a, b string, ok bool) {
	x, y, ok := splitCompositeKeyBytes(key)
	if !ok {
		return "", "", false
	}
	return string(x), string(y), true
}

func share(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total)
}

func shareU64(n, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total)
}

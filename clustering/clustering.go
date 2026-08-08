// Package clustering has NO CALLERS as of M6b, and is kept only as the input to M8.
//
// It is not dead by accident and it is not wired up waiting to be switched on. Three
// things are true of it at once:
//
//  1. It has never worked. Cluster members are persisted with string keys and
//     unmarshalled into map[int]float32, so every cluster fails to decode, and both
//     consumers guarded that with `if err := json.Unmarshal(...); err == nil` and no
//     else. It ran a full similarity walk every 24 hours inside a write transaction,
//     against bbolt's single writer, ending in a destructive DeleteBucket plus
//     CreateBucket, and produced data nothing has ever read (SPEC.md section 8,
//     finding 27).
//  2. It cannot be called any more. It takes a *bbolt.DB, and since M6b nothing
//     outside internal/storage can hold one. That is deliberate: a consumer with a
//     handle can nest a transaction, which is an unrecoverable hang (finding 1).
//  3. It reads the name-topic bucket as a JSON map keyed by name, which the
//     composite-key layout does not store, so it would find nothing even with the
//     codec fixed.
//
// What is worth keeping is the algorithm: the heap-driven agglomerative merge and the
// sparse cosine similarity, which run outside any transaction and are the part M8
// moves into internal/clustering as a pure function over corpus types, with
// content-hashed IDs and diff-based persistence through storage's blob API. The
// member representation has to change with it, because interner ids depend on
// insertion order and therefore mean nothing across processes.
package clustering

import (
	"container/heap"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"sync/atomic"
	"time"

	bbolt "go.etcd.io/bbolt"
	"go.etcd.io/bbolt/errors"
)

// This file provides an optimized, forward-compatible rewrite of the original
// performClustering function. Key points:
// - All expensive computation runs *outside* Bolt write transactions.
// - Uses a max-heap (priority queue) to avoid full O(n^2) recomputation each round.
// - Sparse vector representation (map[int]float32) and cosine similarity with
//   an efficient sparse dot product.
// - Normalizes cluster member weights and prunes tiny weights to control growth.
// - Designed to be incremental: you can pass a list of new proto-clusters to
//   merge into the existing cluster population.

const (
	ConceptClusterBucket = "concept_clusters"
	TopicClusterBucket   = "topic_clusters"
	NameTopicBucket      = "name_topics"

	// tuning
	MergeThreshold    = 0.005 // keep conservative default, caller can change
	PruneEpsilon      = 1e-4
	MaxSeedTopics     = 10
	MinMembersToKeep  = 2
	MaxHeapPopPerIter = 1e6 // safety guard should not hit in normal runs
)

var GlobalMergeCounter uint64

// TopicClusterData tracks associations between topics.
type TopicClusterData struct {
	Count int `json:"count"` // How many times these topics have appeared in the same context.
}

// ConceptCluster holds cluster data using integer term indices for efficiency.
type ConceptCluster struct {
	ID          string          `json:"id"`
	Members     map[int]float32 `json:"members"` // termIdx -> weight
	LastUpdated time.Time       `json:"last_updated"`
}

// helper to marshal/unmarshal clusters with string keys for bolt storage
type persistentCluster struct {
	ID          string             `json:"id"`
	Members     map[string]float32 `json:"members"`
	LastUpdated time.Time          `json:"last_updated"`
}

// pair is a candidate merge pair stored in the max-heap
type pair struct {
	a, b  int
	score float64
	// index in heap provided by container/heap
	idx int
}

// pairHeap implements a max-heap by score
type pairHeap []*pair

func (h pairHeap) Len() int           { return len(h) }
func (h pairHeap) Less(i, j int) bool { return h[i].score > h[j].score }
func (h pairHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].idx = i
	h[j].idx = j
}
func (h *pairHeap) Push(x interface{}) {
	p := x.(*pair)
	p.idx = len(*h)
	*h = append(*h, p)
}
func (h *pairHeap) Pop() interface{} {
	old := *h
	n := len(old)
	p := old[n-1]
	p.idx = -1
	*h = old[0 : n-1]
	return p
}

// performClusteringOptimized is the main entry point. It preserves the functionality
// of your original logic but runs clustering in memory and writes back atomically.
//   - db: bolt DB
//   - incrementalSeeds: optional additional clusters (string->map[string]WordPosData in original form)
//     if nil the function seeds from the DB as before. If you want to make this fully incremental,
//     pass only the newcomers here and they will be treated as proto-clusters to be merged.
func PerformClusteringOptimized(db *bbolt.DB, incrementalSeeds map[string]map[string]WordPosData) error {
	log.Println("[CLUSTERING] Starting optimized clustering pass...")
	start := time.Now()

	// 1) Read all necessary data in a read-only transaction
	var initialClusters []*ConceptCluster
	var nameTopicData map[string]map[string]WordPosData

	if err := db.View(func(tx *bbolt.Tx) error {
		nameTopicB := tx.Bucket([]byte(NameTopicBucket))
		if nameTopicB == nil {
			return fmt.Errorf("required buckets missing")
		}

		// load nameTopic data only into an in-memory structure
		nameTopicData = make(map[string]map[string]WordPosData)
		_ = nameTopicB.ForEach(func(k, v []byte) error {
			var assoc map[string]WordPosData
			if err := json.Unmarshal(v, &assoc); err != nil {
				return nil
			}
			nameTopicData[string(k)] = assoc
			return nil
		})

		return nil
	}); err != nil {
		return fmt.Errorf("db read failed: %w", err)
	}

	// merge incrementalSeeds if provided into nameTopicData (give precedence to incremental)
	for name, assoc := range incrementalSeeds {
		nameTopicData[name] = assoc
	}

	// 2) Build vocabulary (topic/name -> int index)
	vocab := make(map[string]int)
	revVocab := make([]string, 0)
	addToVocab := func(s string) int {
		if idx, ok := vocab[s]; ok {
			return idx
		}
		idx := len(revVocab)
		vocab[s] = idx
		revVocab = append(revVocab, s)
		return idx
	}

	// Convert existing clusters' persistent members into string-keyed maps by re-reading DB for members
	// Simpler: re-open read tx and decode persistent members properly.
	// (We kept initialClusters skeleton above; for correctness, re-read clusters properly.)
	if err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(ConceptClusterBucket))
		_ = b.ForEach(func(k, v []byte) error {
			var pc persistentCluster
			if err := json.Unmarshal(v, &pc); err != nil {
				return nil
			}
			cl := &ConceptCluster{ID: pc.ID, Members: map[int]float32{}, LastUpdated: pc.LastUpdated}
			for sk, w := range pc.Members {
				idx := addToVocab(sk)
				cl.Members[idx] = w
			}
			initialClusters = append(initialClusters, cl)
			return nil
		})
		return nil
	}); err != nil {
		return fmt.Errorf("db read failed on second pass: %w", err)
	}

	// 3) Seed proto-clusters from nameTopicData (only create clusters with >1 member)
	for name, associations := range nameTopicData {
		// convert associations to slice and sort by count
		pairs := make([]topicAssocPair, 0, len(associations))
		for topic, data := range associations {
			pairs = append(pairs, topicAssocPair{Topic: topic, Data: data})
		}
		sort.Slice(pairs, func(i, j int) bool { return pairs[i].Data.Count > pairs[j].Data.Count })

		cl := &ConceptCluster{ID: fmt.Sprintf("name-seed-%s", name), Members: map[int]float32{}, LastUpdated: start}
		// add the name itself
		nameIdx := addToVocab(name)
		cl.Members[nameIdx] = 1.0
		count := 0
		for _, p := range pairs {
			if count >= MaxSeedTopics {
				break
			}
			tidx := addToVocab(p.Topic)
			cl.Members[tidx] = float32(p.Data.Count)
			count++
		}
		if len(cl.Members) >= MinMembersToKeep {
			normalizeAndPrune(cl)
			initialClusters = append(initialClusters, cl)
		}
	}

	log.Printf("[CLUSTERING] Loaded %d initial clusters (DB + name seeds). Vocab size: %d", len(initialClusters), len(vocab))

	// 4) Run agglomerative merging using a heap. All in-memory.
	clusters := initialClusters
	active := make([]bool, len(clusters))
	for i := range active {
		active[i] = true
	}

	h := &pairHeap{}
	heap.Init(h)

	// helper to compute cosine similarity between two clusters
	similarity := func(a, b *ConceptCluster) float64 {
		return cosineSparse(a.Members, b.Members)
	}

	// seed the heap with initial candidate pairs above threshold
	for i := 0; i < len(clusters); i++ {
		for j := i + 1; j < len(clusters); j++ {
			s := similarity(clusters[i], clusters[j])
			if s > MergeThreshold {
				heap.Push(h, &pair{a: i, b: j, score: s})
			}
		}
	}

	// safety guard to avoid pathological loops
	popped := 0

	for h.Len() > 0 {
		if popped > int(MaxHeapPopPerIter) {
			log.Printf("[CLUSTERING] heap pop safety limit reached")
			break
		}
		p := heap.Pop(h).(*pair)
		popped++

		// validate that pair indices are still active
		if p.a >= len(active) || p.b >= len(active) || !active[p.a] || !active[p.b] {
			continue // stale
		}

		// re-evaluate score (lazy update)
		fresh := similarity(clusters[p.a], clusters[p.b])
		if fresh < MergeThreshold {
			continue
		}

		// merge into a new cluster
		a := clusters[p.a]
		b := clusters[p.b]
		newCl := &ConceptCluster{Members: map[int]float32{}, LastUpdated: start}
		// carry a deterministic ID
		mc := atomic.AddUint64(&GlobalMergeCounter, 1)
		newCl.ID = fmt.Sprintf("cluster-%d-%d", start.UnixNano(), mc)

		// sum members
		for k, v := range a.Members {
			newCl.Members[k] += v
		}
		for k, v := range b.Members {
			newCl.Members[k] += v
		}
		// normalize & prune
		normalizeAndPrune(newCl)

		// deactivate old clusters
		active[p.a] = false
		active[p.b] = false

		// append new cluster
		newIdx := len(clusters)
		clusters = append(clusters, newCl)
		active = append(active, true)

		// compute similarity vs all active clusters and push to heap if above threshold
		for i := 0; i < newIdx; i++ {
			if !active[i] {
				continue
			}
			s := similarity(newCl, clusters[i])
			if s > MergeThreshold {
				heap.Push(h, &pair{a: i, b: newIdx, score: s})
			}
		}
	}

	// collect final active clusters
	finalClusters := make([]*ConceptCluster, 0, len(clusters))
	for i, c := range clusters {
		if active[i] {
			finalClusters = append(finalClusters, c)
		}
	}

	// 5) Persist final clusters in a short write transaction
	if err := db.Update(func(tx *bbolt.Tx) error {
		// recreate bucket
		if err := tx.DeleteBucket([]byte(ConceptClusterBucket)); err != nil && err != errors.ErrBucketNotFound {
			return fmt.Errorf("failed to delete old bucket: %w", err)
		}
		nb, err := tx.CreateBucket([]byte(ConceptClusterBucket))
		if err != nil {
			return fmt.Errorf("failed to create bucket: %w", err)
		}

		for _, c := range finalClusters {
			// convert back to string-keyed map for storage
			pc := persistentCluster{ID: c.ID, Members: map[string]float32{}, LastUpdated: c.LastUpdated}
			for tidx, w := range c.Members {
				pc.Members[revVocab[tidx]] = w
			}
			data, err := json.Marshal(pc)
			if err != nil {
				log.Printf("[CLUSTERING] marshal error: %v", err)
				continue
			}
			if err := nb.Put([]byte(c.ID), data); err != nil {
				log.Printf("[CLUSTERING] bucket put error: %v", err)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("db write failed: %w", err)
	}

	log.Printf("[CLUSTERING] Finished in %s. Produced %d final clusters.", time.Since(start), len(finalClusters))
	return nil
}

// normalizeAndPrune normalizes weights to sum to 1 and removes tiny weights
func normalizeAndPrune(c *ConceptCluster) {
	var sum float64
	for _, w := range c.Members {
		sum += float64(w)
	}
	if sum == 0 {
		return
	}
	for k, w := range c.Members {
		v := float32(float64(w) / sum)
		if math.Abs(float64(v)) < PruneEpsilon {
			delete(c.Members, k)
		} else {
			c.Members[k] = v
		}
	}
}

// cosineSparse computes cosine similarity between two sparse vectors represented as map[int]float32
func cosineSparse(a, b map[int]float32) float64 {
	// iterate smaller map for dot product
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var dot float64
	if len(a) < len(b) {
		for k, av := range a {
			if bv, ok := b[k]; ok {
				dot += float64(av) * float64(bv)
			}
		}
	} else {
		for k, bv := range b {
			if av, ok := a[k]; ok {
				dot += float64(av) * float64(bv)
			}
		}
	}
	// compute norms
	var na, nb float64
	for _, v := range a {
		na += float64(v) * float64(v)
	}
	for _, v := range b {
		nb += float64(v) * float64(v)
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Small helper types copied from original for compatibility
type WordPosData struct {
	Count int `json:"count"`
	// other fields omitted: clustering only used Count historically
}

type topicAssocPair struct {
	Topic string
	Data  WordPosData
}

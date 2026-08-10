package generate

import (
	"sort"
	"sync"
	"time"

	"github.com/6586x57890143/peregrine/internal/text"
)

// Memory is a decayed record of what has recently been said in one channel, used to steer
// generation toward the conversation it is answering.
//
// # What it holds, and why the encoding changed (SPEC.md section 8, finding 48)
//
// It used to flatten to one []string with each token REPEATED in proportion to how recent its
// message was, and that single encoding served two consumers needing opposite things. The
// scorer collapsed it with wordSet, discarding the weighting entirely, so a word from the
// fiftieth-oldest message scored exactly what a word from the newest one did. The seed's recent
// tier read word ORDER, which in-place repetition destroys: almost every n-gram window was a
// doubled word, and the windows straddled message boundaries because a flat slice has no
// boundary in it.
//
// So it keeps messages, and exposes the two shapes its two consumers actually want: a weight
// per token for the scorer, and per-message word lists for the seed.
type Memory struct {
	mu      sync.Mutex
	entries []entry
}

type entry struct {
	words []string
	names []string
	decay float64
}

// RecentMessage is one remembered message: its tokens, and how much it has faded.
type RecentMessage struct {
	Words []string
	Decay float64
}

// Add records a message and the canonical names it was about, decaying everything already
// there.
//
// Names travel with the message rather than in a second structure, so they decay together: who
// the channel was talking about five messages ago has faded exactly as much as what was said.
//
// Tokenized on the way IN rather than on the way out. Both accessors below need tokens, and the
// old shape re-tokenized the entire memory through a regex on every reply.
func (m *Memory) Add(content string, about []string) {
	words := text.Tokenize(content)
	if len(words) == 0 && len(about) == 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.entries {
		m.entries[i].decay *= decayPerMessage
	}
	m.entries = append(m.entries, entry{words: words, names: about, decay: 1.0})
	if len(m.entries) > maxEntries {
		m.entries = m.entries[len(m.entries)-maxEntries:]
	}
}

// Weights returns each remembered token with how recent it is, at most 1.0.
//
// The scorer multiplies its RecentContext weight by this, so the term is bounded by that weight
// without needing a tanh: the value is already in [0, 1] by construction. A token in several
// messages keeps its highest, most recent value rather than summing, for the same reason the
// seed tiers keep a maximum: a word said twice is not twice as much context.
func (m *Memory) Weights() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]float64, len(m.entries)*8)
	for _, e := range m.entries {
		for _, w := range e.words {
			if e.decay > out[w] {
				out[w] = e.decay
			}
		}
	}
	return out
}

// Messages returns the remembered messages oldest first, for the seed's recent tier.
//
// Per message, because that tier forms n-gram windows and a window spanning two messages is a
// phrase nobody said.
func (m *Memory) Messages() []RecentMessage {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]RecentMessage, 0, len(m.entries))
	for _, e := range m.entries {
		if len(e.words) == 0 {
			continue
		}
		out = append(out, RecentMessage{Words: e.words, Decay: e.decay})
	}
	return out
}

// Names returns who the channel has recently been talking about, canonical name to decayed
// weight, strongest first and bounded.
//
// BOUNDED because the map is keyed by person and a busy channel meets a lot of people, and
// because the consumer is a seed tier that pays a lookup per name: an unbounded recall list
// would put an unbounded number of corpus reads on the reply path. The floor drops people who
// have faded past the point of being what the conversation is about.
func (m *Memory) Names() []string {
	m.mu.Lock()
	weights := make(map[string]float64, len(m.entries))
	for _, e := range m.entries {
		for _, n := range e.names {
			if e.decay > weights[n] {
				weights[n] = e.decay
			}
		}
	}
	m.mu.Unlock()

	type scored struct {
		name   string
		weight float64
	}
	ranked := make([]scored, 0, len(weights))
	for n, w := range weights {
		if w < recalledNameFloor {
			continue
		}
		ranked = append(ranked, scored{name: n, weight: w})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].weight != ranked[j].weight {
			return ranked[i].weight > ranked[j].weight
		}
		// Deterministic tie-break, because Go's map iteration is randomized and this feeds
		// seed selection, which the golden harness needs to be reproducible.
		return ranked[i].name < ranked[j].name
	})
	if len(ranked) > maxRecalledNames {
		ranked = ranked[:maxRecalledNames]
	}

	out := make([]string, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.name)
	}
	return out
}

// TopicWords returns the most recent content words, strongest first and bounded.
//
// This is what makes a short prompt like "bro what" steer toward whatever the channel has
// actually been about, instead of being answered from nowhere. Stop words are excluded because
// the consumer is CoreTopics, which is keyed by topic and looks each one up in the association
// index: "the" would buy a guaranteed miss per lookup.
//
// Bounded for the same reason Names is: one corpus read per entry, on the reply path.
func (m *Memory) TopicWords() map[string]float64 {
	m.mu.Lock()
	weights := make(map[string]float64, len(m.entries)*8)
	for _, e := range m.entries {
		for _, w := range e.words {
			if text.IsStopWord(w) {
				continue
			}
			if e.decay > weights[w] {
				weights[w] = e.decay
			}
		}
	}
	m.mu.Unlock()

	if len(weights) <= maxRecentTopics {
		return weights
	}

	type scored struct {
		word   string
		weight float64
	}
	ranked := make([]scored, 0, len(weights))
	for w, v := range weights {
		ranked = append(ranked, scored{word: w, weight: v})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].weight != ranked[j].weight {
			return ranked[i].weight > ranked[j].weight
		}
		return ranked[i].word < ranked[j].word
	})

	out := make(map[string]float64, maxRecentTopics)
	for _, r := range ranked[:maxRecentTopics] {
		out[r.word] = r.weight
	}
	return out
}

const (
	// maxEntries bounds one channel's memory.
	maxEntries = 50

	// decayPerMessage is how much older messages fade as new ones arrive.
	decayPerMessage = 0.8

	// maxRecalledNames and recalledNameFloor bound who counts as recently discussed.
	//
	// A recalled name costs a NameTopicsFor lookup on the reply path, so this is a bound on
	// work as much as on relevance. The floor is where a person has faded past being what the
	// conversation is about: at a decay of 0.8 per message, 0.3 is roughly five messages back.
	maxRecalledNames  = 4
	recalledNameFloor = 0.3

	// maxRecentTopics bounds the conversation words that reach CoreTopics, which also costs
	// one association lookup each per step.
	maxRecentTopics = 12

	// maxChannels bounds how many channels are remembered at once.
	//
	// Not optional. The map is keyed by channel ID and grows with every guild the bot
	// joins, and it is the kind of leak that never shows up in testing because a test only
	// ever uses one channel.
	maxChannels = 200
)

// Memories holds one Memory per channel, bounded.
//
// Per channel as of M7b, closing finding G8. There used to be one package-level memory
// shared by every channel in every guild, so a reply in one channel was steered by
// whatever had been said in an unrelated one. That is not chaos, which would be fine, it
// is simply the wrong context: the reply reads as a non-sequitur to the thread it is in,
// so the bot looks like it is not paying attention rather than like it is being funny.
type Memories struct {
	mu    sync.Mutex
	byID  map[string]*channelMemory
	limit int
}

type channelMemory struct {
	mem      Memory
	lastUsed time.Time
}

// NewMemories returns an empty set. limit <= 0 uses the default bound.
func NewMemories(limit int) *Memories {
	if limit <= 0 {
		limit = maxChannels
	}
	return &Memories{byID: map[string]*channelMemory{}, limit: limit}
}

// For returns the memory for one channel, creating it if needed.
func (ms *Memories) For(channelID string) *Memory {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if cm, ok := ms.byID[channelID]; ok {
		cm.lastUsed = time.Now()
		return &cm.mem
	}
	if len(ms.byID) >= ms.limit {
		ms.evictOldest()
	}
	cm := &channelMemory{lastUsed: time.Now()}
	ms.byID[channelID] = cm
	return &cm.mem
}

// Len reports how many channels are remembered. For the status line and for tests that
// need to see the bound working.
func (ms *Memories) Len() int {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return len(ms.byID)
}

// evictOldest drops the least recently used entry. Caller holds the lock.
//
// Oldest-touched-first rather than a real LRU: the difference does not matter for a cap in
// the hundreds, and a timestamp per channel is cheaper to reason about than an intrusive
// list.
func (ms *Memories) evictOldest() {
	var oldestID string
	var oldest time.Time
	for id, cm := range ms.byID {
		if oldestID == "" || cm.lastUsed.Before(oldest) {
			oldestID, oldest = id, cm.lastUsed
		}
	}
	if oldestID != "" {
		delete(ms.byID, oldestID)
	}
}

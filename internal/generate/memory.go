package generate

import (
	"math"
	"sync"
	"time"

	"github.com/6586x57890143/peregrine/internal/text"
)

// Memory is a decayed record of what has recently been said in one channel, used to steer
// generation toward the conversation it is answering.
type Memory struct {
	mu      sync.Mutex
	entries []entry
}

type entry struct {
	content string
	decay   float64
}

// Add records a message, decaying everything already there.
func (m *Memory) Add(content string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.entries {
		m.entries[i].decay *= decayPerMessage
	}
	m.entries = append(m.entries, entry{content: content, decay: 1.0})
	if len(m.entries) > maxEntries {
		m.entries = m.entries[len(m.entries)-maxEntries:]
	}
}

// WeightedWords flattens the memory into a word list, repeating each word in proportion to
// how recent its message was.
//
// Repetition is the weighting mechanism because the consumer is a bag-of-words feature set
// in the scorer: a token appearing five times counts five times, which is what "recent
// context matters more" has to mean when the consumer cannot take a weight.
func (m *Memory) WeightedWords() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []string
	for _, e := range m.entries {
		reps := int(math.Max(1, e.decay*repetitionScale))
		for _, w := range text.Tokenize(e.content) {
			for range reps {
				out = append(out, w)
			}
		}
	}
	return out
}

const (
	// maxEntries bounds one channel's memory.
	maxEntries = 50

	// decayPerMessage is how much older messages fade as new ones arrive.
	decayPerMessage = 0.8

	// repetitionScale turns a decay factor into a repeat count.
	repetitionScale = 5

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

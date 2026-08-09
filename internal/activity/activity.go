// Package activity tracks where people are talking and who is around, from the
// messages the gateway already delivers.
//
// It exists because peregrine kept asking Discord questions it could answer itself.
// getActiveChannels paged every text channel in every guild fifty messages at a time,
// with a 50ms sleep between pages, to count how many recent messages each one held.
// findRandomActiveUser then called that per guild and fetched another hundred messages
// per active channel to collect authors. On a bot in a few large guilds that is
// hundreds of REST calls per aggro tick, answered by Discord with rate limits whose
// retries make it worse, for information that arrived free on the websocket and was
// thrown away (SPEC.md section 8, finding 14).
//
// # Two things this package is not
//
// It is not a metric. Nothing here is persisted or reported; it is a decision input
// for "where should the bot speak" and "who should it bother", and it is allowed to be
// approximate.
//
// It is not the state cache. discordgo's State knows a channel exists, what it is
// called and whether it is NSFW, and its LastMessageID gives recency. It does not know
// volume. So callers use both: this for how busy, State for what and where.
//
// # Bounded, because the alternative is a leak
//
// Both maps are keyed by things that grow without limit as the bot joins guilds and
// meets people, and this project has already had that leak twice: the conversation
// memory was one shared instance until it became a bounded per-channel map (finding
// G8), and the word-game manager kept its own per-channel activity map that had to be
// bounded the same way. That manager's copy is gone now, because two mechanisms
// counting the same thing from the same call site are one mechanism.
package activity

import (
	"sort"
	"sync"
	"time"
)

// Defaults. Each is a bound rather than a tuning parameter: the numbers only need to
// be larger than the working set and small enough that eviction is cheap.
const (
	defaultMaxChannels = 500
	defaultMaxAuthors  = 2000
)

// PerChannelHistory is how many message timestamps are kept per channel by default,
// and therefore the largest count this package can ever report.
//
// Exported because a threshold configured above it could never be met, which would be a
// knob that silently does nothing: internal/config caps
// PEREGRINE_WORDGAME_ACTIVITY_THRESHOLD below this, and its test asserts the
// relationship rather than trusting two files to agree.
const PerChannelHistory = 128

// Options bounds the tracker. The zero value is usable.
type Options struct {
	// MaxChannels and MaxAuthors bound the two maps.
	MaxChannels int
	MaxAuthors  int

	// PerChannel is how many message timestamps are kept per channel.
	//
	// A count therefore SATURATES at this value, which is fine for both questions
	// asked of it: a threshold check (is this channel busy enough for a word game)
	// wants a number well below the cap, and a ranking (which channel is busiest)
	// only needs the order, with the tie broken deterministically. It is worth knowing
	// rather than discovering: two extremely busy channels can tie at the cap.
	PerChannel int

	// Now is injectable so tests can move time instead of sleeping. Nil means
	// time.Now.
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.MaxChannels <= 0 {
		o.MaxChannels = defaultMaxChannels
	}
	if o.MaxAuthors <= 0 {
		o.MaxAuthors = defaultMaxAuthors
	}
	if o.PerChannel <= 0 {
		o.PerChannel = PerChannelHistory
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Channel is one channel's traffic over the window that was asked about.
type Channel struct {
	ID    string
	Count int
}

// Tracker counts recent messages per channel and remembers who has spoken.
//
// One mutex. There were two in the code this replaces, taken in a fixed order at one
// call site and separately at another, which is a deadlock waiting for a third caller.
type Tracker struct {
	mu       sync.Mutex
	opts     Options
	channels map[string]*ring
	authors  map[string]time.Time
}

// New returns a tracker. It starts empty, which matters at startup: for the first
// window after a restart there is nothing here, and callers are expected to have a
// cold-start answer rather than treating empty as "nowhere is active".
func New(o Options) *Tracker {
	o = o.withDefaults()
	return &Tracker{
		opts:     o,
		channels: make(map[string]*ring),
		authors:  make(map[string]time.Time),
	}
}

// ring is a fixed-size circular buffer of message timestamps.
//
// Fixed size rather than a slice that gets pruned, because pruning needs a pass and a
// policy and this needs neither: the oldest entry is simply overwritten. The cost is
// the saturation documented on Options.PerChannel.
type ring struct {
	stamps []time.Time
	next   int
	last   time.Time
}

func (r *ring) add(at time.Time, size int) {
	if len(r.stamps) < size {
		r.stamps = append(r.stamps, at)
	} else {
		r.stamps[r.next] = at
		r.next = (r.next + 1) % size
	}
	r.last = at
}

func (r *ring) countSince(cutoff time.Time) int {
	n := 0
	for _, ts := range r.stamps {
		if ts.After(cutoff) {
			n++
		}
	}
	return n
}

// Note records one message.
//
// Called from the reactor for every message that gets past the learn gate, and
// deliberately not before it: a spam flood would otherwise make a channel look busy
// and attract the bot to exactly the place it should be ignoring. An empty author is
// tolerated (the bot's own output reaches learnMessage that way) and simply does not
// make anybody a candidate for attention.
func (t *Tracker) Note(channelID, authorID string) {
	if channelID == "" {
		return
	}
	now := t.opts.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	c := t.channels[channelID]
	if c == nil {
		c = &ring{}
		t.channels[channelID] = c
	}
	c.add(now, t.opts.PerChannel)
	if len(t.channels) > t.opts.MaxChannels {
		t.evictQuietestChannel(channelID)
	}

	if authorID != "" {
		t.authors[authorID] = now
		if len(t.authors) > t.opts.MaxAuthors {
			t.evictOldestAuthor(authorID)
		}
	}
}

// Count reports how many messages this channel has seen inside window.
func (t *Tracker) Count(channelID string, window time.Duration) int {
	cutoff := t.opts.Now().Add(-window)

	t.mu.Lock()
	defer t.mu.Unlock()

	c := t.channels[channelID]
	if c == nil {
		return 0
	}
	return c.countSince(cutoff)
}

// Busiest returns every channel with traffic inside window, busiest first.
//
// Sorted, with ties broken on channel ID, because a caller picks by index and the old
// version's order was whichever REST call returned first: the bot's choice of where to
// speak depended on network timing.
func (t *Tracker) Busiest(window time.Duration) []Channel {
	cutoff := t.opts.Now().Add(-window)

	t.mu.Lock()
	out := make([]Channel, 0, len(t.channels))
	for id, c := range t.channels {
		if n := c.countSince(cutoff); n > 0 {
			out = append(out, Channel{ID: id, Count: n})
		}
	}
	t.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// RecentAuthors returns everyone who has spoken inside window, in a stable order.
//
// Sorted for the same reason Busiest is: the caller picks one at random, and a
// randomly ordered candidate list plus a random index is two sources of randomness
// where one is wanted, which makes a seeded test meaningless.
func (t *Tracker) RecentAuthors(window time.Duration) []string {
	cutoff := t.opts.Now().Add(-window)

	t.mu.Lock()
	out := make([]string, 0, len(t.authors))
	for id, at := range t.authors {
		if at.After(cutoff) {
			out = append(out, id)
		}
	}
	t.mu.Unlock()

	sort.Strings(out)
	return out
}

// Channels reports how many channels are being tracked. For the status line and for
// tests that need to see the bound working.
func (t *Tracker) Channels() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.channels)
}

// Authors reports how many authors are being remembered.
func (t *Tracker) Authors() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.authors)
}

// evictQuietestChannel drops the channel whose last message is oldest, never the one
// just noted. Called with the lock held.
//
// A linear scan of a map bounded by MaxChannels, and only when the map is at that
// bound. An LRU list would make it constant time and would be more code than the
// problem deserves for a few hundred entries.
func (t *Tracker) evictQuietestChannel(protect string) {
	var (
		victim string
		oldest time.Time
	)
	for id, c := range t.channels {
		if id == protect {
			continue
		}
		if victim == "" || c.last.Before(oldest) {
			victim, oldest = id, c.last
		}
	}
	if victim != "" {
		delete(t.channels, victim)
	}
}

// evictOldestAuthor is the same, for the author map. Called with the lock held.
func (t *Tracker) evictOldestAuthor(protect string) {
	var (
		victim string
		oldest time.Time
	)
	for id, at := range t.authors {
		if id == protect {
			continue
		}
		if victim == "" || at.Before(oldest) {
			victim, oldest = id, at
		}
	}
	if victim != "" {
		delete(t.authors, victim)
	}
}

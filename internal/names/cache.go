package names

import (
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The member cache, and why it lives in this package.
//
// # The cost it removes
//
// Resolve calls GuildMember once per mention to pick up a per-guild nickname, which is the one
// spelling the gateway payload does not carry. discordgo issues that as an unconditional REST
// GET with no state-cache check, so on a history walk it is one request per mention per message
// ever sent. A corpus of half a million messages with a mention on a third of them is six
// figures of requests, which Discord answers with rate limits whose retries make the pass
// slower still. On the live reply path the same call is one request on a message the bot was
// going to answer anyway, which is why this went unnoticed until a full walk existed.
//
// # Why here rather than in the caller
//
// Session is a one-method interface declared by this package, so a caller that wants different
// fetch behaviour supplies it without changing the resolver. Putting the cache behind that same
// interface means every consumer of Resolve and OfMessage can have it, and the two ingest paths
// cannot end up with one cached and one not, which is what happened when it lived inside one
// plugin.

// cachedSession wraps a Session and remembers guild members for a bounded time.
type cachedSession struct {
	inner Session
	ttl   time.Duration
	max   int
	now   func() time.Time // injectable so a test can expire an entry without sleeping

	mu      sync.Mutex
	entries map[string]memberEntry
	saved   int
}

type memberEntry struct {
	member *discordgo.Member // nil records a member who could not be fetched
	at     time.Time
}

// NewCachedSession wraps s so a guild member is fetched at most once per ttl, keeping at most
// max entries.
//
// BOTH BOUNDS ARE REQUIRED and neither is caution. A permanent cache would make the bot use a
// stale nickname forever, which is the opposite of what the name path is for; and the map is
// keyed by person, so without a size bound it grows with everyone the bot ever meets. This
// repository has shipped that exact leak twice, in the conversation memory before M7b and in
// the word-game activity map in M11a.
//
// A non-positive ttl or max returns s unwrapped, so "no caching" is expressible rather than
// being a silently broken cache.
func NewCachedSession(s Session, ttl time.Duration, max int) Session {
	if s == nil || ttl <= 0 || max <= 0 {
		return s
	}
	return &cachedSession{
		inner:   s,
		ttl:     ttl,
		max:     max,
		now:     time.Now,
		entries: make(map[string]memberEntry, max),
	}
}

// GuildMember returns a member, from cache when it can.
//
// A FAILED FETCH IS CACHED TOO, as a nil member with a nil error. Somebody who has left the
// guild is precisely the case a history walk hits over and over, and re-asking Discord for
// every message they ever sent is the worst version of this. Resolve already handles a nil
// member by falling back to the spellings the gateway payload carries, so the nil is a
// complete answer rather than a missing one.
func (c *cachedSession) GuildMember(guildID, userID string, options ...discordgo.RequestOption) (*discordgo.Member, error) {
	key := guildID + "\x00" + userID

	c.mu.Lock()
	if e, ok := c.entries[key]; ok && c.now().Sub(e.at) < c.ttl {
		c.saved++
		c.mu.Unlock()
		return e.member, nil
	}
	c.mu.Unlock()

	// Fetched OUTSIDE the lock. Holding a mutex across a REST round trip would serialize every
	// channel worker in a walk behind one HTTP request, which is the shape CLAUDE.md records
	// about holding imageURLMutex across a store.Update.
	got, err := c.inner.GuildMember(guildID, userID, options...)
	if err != nil {
		got = nil
	}

	c.mu.Lock()
	c.evictIfFull()
	c.entries[key] = memberEntry{member: got, at: c.now()}
	c.mu.Unlock()

	return got, nil
}

// evictIfFull drops the oldest entry when the cache is at its bound. The caller holds the lock.
//
// Oldest-first by fetch time rather than a true LRU, matching generate.Memories: the scan is
// O(n) on a map bounded in the hundreds, and it runs only when the bound is reached.
func (c *cachedSession) evictIfFull() {
	if len(c.entries) < c.max {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, e := range c.entries {
		if oldest.IsZero() || e.at.Before(oldest) {
			oldestKey, oldest = k, e.at
		}
	}
	delete(c.entries, oldestKey)
}

// CacheHits reports how many requests a cached Session avoided, or 0 for one that is not
// caching. For the finishing log line of a walk: a pass that saved nothing means the cache is
// not working, and that is worth being able to see.
func CacheHits(s Session) int {
	c, ok := s.(*cachedSession)
	if !ok {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saved
}

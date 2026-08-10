package names

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// countingSession records how many times the REST call was actually made.
type countingSession struct {
	mu    sync.Mutex
	calls map[string]int
	err   error
}

func newCounting() *countingSession {
	return &countingSession{calls: map[string]int{}}
}

func (c *countingSession) GuildMember(guildID, userID string, _ ...discordgo.RequestOption) (*discordgo.Member, error) {
	c.mu.Lock()
	c.calls[guildID+"/"+userID]++
	c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	return &discordgo.Member{Nick: "nick-" + userID}, nil
}

func (c *countingSession) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

// TestCachedSessionAsksOncePerMember is the reason this type exists.
//
// Resolve calls GuildMember once per mention, and discordgo issues that as an unconditional
// REST GET with no state-cache check. On the live reply path that is one request on a message
// the bot was going to answer anyway; on a history walk it is one per mention per message ever
// sent, which Discord answers with rate limits whose retries make the pass slower still.
func TestCachedSessionAsksOncePerMember(t *testing.T) {
	inner := newCounting()
	c := NewCachedSession(inner, time.Hour, 128)

	for range 50 {
		if _, err := c.GuildMember("g1", "u1"); err != nil {
			t.Fatalf("GuildMember: %v", err)
		}
	}
	if got := inner.total(); got != 1 {
		t.Errorf("made %d requests for one member across 50 lookups, want 1", got)
	}
	if got := CacheHits(c); got != 49 {
		t.Errorf("reported %d avoided requests, want 49", got)
	}
}

// TestCachedSessionExpiresAnEntry.
//
// A permanent cache would make the bot use a stale nickname forever, which is the opposite of
// what the name path is for. Driven by an injected clock rather than a sleep, so the test is
// fast and cannot flake.
func TestCachedSessionExpiresAnEntry(t *testing.T) {
	inner := newCounting()
	c := NewCachedSession(inner, time.Hour, 128)

	now := time.Now()
	c.(*cachedSession).now = func() time.Time { return now }

	if _, err := c.GuildMember("g1", "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GuildMember("g1", "u1"); err != nil {
		t.Fatal(err)
	}
	if got := inner.total(); got != 1 {
		t.Fatalf("made %d requests before expiry, want 1", got)
	}

	now = now.Add(2 * time.Hour)
	if _, err := c.GuildMember("g1", "u1"); err != nil {
		t.Fatal(err)
	}
	if got := inner.total(); got != 2 {
		t.Errorf("made %d requests after the ttl elapsed, want 2: a stale nickname would "+
			"outlive every change to it", got)
	}
}

// TestCachedSessionIsBounded.
//
// The map is keyed by person and grows with everyone the bot meets. This repository has
// shipped that leak twice, in the conversation memory before M7b and in the word-game activity
// map in M11a, so the bound is not caution.
func TestCachedSessionIsBounded(t *testing.T) {
	inner := newCounting()
	const max = 8
	c := NewCachedSession(inner, time.Hour, max)

	for i := range 100 {
		if _, err := c.GuildMember("g1", string(rune('a'+i%26))+string(rune('0'+i/26))); err != nil {
			t.Fatal(err)
		}
	}

	cs := c.(*cachedSession)
	cs.mu.Lock()
	size := len(cs.entries)
	cs.mu.Unlock()

	if size > max {
		t.Errorf("cache holds %d entries with a bound of %d", size, max)
	}
}

// TestCachedSessionRemembersAMissingMember.
//
// Somebody who has left the guild is precisely the case a history walk hits over and over.
// Resolve already handles a nil member by falling back to the gateway payload's spellings, so
// the nil is a complete answer rather than a missing one, and it must not surface as an error
// that would abandon the message.
func TestCachedSessionRemembersAMissingMember(t *testing.T) {
	inner := newCounting()
	inner.err = errors.New("404 unknown member")
	c := NewCachedSession(inner, time.Hour, 128)

	for range 20 {
		member, err := c.GuildMember("g1", "gone")
		if err != nil {
			t.Fatalf("a missing member surfaced an error, which would abandon the message: %v", err)
		}
		if member != nil {
			t.Fatal("a missing member returned a member")
		}
	}
	if got := inner.total(); got != 1 {
		t.Errorf("made %d requests for a member who does not exist, want 1", got)
	}
}

// TestCachedSessionKeysOnGuildAndUser, because the same person in two guilds has two nicknames
// and one of them would otherwise be wrong.
func TestCachedSessionKeysOnGuildAndUser(t *testing.T) {
	inner := newCounting()
	c := NewCachedSession(inner, time.Hour, 128)

	if _, err := c.GuildMember("g1", "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GuildMember("g2", "u1"); err != nil {
		t.Fatal(err)
	}
	if got := inner.total(); got != 2 {
		t.Errorf("made %d requests for one user across two guilds, want 2", got)
	}
}

// TestNoCachingIsExpressible.
//
// A zero ttl or bound returns the session unwrapped, so "do not cache" is a thing a caller can
// say rather than a silently broken cache that expires everything instantly.
func TestNoCachingIsExpressible(t *testing.T) {
	inner := newCounting()
	for _, c := range []Session{
		NewCachedSession(inner, 0, 128),
		NewCachedSession(inner, time.Hour, 0),
	} {
		if c != Session(inner) {
			t.Errorf("a non-positive bound returned a wrapper rather than the session itself")
		}
	}
	if got := CacheHits(inner); got != 0 {
		t.Errorf("CacheHits on an uncached session = %d, want 0", got)
	}
}

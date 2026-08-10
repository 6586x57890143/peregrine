package assocfill

import (
	"errors"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// countingMembers stands in for the REST half of a session.
type countingMembers struct {
	mu    sync.Mutex
	calls map[string]int
	err   error
}

func (c *countingMembers) get(guildID, userID string) (*discordgo.Member, error) {
	c.mu.Lock()
	c.calls[guildID+"/"+userID]++
	c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	return &discordgo.Member{Nick: "nick-" + userID}, nil
}

func (c *countingMembers) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

// cacheOver builds a memberCache whose fetch is the fake rather than a real session.
//
// The production type holds a *discordgo.Session because it also passes three walk methods
// through to it, so the test substitutes at the one method that matters by shadowing the
// fetch, which keeps the cache logic under test rather than a reimplementation of it.
func cacheOver(f *countingMembers) *memberCache {
	c := newMemberCache(nil)
	c.fetch = f.get
	return c
}

// TestMemberCacheAsksOncePerMember is the reason this type exists (SPEC.md section 8, finding
// 46).
//
// discordgo's GuildMember is an unconditional REST GET with no state-cache check, and
// names.Resolve calls it once per mention. On the live path that is one request on a message
// the bot was going to answer anyway; on a full-history walk it is one request per mention per
// message ever sent, which on a real corpus is six figures of avoidable calls that Discord
// answers with rate limits.
func TestMemberCacheAsksOncePerMember(t *testing.T) {
	f := &countingMembers{calls: map[string]int{}}
	c := cacheOver(f)

	for range 50 {
		if _, err := c.GuildMember("g1", "u1"); err != nil {
			t.Fatalf("GuildMember: %v", err)
		}
	}
	if got := f.total(); got != 1 {
		t.Errorf("made %d requests for one member across 50 lookups, want 1", got)
	}
	if got := c.hits(); got != 49 {
		t.Errorf("reported %d avoided requests, want 49", got)
	}
}

// TestMemberCacheRemembersAMissingMember.
//
// Somebody who has left the guild is exactly the case a history walk hits over and over, and
// re-asking for every message they ever sent is the worst version of this. A nil member with a
// nil error is what names.Resolve already handles, by falling back to the spellings the
// gateway payload carries.
func TestMemberCacheRemembersAMissingMember(t *testing.T) {
	f := &countingMembers{calls: map[string]int{}, err: errors.New("404 unknown member")}
	c := cacheOver(f)

	for range 20 {
		member, err := c.GuildMember("g1", "gone")
		if err != nil {
			t.Fatalf("a missing member surfaced an error, which would abort the message: %v", err)
		}
		if member != nil {
			t.Fatal("a missing member returned a member")
		}
	}
	if got := f.total(); got != 1 {
		t.Errorf("made %d requests for a member who does not exist, want 1", got)
	}
}

// TestMemberCacheKeysOnGuildAndUser, because the same person in two guilds has two nicknames
// and one of them would otherwise be wrong.
func TestMemberCacheKeysOnGuildAndUser(t *testing.T) {
	f := &countingMembers{calls: map[string]int{}}
	c := cacheOver(f)

	one, _ := c.GuildMember("g1", "u1")
	two, _ := c.GuildMember("g2", "u1")

	if one == nil || two == nil {
		t.Fatal("a lookup returned nil for a member that exists")
	}
	if got := f.total(); got != 2 {
		t.Errorf("made %d requests for one user across two guilds, want 2", got)
	}
}

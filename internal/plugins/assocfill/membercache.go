package assocfill

import (
	"sync"

	"github.com/bwmarrin/discordgo"
)

// memberCache wraps a session and remembers guild members, because names.Resolve asks for one
// per mention per message and this pass reads every message in history.
//
// # Why this is a blocker rather than an optimization
//
// discordgo's GuildMember is an unconditional REST GET with no state-cache check. names.Resolve
// calls it once per mention to pick up a per-guild nickname, which is the one spelling the
// gateway payload does not carry. On the live path that is one request per mention on a message
// the bot was going to answer anyway. On a full-history walk it is one request per mention per
// message ever sent: a corpus of half a million messages with a mention on a third of them is
// six figures of requests, which Discord answers with rate limits whose retries make the pass
// slower still.
//
// The cache lives here rather than in names for the reason names.Session exists at all: it is a
// one-method interface DECLARED BY THE CONSUMER, so a caller that needs different fetch
// behaviour supplies it without changing the package that does the resolving.
//
// It also passes through the three methods internal/ingest needs, so one object satisfies both
// seams and the walk and the resolver cannot end up holding different sessions.
type memberCache struct {
	session *discordgo.Session

	// fetch is the one REST call this type is about, held as a field so a test can
	// substitute it. The alternative is a second interface for a single method that
	// names.Session already declares, which would be two names for one question.
	fetch func(guildID, userID string) (*discordgo.Member, error)

	mu     sync.Mutex
	member map[string]*discordgo.Member // guildID + NUL + userID
	saved  int
}

func newMemberCache(s *discordgo.Session) *memberCache {
	c := &memberCache{session: s, member: map[string]*discordgo.Member{}}
	c.fetch = func(guildID, userID string) (*discordgo.Member, error) {
		return s.GuildMember(guildID, userID)
	}
	return c
}

// GuildMember satisfies names.Session.
//
// A negative result is cached too, as a nil entry: a member who has left the guild is exactly
// the case a history walk hits repeatedly, and re-asking for every message they ever sent is
// the worst version of this. The nil is returned with a nil error, which names.Resolve already
// handles by falling back to the spellings on the gateway payload.
func (c *memberCache) GuildMember(guildID, userID string, options ...discordgo.RequestOption) (*discordgo.Member, error) {
	key := guildID + "\x00" + userID

	c.mu.Lock()
	if m, ok := c.member[key]; ok {
		c.saved++
		c.mu.Unlock()
		return m, nil
	}
	c.mu.Unlock()

	// Fetched outside the lock. Holding a mutex across a REST round trip would serialize
	// every channel worker in the pass behind one HTTP request, which is the shape
	// CLAUDE.md records about imageURLMutex and a store.Update.
	got, err := c.fetch(guildID, userID)
	if err != nil {
		got = nil
	}

	c.mu.Lock()
	c.member[key] = got
	c.mu.Unlock()

	if got == nil {
		return nil, nil
	}
	return got, nil
}

// hits reports how many requests the cache avoided, for the finishing log line. A pass that
// saved nothing means the cache is not working, and that is worth being able to see.
func (c *memberCache) hits() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saved
}

// The three methods internal/ingest needs, passed straight through.

func (c *memberCache) UserGuilds(limit int, beforeID, afterID string, withCounts bool, options ...discordgo.RequestOption) ([]*discordgo.UserGuild, error) {
	return c.session.UserGuilds(limit, beforeID, afterID, withCounts, options...)
}

func (c *memberCache) GuildChannels(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Channel, error) {
	return c.session.GuildChannels(guildID, options...)
}

func (c *memberCache) ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	return c.session.ChannelMessages(channelID, limit, beforeID, afterID, aroundID, options...)
}

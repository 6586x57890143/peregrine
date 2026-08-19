// Package channels answers "what is this channel" and "where should the bot speak".
//
// It exists because four features needed one or both of those and each had its own answer.
// The image reposter needed a channel's NSFW flag, autonomous posting and interval-mode
// word games needed to pick somewhere to talk, and all three of the latter used to page
// Discord's REST API for it.
//
// # Everything here reads the state cache, never REST
//
// discordgo maintains a channel cache from the gateway, which core.NewSession populates now
// that it requests IntentsGuilds. Before M10a the NSFW check was a ChannelMessage request on
// every message carrying a candidate URL, purely to read one boolean, and it was probably
// the largest rate-limit consumer in the bot. The same missing intent that meant custom
// emotes had never worked was forcing that call (SPEC.md section 8, finding 7).
//
// A cache miss FAILS CLOSED everywhere in this package. Both questions decide whether the
// bot publishes something, so "we could not tell" has to mean "do not".
package channels

import (
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/activity"
)

// Info is what any feature needs to know about a channel. Deliberately small: a feature
// that needs more than this is probably making a decision that belongs elsewhere.
type Info struct {
	ID   string
	Name string
	NSFW bool

	// GuildID is which server this channel is in, and it is how a feature that starts from a
	// channel finds the corpus to use. Several do: the autonomous poster picks a channel and
	// then has to generate from THAT server's text, and image reposting has to stay inside
	// the server an image came from. It comes free from the state cache, which already holds
	// the channel this was built from.
	GuildID string

	// Text reports whether this is a guild text channel. A voice channel can carry
	// messages in Discord's data model and is not somewhere the bot should be talking.
	Text bool
}

// NotSafeForWork reports whether the channel is flagged NSFW or named as such.
//
// The name is checked as well as the flag, because a channel called "nsfw-memes" whose flag
// nobody set is still not somewhere to take media from or post it into.
func (i Info) NotSafeForWork() bool {
	return i.NSFW || strings.Contains(strings.ToLower(i.Name), "nsfw")
}

// Resolver looks a channel up. Declared as an interface so features can be tested with a
// map instead of a gateway connection.
type Resolver interface {
	Channel(id string) (Info, bool)
}

// FromSession returns a Resolver backed by discordgo's state cache.
func FromSession(s *discordgo.Session) Resolver { return stateResolver{s: s} }

type stateResolver struct{ s *discordgo.Session }

func (r stateResolver) Channel(id string) (Info, bool) {
	if r.s == nil || r.s.State == nil {
		return Info{}, false
	}
	ch, err := r.s.State.Channel(id)
	if err != nil || ch == nil {
		return Info{}, false
	}
	return Info{
		ID:      ch.ID,
		Name:    ch.Name,
		NSFW:    ch.NSFW,
		GuildID: ch.GuildID,
		Text:    ch.Type == discordgo.ChannelTypeGuildText,
	}, true
}

// Counter reports how much traffic a channel has seen, busiest first.
// internal/activity's Tracker satisfies it.
//
// The interface names activity.Channel rather than a local type, which costs this package an
// import and buys the Tracker its independence: Go requires exact type identity on an
// interface method's results, so a local Ranked type would have forced an adapter in
// cmd/bot whose only job was renaming two fields.
type Counter interface {
	Busiest(window time.Duration) []activity.Channel
}

// Busiest returns the ID of the liveliest channel the bot may speak in unprompted, or "".
//
// Two callers want this: autonomous posting and interval-mode word games. It was inline in
// the first, so the second would have had to copy it, and a copy is how the two would
// eventually disagree about what "busiest" means.
//
// allow, when non-empty, restricts the choice, and it filters while CHOOSING rather than
// after. That ordering is the fix for a real bug: the old code scored every channel, picked
// the winner, and then rejected it if it was not on the allowlist, so a bot whose busiest
// channel was not listed posted nothing and logged a rejection every single cycle.
//
// The "general" bonus is preserved. It is a judgement about where a bot is welcome to speak
// unprompted rather than a measurement, and it lives here because both callers should share
// the same judgement.
//
// Returning "" on a cold start is correct rather than a gap. The tracker is empty for the
// first window after a restart, and the obvious fallback, ranking by the state cache's
// LastMessageID, offers recency without volume: a channel whose last message was 59 minutes
// ago would win the choice of where to start talking unprompted.
// guildID, when non-empty, restricts the choice to one server. Interval-mode word games need
// that as of M31: the settings that decide whether to post at all are per guild, so the channel
// the decision produces has to be in the guild whose settings were consulted.
func Busiest(c Counter, r Resolver, window time.Duration, allow []string, guildID string) string {
	best := ""
	bestScore := 0.0
	for _, ranked := range c.Busiest(window) {
		info, ok := r.Channel(ranked.ID)
		if !ok || !info.Text || info.NotSafeForWork() {
			continue
		}
		// The allowlist is read per guild rather than as a flat set, so a list naming one
		// server's channels does not silence the bot in every other server. See Allows.
		//
		// The channel is resolved BEFORE the allowlist test now, where the old order tested
		// membership first: the per-guild reading needs to know which guild this candidate is
		// in, and resolving is a state-cache lookup rather than a request.
		if !Allows(r, allow, info.GuildID, ranked.ID) {
			continue
		}
		if guildID != "" && info.GuildID != guildID {
			continue
		}

		score := float64(ranked.Count)
		if strings.Contains(strings.ToLower(info.Name), "general") {
			score *= 1.5
		}
		// Ties break on channel ID, so the choice does not depend on map iteration order.
		// The tracker sorts its result for the same reason: the version this replaced
		// accumulated from a goroutine per channel, so the order was whichever REST call
		// returned first and one caller picked by index.
		if score > bestScore || (score == bestScore && info.ID < best) {
			bestScore = score
			best = info.ID
		}
	}
	return best
}

// Allows applies a channel allowlist the way a multi-guild bot has to read one, M31b.
//
// # A channel allowlist is per guild, even when it is written in one variable
//
// The lists that decide where the bot may speak unprompted are flat sets of channel IDs from
// the environment, written when the bot was in one server. In a second server none of those IDs
// match, so a straight membership test refuses EVERY channel there: setting a list to bind word
// games to one channel in one guild silently turned them off in every other guild.
//
// So a list is read as a statement about the guilds it MENTIONS. If the operator named channels
// in this guild, only those channels qualify here. If they named none, they said nothing about
// this guild and the feature is unrestricted here, which is the same reading an empty list has
// always had: an operator who has not said where something belongs has not said no.
//
// A channel missing from the state cache resolves to no guild and therefore restricts nothing,
// which is the direction that keeps a cold cache from silently unbinding a live setting: the
// cache is populated before any of these callers run, since all of them are driven by traffic
// or by a ticker that starts after READY.
func Allows(r Resolver, allow []string, guildID, channelID string) bool {
	if len(allow) == 0 {
		return true
	}

	var namesThisGuild, namesAnyGuild, any bool
	for _, id := range allow {
		if id == "" {
			// A blank entry is what a trailing comma in the environment produces. Skipped
			// rather than matched, or a stray comma would bind the feature to nothing at all.
			continue
		}
		any = true
		if id == channelID {
			return true
		}
		info, ok := r.Channel(id)
		if !ok || info.GuildID == "" {
			continue
		}
		namesAnyGuild = true
		if info.GuildID == guildID {
			namesThisGuild = true
		}
	}

	switch {
	case !any:
		// Every entry was blank, which is the same as no list.
		return true
	case namesThisGuild:
		// The operator named channels here and this is not one of them.
		return false
	case !namesAnyGuild:
		// Nothing in the list resolves to a guild at all, so there is no per-guild reading to
		// make and this falls back to plain membership, which is what it did before M31b. That
		// is the safe direction rather than the tidy one: an unresolvable list is a cold state
		// cache or a stale ID, and treating either as "no opinion" would quietly UNBIND a live
		// setting.
		return false
	default:
		// The list names other guilds and not this one, so it says nothing about here.
		return true
	}
}

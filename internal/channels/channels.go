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
		ID:   ch.ID,
		Name: ch.Name,
		NSFW: ch.NSFW,
		Text: ch.Type == discordgo.ChannelTypeGuildText,
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
func Busiest(c Counter, r Resolver, window time.Duration, allow []string) string {
	allowed := make(map[string]struct{}, len(allow))
	for _, id := range allow {
		if id != "" {
			allowed[id] = struct{}{}
		}
	}

	best := ""
	bestScore := 0.0
	for _, ranked := range c.Busiest(window) {
		if len(allowed) > 0 {
			if _, ok := allowed[ranked.ID]; !ok {
				continue
			}
		}
		info, ok := r.Channel(ranked.ID)
		if !ok || !info.Text || info.NotSafeForWork() {
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

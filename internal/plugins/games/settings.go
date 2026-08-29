package games

import (
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/6586x57890143/peregrine/internal/channels"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// Runtime settings: where puzzles may run, how they start, and how often.
//
// These three used to live only in the environment, so moving word games to another channel or
// switching the trigger was editing .env and redeploying, which is the wrong lifetime for a
// setting an operator wants to change while people are in the channel. They are a blob in
// BlobConfig now, the same way aggro persists its target.
//
// # The environment supplies the DEFAULTS, and only once
//
// A corpus with no stored settings is seeded from Options on the first Init and never again.
// That is deliberate but it is also the "knob wired to nothing" trap from CLAUDE.md pointing the
// other way: once the operator has used the command, PEREGRINE_WORDGAME_CHANNELS,
// PEREGRINE_WORDGAME_FREQUENCY_MODE and PEREGRINE_WORDGAME_INTERVAL stop having any effect, and
// somebody editing .env during an incident would watch nothing happen. So Init LOGS which of the
// two sources won, and /wordgame-config reset:true writes the environment's values back over the
// stored ones. That is a restart plus a reset to pick up an edited .env, not a reset alone, and
// the log line says which source the running bot is on.
type settings struct {
	// Channels is the allowlist. Empty means anywhere, because an operator who has not said
	// where games belong has not said no.
	Channels []string `json:"channels"`

	Mode     Mode          `json:"mode"`
	Interval time.Duration `json:"interval"`

	// StarterRoles and StarterUsers are who may start a game besides an administrator. Empty
	// means administrators only, which is what every guild had before this existed, so an
	// omitted field is the old behaviour rather than a hole.
	//
	// Two slices rather than one mixed list of snowflakes, because rendering needs to know
	// which it is: <@&id> and <@id> are different markup and a wrong guess prints something
	// nobody can act on. omitempty for the reason the leaderboard's streak fields have it, an
	// older blob loads with zero values rather than being refused.
	//
	// There is deliberately no environment variable seeding these. A grant names a role or a
	// person in ONE guild, which is exactly what PEREGRINE_WORDGAME_CHANNELS being a flat
	// global list turned out to get wrong (M31b), and unlike a channel binding there is no
	// single-guild deployment whose .env already holds the answer.
	StarterRoles []string `json:"starterRoles,omitempty"`
	StarterUsers []string `json:"starterUsers,omitempty"`
}

// settingsKey is the blob. One key rather than three, so a change is one write and cannot land
// half applied.
const settingsKey = "wordgameSettings"

// The interval bounds. The same numbers internal/config enforces on the seed value, restated
// here because this package reads no configuration: a stored interval of one second would be the
// bot talking over the conversation, which is the whole reason that minimum exists.
const (
	minInterval = 5 * time.Minute
	maxInterval = 24 * time.Hour
)

// loadSettings restores one guild's stored settings, or seeds them from Options.
//
// Per guild as of M31, in that guild's own corpus under the key it always used. Sharing one
// blob across servers meant an admin binding a channel in their server rebound it in every
// other server the bot was in, which nobody could see and nobody would think to check.
//
// A load failure seeds from Options rather than failing startup, for the reason the leaderboard
// load states: word games are one optional behaviour and exactly one feature failing should
// disable that one. Unlike the leaderboard there is nothing here that is not re-derivable, since
// the environment still holds a usable answer.
func (s *Service) loadSettings(store *storage.Store, guildID string) settings {
	seed := settings{
		Channels: s.opts.AllowChannels,
		Mode:     s.opts.Mode,
		Interval: s.opts.Interval,
	}

	var stored *settings
	if err := store.View(func(r *storage.Reader) error {
		v, err := r.GetBlob(storage.BlobConfig, settingsKey)
		if err != nil || v == nil {
			return err
		}
		var set settings
		if err := json.Unmarshal(v, &set); err != nil {
			return err
		}
		stored = &set
		return nil
	}); err != nil {
		log.Printf("[WARN] Failed to load word-game settings for guild %s, using the "+
			"environment: %v", guildID, err)
	}

	if stored == nil {
		log.Printf("[WORDGAME] Guild %s: settings from the environment: %s. /wordgame-config "+
			"changes them and they are stored from then on.", guildID, seed)
		return seed
	}

	set := *stored
	// Validated on the way in, not only on the way out. A blob written by an older build, or by
	// one whose mode names differed, must not be able to leave the feature in a state no command
	// can produce.
	if set.Mode != ModeActivity && set.Mode != ModeInterval {
		set.Mode = seed.Mode
	}
	set.Interval = min(max(set.Interval, minInterval), maxInterval)
	log.Printf("[WORDGAME] Guild %s: stored settings: %s. The environment supplies these only "+
		"until the first /wordgame-config, so PEREGRINE_WORDGAME_CHANNELS, _FREQUENCY_MODE and "+
		"_INTERVAL are being ignored. /wordgame-config reset:true writes their values back over "+
		"these.", guildID, set)
	return set
}

// String is what the command prints and what Init logs, which is one renderer rather than two
// that can disagree about what the bot is currently doing.
func (s settings) String() string {
	where := "anywhere"
	if len(s.Channels) > 0 {
		// Channel mentions rather than names: Discord renders them as links, they never notify,
		// and the alternative is a resolver lookup per channel for a line nobody reads twice.
		ids := make([]string, len(s.Channels))
		for i, id := range s.Channels {
			ids[i] = "<#" + id + ">"
		}
		where = strings.Join(ids, " ")
	}
	mode := "activity mode"
	if s.Mode == ModeInterval {
		mode = fmt.Sprintf("interval mode every %s", s.Interval)
	}
	return fmt.Sprintf("%s, in %s, %s", mode, where, s.starters())
}

// starters renders the grant list, which is the only part of these settings that is about
// people rather than about the bot.
//
// Mentions rather than names, for the reason the channel list uses them: Discord renders them
// as links, the guard's AllowedMentions means they cannot notify anybody, and the alternative
// is a resolver lookup per entry for a line nobody reads twice.
func (s settings) starters() string {
	if len(s.StarterRoles)+len(s.StarterUsers) == 0 {
		// Said out loud rather than omitted, because "administrators only" is the state an
		// operator is most likely to be checking for and a missing line reads as unknown.
		return "started by administrators only"
	}
	who := make([]string, 0, len(s.StarterRoles)+len(s.StarterUsers))
	for _, id := range s.StarterRoles {
		who = append(who, "<@&"+id+">")
	}
	for _, id := range s.StarterUsers {
		who = append(who, "<@"+id+">")
	}
	return "started by administrators plus " + strings.Join(who, " ")
}

// mention is one target of a grant: a snowflake, and which of the two kinds of thing it names.
//
// The kind is carried rather than derived, because role and user snowflakes come from the same
// space and nothing about an ID says which it is. Discord ships the answer in an interaction's
// resolved data and this is where it is kept.
type mention struct {
	id     string
	isRole bool
}

// String is the markup Discord renders as a link. It never notifies anybody: the guard sets
// AllowedMentions explicitly on every send.
func (m mention) String() string {
	if m.isRole {
		return "<@&" + m.id + ">"
	}
	return "<@" + m.id + ">"
}

// grant adds or removes one role or member from the list MayStart reads.
//
// Idempotent in both directions, so an operator granting somebody twice gets the same list and
// the same answer rather than a duplicate entry and a board that is subtly different.
func (s *settings) grant(m mention, allow bool) {
	list := &s.StarterUsers
	if m.isRole {
		list = &s.StarterRoles
	}
	if !allow {
		*list = slices.DeleteFunc(*list, func(id string) bool { return id == m.id })
		return
	}
	if !slices.Contains(*list, m.id) {
		*list = append(*list, m.id)
	}
}

// update applies a change and stores it.
//
// The mutation happens under the lock and the write does not, because store.Update takes bbolt's
// single writer and holding a mutex across it would block every read of these settings on
// whatever else is writing to the corpus. Same rule as imageURLMutex not wrapping a store.Update.
func (s *Service) update(guildID string, fn func(*settings)) settings {
	st, err := s.state(guildID)
	if err != nil {
		log.Printf("[ERR] No corpus for guild %s, so its word-game settings were not "+
			"changed: %v", guildID, err)
		return settings{}
	}

	s.mu.Lock()
	fn(&st.set)
	set := st.set
	s.mu.Unlock()

	store, err := s.corpora.For(guildID)
	if err == nil {
		var encoded []byte
		if encoded, err = json.Marshal(set); err == nil {
			err = store.Update(func(w *storage.Writer) error {
				return w.PutBlob(storage.BlobConfig, settingsKey, encoded)
			})
		}
	}
	if err != nil {
		// Applied in memory and not persisted, which is the honest outcome: the operator's
		// change works now and is lost on restart, and the log is the only place that can say so.
		log.Printf("[ERR] Word-game settings changed but not persisted, so they revert on "+
			"restart: %v", err)
	}
	return set
}

// snapshot is one guild's settings as a consistent copy, for a reader that needs more than one
// field. A guild whose corpus cannot be reached reads as the environment's defaults, which is
// the quiet direction: activity mode in every channel is what an unconfigured bot does.
func (s *Service) snapshot(guildID string) settings {
	st, err := s.state(guildID)
	if err != nil {
		return settings{Channels: s.opts.AllowChannels, Mode: s.opts.Mode, Interval: s.opts.Interval}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return st.set
}

// allowed reports whether a puzzle may run in a channel.
//
// PEREGRINE_IGNORE_CHANNELS is the guard's denylist and says where the bot must not speak at
// all; this is the allowlist for one feature, so a server that wants puzzles in exactly one
// channel does not have to list every other channel it has.
//
// Read PER GUILD through channels.Allows, which is the M31b fix. Settings are per guild, but a
// guild that has never run /wordgame-config is seeded from PEREGRINE_WORDGAME_CHANNELS, and that
// is one flat list of channel IDs written when the bot was in one server: a straight membership
// test refused every channel in every OTHER server, so binding games to a channel in one guild
// turned them off everywhere else. A list is a statement about the guilds it names.
func (s *Service) allowed(guildID, channelID string) bool {
	return channels.Allows(s.resolver, s.snapshot(guildID).Channels, guildID, channelID)
}

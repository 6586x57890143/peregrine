package games

import (
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

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

// loadSettings restores the stored settings, or seeds them from Options.
//
// A load failure seeds from Options rather than failing startup, for the reason the leaderboard
// load states: word games are one optional behaviour and exactly one feature failing should
// disable that one. Unlike the leaderboard there is nothing here that is not re-derivable, since
// the environment still holds a usable answer.
func (s *Service) loadSettings() {
	seed := settings{
		Channels: s.opts.AllowChannels,
		Mode:     s.opts.Mode,
		Interval: s.opts.Interval,
	}

	var stored *settings
	if err := s.store.View(func(r *storage.Reader) error {
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
		log.Printf("[WARN] Failed to load word-game settings, using the environment: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if stored == nil {
		s.set = seed
		log.Printf("[WORDGAME] Settings from the environment: %s. /wordgame-config changes them "+
			"and they are stored from then on.", seed)
		return
	}
	s.set = *stored
	// Validated on the way in, not only on the way out. A blob written by an older build, or by
	// one whose mode names differed, must not be able to leave the feature in a state no command
	// can produce.
	if s.set.Mode != ModeActivity && s.set.Mode != ModeInterval {
		s.set.Mode = seed.Mode
	}
	s.set.Interval = min(max(s.set.Interval, minInterval), maxInterval)
	log.Printf("[WORDGAME] Stored settings: %s. The environment supplies these only until the "+
		"first /wordgame-config, so PEREGRINE_WORDGAME_CHANNELS, _FREQUENCY_MODE and _INTERVAL "+
		"are being ignored. /wordgame-config reset:true writes their values back over these.", s.set)
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
	if s.Mode == ModeInterval {
		return fmt.Sprintf("interval mode every %s, in %s", s.Interval, where)
	}
	return fmt.Sprintf("activity mode, in %s", where)
}

// update applies a change and stores it.
//
// The mutation happens under the lock and the write does not, because store.Update takes bbolt's
// single writer and holding a mutex across it would block every read of these settings on
// whatever else is writing to the corpus. Same rule as imageURLMutex not wrapping a store.Update.
func (s *Service) update(fn func(*settings)) settings {
	s.mu.Lock()
	fn(&s.set)
	set := s.set
	s.mu.Unlock()

	encoded, err := json.Marshal(set)
	if err == nil {
		err = s.store.Update(func(w *storage.Writer) error {
			return w.PutBlob(storage.BlobConfig, settingsKey, encoded)
		})
	}
	if err != nil {
		// Applied in memory and not persisted, which is the honest outcome: the operator's
		// change works now and is lost on restart, and the log is the only place that can say so.
		log.Printf("[ERR] Word-game settings changed but not persisted, so they revert on "+
			"restart: %v", err)
	}
	return set
}

// snapshot is the settings as one consistent copy, for a reader that needs more than one field.
func (s *Service) snapshot() settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.set
}

// allowed reports whether a puzzle may run in a channel.
//
// PEREGRINE_IGNORE_CHANNELS is the guard's denylist and says where the bot must not speak at
// all; this is the allowlist for one feature, so a server that wants puzzles in exactly one
// channel does not have to list every other channel it has.
func (s *Service) allowed(channelID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.set.Channels) == 0 || slices.Contains(s.set.Channels, channelID)
}

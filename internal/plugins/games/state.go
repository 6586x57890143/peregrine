package games

import (
	"encoding/json"
	"log"
	"time"

	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/wordgame"
)

// Per-guild word-game state, M31.
//
// A weekly leaderboard and a set of word-game settings both belong to ONE server. Before the
// corpus split they were single keys in a single corpus, which meant two guilds shared a board
// (people competing with strangers whose messages they cannot see) and shared a channel
// binding (M30's /wordgame-config in one server silently rebinding another). Each now lives in
// its own guild's corpus, under exactly the keys it used to use, so a corpus carried over from
// a single-guild deployment reads back unchanged.
type guildState struct {
	board *wordgame.Leaderboard
	set   settings

	// lastInterval is when interval mode last posted HERE. Per guild because the interval is,
	// and not persisted for the reason it never was: a restart owing a puzzle for time the bot
	// was not running is not a setting.
	lastInterval time.Time
}

// state returns one guild's board and settings, loading them on first use.
//
// Lazy rather than loaded once at startup, because Init runs before the gateway is open and a
// guild the bot joins at lunchtime still needs a board. The mutex is held across the load,
// which is a corpus read rather than a Discord call: two goroutines racing to create the first
// board for a guild would otherwise both write one, and the loser's wins would vanish.
func (s *Service) state(guildID string) (*guildState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if st, ok := s.guilds[guildID]; ok {
		return st, nil
	}
	store, err := s.corpora.For(guildID)
	if err != nil {
		return nil, err
	}

	st := &guildState{}
	st.board = s.loadBoard(store, guildID)
	st.set = s.loadSettings(store, guildID)
	s.guilds[guildID] = st
	return st, nil
}

// loadBoard reads one guild's leaderboard, or starts a fresh one.
//
// A load failure starts a fresh board rather than failing, but it is the one place this package
// tolerates data loss reluctantly: a week of wins is not re-derivable from anything, unlike the
// corpus around it.
func (s *Service) loadBoard(store *storage.Store, guildID string) *wordgame.Leaderboard {
	var board *wordgame.Leaderboard
	if err := store.View(func(r *storage.Reader) error {
		v, err := r.GetBlob(storage.BlobLeaderboard, "current")
		if err != nil {
			return err
		}
		if v == nil {
			board = wordgame.NewLeaderboard(time.Now())
			return nil
		}
		var loaded wordgame.Leaderboard
		if err := json.Unmarshal(v, &loaded); err != nil {
			return err
		}
		board = &loaded
		return nil
	}); err != nil {
		log.Printf("[WARN] Failed to load the leaderboard for guild %s, starting fresh: %v",
			guildID, err)
		return wordgame.NewLeaderboard(time.Now())
	}

	// A board written before points existed ranks everybody at zero, which would show an empty
	// leaderboard to a server that has been playing all week. Converting on load is the one
	// moment the number is knowable and the board is not yet being read.
	//
	// Here rather than in internal/wordgame because PointsBase is configuration and that
	// package reads none, which is the same reason ScanTopics leaves its filters to health.
	if converted := board.BackfillPoints(s.opts.PointsBase); converted > 0 {
		log.Printf("[LEADERBOARD] Converted %d entries from wins to points at %d each in guild "+
			"%s. This happens once, on the first load after the hint ladder shipped.",
			converted, s.opts.PointsBase, guildID)
	}
	return board
}

// board is one guild's leaderboard, or nil when its corpus cannot be reached.
func (s *Service) board(guildID string) *wordgame.Leaderboard {
	st, err := s.state(guildID)
	if err != nil {
		return nil
	}
	return st.board
}

// saveBoard persists one guild's leaderboard.
func (s *Service) saveBoard(guildID string) error {
	st, err := s.state(guildID)
	if err != nil {
		return err
	}
	store, err := s.corpora.For(guildID)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(st.board)
	if err != nil {
		return err
	}
	return store.Update(func(w *storage.Writer) error {
		return w.PutBlob(storage.BlobLeaderboard, "current", encoded)
	})
}

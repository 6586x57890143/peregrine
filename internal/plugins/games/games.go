// Package games is the Discord half of the word games: announcing puzzles, deleting them,
// recording wins, and the two bang commands.
//
// internal/wordgame owns the game itself and performs no I/O at all: it returns what should
// be said or deleted and this package sends it through the guard. That split is what stops
// a word-game announcement skipping mention suppression or the emit gate, and it is what
// makes the whole game testable without a gateway connection.
//
// # One sweep, not a goroutine per game
//
// Every started game used to spawn up to three goroutines: one to expire it after 60
// seconds and one per announcement to delete it 30 seconds later. None took a context, so
// after shutdown they woke against a closed session and logged failures for a bot that had
// stopped, and the count was bounded only by how often people played. Manager.Expired and
// Manager.DueDeletions are swept by one core.RunLoop here, which makes the sweep
// panic-isolated and context-bound for free.
package games

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/channels"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/wordgame"
)

// Guard is the send chokepoint.
//
// It names discordgo's Message because that is what discordguard returns, and Go requires
// exact type identity on an interface method's results: a local shape would have bought a
// smaller import in exchange for an adapter whose only job was copying an ID.
type Guard interface {
	Send(channelID, content string) (*discordgo.Message, bool)
	SendEmbed(channelID string, embed *discordgo.MessageEmbed) (*discordgo.Message, bool)
	Delete(channelID, messageID string) bool
}

// Mode selects how puzzles start.
type Mode string

const (
	// ModeActivity starts a puzzle when a channel is busy, which reads as participation.
	ModeActivity Mode = "activity"

	// ModeInterval starts one on a timer, which reads as noise, and is why activity is the
	// default.
	ModeInterval Mode = "interval"
)

// Options are the dials.
type Options struct {
	Enabled bool
	Mode    Mode

	// Interval paces ModeInterval. It is a core.Loop's period now, which is what makes the
	// setting mean what its name says: it used to pace the AUTONOMOUS POSTER, so
	// PEREGRINE_WORDGAME_INTERVAL controlled how often the bot said something that was not
	// a word game (SPEC.md section 8, finding 30).
	Interval time.Duration

	// LeaderboardTick is how often the week boundary is checked. Any interval short of a
	// week works, because the check compares boundaries and is idempotent: it catches up
	// rather than having to be observed at the moment the week turns, which is what the NTP
	// version got wrong (finding 17).
	LeaderboardTick time.Duration

	// SweepTick is how often expired games and due deletions are collected. The resolution
	// of a game's timeout is therefore this, so a puzzle can outlive its deadline by up to
	// one tick, which is invisible.
	SweepTick time.Duration

	// ActiveChannelWindow is how recent traffic must be for interval mode to consider a
	// channel worth posting in.
	ActiveChannelWindow time.Duration

	// AllowChannels restricts interval mode, and it is the autonomous-post allowlist:
	// an operator who has said "only speak unprompted in these channels" has said it about
	// this too.
	AllowChannels []string

	// AdminUserID may run !wordgame. Empty refuses everyone.
	AdminUserID string
}

// Service is the feature.
type Service struct {
	store    *storage.Store
	guard    Guard
	manager  *wordgame.Manager
	counter  channels.Counter
	resolver channels.Resolver
	opts     Options

	board *wordgame.Leaderboard

	loops       sync.WaitGroup
	cancelLoops context.CancelFunc
	logger      *slog.Logger
}

// New builds the service.
func New(store *storage.Store, guard Guard, manager *wordgame.Manager,
	counter channels.Counter, resolver channels.Resolver, opts Options) *Service {
	return &Service{
		store: store, guard: guard, manager: manager,
		counter: counter, resolver: resolver, opts: opts,
	}
}

func (s *Service) Name() string { return "games" }

// Init loads the leaderboard.
//
// A load failure starts a fresh board rather than failing startup, but it is the one place
// this package tolerates data loss reluctantly: a week of wins is not re-derivable from
// anything, unlike the corpus.
func (s *Service) Init(deps core.Deps) error {
	s.logger = deps.Logger

	if err := s.store.View(func(r *storage.Reader) error {
		v, err := r.GetBlob(storage.BlobLeaderboard, "current")
		if err != nil {
			return err
		}
		if v == nil {
			s.board = wordgame.NewLeaderboard(time.Now())
			return nil
		}
		var board wordgame.Leaderboard
		if err := json.Unmarshal(v, &board); err != nil {
			return err
		}
		s.board = &board
		return nil
	}); err != nil {
		log.Printf("[WARN] Failed to load the leaderboard, starting fresh: %v", err)
		s.board = wordgame.NewLeaderboard(time.Now())
		return nil
	}
	log.Println("[INFO] Leaderboard loaded.")
	return nil
}

// Start launches the sweep, the weekly reset check and, in interval mode, the poster.
func (s *Service) Start(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancelLoops = cancel

	loops := []core.Loop{{
		Name:  "leaderboard-reset",
		Every: s.opts.LeaderboardTick,
		// Immediate, which the NTP version could not usefully be: it only returned true
		// inside one hour on Monday, so a startup check almost always answered no.
		// Comparing week boundaries makes a check at startup the useful one, because that is
		// when a bot returning from downtime notices the week turned while it was off.
		Immediate: true,
		Fn:        func(context.Context) { s.maybeReset() },
	}}

	if s.opts.Enabled {
		loops = append(loops, core.Loop{
			Name:  "wordgame-sweep",
			Every: s.opts.SweepTick,
			Fn:    func(context.Context) { s.sweep() },
		})
		if s.opts.Mode == ModeInterval {
			loops = append(loops, core.Loop{
				Name:  "wordgame-interval",
				Every: s.opts.Interval,
				Fn:    func(context.Context) { s.startInterval() },
			})
		}
	}

	for _, l := range loops {
		core.RunLoop(loopCtx, &s.loops, s.logger, l)
	}
	return nil
}

// Shutdown stops the loops, waits for them, and saves the board.
//
// The save lands here rather than in cmd/bot, which is what puts it strictly before the
// store closes: the registry shuts services down in reverse start order and cmd/bot closes
// the corpus after that returns.
func (s *Service) Shutdown(ctx context.Context) error {
	if s.cancelLoops != nil {
		s.cancelLoops()
	}
	done := make(chan struct{})
	go func() {
		s.loops.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		log.Println("[WARN] Word-game loops still running at the shutdown deadline.")
	}

	if s.board != nil {
		if err := s.save(); err != nil {
			return fmt.Errorf("save leaderboard: %w", err)
		}
	}
	return nil
}

// Guess handles a message that might solve the live puzzle, and reports whether it did.
//
// It does NOT consume the message, and that is a decision rather than an oversight: a guess
// is a real word a real person typed in conversation, and the deletion below is cleanup of a
// puzzle exchange rather than a judgement about the content. Consuming it would stop the bot
// learning from a legitimate message because of a housekeeping decision (SPEC.md section 10).
func (s *Service) Guess(channelID, messageID, content, authorID, displayName string) bool {
	if !s.opts.Enabled || !s.manager.Available() {
		return false
	}

	won, solved := s.manager.Guess(channelID, content)
	if !solved {
		// No win. Start a game if the channel has earned one. The Manager asks the shared
		// activity tracker how busy the channel has been rather than counting for itself.
		if s.manager.MaybeStart(channelID) {
			s.start(channelID)
		}
		return false
	}

	// Through the guard, which matters for a reason that is not obvious: the winner's
	// nickname is interpolated into this string, and a nickname is user-controlled text that
	// can contain a role mention. Even a message the bot composes itself is
	// untrusted-input-shaped.
	if announcement, ok := s.guard.Send(channelID, winnerMessage(displayName, won.Word, time.Since(won.StartedAt))); ok {
		s.manager.DeleteLater(channelID, announcement.ID)
	}

	s.board.AddWin(authorID, displayName)
	if err := s.save(); err != nil {
		log.Printf("[WORDGAME] failed to persist a win: %v", err)
	}

	// Tidy up: the puzzle announcement and the winning guess.
	s.guard.Delete(channelID, won.MessageID)
	s.guard.Delete(channelID, messageID)
	return true
}

// Command handles !wordgame and !leaderboard, and reports whether it recognized one.
//
// The caller decides what recognition means for the message; here it only means the command
// ran, or was refused for a reason the operator can see.
func (s *Service) Command(cmd, channelID, authorID string, names func(userID string) string) bool {
	switch cmd {
	case "!leaderboard":
		// The author's ID reaches the board, which is what makes the eleventh slot possible:
		// showing somebody their own rank requires knowing which of the rows is theirs, and
		// the only thing that identifies them is their ID.
		s.postLeaderboard(channelID, authorID, names)
		return true
	case "!wordgame":
		// Only a command when word games are available at all. With the feature off it is
		// not a command, so the caller treats it as chat, which is what it is. Consuming it
		// would mean the bot silently ignored a message for a feature that is not running.
		if !s.opts.Enabled || !s.manager.Available() {
			return false
		}
		s.startOnRequest(channelID, authorID)
		return true
	}
	return false
}

// Authorized reports whether a user may run an operator command.
//
// It fails CLOSED: an unset PEREGRINE_BOOTSTRAP_ADMIN_USER_ID refuses everyone, never allows
// everyone. Getting that direction wrong on an empty string is how a missing variable turns
// an operator-only command into a public one, and it is the only authorization check in the
// codebase (SPEC.md section 8, finding 19).
func (s *Service) Authorized(userID string) bool {
	if s.opts.AdminUserID == "" {
		return false
	}
	return userID == s.opts.AdminUserID
}

// start begins a puzzle and announces it.
//
// Announcing and recording the message ID are two steps because the ID does not exist until
// the send has happened, and the send can be refused: the guard turns down a paused bot or
// an ignored channel. A game whose announcement was refused is abandoned rather than left as
// an invisible puzzle blocking the channel until it times out.
func (s *Service) start(channelID string) {
	g, err := s.manager.Start(channelID)
	if err != nil {
		// ErrGameInProgress is ordinary and ErrNoDictionary is checked by the caller, so
		// neither is worth more than this.
		return
	}
	msg, ok := s.guard.Send(channelID, scrambleMessage(g.Scrambled))
	if !ok {
		s.manager.Abandon(channelID)
		return
	}
	s.manager.Announced(channelID, msg.ID)
	log.Printf("[WORDGAME] Started a game in channel %s.", channelID)
}

// startOnRequest is !wordgame: the operator asking for a puzzle here, now.
func (s *Service) startOnRequest(channelID, authorID string) {
	if !s.Authorized(authorID) {
		return
	}

	g, err := s.manager.Start(channelID)
	switch {
	case errors.Is(err, wordgame.ErrGameInProgress):
		// Reported to the channel, because the operator asked for something and deserves to
		// know why it did not happen.
		s.guard.Send(channelID, "A word game is already in progress in this channel!")
		return
	case err != nil:
		log.Printf("[WORDGAME] Failed to start a game on request: %v", err)
		return
	}

	msg, ok := s.guard.Send(channelID, scrambleMessage(g.Scrambled))
	if !ok {
		s.manager.Abandon(channelID)
		return
	}
	s.manager.Announced(channelID, msg.ID)
	log.Printf("[WORDGAME] Started a game on request in channel %s.", channelID)
}

// startInterval posts a puzzle on a timer, in interval mode.
//
// It picks the busiest channel the bot can see, for the same reason the activity trigger
// exists at all: a puzzle in a dead channel is the bot talking to itself.
func (s *Service) startInterval() {
	if !s.manager.Available() {
		return
	}
	channelID := channels.Busiest(s.counter, s.resolver, s.opts.ActiveChannelWindow, s.opts.AllowChannels)
	if channelID == "" {
		log.Println("[WORDGAME] No active channel found for an interval game.")
		return
	}
	s.start(channelID)
}

// sweep ends expired puzzles and clears announcements whose time is up.
func (s *Service) sweep() {
	for _, g := range s.manager.Expired() {
		if announcement, ok := s.guard.Send(g.ChannelID, timeUpMessage(g.Word)); ok {
			s.manager.DeleteLater(g.ChannelID, announcement.ID)
		}
		s.guard.Delete(g.ChannelID, g.MessageID)
		log.Printf("[WORDGAME] Game timed out in channel %s.", g.ChannelID)
	}
	for _, p := range s.manager.DueDeletions() {
		s.guard.Delete(p.ChannelID, p.MessageID)
	}
}

// maybeReset clears the weekly tally when the week has turned over.
//
// THE NTP QUERY IS GONE (finding 17). The old version asked pool.ntp.org what day it was,
// hourly, and reset only when the answer was Monday between 00:00 and 00:59 UTC. It went to
// the network for something time.Now() answers; a failed query inside that one-hour window
// skipped the reset for a WHOLE WEEK; and the reset had to be observed within that hour, so
// a restart moved the phase and downtime across Monday morning meant it never happened.
//
// Comparing week boundaries CATCHES UP: a bot that was off all Monday resets on its first
// tick back, because the week it holds is still the old one.
func (s *Service) maybeReset() {
	if s.board == nil || !s.board.MaybeReset(time.Now()) {
		return
	}
	log.Printf("[LEADERBOARD] New week starting %s, leaderboard reset.",
		s.board.WeekStart().Format(time.DateOnly))
	if err := s.save(); err != nil {
		log.Printf("[ERR] Failed to persist the leaderboard reset: %v", err)
	}
}

// postLeaderboard answers !leaderboard with the word-game wins and the chat leaderboard.
//
// Deliberately NOT gated on the feature flag. The chat half reads the stats bucket, which is
// populated on every message regardless of whether the scramble game runs, so the command is
// useful with word games off.
func (s *Service) postLeaderboard(channelID, viewerID string, names func(userID string) string) {
	var chatScores map[string]int
	if err := s.store.View(func(r *storage.Reader) error {
		var err error
		chatScores, err = weeklyScores(r)
		return err
	}); err != nil {
		log.Printf("[LEADERBOARD] Error loading user stats: %v", err)
		s.guard.Send(channelID, "Could not generate the leaderboard.")
		return
	}

	// RANK FIRST, RESOLVE NAMES SECOND, and that order is the entire performance fix.
	//
	// This used to resolve a display name for EVERY user in the week's stats before sorting
	// anything, through an uncached GuildMember REST GET with a User fallback. On a server
	// with two hundred weekly talkers that was two hundred-odd sequential, rate-limited
	// requests to render twenty rows, and the command took long enough that people assumed
	// the bot had ignored them.
	//
	// A rank is one plus the number of people strictly ahead, so it needs no names at all.
	// Only the rows that are actually displayed need one, which is at most eleven per board.
	//
	// It also fixes a real defect rather than only the speed: the old code keyed the board by
	// resolved NAME, so two people with the same nickname merged into one row.
	now := time.Now()
	wins := wordgame.Rank(s.board.Scores(), viewerID, leaderboardRows)
	chat := wordgame.Rank(chatScores, viewerID, leaderboardRows)

	// Memoized across BOTH boards, so somebody who is on the word-game board and the chat
	// board costs one lookup rather than two. In practice that is most of the overlap.
	resolve := memoize(names)
	wins = wins.WithNames(resolve)
	chat = chat.WithNames(resolve)

	embed := leaderboardEmbed(wins, chat, s.board.NextReset(now), leaderboardFooter(wins, chat))
	if _, ok := s.guard.SendEmbed(channelID, embed); !ok {
		// The guard has already logged whether this was a refusal or a failure. Said here as
		// well because a command that produced nothing is a question the operator will be
		// asked, and silence on the reply path is the bug finding 32 was about.
		log.Printf("[LEADERBOARD] the board was not sent in %s", channelID)
	}
}

// memoize wraps a name resolver so each user is looked up at most once.
//
// Not concurrency-safe, and does not need to be: one command invocation resolves its own
// board on one goroutine. A mutex here would be structure with no failure mode behind it.
func memoize(resolve func(userID string) string) func(string) string {
	cache := map[string]string{}
	return func(userID string) string {
		if name, ok := cache[userID]; ok {
			return name
		}
		name := resolve(userID)
		cache[userID] = name
		return name
	}
}

// weeklyScores returns each user's message count for the current week.
//
// It no longer skips keys that are not numeric, because the stats bucket no longer holds a
// non-user key: total_messages_learned is a meta counter. Anything that forgot to skip it
// used to decode an integer as a stat and count a phantom user.
func weeklyScores(r *storage.Reader) (map[string]int, error) {
	all, err := r.AllUserStats()
	if err != nil {
		return nil, err
	}
	start := corpus.StartOfWeekUTC(time.Now())
	scores := make(map[string]int, len(all))
	for userID, stat := range all {
		if !stat.LastTimestamp.Before(start) {
			scores[userID] = int(stat.Count)
		}
	}
	return scores, nil
}

// save persists the board.
//
// json.Marshal is safe to call concurrently with AddWin, because Leaderboard implements
// MarshalJSON and takes its own lock. It was NOT safe before M11a: the mutex was an exported
// field of the marshalled struct and the marshalling ran outside it, so a win landing during
// a save was a concurrent map read and write, which in Go is a fatal runtime error rather
// than a recoverable panic.
func (s *Service) save() error {
	encoded, err := json.Marshal(s.board)
	if err != nil {
		return err
	}
	return s.store.Update(func(w *storage.Writer) error {
		return w.PutBlob(storage.BlobLeaderboard, "current", encoded)
	})
}

func scrambleMessage(scrambled string) string {
	return fmt.Sprintf("✨ **Word Scramble!** ✨\n\nUnscramble this word: **%s**", scrambled)
}

func timeUpMessage(original string) string {
	return fmt.Sprintf("Time is up! The word was **%s**.", original)
}

// winnerMessage announces a solved scramble. The nickname is interpolated into it, which is
// why it goes through the guard like any other send.
func winnerMessage(winner, word string, solveTime time.Duration) string {
	return fmt.Sprintf("🎉 **%s** guessed the word **%s** in %.2f seconds!", winner, word, solveTime.Seconds())
}

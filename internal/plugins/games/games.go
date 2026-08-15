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
	"strconv"
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
	Edit(channelID, messageID, content string) bool
	Delete(channelID, messageID string) bool

	// The interaction half, M26. Respond is a gated send like every other method here;
	// RegisterCommands is a write routed through the guard so it is logged and has one home.
	Respond(i *discordgo.Interaction, content string, ephemeral bool) bool
	RegisterCommands(appID string, commands []*discordgo.ApplicationCommand) bool
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

	// AdminUserID may run !wordgame. Empty refuses everyone who is not a guild administrator.
	AdminUserID string

	// PointsBase is what a puzzle solved with no hints showing is worth, and every delivered
	// rung of the hint ladder takes one off that.
	//
	// Config refuses a base at or below the rung count, naming both, because a ladder that can
	// reach zero means its bottom rungs are free and the wait-or-guess decision the ladder
	// exists to create does not exist. Same shape as the HINT_AFTER/TIMEOUT check next to it.
	PointsBase int
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

	// The slash command's two dependencies, both filled in by Init from core.Deps. Nil in
	// tests that do not exercise it, which every path here tolerates: a service that needed a
	// gateway to be testable would be the shape internal/wordgame exists to avoid.
	session    *discordgo.Session
	dispatcher *core.Dispatcher
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
	s.session = deps.Session
	s.dispatcher = deps.Dispatcher

	// Registered HERE rather than in Start, for the reason chat.Init states about its own
	// handlers: discordgo begins dispatching inside session.Open, and Open happens between
	// Init and Start, so a handler registered in Start drops everything that arrived in that
	// window. For an interaction that means a command that silently never answers.
	if s.session != nil {
		s.session.AddHandler(s.onInteraction)
	}

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
	// A board written before points existed ranks everybody at zero, which would show an empty
	// leaderboard to a server that has been playing all week. Converting on load is the one
	// moment the number is knowable and the board is not yet being read.
	//
	// Here rather than in internal/wordgame because PointsBase is configuration and that
	// package reads none, which is the same reason ScanTopics leaves its filters to health.
	if converted := s.board.BackfillPoints(s.opts.PointsBase); converted > 0 {
		log.Printf("[LEADERBOARD] Converted %d entries from wins to points at %d each. This "+
			"happens once, on the first load after the hint ladder shipped.",
			converted, s.opts.PointsBase)
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

	// After READY, because it is a REST call and this is the first moment the bot's own
	// application ID is knowable. Registration is idempotent and the set is a bulk overwrite,
	// so doing it on every startup is how a renamed or removed command stops being visible.
	if s.opts.Enabled {
		s.registerCommands()
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
	solveTime := time.Since(won.StartedAt)
	points := s.points(won)
	if announcement, ok := s.guard.SendEmbed(channelID, solvedEmbed(displayName, won.Word, solveTime, points)); ok {
		s.manager.DeleteLater(channelID, announcement.ID)
	}

	// The solve time reaches the board now, which is what makes a weekly record possible. It
	// was computed for the announcement and then thrown away.
	//
	// A gauntlet win is an ORDINARY win and is worth exactly what the same puzzle would have
	// been worth on its own. A second scoring economy for runs would mean two ways to reach one
	// leaderboard and a reason to argue about which was the real one.
	s.board.AddWin(authorID, displayName, solveTime, points)
	if err := s.save(); err != nil {
		log.Printf("[WORDGAME] failed to persist a win: %v", err)
	}

	// Tidy up: the puzzle announcement and the winning guess.
	s.guard.Delete(channelID, won.MessageID)
	s.guard.Delete(channelID, messageID)

	// The last round of a run says so. A gauntlet that simply stops is indistinguishable from
	// one that broke, which is the shape finding 32 names: the bot's silence is a feature, the
	// player's inability to tell what it meant is not.
	if won.Rounds > 0 && won.Round == won.Rounds {
		s.guard.SendEmbed(channelID, gauntletDoneEmbed(won.Rounds))
	}
	return true
}

// Command handles !wordgame and !leaderboard, and reports whether it recognized one.
//
// The caller decides what recognition means for the message; here it only means the command
// ran, or was refused for a reason the operator can see.
func (s *Service) Command(cmd, arg, channelID string, who Requester, names func(userID string) string) bool {
	switch cmd {
	case "!leaderboard":
		// The author's ID reaches the board, which is what makes the eleventh slot possible:
		// showing somebody their own rank requires knowing which of the rows is theirs, and
		// the only thing that identifies them is their ID.
		s.postLeaderboard(channelID, who.UserID, names)
		return true
	case "!wordgame":
		// Only a command when word games are available at all. With the feature off it is
		// not a command, so the caller treats it as chat, which is what it is. Consuming it
		// would mean the bot silently ignored a message for a feature that is not running.
		if !s.opts.Enabled || !s.manager.Available() {
			return false
		}
		s.startOnRequest(channelID, who, arg)
		return true
	}
	return false
}

// Requester is who is asking, and what Discord says they may do where they are asking it.
//
// A struct rather than two arguments because it crosses three packages (chat resolves it, games
// judges it, and M26's interaction handler will fill it from a different source), and a bare
// pair of a string and an int64 is exactly the shape a caller eventually passes in the wrong
// order.
//
// Permissions is Discord's COMPUTED permission set for this user in this channel, already
// folded over roles and overwrites. Zero means "we could not tell", which is why it fails
// closed below rather than being treated as a plain user.
type Requester struct {
	UserID      string
	Permissions int64
}

// Authorized reports whether a user may run an operator command.
//
// It fails CLOSED: an unset PEREGRINE_BOOTSTRAP_ADMIN_USER_ID and no Administrator bit refuses
// everyone, never allows everyone. Getting that direction wrong on an empty string is how a
// missing variable turns an operator-only command into a public one, and it is the only
// authorization check in the codebase (SPEC.md section 8, finding 19).
//
// # Two ways in, still one check
//
// The bootstrap admin ID was the only answer until M25, and it is a single person: a bot whose
// word games can only be started by whoever holds one Discord ID is a bot whose word games stop
// when that person is asleep. Anyone Discord already trusts with Administrator in the guild is
// somebody the server has made that decision about, and deferring to it is strictly better than
// this bot maintaining a second, worse opinion about who runs the server.
//
// It stays ONE function. The failure mode a second copy has is the empty case, which fails OPEN,
// and no behavioural test can cover the command nobody has written yet: that is the whole
// argument the AST test pinning this exists to enforce.
func (s *Service) Authorized(r Requester) bool {
	if r.UserID != "" && r.UserID == s.opts.AdminUserID {
		return true
	}
	return r.Permissions&discordgo.PermissionAdministrator != 0
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
	if !s.announce(g) {
		// Abandoned HERE rather than inside announce, because only the start path wants that:
		// an unannounced puzzle is invisible and would block the channel until it timed out
		// against something nobody ever saw, whereas a refused hint leaves a playable game.
		s.manager.Abandon(channelID)
		return
	}
	log.Printf("[WORDGAME] Started a game in channel %s.", channelID)
}

// announce posts a puzzle's card and records the message it landed as, replacing whatever card
// the game had before.
//
// # Post the new one, THEN delete the old one
//
// The order is the whole safety of the repost. Deleting first opens a window in which a live
// puzzle has no visible card at all, and if the send is then refused or rate-limited the window
// never closes: the game runs to its timeout with nobody able to see what they were supposed to
// be solving. Posting first costs at most a moment of two cards, which is untidy and playable.
// Same shape as backup writing a temp name before renaming it.
//
// The superseded ID comes back from Announced rather than being remembered here, because a win
// or an expiry can land between the send and the record, and the Manager holds the only
// authoritative answer to which card a game is currently wearing.
//
// # It does NOT abandon the game on a refusal, and the caller must decide
//
// This function serves two moments that want opposite things from a refused send. A puzzle whose
// FIRST card was refused is invisible and has to be abandoned, or it blocks the channel until it
// times out against something nobody ever saw. A puzzle whose HINT was refused still has its
// original card up and is perfectly playable, so abandoning it would delete a live game because
// a decoration failed to render. Folding the abandon in here got that second case wrong, and a
// test asking what a refused hint costs the winner is what found it.
func (s *Service) announce(g *wordgame.Game) bool {
	msg, ok := s.guard.SendEmbed(g.ChannelID, puzzleEmbed(g, s.points(g)))
	if !ok {
		return false
	}
	if old := s.manager.Announced(g.ChannelID, msg.ID); old != "" {
		s.guard.Delete(g.ChannelID, old)
	}
	return true
}

// points is what solving this puzzle right now is worth: the base, less one for every rung of
// the ladder that has actually been delivered, and never less than one.
//
// A DELIVERED rung, not a due one. The distinction is the player's money and is why DueHints
// stopped advancing the level itself: a hint the guard refused to post never reached the
// channel, and charging for help nobody could see is the kind of unfairness that is invisible
// from the log and obvious to whoever lost the point.
func (s *Service) points(g *wordgame.Game) int {
	return max(s.opts.PointsBase-g.HintLevel, 1)
}

// startGauntlet queues a run and starts its first puzzle.
//
// The first one starts here and every later one from the sweep, which is what makes the run
// advance on the previous puzzle CONCLUDING rather than on a clock.
func (s *Service) startGauntlet(channelID string, n int) {
	queued, err := s.manager.Queue(channelID, n)
	switch {
	case errors.Is(err, wordgame.ErrGauntletInProgress):
		remaining, total := s.manager.Gauntlet(channelID)
		s.guard.Send(channelID, fmt.Sprintf(
			"a gauntlet is already running here: %d of %d to go", remaining, total))
		return
	case err != nil:
		log.Printf("[WORDGAME] Failed to queue a gauntlet in %s: %v", channelID, err)
		return
	}

	if queued < n {
		// Said, rather than silently clamped. An operator who asked for fifty and got ten
		// should learn that from the bot rather than by counting.
		s.guard.Send(channelID, fmt.Sprintf(
			"gauntlet of %d starting (%d is the most I queue at once)", queued, queued))
	}
	log.Printf("[WORDGAME] Gauntlet of %d queued in channel %s.", queued, channelID)
	s.start(channelID)
}

// startOnRequest is !wordgame: the operator asking for a puzzle here, now, optionally on a
// word they chose.
//
// # The refusal is logged, and it was not
//
// This used to `return` on an unauthorized caller with no log line and no reply, which is
// indistinguishable from the bot being broken. The operator case is the one that mattered:
// with PEREGRINE_BOOTSTRAP_ADMIN_USER_ID unset, Authorized fails closed and refuses EVERYONE
// including the person who deployed the bot, and the only evidence was a command that did
// nothing. That is finding 32's shape in a command rather than in the reply path: the bot
// staying quiet is the design, the operator being unable to tell why is the bug.
//
// It stays silent IN THE CHANNEL, which is a different decision from staying silent in the
// log. Answering a non-admin advertises that the command exists and that they are not allowed
// to use it, which is an invitation. The message is still consumed either way.
func (s *Service) startOnRequest(channelID string, who Requester, arg string) {
	if !s.Authorized(who) {
		if s.opts.AdminUserID == "" {
			log.Printf("[WORDGAME] !wordgame in %s was refused because "+
				"PEREGRINE_BOOTSTRAP_ADMIN_USER_ID is unset and the caller is not a guild "+
				"administrator, so the check fails closed and refuses everyone. Set it to "+
				"your own Discord user ID, or give yourself Administrator.", channelID)
			return
		}
		log.Printf("[WORDGAME] !wordgame in %s from %s was refused: not the configured admin "+
			"and not a guild administrator.", channelID, who.UserID)
		return
	}

	// A number is a gauntlet, a word is a planted puzzle. commandFor has already established
	// that the argument is one or the other, so this only has to tell them apart.
	if n, ok := gauntletArg(arg); ok {
		s.startGauntlet(channelID, n)
		return
	}

	g, err := s.manager.StartWord(channelID, arg)
	switch {
	case errors.Is(err, wordgame.ErrGameInProgress):
		// Reported to the channel, because the operator asked for something and deserves to
		// know why it did not happen.
		s.guard.Send(channelID, "a game is already running here")
		return
	case errors.Is(err, wordgame.ErrUnusableWord):
		// Also reported, and it names the rules rather than saying no. The operator typed a
		// word and the interesting information is which rule it broke.
		minLen, maxLen := s.manager.WordBounds()
		s.guard.Send(channelID, fmt.Sprintf(
			"cannot scramble that. a puzzle word is %d to %d letters, letters only, and "+
				"needs at least two different ones", minLen, maxLen))
		return
	case err != nil:
		log.Printf("[WORDGAME] Failed to start a game on request: %v", err)
		return
	}

	if !s.announce(g) {
		s.manager.Abandon(channelID)
		// Said out loud, because the guard refusing this is the other way an operator command
		// silently does nothing: an ignored channel or a paused bot, neither of which is
		// visible from the empty channel.
		log.Printf("[WORDGAME] A requested game in %s was abandoned: the guard refused the "+
			"announcement, so the channel is on PEREGRINE_IGNORE_CHANNELS or writes are paused.",
			channelID)
		return
	}
	if g.Planted {
		log.Printf("[WORDGAME] Started a game on request in channel %s on a chosen word.", channelID)
		return
	}
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

// sweep ends expired puzzles, reveals hints and clears announcements whose time is up.
//
// One loop for every deadline this feature has. A hint is not a fourth timer: it is another
// thing the tick already running notices, which is what keeps M11a's "one sweep, not a
// goroutine per game" true as the feature grows.
func (s *Service) sweep() {
	// Hints first, so a puzzle that is both due a hint and about to expire does not get one
	// after the answer has been announced.
	//
	// # A REPOST, where M21b deliberately chose an edit
	//
	// That decision was made for a stated reason: an edit puts the hint "where people are
	// already looking". It holds exactly as long as the announcement is still on screen, and in
	// the channels this bot lives in it is not. An edit to a card that scrolled away twenty
	// messages ago is invisible, so the feature meant to rescue a stalling puzzle did nothing
	// at the only moment it was needed. Reposting surfaces it. The cost is a deleted message
	// per rung, which is why the old card is removed rather than left as a duplicate.
	//
	// Reversing a documented decision, so: if this is ever changed back, the thing to answer is
	// how a hint reaches somebody who has scrolled past the puzzle.
	for _, g := range s.manager.DueHints() {
		// The game arrives already carrying the rung on offer, so the card renders with the
		// hint on it. Committing that rung is a separate call, made only once it has landed.
		level := g.HintLevel
		if !s.announce(g) {
			// Not acknowledged, so the rung stays undelivered and the player is not charged
			// for it. It will come due again on the next tick.
			log.Printf("[WORDGAME] Could not repost a hint in channel %s.", g.ChannelID)
			continue
		}
		s.manager.HintDelivered(g.ChannelID, level)
	}

	for _, g := range s.manager.Expired() {
		if announcement, ok := s.guard.SendEmbed(g.ChannelID, timeoutEmbed(g.Word)); ok {
			s.manager.DeleteLater(g.ChannelID, announcement.ID)
		}
		s.guard.Delete(g.ChannelID, g.MessageID)
		log.Printf("[WORDGAME] Game timed out in channel %s.", g.ChannelID)
	}

	// Gauntlet successors after the expiries, so a puzzle that just timed out can owe its
	// replacement on the same tick rather than waiting a full one, and before the deletions so
	// the two are not both reaching for the announcement of the game that just ended.
	for _, channelID := range s.manager.DueStarts() {
		s.start(channelID)
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
		s.guard.Send(channelID, "could not build the leaderboard")
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

	// The footer's two record holders go through the SAME memoized resolver, so a record held
	// by somebody already on a board costs nothing extra. That is at most two more lookups.
	footer := leaderboardFooter(s.board, wins, chat, resolve)

	embed := leaderboardEmbed(wins, chat, s.board.NextReset(now), footer)
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

// gauntletArg reads "!wordgame 5" and reports the count.
//
// Digits only and nothing else, so it cannot collide with a planted word: commandFor already
// guarantees the argument is one token, and a token is either all letters or all digits by the
// time it arrives here. A leading zero or an absurd number is simply not a count, and the
// Manager clamps what it accepts, so the parse is allowed to be this small.
func gauntletArg(arg string) (int, bool) {
	if arg == "" {
		return 0, false
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

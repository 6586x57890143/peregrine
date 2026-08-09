package games

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"io"
	"log/slog"

	"github.com/6586x57890143/peregrine/internal/activity"
	"github.com/6586x57890143/peregrine/internal/channels"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/wordgame"
)

func snowflake(n int) string {
	return strconv.FormatUint((uint64(n)<<22)|1, 10)
}

type fakeGuard struct {
	mu      sync.Mutex
	sent    []string
	deletes []string
	refuse  bool
}

func (g *fakeGuard) Send(_, content string) (*discordgo.Message, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refuse {
		return nil, false
	}
	g.sent = append(g.sent, content)
	return &discordgo.Message{ID: snowflake(870000 + len(g.sent))}, true
}

func (g *fakeGuard) Delete(_, messageID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deletes = append(g.deletes, messageID)
	return true
}

func (g *fakeGuard) posts() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.sent...)
}

func (g *fakeGuard) deleted() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.deletes...)
}

type fakeChannels map[string]channels.Info

func (f fakeChannels) Channel(id string) (channels.Info, bool) {
	info, ok := f[id]
	return info, ok
}

func fixture(t *testing.T, opts Options) (*Service, *fakeGuard, *wordgame.Manager, *activity.Tracker) {
	t.Helper()
	return fixtureWithTimeout(t, opts, time.Minute)
}

// fixtureWithTimeout is fixture with a chosen puzzle timeout, so the sweep can be tested by
// making a game expire rather than by reaching into the Manager's clock. A test-only setter on
// the Manager would be production API that exists for tests.
func fixtureWithTimeout(t *testing.T, opts Options, timeout time.Duration) (*Service, *fakeGuard, *wordgame.Manager, *activity.Tracker) {
	t.Helper()

	dict, err := wordgame.LoadDictionary("", wordgame.DictionaryOptions{MinLength: 5, MaxLength: 12})
	if err != nil {
		t.Fatalf("LoadDictionary: %v", err)
	}
	tracker := activity.New(activity.Options{})
	manager := wordgame.NewManager(dict, nil, tracker, wordgame.Options{
		Timeout:           timeout,
		AnnounceTTL:       30 * time.Second,
		ActivityWindow:    5 * time.Minute,
		ActivityThreshold: 5,
		TriggerChance:     0, // never by chance: a fixture that started puzzles at random
		// would make unrelated tests flaky in a way nobody would attribute to word games.
	})
	guard := &fakeGuard{}
	chans := fakeChannels{"c1": {ID: "c1", Name: "memes", Text: true}}

	s := New(dbtest.Store(t), guard, manager, tracker, chans, opts)
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s, guard, manager, tracker
}

func enabled() Options {
	return Options{
		Enabled:             true,
		Mode:                ModeActivity,
		Interval:            30 * time.Minute,
		LeaderboardTick:     time.Hour,
		SweepTick:           5 * time.Second,
		ActiveChannelWindow: time.Hour,
		AdminUserID:         snowflake(1),
	}
}

// ---------------------------------------------------------------- finding 19

// TestAuthorizedFailsClosed. An unset PEREGRINE_BOOTSTRAP_ADMIN_USER_ID must refuse everyone,
// never allow everyone: getting that direction wrong on an empty string turns a missing
// variable into a public operator command.
//
// Verified by reverting: with the empty check removed, an empty user ID matches an empty admin
// ID and the command becomes available to anyone.
func TestAuthorizedFailsClosed(t *testing.T) {
	opts := enabled()
	opts.AdminUserID = ""
	s, _, _, _ := fixture(t, opts)

	for _, id := range []string{"", snowflake(1), "anyone"} {
		if s.Authorized(id) {
			t.Errorf("Authorized(%q) = true with no admin configured; it must fail closed", id)
		}
	}

	opts.AdminUserID = snowflake(77)
	s2, _, _, _ := fixture(t, opts)
	if !s2.Authorized(snowflake(77)) {
		t.Error("the configured admin was refused")
	}
	if s2.Authorized(snowflake(78)) {
		t.Error("a non-admin was authorized")
	}
	if s2.Authorized("") {
		t.Error("an empty user ID was authorized against a configured admin")
	}
}

// TestWordgameOnRequestNeedsAuthorization, which is the only authorization check the bot has.
func TestWordgameOnRequestNeedsAuthorization(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())

	if consumed := s.Command("!wordgame", "c1", snowflake(999), noNames); !consumed {
		t.Error("an unauthorized !wordgame was not consumed; it is still a command rather than " +
			"something to reply to")
	}
	if posts := guard.posts(); len(posts) != 0 {
		t.Errorf("an unauthorized !wordgame started a game: %v", posts)
	}

	if consumed := s.Command("!wordgame", "c1", snowflake(1), noNames); !consumed {
		t.Error("an authorized !wordgame was not consumed")
	}
	if posts := guard.posts(); len(posts) != 1 || !strings.Contains(posts[0], "Unscramble") {
		t.Errorf("posts = %v, want a scramble announcement", posts)
	}
}

// ---------------------------------------------------------------- the game

// TestAnnouncingIsTwoStepsSoARefusalDoesNotBlockTheChannel.
//
// The message ID does not exist until the send has happened, and the send can be refused: the
// guard turns down a paused bot or an ignored channel. A game whose announcement was refused is
// abandoned rather than left as an invisible puzzle blocking the channel until it times out.
func TestAnnouncingIsTwoStepsSoARefusalDoesNotBlockTheChannel(t *testing.T) {
	s, guard, manager, _ := fixture(t, enabled())
	guard.refuse = true

	s.start("c1")
	if manager.Active() != 0 {
		t.Error("a game whose announcement was refused is still live, so the channel is blocked " +
			"by a puzzle nobody can see")
	}

	guard.refuse = false
	s.start("c1")
	if manager.Active() != 1 {
		t.Error("a game was not started once the announcement went through")
	}
}

// TestAWinAnnouncesDeletesAndScores.
func TestAWinAnnouncesDeletesAndScores(t *testing.T) {
	s, guard, manager, _ := fixture(t, enabled())

	g, err := manager.Start("c1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	manager.Announced("c1", snowflake(500))

	const guessID = "1600000000000000001"
	if !s.Guess("c1", guessID, g.Word, snowflake(42), "winner") {
		t.Fatal("a correct guess was not recognized as a win")
	}

	posts := guard.posts()
	if len(posts) != 1 || !strings.Contains(posts[0], "winner") {
		t.Errorf("posts = %v, want a win announcement naming the winner", posts)
	}
	// The puzzle and the guess both go, which is the tidy-up the SPEC section 10 decision is
	// about: the guess is still learned, because it is a real word a real person typed.
	deleted := guard.deleted()
	if len(deleted) != 2 {
		t.Errorf("deleted %v, want the puzzle and the winning guess", deleted)
	}
}

// TestAWrongGuessIsNotAWin, and does not delete anything.
func TestAWrongGuessIsNotAWin(t *testing.T) {
	s, guard, manager, _ := fixture(t, enabled())

	if _, err := manager.Start("c1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.Guess("c1", snowflake(501), "definitelynottheword", snowflake(42), "player") {
		t.Error("a wrong guess was treated as a win")
	}
	if deleted := guard.deleted(); len(deleted) != 0 {
		t.Errorf("deleted %v for a wrong guess", deleted)
	}
}

// TestGuessesAreIgnoredWithTheFeatureOff, so a channel is not silently having its messages
// compared against a puzzle that cannot exist.
func TestGuessesAreIgnoredWithTheFeatureOff(t *testing.T) {
	opts := enabled()
	opts.Enabled = false
	s, _, manager, _ := fixture(t, opts)

	g, err := manager.Start("c1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.Guess("c1", snowflake(502), g.Word, snowflake(42), "player") {
		t.Error("a guess was processed with word games disabled")
	}
}

// TestTheSweepEndsExpiredGames rather than a timer per game. Every started game used to spawn
// up to three goroutines that slept and then acted, none of which took a context, so after
// shutdown they woke against a closed session.
func TestTheSweepEndsExpiredGames(t *testing.T) {
	// A long timeout first, so "nothing has expired" is observable.
	s, guard, manager, _ := fixture(t, enabled())
	if _, err := manager.Start("c1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	manager.Announced("c1", snowflake(503))
	s.sweep()
	if posts := guard.posts(); len(posts) != 0 {
		t.Errorf("the sweep ended a game before its timeout: %v", posts)
	}

	// And a puzzle that is already over by the time the sweep runs. A millisecond timeout plus
	// a short wait rather than a nanosecond one: Windows' clock resolution is coarse enough
	// that a nanosecond deadline is not reliably in the past by the next statement.
	s, guard, manager, _ = fixtureWithTimeout(t, enabled(), time.Millisecond)
	g, err := manager.Start("c1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	manager.Announced("c1", snowflake(504))
	time.Sleep(25 * time.Millisecond)
	s.sweep()

	posts := guard.posts()
	if len(posts) != 1 || !strings.Contains(posts[0], g.Word) {
		t.Errorf("posts = %v, want a time-up message naming the word", posts)
	}
	if manager.Active() != 0 {
		t.Error("the expired game is still live after a sweep")
	}
}

// ---------------------------------------------------------------- the leaderboard

// TestTheWeeklyResetCatchesUp is the pin for finding 17.
//
// The old check asked pool.ntp.org what day it was and reset only when the answer was Monday
// between 00:00 and 00:59 UTC. A failed query in that one-hour window skipped the reset for a
// week, and downtime across Monday morning skipped it entirely. Comparing week boundaries
// catches up instead: a bot that was off all Monday resets on its first tick back.
func TestTheWeeklyResetCatchesUp(t *testing.T) {
	s, _, _, _ := fixture(t, enabled())

	s.board.AddWin(snowflake(42), "winner")
	if got := len(s.board.Entries()); got != 1 {
		t.Fatalf("the board holds %d entries, want the win just recorded", got)
	}

	// A week and a half later, which is a bot that was off across a Monday.
	future := s.board.WeekStart().AddDate(0, 0, 10)
	if !s.board.MaybeReset(future) {
		t.Fatal("the board did not reset for a week that had clearly turned")
	}
	if got := len(s.board.Entries()); got != 0 {
		t.Errorf("the board holds %d entries after a reset, want 0", got)
	}
}

// TestTheLeaderboardCommandWorksWithTheGameOff.
//
// Deliberately not gated on the feature flag: the chat half reads the stats bucket, which is
// populated on every message regardless of whether the scramble game runs.
func TestTheLeaderboardCommandWorksWithTheGameOff(t *testing.T) {
	opts := enabled()
	opts.Enabled = false
	s, guard, _, _ := fixture(t, opts)

	if consumed := s.Command("!leaderboard", "c1", snowflake(42), noNames); !consumed {
		t.Error("!leaderboard was not consumed with word games off")
	}
	if posts := guard.posts(); len(posts) != 1 {
		t.Errorf("posts = %v, want one leaderboard message", posts)
	}
}

// TestAnUnknownCommandIsNotConsumed, so ordinary chat is not swallowed.
func TestAnUnknownCommandIsNotConsumed(t *testing.T) {
	s, _, _, _ := fixture(t, enabled())
	if s.Command("!nonsense", "c1", snowflake(42), noNames) {
		t.Error("an unrecognized command was consumed")
	}
}

// TestIntervalModePicksAChannelWithTraffic. A puzzle in a dead channel is the bot talking to
// itself, which is the same reason the activity trigger exists at all.
func TestIntervalModePicksAChannelWithTraffic(t *testing.T) {
	opts := enabled()
	opts.Mode = ModeInterval
	s, guard, _, tracker := fixture(t, opts)

	// Nothing tracked yet: a cold start has nowhere to post.
	s.startInterval()
	if posts := guard.posts(); len(posts) != 0 {
		t.Errorf("posted a puzzle with no active channel: %v", posts)
	}

	for range 5 {
		tracker.Note("c1", snowflake(42))
	}
	s.startInterval()
	if posts := guard.posts(); len(posts) != 1 || !strings.Contains(posts[0], "Unscramble") {
		t.Errorf("posts = %v, want a scramble in the channel with traffic", posts)
	}
}

func noNames(userID string) string { return userID }

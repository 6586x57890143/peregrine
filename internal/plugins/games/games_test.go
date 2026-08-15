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
	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/wordgame"
)

func snowflake(n int) string {
	return strconv.FormatUint((uint64(n)<<22)|1, 10)
}

type fakeGuard struct {
	mu      sync.Mutex
	sent    []string
	embeds  []*discordgo.MessageEmbed
	deletes []string
	edited  map[string]string
	refuse  bool

	responses     []response
	registered    []*discordgo.ApplicationCommand
	registeredApp string
}

func (g *fakeGuard) Edit(_, messageID, content string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refuse {
		return false
	}
	if g.edited == nil {
		g.edited = map[string]string{}
	}
	g.edited[messageID] = content
	return true
}

func (g *fakeGuard) edits() map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]string, len(g.edited))
	for k, v := range g.edited {
		out[k] = v
	}
	return out
}

func (g *fakeGuard) SendEmbed(_ string, embed *discordgo.MessageEmbed) (*discordgo.Message, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refuse {
		return nil, false
	}
	g.embeds = append(g.embeds, embed)
	// Also recorded as text, so posts() answers "what did the bot say" rather than "which
	// method did it use". The puzzle moved from a plain send to an embed in M25 and every
	// assertion about the words in it is still the assertion worth making; a fake that
	// distinguished them would have turned a rendering change into a test rewrite and taught
	// nobody anything.
	g.sent = append(g.sent, flatten(embed))
	return &discordgo.Message{ID: snowflake(880000 + len(g.embeds))}, true
}

// flatten is every rendering field of an embed, in the spirit of discordguard's embedText:
// whatever a reader would see.
func flatten(e *discordgo.MessageEmbed) string {
	if e == nil {
		return ""
	}
	parts := []string{e.Title, e.Description}
	for _, f := range e.Fields {
		if f != nil {
			parts = append(parts, f.Name, f.Value)
		}
	}
	if e.Footer != nil {
		parts = append(parts, e.Footer.Text)
	}
	return strings.Join(parts, "\n")
}

func (g *fakeGuard) posted() []*discordgo.MessageEmbed {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]*discordgo.MessageEmbed(nil), g.embeds...)
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

// responded records what the ephemeral half of a slash command said, separately from posts()
// because the whole point of the split is that these two go to different audiences: a test that
// merged them could not tell a public puzzle from a private refusal.
type response struct {
	content   string
	ephemeral bool
}

func (g *fakeGuard) Respond(_ *discordgo.Interaction, content string, ephemeral bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refuse {
		return false
	}
	g.responses = append(g.responses, response{content: content, ephemeral: ephemeral})
	return true
}

func (g *fakeGuard) RegisterCommands(appID string, commands []*discordgo.ApplicationCommand) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.registeredApp = appID
	g.registered = commands
	return true
}

func (g *fakeGuard) responded() []response {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]response(nil), g.responses...)
}

func (g *fakeGuard) Delete(_, messageID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deletes = append(g.deletes, messageID)
	return true
}

// The structural assertions, M27.
//
// Most of the tests here used to ask "did the bot say the word Unscramble", which is really
// asking "did a puzzle go up" and answering it through prose. That made an embed restyle look
// like a dozen regressions, so the checks that are about STRUCTURE now read the embed: the state
// colour is not decoration, it is the one field that says which of four things this card is.
//
// Copy assertions survive only where the copy IS the behaviour: the round numbering, the stake
// being visible before the first hint, and the refusal that names the length bounds.

// maskFragment is what an unrevealed letter looks like in a puzzle message.
//
// The escaped underscore from internal/wordgame's hidden constant, spelled out here rather than
// imported, because it is unexported there and because a test that read the same constant the
// renderer does could not tell a mask from an empty string.
const maskFragment = `\_ \_`

// The structural assertions.
//
// Most of these tests used to ask "did the bot say the word Unscramble", which is really asking
// "did a puzzle go up" and answering it through prose, so a copy change looked like a dozen
// regressions. M27 moved them onto the embed's state colour; M28 took the embed away again, so
// they read the message's PAYLOAD instead: the header line that carries the scramble, the mask
// that only a hinted puzzle has, and the rung counter that only appears once one has landed.
//
// That is a better anchor than either. A colour was decoration the renderer happened to set, and
// prose is wording; these are the things the message exists to convey, so a test asserting on
// them fails when the feature breaks and not when somebody rewrites a sentence.

// puzzles returns every message that is a puzzle announcement, live or hinted.
func (g *fakeGuard) puzzles() []string {
	var out []string
	for _, p := range g.posts() {
		if strings.HasPrefix(p, "## ") {
			out = append(out, p)
		}
	}
	return out
}

// onePuzzle asserts that exactly one puzzle announcement went up, and returns it.
func onePuzzle(t *testing.T, g *fakeGuard) string {
	t.Helper()
	live := g.puzzles()
	if len(live) != 1 {
		t.Fatalf("posted %d puzzle announcements, want 1. All posts:\n%s",
			len(live), strings.Join(g.posts(), "\n---\n"))
	}
	return live[0]
}

// hintedPuzzles are the announcements carrying a revealed mask.
func (g *fakeGuard) hintedPuzzles() []string {
	var out []string
	for _, p := range g.puzzles() {
		if strings.Contains(p, maskFragment) {
			out = append(out, p)
		}
	}
	return out
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

		HintLevels: 3,
		// No gap, so a gauntlet's successor is due the instant the previous puzzle concludes
		// and the sweep can be driven without an injected clock. WHEN the gap applies is pinned
		// in the Manager's own tests, which have one.
		GauntletGap: 0,
		GauntletMax: 10,
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

		// The PRODUCTION default from internal/config, verbatim, so these tests exercise what
		// actually ships. A zero here would floor every solve at one point and quietly make the
		// scoring tests pass for the wrong reason.
		PointsBase: 4,
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
		if s.Authorized(Requester{UserID: id}) {
			t.Errorf("Authorized(%q) = true with no admin configured and no Administrator bit; "+
				"it must fail closed", id)
		}
	}

	opts.AdminUserID = snowflake(77)
	s2, _, _, _ := fixture(t, opts)
	if !s2.Authorized(Requester{UserID: snowflake(77)}) {
		t.Error("the configured admin was refused")
	}
	if s2.Authorized(Requester{UserID: snowflake(78)}) {
		t.Error("a non-admin was authorized")
	}
	if s2.Authorized(Requester{}) {
		t.Error("a zero-value Requester was authorized; \"we could not tell\" has to mean no")
	}
}

// TestAdministratorsMayStartGames is M25's widening, and it is one function wider rather than a
// second check: the failure mode a second copy has is the empty case, which fails OPEN, and no
// behavioural test can cover the command nobody has written yet.
//
// The bootstrap admin ID is a single person, so a bot whose word games only start when one
// specific Discord user is awake is a bot whose word games mostly do not start. Anyone Discord
// already trusts with Administrator is somebody the server has made that decision about.
func TestAdministratorsMayStartGames(t *testing.T) {
	opts := enabled()
	opts.AdminUserID = "" // nobody is the bootstrap admin
	s, _, _, _ := fixture(t, opts)

	admin := Requester{UserID: snowflake(500), Permissions: discordgo.PermissionAdministrator}
	if !s.Authorized(admin) {
		t.Error("a guild administrator was refused with no bootstrap admin set; the whole point " +
			"of the widening is that the server's own answer counts")
	}

	// A permission that is not Administrator is not authorization. Manage Messages is the one
	// worth pinning, because the bot asks for it itself and a bitwise test written with & on the
	// wrong constant would let every moderator through.
	mod := Requester{UserID: snowflake(501), Permissions: discordgo.PermissionManageMessages}
	if s.Authorized(mod) {
		t.Error("Manage Messages authorized an operator command; only Administrator does")
	}
}

// admin builds a Requester that Authorized accepts on the bootstrap-ID path, for the tests
// whose subject is not authorization.
func admin(id string) Requester { return Requester{UserID: id} }

// TestWordgameOnRequestNeedsAuthorization, which is the only authorization check the bot has.
func TestWordgameOnRequestNeedsAuthorization(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())

	if consumed := s.Command("!wordgame", "", "c1", Requester{UserID: snowflake(999)}, noNames); !consumed {
		t.Error("an unauthorized !wordgame was not consumed; it is still a command rather than " +
			"something to reply to")
	}
	if posts := guard.posts(); len(posts) != 0 {
		t.Errorf("an unauthorized !wordgame started a game: %v", posts)
	}

	if consumed := s.Command("!wordgame", "", "c1", admin(snowflake(1)), noNames); !consumed {
		t.Error("an authorized !wordgame was not consumed")
	}
	onePuzzle(t, guard)
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

	s.board.AddWin(snowflake(42), "winner", time.Second, 1)
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

	if consumed := s.Command("!leaderboard", "", "c1", admin(snowflake(42)), noNames); !consumed {
		t.Error("!leaderboard was not consumed with word games off")
	}
	// An embed now, not a code block. The board is deliberately NOT gated on the feature
	// flag: its chat half reads the stats bucket, which is populated whether or not the
	// scramble game runs.
	if embeds := guard.posted(); len(embeds) != 1 {
		t.Errorf("got %d leaderboard embeds, want 1", len(embeds))
	}
}

// TestAnUnknownCommandIsNotConsumed, so ordinary chat is not swallowed.
func TestAnUnknownCommandIsNotConsumed(t *testing.T) {
	s, _, _, _ := fixture(t, enabled())
	if s.Command("!nonsense", "", "c1", admin(snowflake(42)), noNames) {
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
	onePuzzle(t, guard)
}

func noNames(userID string) string { return userID }

// ------------------------------------------------------- the word-game pass, M21b

// TestTheSweepRepostsTheAnnouncementToDeliverAHint.
//
// M21b made this an EDIT for a stated reason: the hint arrives "where people are already
// looking". That holds only while the announcement is still on screen, and in the channels this
// bot lives in it is not, so an edit to a card twenty messages up is invisible and the feature
// meant to rescue a stalling puzzle did nothing at the one moment it mattered. M25 reposts.
//
// The ordering is the part with teeth, and it is asserted below: the new card goes up BEFORE the
// old one comes down.
func TestTheSweepRepostsTheAnnouncementToDeliverAHint(t *testing.T) {
	dict, err := wordgame.LoadDictionary("", wordgame.DictionaryOptions{MinLength: 5, MaxLength: 12})
	if err != nil {
		t.Fatalf("LoadDictionary: %v", err)
	}
	tracker := activity.New(activity.Options{})
	manager := wordgame.NewManager(dict, nil, tracker, wordgame.Options{
		Timeout: time.Minute,
		// Milliseconds, and slept past below. WHEN a hint is due is pinned in the Manager's
		// own tests against an injected clock; this one is about the wiring, and a nanosecond
		// deadline is not reliably in the past on a platform whose clock ticks in
		// milliseconds, which is a flake rather than a finding.
		HintAfter: time.Millisecond,
	})
	guard := &fakeGuard{}
	s := New(dbtest.Store(t), guard, manager, tracker, fakeChannels{}, enabled())
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	s.start("c1")
	posts := guard.posts()
	if len(posts) != 1 {
		t.Fatalf("posts = %v, want the puzzle", posts)
	}

	time.Sleep(20 * time.Millisecond)

	s.sweep()

	posts = guard.posts()
	if len(posts) != 2 {
		t.Fatalf("posts = %d, want the puzzle and then the card that replaced it", len(posts))
	}
	// Structural rather than by wording: a mask is the thing a hint actually adds, and the rung
	// counter only appears once one has landed.
	hinted := guard.hintedPuzzles()
	if len(hinted) != 1 {
		t.Fatalf("posted %d hinted announcements, want 1:\n%s",
			len(hinted), strings.Join(posts, "\n---\n"))
	}
	if !strings.Contains(hinted[0], "hint 1/") {
		t.Errorf("the repost does not say which rung it is:\n%s", hinted[0])
	}
	if got := guard.edits(); len(got) != 0 {
		t.Errorf("the hint was delivered by editing (%v); that is the behaviour M25 replaced, "+
			"and an edit does not reach anybody who has scrolled past the puzzle", got)
	}

	// The superseded card is removed, so the channel is not left showing two live puzzles. It
	// is deleted AFTER the replacement went up, which is the ordering that matters: the other
	// way round, a refused send leaves a live puzzle with nothing to look at until it times out.
	if got := guard.deleted(); len(got) != 1 {
		t.Errorf("deleted = %v, want exactly the one card the repost replaced", got)
	}

	// Once. A second sweep on the same rung would delete the card people are reading and put an
	// identical one back.
	s.sweep()
	if got := guard.posts(); len(got) != 2 {
		t.Errorf("the same rung was delivered twice: %d posts", len(got))
	}
}

// TestAPlantedWordBecomesThePuzzle. !wordgame <word> is the operator choosing the joke rather
// than taking what the dictionary offers.
func TestAPlantedWordBecomesThePuzzle(t *testing.T) {
	s, guard, manager, _ := fixture(t, enabled())

	if consumed := s.Command("!wordgame", "banana", "c1", admin(snowflake(1)), noNames); !consumed {
		t.Fatal("!wordgame with a word was not consumed")
	}
	if manager.Active() != 1 {
		t.Fatal("no game started")
	}
	if _, solved := manager.Guess("c1", "BANANA"); !solved {
		t.Error("the planted word is not the answer")
	}
	if posts := guard.posts(); len(posts) != 1 || strings.Contains(posts[0], "banana") {
		t.Errorf("posts = %v, want a scramble that does not print its own answer", posts)
	}
}

// TestAnUnusableWordIsRefusedOutLoudNamingTheRules.
//
// The operator typed a word, so the interesting information is which rule it broke. Answering
// "no" without saying why is the shape of silence this milestone exists to remove.
func TestAnUnusableWordIsRefusedOutLoudNamingTheRules(t *testing.T) {
	s, guard, manager, _ := fixture(t, enabled())

	if consumed := s.Command("!wordgame", "aaaaa", "c1", admin(snowflake(1)), noNames); !consumed {
		t.Fatal("a refused !wordgame was not consumed; it is still a command")
	}
	if manager.Active() != 0 {
		t.Fatal("a word with one distinct letter started a game, which is what made scramble recurse")
	}
	posts := guard.posts()
	if len(posts) != 1 {
		t.Fatalf("posts = %v, want one refusal", posts)
	}
	// The bounds, so the answer is actionable rather than a shrug.
	if !strings.Contains(posts[0], "5") || !strings.Contains(posts[0], "12") {
		t.Errorf("the refusal does not name the length bounds:\n%s", posts[0])
	}
}

// TestAPlantedWordFromANonAdminStartsNothing. Same authorization as the bare form, and still
// silent in the channel: answering advertises that the command exists.
func TestAPlantedWordFromANonAdminStartsNothing(t *testing.T) {
	s, guard, manager, _ := fixture(t, enabled())

	if consumed := s.Command("!wordgame", "banana", "c1", admin(snowflake(999)), noNames); !consumed {
		t.Error("an unauthorized !wordgame was not consumed")
	}
	if manager.Active() != 0 {
		t.Error("a non-admin planted a word")
	}
	if posts := guard.posts(); len(posts) != 0 {
		t.Errorf("a non-admin was told the command exists: %v", posts)
	}
}

// ------------------------------------------------------- the leaderboard, M21a

// THE REGRESSION THIS ROW EXISTS TO PREVENT.
//
// !leaderboard used to resolve a display name for EVERY user in the week's stats before
// sorting anything, through an uncached GuildMember REST GET with a User fallback. On a server
// with two hundred weekly talkers that was two hundred-odd sequential, rate-limited requests
// to render twenty rows, and it took long enough that people assumed the bot had ignored them.
//
// Counting the resolver calls is the only way to pin it. A behavioural test on the rendered
// board passes just as happily with two hundred lookups behind it.
func TestTheLeaderboardResolvesOnlyTheNamesItRenders(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())

	// Two hundred players on the word-game board, all with distinct scores.
	for i := range 200 {
		id := snowflake(1000 + i)
		for range 200 - i {
			s.board.AddWin(id, "player", time.Second, 1)
		}
	}

	var calls int
	names := func(userID string) string {
		calls++
		return "name-" + userID
	}

	// The viewer sits well outside the top ten, so their own row is resolved too.
	s.Command("!leaderboard", "", "c1", admin(snowflake(1150)), names)

	// Ten rows plus the viewer on the word-game board. The chat board is empty in this
	// fixture, so it needs none.
	const want = 11
	if calls > want {
		t.Errorf("resolved %d names to render %d rows. Before M21a this was one lookup per "+
			"weekly talker, which is the whole reason the command was slow", calls, want)
	}
	if len(guard.posted()) != 1 {
		t.Fatalf("got %d embeds, want 1", len(guard.posted()))
	}
}

// Somebody who appears on both boards is one person and costs one lookup, not two.
func TestANameOnBothBoardsIsResolvedOnce(t *testing.T) {
	s, _, _, _ := fixture(t, enabled())

	viewer := snowflake(500)
	s.board.AddWin(viewer, "player", time.Second, 1)

	// The same person also has chat activity.
	if err := s.store.Update(func(w *storage.Writer) error {
		return w.IncUserStat(viewer, time.Now())
	}); err != nil {
		t.Fatalf("seeding a chat stat: %v", err)
	}

	seen := map[string]int{}
	s.Command("!leaderboard", "", "c1", admin(viewer), func(userID string) string {
		seen[userID]++
		return "name"
	})

	if seen[viewer] != 1 {
		t.Errorf("the viewer was resolved %d times across two boards, want 1", seen[viewer])
	}
}

// The eleventh slot, end to end: somebody at 18th sees 18th.
func TestTheBoardShowsTheViewerTheirOwnRank(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())

	for i := range 30 {
		id := snowflake(2000 + i)
		for range 30 - i {
			s.board.AddWin(id, "player", time.Second, 1)
		}
	}

	viewer := snowflake(2017) // eighteenth by score
	s.Command("!leaderboard", "", "c1", admin(viewer), func(userID string) string { return "user-" + userID })

	embeds := guard.posted()
	if len(embeds) != 1 {
		t.Fatalf("got %d embeds, want 1", len(embeds))
	}
	value := embeds[0].Fields[0].Value
	if !strings.Contains(value, "`18`") {
		t.Errorf("the viewer sits at 18th and the board does not show it:\n%s", value)
	}
	if !strings.Contains(value, "user-"+viewer) {
		t.Errorf("the viewer's own row is missing:\n%s", value)
	}
}

// A viewer with nothing this week is told so rather than left to wonder. A missing row is
// indistinguishable from a bug.
func TestTheBoardTellsAViewerWithNoScoreThatTheyHaveNone(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())
	s.board.AddWin(snowflake(3001), "somebody", time.Second, 1)

	s.Command("!leaderboard", "", "c1", admin(snowflake(9999)), func(userID string) string { return "n" })

	embeds := guard.posted()
	if len(embeds) != 1 {
		t.Fatalf("got %d embeds, want 1", len(embeds))
	}
	// "points", not "wins": the column holds points and that is what the board ranks by. A
	// column of points labelled wins invites the reading that somebody with 12 has won twelve
	// games, which is the misreading the unit argument in renderBoard's doc is about.
	if !strings.Contains(embeds[0].Fields[0].Value, "no points this week") {
		t.Errorf("a viewer with no wins is not told so:\n%s", embeds[0].Fields[0].Value)
	}
}

// The board goes through SendEmbed, which is what applies the emit gate to every text field
// and suppresses mentions. A refused send is reported rather than silently producing nothing.
func TestARefusedBoardIsNotRetriedAsPlainText(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())
	guard.refuse = true

	if consumed := s.Command("!leaderboard", "", "c1", admin(snowflake(1)), noNames); !consumed {
		t.Error("!leaderboard was not consumed when the guard refused it")
	}
	if got := guard.posts(); len(got) != 0 {
		t.Errorf("a refused embed fell back to a plain message: %v", got)
	}
}

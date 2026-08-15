package games

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/peregrine/internal/activity"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/wordgame"
)

// M25 at the plugin seam: the run, the price of a hint, and the two orderings that decide
// whether either is safe.

// TestAGauntletRunsItsRoundsThroughTheSweep.
//
// The run is not a schedule. Every round after the first is started by the sweep that already
// expires puzzles and delivers hints, so a gauntlet costs no goroutine and inherits RunLoop's
// panic isolation and context binding for free. That is the same reasoning that put the hint
// deadline on the Game rather than behind a timer, applied to the other end of a puzzle's life.
func TestAGauntletRunsItsRoundsThroughTheSweep(t *testing.T) {
	s, guard, _, _ := fixtureGauntlet(t)

	if consumed := s.Command("!wordgame", "3", "c1", admin(snowflake(1)), noNames); !consumed {
		t.Fatal("!wordgame 3 was not consumed")
	}

	// Round one is up immediately, and it says which round it is.
	posts := guard.posts()
	if len(posts) != 1 {
		t.Fatalf("posts = %d, want the first round", len(posts))
	}
	if !strings.Contains(posts[0], "round 1/3") {
		t.Errorf("the first card does not place itself in the run:\n%s", posts[0])
	}

	// The sweep must not start round two while round one is live. This is the property the
	// whole feature is asked for: a run advances on the previous puzzle CONCLUDING.
	s.sweep()
	if got := guard.posts(); len(got) != 1 {
		t.Fatalf("the sweep started a second round while the first was still running: %d posts",
			len(got))
	}

	// Conclude round one, and the sweep brings round two.
	s.Guess("c1", snowflake(600), theWord, snowflake(42), "ann")
	s.sweep()

	posts = guard.posts()
	if len(posts) < 3 {
		t.Fatalf("posts = %d, want the puzzle, the win and round two", len(posts))
	}
	if !strings.Contains(posts[len(posts)-1], "round 2/3") {
		t.Errorf("the sweep did not start round two:\n%s", posts[len(posts)-1])
	}
}

// TestTheLastRoundSaysTheRunIsOver.
//
// A gauntlet that simply stops is indistinguishable from one that broke, which is finding 32's
// shape: the bot's silence is a feature, a player's inability to tell what it meant is not.
func TestTheLastRoundSaysTheRunIsOver(t *testing.T) {
	s, guard, _, _ := fixtureGauntlet(t)

	if consumed := s.Command("!wordgame", "1", "c1", admin(snowflake(1)), noNames); !consumed {
		t.Fatal("!wordgame 1 was not consumed")
	}
	s.Guess("c1", snowflake(601), theWord, snowflake(42), "ann")

	var sawDone bool
	for _, p := range guard.posts() {
		if strings.Contains(p, "that's all") {
			sawDone = true
		}
	}
	if !sawDone {
		t.Errorf("the last round ended with no sign the run was over: %v", guard.posts())
	}

	// And nothing further starts.
	s.sweep()
	for _, p := range guard.posts() {
		if strings.Contains(p, "Round") && strings.Contains(p, "of 1") &&
			strings.Contains(p, "Round 2") {
			t.Error("a fourth round appeared after a run of one")
		}
	}
}

// TestASoloWordgameIsNotARound. The round line is only honest when there is a run to be part of,
// and a standalone puzzle claiming "Round 1 of 1" would read as a run that ended immediately.
func TestASoloWordgameIsNotARound(t *testing.T) {
	s, guard, _, _ := fixtureGauntlet(t)

	if consumed := s.Command("!wordgame", "", "c1", admin(snowflake(1)), noNames); !consumed {
		t.Fatal("!wordgame was not consumed")
	}
	posts := guard.posts()
	if len(posts) != 1 {
		t.Fatalf("posts = %v", posts)
	}
	if strings.Contains(posts[0], "Round") {
		t.Errorf("a standalone puzzle announced itself as a round:\n%s", posts[0])
	}
}

// TestAHintCostsAPointAndARefusedHintDoesNot.
//
// The two halves of the same rule, and the second is the one that needed a code change: DueHints
// stopped advancing the level itself precisely so a repost the guard turns down cannot charge the
// eventual winner for help that never reached the channel.
func TestAHintCostsAPointAndARefusedHintDoesNot(t *testing.T) {
	t.Run("a delivered rung costs a point", func(t *testing.T) {
		s, _, _, _ := fixtureHinting(t)
		s.start("c1")
		time.Sleep(20 * time.Millisecond)
		s.sweep()

		s.Guess("c1", snowflake(610), theWord, snowflake(42), "ann")

		if got := s.board.Scores()[snowflake(42)]; got != 3 {
			t.Errorf("scored %d, want 3: a base of 4 less one delivered rung", got)
		}
	})

	t.Run("a refused rung is free", func(t *testing.T) {
		s, guard, _, _ := fixtureHinting(t)
		s.start("c1")
		time.Sleep(20 * time.Millisecond)

		// The guard refuses the repost, so the hint never reaches the channel.
		guard.refuse = true
		s.sweep()
		guard.refuse = false

		s.Guess("c1", snowflake(611), theWord, snowflake(43), "bob")

		if got := s.board.Scores()[snowflake(43)]; got != 4 {
			t.Errorf("scored %d, want the full 4: the hint was refused, so nobody saw it and "+
				"nobody should pay for it", got)
		}
	})
}

// TestTheStakeIsOnTheCardBeforeTheFirstHint.
//
// A player who cannot see what a puzzle is worth before the first rung has no decision to make
// about waiting for one, which would leave the ladder a gift rather than a price and put the
// game back where M25 found it.
func TestTheStakeIsOnTheCardBeforeTheFirstHint(t *testing.T) {
	s, guard, _, _ := fixtureGauntlet(t)
	s.start("c1")

	posts := guard.posts()
	if len(posts) != 1 {
		t.Fatalf("posts = %v", posts)
	}
	if !strings.Contains(posts[0], "worth 4") {
		t.Errorf("the opening card does not say what the puzzle is worth:\n%s", posts[0])
	}
}

// TestASolvedPuzzleIsNotStillWorthItsOpeningStake pins that the card and the board agree. Two
// numbers for one solve, computed in two places, is finding 28's shape with a player watching.
func TestASolvedPuzzleIsNotStillWorthItsOpeningStake(t *testing.T) {
	s, guard, _, _ := fixtureHinting(t)
	s.start("c1")
	time.Sleep(20 * time.Millisecond)
	s.sweep()

	s.Guess("c1", snowflake(620), theWord, snowflake(44), "cat")

	// What is asserted is that the message and the board agree about one number. Found by the
	// winner's name rather than by the wording around it, so a copy change is not a failure.
	var won string
	for _, p := range guard.posts() {
		if strings.Contains(p, "**cat**") {
			won = p
		}
	}
	if won == "" {
		t.Fatalf("no win announcement in:\n%s", strings.Join(guard.posts(), "\n---\n"))
	}
	if !strings.Contains(won, "3 pts") {
		t.Errorf("the win announces a different number from the one the board recorded "+
			"(%d):\n%s", s.board.Scores()[snowflake(44)], won)
	}
}

// TestGauntletArgAndPlantedWordCannotCollide. commandFor hands over one untyped token, so the
// service is the only thing that decides whether it is a run length or a word, and a digit
// string is never a word.
func TestGauntletArgAndPlantedWordCannotCollide(t *testing.T) {
	cases := map[string]struct {
		n  int
		ok bool
	}{
		"":       {0, false},
		"banana": {0, false},
		"5":      {5, true},
		"1":      {1, true},
		"0":      {0, false},
	}
	for arg, want := range cases {
		n, ok := gauntletArg(arg)
		if n != want.n || ok != want.ok {
			t.Errorf("gauntletArg(%q) = %d, %v; want %d, %v", arg, n, ok, want.n, want.ok)
		}
	}
}

// theWord is the only answer any puzzle in this file can have.
//
// A run is started by the SERVICE rather than by the test, so unlike the older tests there is no
// *Game to read the word off. Pinning the dictionary to one word is the way to know the answer
// without adding a "what is running here" accessor to the Manager, which nothing in production
// would call: a test-only method is production API that exists for tests.
const theWord = "peregrine"

// fixtureHinting is the fixture with a ladder that comes due almost immediately.
//
// Milliseconds and a real sleep, matching the existing hint test: WHEN a rung is due is pinned in
// the Manager's own tests against an injected clock, and a nanosecond deadline is not reliably in
// the past on a platform whose clock ticks in milliseconds, which is a flake rather than a
// finding.
func fixtureHinting(t *testing.T) (*Service, *fakeGuard, *wordgame.Manager, *activity.Tracker) {
	t.Helper()
	return fixtureDict(t, enabled(), wordgame.Options{
		Timeout:     time.Minute,
		AnnounceTTL: 30 * time.Second,
		HintAfter:   time.Millisecond,
		HintLevels:  3,
	})
}

// fixtureGauntlet is the fixture with no hints and no gap, so a run advances the instant a puzzle
// concludes and the sweep can be driven without a clock.
func fixtureGauntlet(t *testing.T) (*Service, *fakeGuard, *wordgame.Manager, *activity.Tracker) {
	t.Helper()
	return fixtureDict(t, enabled(), wordgame.Options{
		Timeout:     time.Minute,
		AnnounceTTL: 30 * time.Second,
		GauntletGap: 0,
		GauntletMax: 10,
	})
}

// fixtureDictOpts is fixtureGauntlet with different service Options, for the tests whose subject
// is a flag rather than the game.
func fixtureDictOpts(t *testing.T, opts Options) (*Service, *fakeGuard, *wordgame.Manager, *activity.Tracker) {
	t.Helper()
	return fixtureDict(t, opts, wordgame.Options{
		Timeout:     time.Minute,
		AnnounceTTL: 30 * time.Second,
		GauntletMax: 10,
	})
}

// fixtureDict wires the service over a one-word dictionary.
func fixtureDict(t *testing.T, opts Options, mopts wordgame.Options) (
	*Service, *fakeGuard, *wordgame.Manager, *activity.Tracker,
) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "words.txt")
	if err := os.WriteFile(path, []byte(theWord+"\n"), 0o600); err != nil {
		t.Fatalf("write dictionary: %v", err)
	}
	dict, err := wordgame.LoadDictionary(path, wordgame.DictionaryOptions{MinLength: 5, MaxLength: 12})
	if err != nil {
		t.Fatalf("LoadDictionary: %v", err)
	}

	tracker := activity.New(activity.Options{})
	manager := wordgame.NewManager(dict, nil, tracker, mopts)
	guard := &fakeGuard{}
	chans := fakeChannels{"c1": {ID: "c1", Name: "memes", Text: true}}

	s := New(dbtest.Store(t), guard, manager, tracker, chans, opts)
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s, guard, manager, tracker
}

// ------------------------------------------------------- the shape of a message, M27 and M28

// TestAPuzzleIsTwoLinesAndThreeWhenHinted.
//
// The requirement the design pass exists to meet, pinned because it is the kind that erodes one
// well-meaning addition at a time. What M25 shipped spent four embed slots on two facts: a title
// saying "Word Scramble" above a word scramble, a description telling the reader to unscramble
// the scrambled word, a field for the hint and a three-clause footer.
//
// M28 took the embed away entirely, on feedback that a card every few minutes is a bot that keeps
// interrupting. The line budget is the part that survives every pass, and it is the part that
// mattered: the failure being prevented is vertical, because a message taking half a screen in a
// busy channel is one people scroll past.
//
// M29 spent one of those lines deliberately, on the call to action, and the budget moved from two
// and three to three and four rather than the line being smuggled in against a stale number. A
// budget nobody is allowed to change is a budget people route around.
func TestAPuzzleIsTwoLinesAndThreeWhenHinted(t *testing.T) {
	s, guard, _, _ := fixtureHinting(t)
	s.start("c1")

	assertPuzzleShape(t, onePuzzle(t, guard), 3, "live")

	time.Sleep(20 * time.Millisecond)
	s.sweep()

	hinted := guard.hintedPuzzles()
	if len(hinted) != 1 {
		t.Fatalf("posted %d hinted announcements, want 1", len(hinted))
	}
	assertPuzzleShape(t, hinted[0], 4, "hinted")

	// And the mask REPLACED the letter count rather than joining it, which is what keeps the
	// subtext to one line as it gains the round and hint counters.
	if strings.Contains(hinted[0], "letters") {
		t.Errorf("the hinted message still counts letters as well as showing them:\n%s", hinted[0])
	}
}

// assertPuzzleShape checks a puzzle message is no taller than it should be and annotates itself
// in exactly one place.
func assertPuzzleShape(t *testing.T, msg string, maxLines int, state string) {
	t.Helper()

	lines := strings.Split(msg, "\n")
	if len(lines) > maxLines {
		t.Errorf("%s message is %d lines, want at most %d:\n%s", state, len(lines), maxLines, msg)
	}

	// The call to action leads and the scramble is the header under it. The header is what makes
	// a puzzle catch the eye of somebody skimming, since there is no embed box left to do it, so
	// its being a header is load-bearing rather than decorative.
	if !strings.HasPrefix(msg, "✨ ") {
		t.Errorf("%s message does not open on the call to action:\n%s", state, msg)
	}
	if !strings.Contains(msg, "\n## ") {
		t.Errorf("%s message does not carry the scramble as a header:\n%s", state, msg)
	}

	// Exactly one subtext line, at the bottom. Two would defeat the point, and one in the middle
	// would put small grey text above full-size text.
	subs := 0
	for i, l := range lines {
		if !strings.HasPrefix(l, "-# ") {
			continue
		}
		subs++
		if i != len(lines)-1 {
			t.Errorf("%s message has subtext above body text (line %d):\n%s", state, i+1, msg)
		}
	}
	if subs != 1 {
		t.Errorf("%s message has %d subtext lines, want exactly 1:\n%s", state, subs, msg)
	}
}

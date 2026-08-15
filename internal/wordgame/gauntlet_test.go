package wordgame

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// M25: the hint ladder, the gauntlet and the points that connect them.
//
// The ladder and the scoring are one feature described from two ends. A rung is only a "hint" if
// it costs something; otherwise it is a gift and the game has no decision in it. So the tests
// about what a rung reveals and the tests about what a solve is worth belong together.

// ---------------------------------------------------------------- the ladder

// TestEachRungRevealsMoreAndNeverTheWholeWord.
//
// The superset property is what makes a ladder a ladder, and it holds because the reveal ORDER is
// fixed once when the game starts. Drawing positions afresh at each rung would let a letter shown
// at rung 2 disappear at rung 3, and a hint that takes something back is worse than no hint.
//
// The other half is that the top rung is never the answer. At least two letters stay hidden
// however far it goes, because a final hint that spells the word out turns the last stretch of
// every puzzle into a typing race and makes the cheapest solve the one nobody had to think about.
func TestEachRungRevealsMoreAndNeverTheWholeWord(t *testing.T) {
	for _, word := range []string{"peregrine", "banana", "abcde", "scholastic"} {
		t.Run(word, func(t *testing.T) {
			o := testOpts()
			o.HintAfter = 10 * time.Second
			o.HintLevels = 3
			m := NewManager(dictOf(word), seeded(7, 8), counting(0), o)

			g, err := m.Start("chan")
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if g.Rungs() == 0 {
				t.Fatalf("%q got no ladder at all", word)
			}

			prev := -1
			var prevMask string
			for level := 1; level <= g.Rungs(); level++ {
				g.HintLevel = level
				n := g.Revealed()
				if n <= prev {
					t.Fatalf("rung %d reveals %d letters and rung %d revealed %d: a rung that "+
						"does not beat the one below it still costs a point", level, n, level-1, prev)
				}
				if n > g.Letters()-2 {
					t.Errorf("rung %d reveals %d of %d letters; at least two must stay hidden or "+
						"the last hint is the answer", level, n, g.Letters())
				}

				got := g.Mask()
				if prevMask != "" && !isSupersetMask(prevMask, got) {
					t.Errorf("rung %d mask %q is not a superset of rung %d mask %q",
						level, got, level-1, prevMask)
				}
				prev, prevMask = n, got
			}
		})
	}
}

// isSupersetMask reports whether every position revealed in a is also revealed in b.
func isSupersetMask(a, b string) bool {
	as, bs := strings.Split(a, " "), strings.Split(b, " ")
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] != hidden && bs[i] != as[i] {
			return false
		}
	}
	return true
}

// TestTheFirstRungIsAlwaysTheOpeningLetter, because that is the one people use: it is what M21b's
// single hint revealed, and knowing a word starts with "p" prunes the search in a way that
// knowing its seventh letter does not.
//
// Several seeds, because the rest of the order is shuffled and one seed could pass by luck.
func TestTheFirstRungIsAlwaysTheOpeningLetter(t *testing.T) {
	o := testOpts()
	o.HintAfter = 10 * time.Second

	for seed := range uint64(20) {
		m := NewManager(dictOf("peregrine"), seeded(seed, seed+1), counting(0), o)
		g, err := m.Start("chan")
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		g.HintLevel = 1
		if got := g.Mask(); !strings.HasPrefix(got, "p ") {
			t.Fatalf("seed %d: first rung mask = %q, want it to open on \"p\"", seed, got)
		}
	}
}

// TestAShortWordGetsAShorterLadderRatherThanEmptyRungs.
//
// The configured rung count is a CEILING, not a promise. A five-letter word cannot support six
// rungs that each reveal something new while still keeping two letters hidden, and the wrong
// answer would be to deliver one showing exactly what the last one did: the player pays a point
// and learns nothing. A short ladder is the honest version.
func TestAShortWordGetsAShorterLadderRatherThanEmptyRungs(t *testing.T) {
	o := testOpts()
	o.HintAfter = 10 * time.Second
	o.HintLevels = 6

	m := NewManager(dictOf("abcde"), seeded(1, 2), counting(0), o)
	g, err := m.Start("chan")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if g.Rungs() >= 6 {
		t.Errorf("a five-letter word got %d rungs against a ceiling of 6, so some of them "+
			"cannot be revealing anything new", g.Rungs())
	}
	if g.Rungs() < 1 {
		t.Error("a five-letter word got no ladder at all, so hints are off for exactly the " +
			"words most likely to stall a channel")
	}
}

// TestAnUnacknowledgedRungStaysDue is the fairness pin, and the reason DueHints stopped mutating.
//
// The guard can refuse a repost: the bot is paused, the channel is on the ignore list, Discord
// rate-limited it. Advancing the level anyway would charge the eventual winner a point for a hint
// that never reached the channel, which is invisible in the log and obvious to whoever lost it.
func TestAnUnacknowledgedRungStaysDue(t *testing.T) {
	o := testOpts()
	o.HintAfter = 10 * time.Second
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), o)

	now := time.Now()
	m.now = func() time.Time { return now }
	if _, err := m.Start("chan"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Announced("chan", "msg1")
	now = now.Add(20 * time.Second)

	// Three sweeps that all fail to deliver. The rung must still be on offer, and must still be
	// the FIRST rung: a failed delivery cannot advance the ladder.
	for i := range 3 {
		due := m.DueHints()
		if len(due) != 1 {
			t.Fatalf("attempt %d: due = %d, want the rung still on offer", i, len(due))
		}
		if due[0].HintLevel != 1 {
			t.Fatalf("attempt %d: offered rung %d, want 1: an undelivered rung advanced the "+
				"ladder", i, due[0].HintLevel)
		}
	}

	m.HintDelivered("chan", 1)
	if due := m.DueHints(); len(due) != 0 {
		t.Errorf("the acknowledged rung is still due (%d)", len(due))
	}
}

// TestDueHintsHandsBackACopy.
//
// The live game stays in the map, where Guess and Announced write to it under the lock, so
// handing out the pointer would be a data race against the sweep reading it. Every other reader
// of a *Game here gets one the Manager has already deleted.
//
// The copy also carries the PENDING level, so the caller renders the card as it WILL look rather
// than as it currently does. Without that the repost goes out as a hint card with no hint on it,
// which is the bug this shape was found by.
func TestDueHintsHandsBackACopy(t *testing.T) {
	o := testOpts()
	o.HintAfter = 10 * time.Second
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), o)

	now := time.Now()
	m.now = func() time.Time { return now }
	g, err := m.Start("chan")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Announced("chan", "msg1")
	now = now.Add(20 * time.Second)

	due := m.DueHints()
	if len(due) != 1 {
		t.Fatalf("due = %d, want one", len(due))
	}
	if due[0] == g {
		t.Error("DueHints returned the live game, which the Manager still writes to under its " +
			"own lock while the sweep reads it")
	}
	if due[0].HintLevel != 1 {
		t.Errorf("the copy carries level %d, want the pending rung 1: rendering the current "+
			"level produces a hint card with no hint on it", due[0].HintLevel)
	}
	if g.HintLevel != 0 {
		t.Errorf("the live game advanced to %d without being acknowledged", g.HintLevel)
	}
}

// TestAnnouncedReturnsTheCardItReplaced.
//
// A repost changes a game's message ID mid-life, so the old card has to reach the caller or it
// leaks: a win landing between the send and this call would delete whichever ID the game happened
// to be holding and orphan the other. The Manager holds the only authoritative answer, which is
// why it hands it back rather than the caller remembering.
func TestAnnouncedReturnsTheCardItReplaced(t *testing.T) {
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), testOpts())
	if _, err := m.Start("chan"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := m.Announced("chan", "msg1"); got != "" {
		t.Errorf("the first announcement superseded %q, want nothing", got)
	}
	if got := m.Announced("chan", "msg2"); got != "msg1" {
		t.Errorf("superseded = %q, want msg1", got)
	}
	// The same ID twice is not a replacement, and deleting it would remove the live card.
	if got := m.Announced("chan", "msg2"); got != "" {
		t.Errorf("re-recording the same ID reported %q as superseded, which would delete the "+
			"card currently on screen", got)
	}
}

// ---------------------------------------------------------------- the gauntlet

// TestAGauntletAdvancesOnlyWhenThePreviousPuzzleHasConcluded is the whole point of a run: a slow
// round pushes the rest back rather than stacking puzzles on top of each other.
func TestAGauntletAdvancesOnlyWhenThePreviousPuzzleHasConcluded(t *testing.T) {
	o := testOpts()
	o.GauntletGap = 5 * time.Second
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), o)

	now := time.Now()
	m.now = func() time.Time { return now }

	if n, err := m.Queue("chan", 3); err != nil || n != 3 {
		t.Fatalf("Queue = %d, %v", n, err)
	}
	g, err := m.Start("chan")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if g.Round != 1 || g.Rounds != 3 {
		t.Errorf("first puzzle is round %d of %d, want 1 of 3", g.Round, g.Rounds)
	}

	// A game is live, so nothing is due however long we wait.
	now = now.Add(time.Hour)
	if due := m.DueStarts(); len(due) != 0 {
		t.Fatalf("a successor was due while the previous puzzle was still running: %v", due)
	}

	// Solved. Now the gap applies, and only then is the next one due.
	if _, ok := m.Guess("chan", "peregrine"); !ok {
		t.Fatal("Guess did not solve")
	}
	if due := m.DueStarts(); len(due) != 0 {
		t.Error("the successor was due immediately; the gap exists so a puzzle does not appear " +
			"in the same instant as the previous answer")
	}
	now = now.Add(6 * time.Second)
	if due := m.DueStarts(); len(due) != 1 || due[0] != "chan" {
		t.Fatalf("DueStarts = %v, want chan", due)
	}

	g2, err := m.Start("chan")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if g2.Round != 2 || g2.Rounds != 3 {
		t.Errorf("second puzzle is round %d of %d, want 2 of 3", g2.Round, g2.Rounds)
	}
}

// TestATimeoutAdvancesAGauntletToo.
//
// Both endings are equally "the previous one has finished", and recording it at only one of them
// would stall a run on whichever ending the channel happened to produce, which is the ending
// nobody is around for.
func TestATimeoutAdvancesAGauntletToo(t *testing.T) {
	o := testOpts()
	o.GauntletGap = 0
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), o)

	now := time.Now()
	m.now = func() time.Time { return now }

	if _, err := m.Queue("chan", 2); err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if _, err := m.Start("chan"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	now = now.Add(2 * time.Minute)
	if got := m.Expired(); len(got) != 1 {
		t.Fatalf("Expired = %d, want the timed-out puzzle", len(got))
	}
	if due := m.DueStarts(); len(due) != 1 {
		t.Errorf("a run stalled on a timeout: DueStarts = %v", due)
	}
}

// TestTheLastRoundEndsTheRun, so DueStarts stops offering and the queue does not linger.
func TestTheLastRoundEndsTheRun(t *testing.T) {
	o := testOpts()
	o.GauntletGap = 0
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), o)

	if _, err := m.Queue("chan", 1); err != nil {
		t.Fatalf("Queue: %v", err)
	}
	g, err := m.Start("chan")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if g.Round != 1 || g.Rounds != 1 {
		t.Errorf("round %d of %d, want 1 of 1", g.Round, g.Rounds)
	}
	if _, ok := m.Guess("chan", "peregrine"); !ok {
		t.Fatal("Guess did not solve")
	}
	if due := m.DueStarts(); len(due) != 0 {
		t.Errorf("the run kept offering after its last round: %v", due)
	}
	if remaining, total := m.Gauntlet("chan"); remaining != 0 || total != 0 {
		t.Errorf("the finished run is still recorded: %d of %d", remaining, total)
	}
}

// TestQueueRefusesASecondRunAndClampsToTheCap.
//
// Two overlapping runs in one channel have no sensible round numbering and no sensible end.
// Asking for more than the cap is clamped rather than refused, because it is a reasonable thing
// to want and the cap is the operator's answer to it; asking for zero is not a request at all.
func TestQueueRefusesASecondRunAndClampsToTheCap(t *testing.T) {
	o := testOpts()
	o.GauntletMax = 4
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), o)

	n, err := m.Queue("chan", 100)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if n != 4 {
		t.Errorf("Queue(100) = %d, want the cap of 4", n)
	}
	if _, err := m.Queue("chan", 2); !errors.Is(err, ErrGauntletInProgress) {
		t.Errorf("a second run was accepted: %v", err)
	}
	if _, err := m.Queue("other", 0); !errors.Is(err, ErrBadGauntlet) {
		t.Errorf("a run of zero was accepted: %v", err)
	}
}

// TestTheActivityTriggerStandsAsideForAGauntlet.
//
// A channel part-way through a run has not "earned" a random puzzle: the run owns the channel
// until it finishes, and interleaving would break the round numbering as well as the pacing.
func TestTheActivityTriggerStandsAsideForAGauntlet(t *testing.T) {
	o := testOpts()
	o.GauntletGap = 0
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(100), o)

	if !m.MaybeStart("chan") {
		t.Fatal("the fixture does not trigger at all, so the assertion below would prove nothing")
	}
	if _, err := m.Queue("chan", 3); err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if m.MaybeStart("chan") {
		t.Error("the activity trigger fired in a channel that is running a gauntlet")
	}
}

// TestTheGauntletMapsAreBounded.
//
// Keyed by channel, so they grow with every guild the bot joins: the leak this repo has already
// shipped twice, and one a test using a single channel would never reveal.
func TestTheGauntletMapsAreBounded(t *testing.T) {
	o := testOpts()
	o.MaxChannels = 5
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), o)

	for i := range 50 {
		if _, err := m.Queue(fmt.Sprintf("chan%02d", i), 2); err != nil {
			t.Fatalf("Queue: %v", err)
		}
	}

	m.mu.Lock()
	queued, rounds, ready := len(m.queued), len(m.rounds), len(m.readyAt)
	m.mu.Unlock()

	if queued > o.MaxChannels {
		t.Errorf("queued holds %d entries against MaxChannels=%d", queued, o.MaxChannels)
	}
	if rounds > o.MaxChannels || ready > o.MaxChannels {
		t.Errorf("the sidecar maps grew past the bound: rounds=%d readyAt=%d", rounds, ready)
	}
}

// TestAbandonCancelsTheRun.
//
// The guard refuses for reasons that do not clear on their own (a paused bot, an ignored
// channel), so leaving the queue in place would march the whole gauntlet through the same
// refusal one puzzle at a time and log it every time.
func TestAbandonCancelsTheRun(t *testing.T) {
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), testOpts())

	if _, err := m.Queue("chan", 5); err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if _, err := m.Start("chan"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Abandon("chan")

	if remaining, _ := m.Gauntlet("chan"); remaining != 0 {
		t.Errorf("%d puzzles still queued after the announcement was refused", remaining)
	}
	if due := m.DueStarts(); len(due) != 0 {
		t.Errorf("an abandoned run still offers successors: %v", due)
	}
}

// ---------------------------------------------------------------- points

// TestPointsAreBackfilledOnceForABoardWrittenBeforeTheyExisted.
//
// Ranking a week already in progress off a Points field that is zero for everybody shows an empty
// board to a server that has been playing all week. Overpaying preserves the ORDER, which is the
// part players notice, and the next weekly reset makes the question moot.
func TestPointsAreBackfilledOnceForABoardWrittenBeforeTheyExisted(t *testing.T) {
	// An M21-era blob: wins, no points.
	blob := `{"week_start":"` + StartOfWeekUTC(time.Now()).Format(time.RFC3339) +
		`","scores":{"a":{"user_id":"a","username":"ann","wins":3},` +
		`"b":{"user_id":"b","username":"bob","wins":1}}}`

	var l Leaderboard
	if err := json.Unmarshal([]byte(blob), &l); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := l.Scores(); got["a"] != 0 {
		t.Fatalf("the fixture already carries points (%v), so this proves nothing", got)
	}

	if n := l.BackfillPoints(4); n != 2 {
		t.Errorf("converted %d entries, want 2", n)
	}
	if got := l.Scores(); got["a"] != 12 || got["b"] != 4 {
		t.Errorf("scores = %v, want a=12 b=4 at four points a win", got)
	}

	// Idempotent. A second call after a win has landed must not inflate anybody.
	l.AddWin("b", "bob", time.Second, 3)
	if n := l.BackfillPoints(4); n != 0 {
		t.Errorf("a second backfill converted %d entries", n)
	}
	if got := l.Scores(); got["b"] != 7 {
		t.Errorf("b = %d, want 7: the backfilled 4 plus a 3-point win", got["b"])
	}
}

// TestTheBoardRanksByPointsRatherThanWins, which is the change: a solve is worth what it cost in
// help, so somebody with fewer but harder wins outranks somebody with more easy ones.
func TestTheBoardRanksByPointsRatherThanWins(t *testing.T) {
	l := NewLeaderboard(time.Now())

	for range 3 {
		l.AddWin("many", "many", time.Second, 1)
	}
	l.AddWin("few", "few", time.Second, 4)
	l.AddWin("few", "few", time.Second, 4)

	b := Rank(l.Scores(), "", 10)
	if len(b.Top) < 2 {
		t.Fatalf("board = %v", b.Top)
	}
	if b.Top[0].UserID != "few" {
		t.Errorf("top row is %q with %d; two four-point wins must outrank three one-point ones",
			b.Top[0].UserID, b.Top[0].Score)
	}
}

// TestAWinIsAlwaysWorthSomething. A player who needed the whole ladder still beat everybody else
// in the channel to the answer, and a zero-point win would read as not having won.
func TestAWinIsAlwaysWorthSomething(t *testing.T) {
	l := NewLeaderboard(time.Now())
	l.AddWin("a", "ann", time.Second, 0)
	l.AddWin("b", "bob", time.Second, -5)

	got := l.Scores()
	if got["a"] != 1 || got["b"] != 1 {
		t.Errorf("scores = %v, want both floored to 1", got)
	}
}

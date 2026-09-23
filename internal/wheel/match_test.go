package wheel

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

// --- lobby ---

func TestOpenJoinsHost(t *testing.T) {
	f := newFixture(t, testOpts())
	f.open("a")
	v := f.view()
	if v.Phase != Lobby || v.HostID != "a" || len(v.Players) != 1 || v.Players[0].UserID != "a" {
		t.Fatalf("view %+v", v)
	}
	if !v.Deadline.Equal(f.now.Add(testOpts().Lobby)) {
		t.Fatalf("deadline %v", v.Deadline)
	}
}

func TestOpenRefusesSecondMatchInChannel(t *testing.T) {
	f := newFixture(t, testOpts())
	f.open("a")
	if _, err := f.m.Open(testGuild, testChannel, "b", "b"); !errors.Is(err, ErrMatchInProgress) {
		t.Fatalf("err = %v", err)
	}
}

func TestJoinTwiceRefused(t *testing.T) {
	f := newFixture(t, testOpts())
	f.open("a", "b")
	if _, err := f.m.Act(testChannel, Action{Kind: Join, UserID: "b"}); !errors.Is(err, ErrAlreadyJoined) {
		t.Fatalf("err = %v", err)
	}
}

func TestJoinFullLobbyRefused(t *testing.T) {
	f := newFixture(t, testOpts())
	f.open("a", "b", "c")
	if _, err := f.m.Act(testChannel, Action{Kind: Join, UserID: "d"}); !errors.Is(err, ErrLobbyFull) {
		t.Fatalf("err = %v", err)
	}
}

func TestJoinAfterStartRefused(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	if _, err := f.m.Act(testChannel, Action{Kind: Join, UserID: "b"}); !errors.Is(err, ErrWrongPhase) {
		t.Fatalf("err = %v", err)
	}
}

func TestLeaveLobbyTransfersHost(t *testing.T) {
	f := newFixture(t, testOpts())
	f.open("a", "b", "c")
	f.ok(Action{Kind: Leave, UserID: "a"})
	if v := f.view(); v.HostID != "b" || len(v.Players) != 2 {
		t.Fatalf("view %+v", v)
	}
}

func TestLastLeaveAbortsLobbyWithoutAwards(t *testing.T) {
	f := newFixture(t, testOpts())
	f.open("a")
	u := f.ok(Action{Kind: Leave, UserID: "a"})
	if u.Result == nil || u.Result.Outcome != Aborted || u.Result.Reason != NoPlayers || len(u.Result.Awards) != 0 {
		t.Fatalf("result %+v", u.Result)
	}
	if f.m.Active() != 0 {
		t.Fatal("the match is still live")
	}
}

func TestStartByNonHostRefused(t *testing.T) {
	f := newFixture(t, testOpts())
	f.open("a", "b")
	if _, err := f.m.Act(testChannel, Action{Kind: Start, UserID: "b"}); !errors.Is(err, ErrNotHost) {
		t.Fatalf("err = %v", err)
	}
}

func TestStartBelowMinRefused(t *testing.T) {
	opts := testOpts()
	opts.MinPlayers = 2
	f := newFixture(t, opts)
	f.open("a")
	if _, err := f.m.Act(testChannel, Action{Kind: Start, UserID: "a"}); !errors.Is(err, ErrTooFewPlayers) {
		t.Fatalf("err = %v", err)
	}
}

func TestLobbyDeadlineStartsWithEnough(t *testing.T) {
	f := newFixture(t, testOpts())
	f.open("a", "b")
	f.advance(59 * time.Second)
	if len(f.m.Tick()) != 0 {
		t.Fatal("the lobby closed early")
	}
	f.advance(time.Second)
	us := f.m.Tick()
	if len(us) != 1 || us[0].View.Phase != Round || !has(us[0].Events, RoundStart) {
		t.Fatalf("updates %+v", us)
	}
}

func TestLobbyDeadlineAbortsBelowMin(t *testing.T) {
	opts := testOpts()
	opts.MinPlayers = 2
	f := newFixture(t, opts)
	f.open("a")
	f.advance(opts.Lobby)
	us := f.m.Tick()
	if len(us) != 1 || us[0].Result == nil || us[0].Result.Reason != TooFewPlayers {
		t.Fatalf("updates %+v", us)
	}
}

func TestSeatsShuffledOnceAndOpeningSeatRotatesPerRound(t *testing.T) {
	opts := testOpts()
	opts.Rounds = 2
	f := newFixture(t, opts)
	f.src.reverse = true
	f.begin("a", "b", "c") // seats become c, b, a
	if v := f.view(); v.Current != "c" || v.Players[0].UserID != "c" {
		t.Fatalf("round 1 opens on %s, seats %+v", v.Current, v.Players)
	}
	f.solve("hello world")
	if v := f.view(); v.Round != 2 || v.Current != "b" {
		t.Fatalf("round %d opens on %s", v.Round, v.Current)
	}
	if f.src.shuffles != 1 {
		t.Fatalf("%d shuffles", f.src.shuffles)
	}
}

// --- turns ---

func TestGoldSpinThenConsonantHitPaysValueTimesCount(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	before := f.view()
	f.advance(10 * time.Second)
	u := f.earn("l")
	v := u.View
	if seat(v, "a").Round != 1800 {
		t.Fatalf("round bank %d, want 600 x 3", seat(v, "a").Round)
	}
	if v.Current != "a" || v.Turn != before.Turn {
		t.Fatalf("a hit passed the turn: current %s turn %d->%d", v.Current, before.Turn, v.Turn)
	}
	if !v.Deadline.Equal(f.now.Add(testOpts().TurnTimeout)) {
		t.Fatal("a hit did not refresh the deadline")
	}
	if e := u.Events[0]; e.Kind != LetterHit || e.Letter != 'L' || e.Count != 3 || e.Amount != 1800 {
		t.Fatalf("event %+v", e)
	}
}

func TestConsonantMissPassesTurn(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	turn := f.view().Turn
	f.spinTo(w600)
	u := f.do(Consonant, "z")
	if u.View.Current != "b" || u.View.Turn != turn+1 || u.Events[0].Kind != LetterMiss {
		t.Fatalf("view %+v events %v", u.View, kinds(u.Events))
	}
}

func TestRepeatedConsonantPassesAndPaysNothing(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	f.earn("l")
	u := f.earn("l")
	if u.Events[0].Kind != LetterRepeat || u.View.Current != "b" || seat(u.View, "a").Round != 1800 {
		t.Fatalf("events %v view %+v", kinds(u.Events), u.View)
	}
}

func TestConsonantWithoutSpinRefused(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	if _, err := f.try(Consonant, "l"); !errors.Is(err, ErrSpinFirst) {
		t.Fatalf("err = %v", err)
	}
}

func TestSpinWhilePendingRefused(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	f.spinTo(w600)
	for _, k := range []ActionKind{Spin, Vowel, Solve} {
		if _, err := f.try(k, "o"); !errors.Is(err, ErrMustCallConsonant) {
			t.Fatalf("%v: err = %v", k, err)
		}
	}
}

// Malformed input comes from a modal typo, so it costs nothing; a repeated letter is a
// legal move and costs the turn. The difference is the whole point of the table.
func TestBadLetterRefusedWithoutPenalty(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	f.spinTo(w600)
	before := f.view()
	for _, in := range []string{"a", "1", "ll", "", " ", "é", "_"} {
		if _, err := f.try(Consonant, in); !errors.Is(err, ErrBadLetter) {
			t.Errorf("%q: err = %v", in, err)
		}
	}
	if !reflect.DeepEqual(before, f.view()) {
		t.Fatal("a refused letter changed the match")
	}
	if u := f.do(Consonant, " l "); u.Events[0].Kind != LetterHit {
		t.Fatal("a lowercase letter with spaces was not accepted")
	}
}

func TestBankruptZeroesRoundBankNotMatchBank(t *testing.T) {
	opts := testOpts()
	opts.Rounds = 2
	f := newFixture(t, opts, "phrase|hello world", "thing|good job", "place|big if true")
	f.begin("a")
	f.earn("l")
	f.solve("hello world")
	f.earn("d")
	u := f.spinTo(wBankrupt)
	p := seat(u.View, "a")
	if p.Round != 0 || p.Bank != 1800 {
		t.Fatalf("after bankrupt: round %d bank %d", p.Round, p.Bank)
	}
}

func TestLoseATurnPasses(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	f.earn("l")
	u := f.spinTo(wLoseTurn)
	if u.View.Current != "b" || seat(u.View, "a").Round != 1800 {
		t.Fatalf("view %+v", u.View)
	}
}

func TestVowelCostsEvenOnMiss(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	f.earn("l")
	u := f.do(Vowel, "a")
	if u.Events[0].Kind != LetterMiss || seat(u.View, "a").Round != 1800-VowelCost || u.View.Current != "b" {
		t.Fatalf("events %v view %+v", kinds(u.Events), u.View)
	}
}

func TestVowelUnaffordableRefused(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	if _, err := f.try(Vowel, "o"); !errors.Is(err, ErrCannotAffordVowel) {
		t.Fatalf("err = %v", err)
	}
	if f.view().CanVowel {
		t.Fatal("CanVowel with an empty round bank")
	}
}

func TestVowelHitContinuesTurn(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	f.earn("l")
	turn := f.view().Turn
	u := f.do(Vowel, "o")
	if u.View.Current != "a" || u.View.Turn != turn || u.Events[0].Count != 2 {
		t.Fatalf("view %+v events %+v", u.View, u.Events)
	}
}

func TestRepeatVowelChargesAndPasses(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	f.earn("l")
	f.do(Vowel, "o")
	u := f.do(Vowel, "o")
	if u.Events[0].Kind != LetterRepeat || seat(u.View, "a").Round != 1800-2*VowelCost || u.View.Current != "b" {
		t.Fatalf("events %v view %+v", kinds(u.Events), u.View)
	}
}

func TestNoConsonantsLeftRefusesSpinAndClearsCanSpin(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	for _, l := range []string{"h", "l", "w", "r", "d"} {
		f.earn(l)
	}
	if f.view().CanSpin {
		t.Fatal("CanSpin with every consonant showing")
	}
	if _, err := f.try(Spin, ""); !errors.Is(err, ErrNoConsonantsLeft) {
		t.Fatalf("err = %v", err)
	}
}

func TestNoVowelsLeftRefusesVowel(t *testing.T) {
	f := newFixture(t, testOpts(), "person|bob dylan")
	f.begin("a")
	f.earn("b")
	f.earn("d")
	f.do(Vowel, "o")
	f.do(Vowel, "a")
	if f.view().CanVowel {
		t.Fatal("CanVowel with every vowel showing")
	}
	if _, err := f.try(Vowel, "e"); !errors.Is(err, ErrNoVowelsLeft) {
		t.Fatalf("err = %v", err)
	}
}

func TestRevealingLastLetterSolvesForRevealer(t *testing.T) {
	f := newFixture(t, testOpts(), "band|abba", "thing|good job")
	f.begin("a")
	f.earn("b")
	u := f.do(Vowel, "a")
	if !has(u.Events, Solved) || seat(u.View, "a").Bank != 1200-VowelCost {
		t.Fatalf("events %v view %+v", kinds(u.Events), u.View)
	}
}

func TestCorrectSolveBanksSolverAndDiscardsOthers(t *testing.T) {
	opts := testOpts()
	opts.Rounds = 2
	f := newFixture(t, opts, "phrase|hello world", "thing|good job", "place|big if true")
	f.begin("a", "b")
	f.earn("l")
	f.spinTo(w600)
	f.do(Consonant, "z") // a misses; b's turn
	f.earn("h")
	u := f.solve("Hello, World!")
	a, b := seat(u.View, "a"), seat(u.View, "b")
	if b.Bank != 600 || a.Bank != 0 || a.Round != 0 || b.Round != 0 {
		t.Fatalf("a %+v b %+v", a, b)
	}
	if e := u.Events[0]; e.Kind != Solved || e.Text != "HELLO WORLD" || e.Amount != 600 {
		t.Fatalf("event %+v", e)
	}
	if u.View.Round != 2 {
		t.Fatalf("round %d", u.View.Round)
	}
}

func TestWrongSolvePasses(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	u := f.solve("goodbye world")
	if u.Events[0].Kind != WrongSolve || u.Events[0].Text != "" || u.View.Current != "b" {
		t.Fatalf("events %+v current %s", u.Events, u.View.Current)
	}
}

func TestEmptySolveRefusedWithoutPenalty(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	before := f.view()
	if _, err := f.try(Solve, " ?! "); !errors.Is(err, ErrEmptySolve) {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(before, f.view()) {
		t.Fatal("an empty solve changed the match")
	}
}

func TestOutOfTurnRefused(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	v := f.view()
	if _, err := f.m.Act(testChannel, Action{Kind: Spin, UserID: "b", Turn: v.Turn}); !errors.Is(err, ErrNotYourTurn) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.m.Act(testChannel, Action{Kind: Spin, UserID: "zed", Turn: v.Turn}); !errors.Is(err, ErrNotYourTurn) {
		t.Fatalf("stranger: err = %v", err)
	}
}

// The failure mode is a click aimed at a turn that timed out, landing on the same
// player's NEXT turn: in a solo match the turn passes straight back to them.
func TestStaleTurnTokenRefusedAfterSoloTimeout(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	old := f.view().Turn
	f.advance(testOpts().TurnTimeout)
	f.m.Tick()
	v := f.view()
	if v.Current != "a" || v.Turn == old {
		t.Fatalf("current %s turn %d", v.Current, v.Turn)
	}
	if _, err := f.m.Act(testChannel, Action{Kind: Spin, UserID: "a", Turn: old}); !errors.Is(err, ErrStaleTurn) {
		t.Fatalf("err = %v", err)
	}
}

// --- leaving and timeouts ---

func TestLeaveOnOwnTurnPassesAndDropsPending(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	f.spinTo(w600)
	u := f.ok(Action{Kind: Leave, UserID: "a"})
	if u.View.Current != "b" || u.View.Pending != nil || !seat(u.View, "a").Left {
		t.Fatalf("view %+v", u.View)
	}
	if _, err := f.m.Act(testChannel, Action{Kind: Leave, UserID: "a"}); !errors.Is(err, ErrNotJoined) {
		t.Fatalf("leaving twice: err = %v", err)
	}
}

func TestLeaverKeepsBankedGoldButCannotWin(t *testing.T) {
	opts := testOpts()
	opts.Rounds = 2
	f := newFixture(t, opts, "phrase|hello world", "phrase|big if true", "thing|good job")
	f.begin("a", "b")
	f.earn("l")
	f.solve("hello world") // a banks 1800; round 2 opens on b
	f.ok(Action{Kind: Leave, UserID: "a"})
	f.earn("g")
	f.solve("big if true") // b banks 600; the bonus goes to b, not to a
	if v := f.view(); v.Phase != BonusPick || v.Current != "b" {
		t.Fatalf("phase %v current %s", v.Phase, v.Current)
	}
	f.do(Pick, "c d m a")
	u := f.solve("wrong")
	r := u.Result
	if r == nil || r.Winner != "b" {
		t.Fatalf("result %+v", r)
	}
	want := []Award{{UserID: "a", Name: "a", Gold: 1800}, {UserID: "b", Name: "b", Gold: 600}}
	if !reflect.DeepEqual(r.Awards, want) {
		t.Fatalf("awards %+v", r.Awards)
	}
}

func TestEveryoneLeavesAbortsAndPaysMatchBanks(t *testing.T) {
	opts := testOpts()
	opts.Rounds = 2
	f := newFixture(t, opts, "phrase|hello world", "phrase|big if true", "thing|good job")
	f.begin("a", "b")
	f.earn("l")
	f.solve("hello world")
	f.ok(Action{Kind: Leave, UserID: "b"})
	u := f.ok(Action{Kind: Leave, UserID: "a"})
	r := u.Result
	if r == nil || r.Outcome != Aborted || r.Reason != NoPlayers || r.Winner != "" {
		t.Fatalf("result %+v", r)
	}
	if len(r.Awards) != 1 || r.Awards[0].Gold != 1800 {
		t.Fatalf("awards %+v", r.Awards)
	}
}

func TestTimeoutPassesAndStrikes(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	f.advance(testOpts().TurnTimeout)
	us := f.m.Tick()
	if len(us) != 1 || us[0].View.Current != "b" || seat(us[0].View, "a").Strikes != 1 || !has(us[0].Events, TimedOut) {
		t.Fatalf("updates %+v", us)
	}
}

func TestOwnActionResetsStrikes(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	f.advance(testOpts().TurnTimeout)
	f.m.Tick()
	if seat(f.view(), "a").Strikes != 1 {
		t.Fatal("no strike")
	}
	f.spinTo(w600)
	if s := seat(f.view(), "a").Strikes; s != 0 {
		t.Fatalf("strikes %d after acting", s)
	}
}

func TestStrikeLimitRemovesPlayer(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	for range 3 { // a, b, a: a's second strike
		f.advance(testOpts().TurnTimeout)
		f.m.Tick()
	}
	v := f.view()
	if !seat(v, "a").Left || v.Current != "b" {
		t.Fatalf("view %+v", v)
	}
	if !has(v.Last, StruckOut) {
		t.Fatalf("last %v", kinds(v.Last))
	}
}

// A sweep that ran late must not skip several players' turns in one go.
func TestOneTimeoutPerTick(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b", "c")
	f.advance(5 * testOpts().TurnTimeout)
	f.m.Tick()
	if v := f.view(); v.Current != "b" {
		t.Fatalf("current %s after one tick", v.Current)
	}
}

func TestTimeLimitEndsWithoutBonus(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	f.earn("l")
	f.advance(testOpts().MaxDuration)
	us := f.m.Tick()
	r := us[0].Result
	if r == nil || r.Outcome != Done || r.Reason != TimeLimit || r.BonusPlayed || r.Winner != "" {
		t.Fatalf("result %+v", r)
	}
}

func TestSoloMatchPlaysThroughToBonus(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	turn := f.view().Turn
	u := f.spinTo(wLoseTurn)
	if u.View.Current != "a" || u.View.Turn != turn+1 {
		t.Fatalf("lose a turn in a solo match: current %s turn %d->%d", u.View.Current, turn, u.View.Turn)
	}
	f.earn("l")
	u = f.solve("hello world")
	if u.View.Phase != BonusPick || !has(u.Events, BonusStart) {
		t.Fatalf("phase %v events %v", u.View.Phase, kinds(u.Events))
	}
}

// --- bonus ---

func TestBonusGoesToTopMatchBankTieBySeat(t *testing.T) {
	opts := testOpts()
	opts.Rounds = 2
	f := newFixture(t, opts, "phrase|hello world", "thing|hello world", "thing|good job")
	f.begin("a", "b")
	f.earn("l")
	f.solve("hello world") // a: 1800
	f.earn("l")
	u := f.solve("hello world") // b: 1800
	if u.View.Phase != BonusPick || u.View.Current != "a" {
		t.Fatalf("bonus went to %s in phase %v", u.View.Current, u.View.Phase)
	}
}

func TestNoBonusAndNoWinnerWhenNobodyBanked(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	u := f.solve("hello world")
	r := u.Result
	if r == nil || r.Outcome != Done || r.BonusPlayed || r.Winner != "" || len(r.Awards) != 0 {
		t.Fatalf("result %+v", r)
	}
}

// bonus plays a one-round solo match to the bonus on GOOD JOB.
func bonus(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, testOpts())
	f.begin("a")
	f.earn("l")
	f.solve("hello world")
	return f
}

func TestBonusRevealsRSTLNE(t *testing.T) {
	f := bonus(t)
	v := f.view()
	if string(v.Called) != "ELNRST" || v.Board != "____ ___" || v.Category != "thing" {
		t.Fatalf("called %q board %q category %q", string(v.Called), v.Board, v.Category)
	}
}

func TestBonusPickValidation(t *testing.T) {
	f := bonus(t)
	before := f.view()
	for _, in := range []string{"gdb", "gdboa", "ggdo", "gdro", "gdbe", "gdoa", "gdbj", "g1dbo", "g.d.b.o"} {
		if _, err := f.try(Pick, in); !errors.Is(err, ErrBadBonusPick) {
			t.Errorf("%q: err = %v", in, err)
		}
	}
	if !reflect.DeepEqual(before, f.view()) {
		t.Fatal("a refused pick changed the match")
	}
	u := f.do(Pick, "g, d, b / o")
	if u.View.Phase != BonusSolve || u.View.Board != "GOOD _OB" || u.Events[0].Text != "BDGO" {
		t.Fatalf("phase %v board %q events %+v", u.View.Phase, u.View.Board, u.Events)
	}
}

func TestBonusCorrectAddsPrize(t *testing.T) {
	f := bonus(t)
	f.do(Pick, "gdbo")
	u := f.solve("good job")
	r := u.Result
	if r == nil || !r.BonusWon || r.BonusPrize != bonusPrizes[0] || r.Awards[0].Gold != 1800+bonusPrizes[0] || r.Winner != "a" {
		t.Fatalf("result %+v", r)
	}
}

func TestBonusWrongNoPrize(t *testing.T) {
	f := bonus(t)
	f.do(Pick, "gdbo")
	r := f.solve("good jam").Result
	if r == nil || !r.BonusPlayed || r.BonusWon || r.Awards[0].Gold != 1800 {
		t.Fatalf("result %+v", r)
	}
}

func TestBonusPickTimeoutForfeits(t *testing.T) {
	f := bonus(t)
	f.advance(testOpts().TurnTimeout)
	us := f.m.Tick()
	if r := us[0].Result; r == nil || !r.BonusPlayed || r.BonusWon || r.Awards[0].Gold != 1800 {
		t.Fatalf("result %+v", us[0].Result)
	}
}

func TestBonusSolveTimeoutForfeits(t *testing.T) {
	f := bonus(t)
	f.do(Pick, "gdbo")
	f.advance(testOpts().TurnTimeout)
	us := f.m.Tick()
	if r := us[0].Result; r == nil || r.BonusWon || !has(us[0].Events, BonusLost) {
		t.Fatalf("updates %+v", us)
	}
}

func TestOnlyBonusPlayerMayAct(t *testing.T) {
	opts := testOpts()
	f := newFixture(t, opts)
	f.begin("a", "b")
	f.earn("l")
	f.solve("hello world")
	v := f.view()
	if _, err := f.m.Act(testChannel, Action{Kind: Pick, UserID: "b", Turn: v.Turn, Text: "gdbo"}); !errors.Is(err, ErrNotYourTurn) {
		t.Fatalf("err = %v", err)
	}
	for _, k := range []ActionKind{Spin, Consonant, Vowel} {
		if _, err := f.try(k, "o"); !errors.Is(err, ErrWrongPhase) {
			t.Fatalf("%v in the bonus: err = %v", k, err)
		}
	}
}

func TestBonusPrizeHiddenUntilDone(t *testing.T) {
	f := bonus(t)
	f.src.other = nil
	if f.view().BonusPrize != 0 {
		t.Fatal("the prize shows before the solve")
	}
	f.do(Pick, "gdbo")
	if f.view().BonusPrize != 0 {
		t.Fatal("the prize shows before the solve")
	}
	u := f.solve("good job")
	if u.View.BonusPrize == 0 || u.View.Board != "GOOD JOB" {
		t.Fatalf("final view %+v", u.View)
	}
}

func TestCalledIsSortedAndUnique(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	f.earn("l")
	f.earn("h")
	f.do(Vowel, "o")
	v := f.view()
	if !slices.IsSorted(v.Called) || string(v.Called) != "HLO" {
		t.Fatalf("called %q", string(v.Called))
	}
}

// --- intermission ---

// A solve used to replace the board in the same instant, so nobody saw the answer or who got
// it. The recap holds both, and the board shows the whole phrase.
func TestASolvePausesOnARecap(t *testing.T) {
	opts := testOpts()
	opts.Rounds = 2
	f := newFixture(t, opts, "phrase|hello world", "thing|good job", "place|big if true")
	f.begin("a", "b")
	f.earn("l")
	turn := f.view().Turn
	u := f.do(Solve, "hello world")
	v := u.View
	if v.Phase != Intermission || v.Current != "" || v.Turn == turn {
		t.Fatalf("phase %v current %q turn %d->%d", v.Phase, v.Current, turn, v.Turn)
	}
	want := Recap{Round: 1, Category: "phrase", Phrase: "HELLO WORLD", SolverID: "a", SolverName: "a", Gold: 1800}
	if v.Recap == nil || *v.Recap != want || v.Board != "HELLO WORLD" {
		t.Fatalf("recap %+v board %q", v.Recap, v.Board)
	}
	if !v.Deadline.Equal(f.now.Add(8 * time.Second)) {
		t.Fatalf("recap ends %v", v.Deadline)
	}
	if _, err := f.m.Act(testChannel, Action{Kind: Spin, UserID: "b", Turn: v.Turn}); !errors.Is(err, ErrWrongPhase) {
		t.Fatalf("spinning during a recap: %v", err)
	}
}

func TestTheRecapEndsOnItsOwn(t *testing.T) {
	opts := testOpts()
	opts.Rounds = 2
	opts.RecapPause = 5 * time.Second
	f := newFixture(t, opts, "phrase|hello world", "thing|good job", "place|big if true")
	f.begin("a")
	f.do(Solve, "hello world")
	f.advance(4 * time.Second)
	if len(f.m.Tick()) != 0 {
		t.Fatal("the recap ended early")
	}
	f.advance(time.Second)
	us := f.m.Tick()
	if len(us) != 1 || us[0].View.Phase != Round || us[0].View.Round != 2 || !has(us[0].Events, RoundStart) || us[0].View.Recap != nil {
		t.Fatalf("updates %+v", us)
	}
}

// Anyone still playing may skip the pause; somebody watching, or somebody who left, may not.
func TestAnyPlayerMayEndTheRecapButNotAStranger(t *testing.T) {
	opts := testOpts()
	opts.Rounds = 2
	f := newFixture(t, opts, "phrase|hello world", "thing|good job", "place|big if true")
	f.begin("a", "b", "c")
	f.ok(Action{Kind: Leave, UserID: "c"})
	f.do(Solve, "hello world")
	for _, who := range []string{"zed", "c"} {
		if _, err := f.m.Act(testChannel, Action{Kind: Next, UserID: who}); !errors.Is(err, ErrNotJoined) {
			t.Fatalf("%s: %v", who, err)
		}
	}
	u := f.ok(Action{Kind: Next, UserID: "b"})
	if u.View.Phase != Round || u.View.Round != 2 {
		t.Fatalf("view %+v", u.View)
	}
	if _, err := f.m.Act(testChannel, Action{Kind: Next, UserID: "b"}); !errors.Is(err, ErrWrongPhase) {
		t.Fatalf("a second press after the recap: %v", err)
	}
}

func TestTheLastRecapLeadsToTheBonus(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	f.earn("l")
	if u := f.do(Solve, "hello world"); u.View.Phase != Intermission || u.View.Recap.Round != 1 {
		t.Fatalf("the last round skipped its recap: %+v", u.View)
	}
	if u := f.ok(Action{Kind: Next, UserID: "a"}); u.View.Phase != BonusPick {
		t.Fatalf("phase %v", u.View.Phase)
	}
}

func TestTheTimeLimitStillAppliesDuringARecap(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	f.earn("l")
	f.do(Solve, "hello world")
	f.advance(testOpts().MaxDuration)
	us := f.m.Tick()
	if len(us) != 1 || us[0].Result == nil || us[0].Result.Reason != TimeLimit || us[0].Result.Awards[0].Gold != 1800 {
		t.Fatalf("updates %+v", us)
	}
}

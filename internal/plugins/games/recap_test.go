package games

import (
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/peregrine/internal/wordgame"
)

// M29: the run recap, and the ending that used to say nothing.

// TestAFinishedRunRecapsWhatPeopleTook.
//
// The thing a run has that a lone puzzle does not is a RESULT, and until now it ended with a
// pointer at the weekly board and no mention of what had just happened. Every number in the recap
// is ordinary points already counted on that board, so this is a report rather than a second
// scoring economy.
func TestAFinishedRunRecapsWhatPeopleTook(t *testing.T) {
	s, guard, _, _ := fixtureGauntlet(t)

	if consumed := s.Command("!wordgame", "2", testGuild, "c1", admin(snowflake(1))); !consumed {
		t.Fatal("!wordgame 2 was not consumed")
	}
	s.Guess(testGuild, "c1", snowflake(700), theWord, snowflake(42), "ann")
	s.sweep() // round two
	s.Guess(testGuild, "c1", snowflake(701), theWord, snowflake(43), "bob")

	recap := lastRecap(t, guard)
	if !strings.Contains(recap, "**ann**") || !strings.Contains(recap, "**bob**") {
		t.Errorf("the recap does not name both winners:\n%s", recap)
	}
	// Four apiece: the fixture's base, with no hints in play.
	if strings.Count(recap, " 4") != 2 {
		t.Errorf("the recap does not carry what each of them took:\n%s", recap)
	}
	if !strings.Contains(recap, "!leaderboard") {
		t.Error("the recap dropped the pointer at the weekly board, which is the season the " +
			"run is a match inside")
	}
}

// TestARunThatNobodyWonStillSaysSo. A missing line is indistinguishable from a bug, which is the
// same reason the leaderboard says "no points this week" rather than omitting the row.
func TestARunThatNobodyWonStillSaysSo(t *testing.T) {
	if got := gauntletDoneMessage(3, nil); !strings.Contains(got, "nobody scored") {
		t.Errorf("a run nobody scored in says nothing about it:\n%s", got)
	}
}

// TestARunEndingOnATimeoutStillAnnouncesItself is the gap M29 found.
//
// The closing message was sent from the win path only, so a run whose LAST round nobody got just
// stopped. That is finding 32's shape on the ending most likely to leave people wondering whether
// the bot broke: a run that ends badly is exactly when silence reads as a failure.
func TestARunEndingOnATimeoutStillAnnouncesItself(t *testing.T) {
	s, guard, _, _ := fixtureDict(t, enabled(), wordgame.Options{
		Timeout:     time.Millisecond,
		AnnounceTTL: 30 * time.Second,
		GauntletMax: 10,
	})

	if _, err := s.manager.Queue("c1", 1); err != nil {
		t.Fatalf("Queue: %v", err)
	}
	s.start("c1")
	time.Sleep(20 * time.Millisecond)
	s.sweep()

	var sawDone bool
	for _, p := range guard.posts() {
		if strings.Contains(p, "that's all") {
			sawDone = true
		}
	}
	if !sawDone {
		t.Errorf("a run whose last round timed out ended in silence:\n%s",
			strings.Join(guard.posts(), "\n---\n"))
	}
}

// TestTheTallyIsTakenRatherThanRead. Whoever reads a run's standings owns them: a tally left
// behind after the run it describes has ended is a per-channel map growing with every gauntlet
// the bot ever runs, which is the leak this repo has shipped twice.
func TestTheTallyIsTakenRatherThanRead(t *testing.T) {
	m := wordgame.NewManager(nil, nil, nil, wordgame.Options{})
	m.RecordRunWin("c1", "u1", "ann", 4)

	if got := m.TakeRunTally("c1"); len(got) != 1 {
		t.Fatalf("first take = %d entries, want 1", len(got))
	}
	if got := m.TakeRunTally("c1"); len(got) != 0 {
		t.Errorf("second take = %d entries, want none: the tally outlived the run it described", len(got))
	}
}

// TestStandingsAreOrderedAndStable. Points first, then wins, then user ID, which is arbitrary and
// stable. Stability is the requirement rather than the ordering: an unstable order makes two
// renderings of one result disagree.
func TestStandingsAreOrderedAndStable(t *testing.T) {
	m := wordgame.NewManager(nil, nil, nil, wordgame.Options{})
	m.RecordRunWin("c1", "b", "bob", 3)
	m.RecordRunWin("c1", "a", "ann", 3)
	m.RecordRunWin("c1", "c", "cai", 9)

	got := m.TakeRunTally("c1")
	if len(got) != 3 {
		t.Fatalf("got %d standings, want 3", len(got))
	}
	if got[0].UserID != "c" {
		t.Errorf("top standing is %q, want the highest score", got[0].UserID)
	}
	if got[1].UserID != "a" || got[2].UserID != "b" {
		t.Errorf("a tie ordered %q then %q, want the stable user-ID order",
			got[1].UserID, got[2].UserID)
	}
}

// TestAbandoningARunForgetsItsTally, so a run the guard refused cannot leave standings behind for
// a recap that will never be printed.
func TestAbandoningARunForgetsItsTally(t *testing.T) {
	m := wordgame.NewManager(nil, nil, nil, wordgame.Options{})
	m.RecordRunWin("c1", "u1", "ann", 4)
	m.Abandon("c1")

	if got := m.TakeRunTally("c1"); len(got) != 0 {
		t.Errorf("an abandoned run kept %d standings", len(got))
	}
}

// TestASoloWinIsNotRecorded. A lone puzzle has no run to report on, and recording one would
// accumulate standings in a channel that never prints them.
func TestASoloWinIsNotRecorded(t *testing.T) {
	s, _, manager, _ := fixtureGauntlet(t)
	s.start("c1")
	s.Guess(testGuild, "c1", snowflake(710), theWord, snowflake(42), "ann")

	if got := manager.TakeRunTally("c1"); len(got) != 0 {
		t.Errorf("a standalone win was recorded against a run: %+v", got)
	}
}

// lastRecap finds the run-closing message.
func lastRecap(t *testing.T, g *fakeGuard) string {
	t.Helper()
	for _, p := range g.posts() {
		if strings.Contains(p, "that's all") {
			return p
		}
	}
	t.Fatalf("no run-closing message in:\n%s", strings.Join(g.posts(), "\n---\n"))
	return ""
}

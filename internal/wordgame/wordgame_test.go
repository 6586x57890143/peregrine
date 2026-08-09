package wordgame

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func seeded(a, b uint64) Source { return rand.New(rand.NewPCG(a, b)) }

// dictOf builds a Dictionary from literal words, bypassing the file load, so a test can
// use a word the real list would never contain.
func dictOf(words ...string) *Dictionary { return &Dictionary{words: words} }

func testOpts() Options {
	return Options{
		Timeout:           time.Minute,
		AnnounceTTL:       30 * time.Second,
		ActivityWindow:    5 * time.Minute,
		ActivityThreshold: 3,
		TriggerChance:     1.0, // deterministic: always fire once the threshold is met
		MaxChannels:       10,
	}
}

// ---------------------------------------------------------------- the scramble

// TestScrambleTerminatesOnAWordOfIdenticalLetters is the crash pin.
//
// The old implementation recursed whenever a shuffle reproduced the original word, with no
// depth limit. For a word whose letters are all the same, that condition can NEVER be
// false, so it recursed until the stack died and took the process with it. It was
// unreachable with the shipped dictionary and reachable with an operator's, which is the
// worst combination: it could only ever have fired in production.
//
// This test would not have failed before, it would have crashed the test binary, which is
// the point.
func TestScrambleTerminatesOnAWordOfIdenticalLetters(t *testing.T) {
	for _, word := range []string{"aa", "aaa", "aaaaaaaa"} {
		got := scramble(seeded(1, 2), word)
		if got != word {
			t.Errorf("scramble(%q) = %q; a word with one distinct letter cannot differ, so "+
				"returning it unchanged is the only correct answer", word, got)
		}
	}
}

// TestScrambleAlwaysDiffersForARealWord. A puzzle whose answer is printed in the question
// is not a puzzle, and the shuffle can legitimately reproduce the input by chance.
func TestScrambleAlwaysDiffersForARealWord(t *testing.T) {
	src := seeded(3, 4)
	for _, word := range []string{"ab", "aab", "abab", "banana", "peregrine"} {
		for range 200 {
			if got := scramble(src, word); got == word {
				t.Fatalf("scramble(%q) returned the original word", word)
			}
		}
	}
}

// TestScrambleKeepsTheSameLetters, because a scramble that lost or gained a letter makes
// the puzzle unsolvable and nobody would be able to say why.
func TestScrambleKeepsTheSameLetters(t *testing.T) {
	src := seeded(5, 6)
	for _, word := range []string{"banana", "peregrine", "abab"} {
		got := scramble(src, word)
		if sortedRunes(got) != sortedRunes(word) {
			t.Errorf("scramble(%q) = %q, which is not an anagram", word, got)
		}
	}
}

// TestRotateAlwaysChangesAMultiLetterWord covers the fallback the attempt bound falls
// through to. "abab" is the interesting case: rotating by two leaves it unchanged, so a
// fallback that tried only one offset would return the original.
func TestRotateAlwaysChangesAMultiLetterWord(t *testing.T) {
	for _, word := range []string{"ab", "abab", "aabb", "abcabc"} {
		if got := rotate([]rune(word)); got == word {
			t.Errorf("rotate(%q) returned the original; the fallback must find an offset "+
				"that changes the word", word)
		}
	}
}

func sortedRunes(s string) string {
	r := []rune(s)
	for i := 1; i < len(r); i++ {
		for j := i; j > 0 && r[j] < r[j-1]; j-- {
			r[j], r[j-1] = r[j-1], r[j]
		}
	}
	return string(r)
}

// ---------------------------------------------------------------- the dictionary

// TestDictionaryRejectsUnscrambleableWords is the other half of the crash fix, and having
// both is deliberate: this one depends on the operator's word list and the scramble bound
// does not, so neither alone would be enough.
func TestDictionaryRejectsUnscrambleableWords(t *testing.T) {
	path := writeWords(t, "aaaaa", "bbbbbb", "banana", "peregrine")

	d, err := LoadDictionary(path, DictionaryOptions{MinLength: 5, MaxLength: 12})
	if err != nil {
		t.Fatalf("LoadDictionary: %v", err)
	}
	for _, w := range d.words {
		if w == "aaaaa" || w == "bbbbbb" {
			t.Errorf("dictionary kept %q, which cannot be scrambled into anything else", w)
		}
	}
	if d.Len() != 2 {
		t.Errorf("kept %d words, want the 2 scrambleable ones: %v", d.Len(), d.words)
	}
}

// TestDictionaryFiltersOnRunesNotBytes. The old filter was `len(word) > 4`, comparing
// bytes, so a five-letter word with an accented character counted as six and a
// four-letter one could slip through.
func TestDictionaryFiltersOnRunesNotBytes(t *testing.T) {
	// Five runes, seven bytes.
	const accented = "crèpe"
	path := writeWords(t, accented, "abcd", "abcde")

	d, err := LoadDictionary(path, DictionaryOptions{MinLength: 5, MaxLength: 12})
	if err != nil {
		t.Fatalf("LoadDictionary: %v", err)
	}
	if len(d.words) != 2 {
		t.Fatalf("kept %v, want the two five-rune words", d.words)
	}
	var sawAccented bool
	for _, w := range d.words {
		if w == accented {
			sawAccented = true
		}
		if w == "abcd" {
			t.Error("kept a four-rune word")
		}
	}
	if !sawAccented {
		t.Errorf("dropped %q, which is five RUNES even though it is seven bytes", accented)
	}
}

func TestDictionaryRejectsNonLetters(t *testing.T) {
	path := writeWords(t, "two words", "hyphen-ated", "digits123", "cleanword")

	d, err := LoadDictionary(path, DictionaryOptions{})
	if err != nil {
		t.Fatalf("LoadDictionary: %v", err)
	}
	if d.Len() != 1 || d.words[0] != "cleanword" {
		t.Errorf("kept %v, want only the letters-only word: an answer nobody can type is "+
			"not a puzzle", d.words)
	}
}

// TestDictionaryWithNoUsableWordsIsAnError, and the message says why entries were
// rejected: "the dictionary is empty" sends an operator looking at a file that has plenty
// of words in it.
func TestDictionaryWithNoUsableWordsIsAnError(t *testing.T) {
	path := writeWords(t, "a", "bb", "aaaa")

	_, err := LoadDictionary(path, DictionaryOptions{MinLength: 5, MaxLength: 12})
	if err == nil {
		t.Fatal("a dictionary with no usable words must be an error")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error should say how many entries were rejected and why, got: %v", err)
	}
}

func writeWords(t *testing.T, words ...string) string {
	t.Helper()
	path := t.TempDir() + "/words.txt"
	if err := os.WriteFile(path, []byte(strings.Join(words, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write dictionary: %v", err)
	}
	return path
}

// ---------------------------------------------------------------- the manager

// counter is a settable message count standing in for internal/activity's tracker,
// which satisfies wordgame.Counter structurally. It also records the window it was
// asked about, because which window the Manager passes is part of the contract: how
// busy is busy enough for a word game is this feature's judgement, not the tracker's.
type counter struct {
	n      int
	asked  time.Duration
	called int
}

func (c *counter) Count(_ string, window time.Duration) int {
	c.asked = window
	c.called++
	return c.n
}

func counting(n int) *counter { return &counter{n: n} }

func TestStartAndSolveAGame(t *testing.T) {
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), testOpts())

	g, err := m.Start("chan")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if g.Word != "peregrine" {
		t.Fatalf("word = %q", g.Word)
	}
	if g.Scrambled == g.Word {
		t.Error("the puzzle prints its own answer")
	}
	m.Announced("chan", "msg1")

	if _, ok := m.Guess("chan", "nonsense"); ok {
		t.Error("a wrong guess won")
	}
	// Case and surrounding space are forgiven: typing the right word in the wrong case
	// has solved the puzzle.
	won, ok := m.Guess("chan", "  PEREGRINE ")
	if !ok {
		t.Fatal("the right answer in the wrong case did not win")
	}
	if won.MessageID != "msg1" {
		t.Errorf("resolved game carries MessageID %q, want the announcement's", won.MessageID)
	}
	if m.Active() != 0 {
		t.Error("the game is still live after being solved")
	}
}

// TestOnlyOneWinnerPerGame. The old check and removal were separate statements under a
// lock released in between, so two people guessing at the same instant could both be told
// they had won and the leaderboard would record two wins for one puzzle.
func TestOnlyOneWinnerPerGame(t *testing.T) {
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), testOpts())
	if _, err := m.Start("chan"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := m.Guess("chan", "peregrine"); ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Errorf("%d simultaneous correct guesses all won, want exactly 1", wins)
	}
}

func TestStartRefusesASecondGameInTheSameChannel(t *testing.T) {
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), testOpts())
	if _, err := m.Start("chan"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := m.Start("chan"); !errors.Is(err, ErrGameInProgress) {
		t.Errorf("second Start returned %v, want ErrGameInProgress", err)
	}
	// A different channel is unaffected, because games are per channel.
	if _, err := m.Start("other"); err != nil {
		t.Errorf("Start in another channel: %v", err)
	}
}

func TestAnUnavailableManagerRefusesToStart(t *testing.T) {
	m := NewManager(dictOf(), seeded(1, 2), counting(0), testOpts())
	if m.Available() {
		t.Fatal("a manager with no words reports itself available")
	}
	if _, err := m.Start("chan"); !errors.Is(err, ErrNoDictionary) {
		t.Errorf("Start returned %v, want ErrNoDictionary", err)
	}
	if m.MaybeStart("chan") {
		t.Error("an unavailable manager asked for a game to be started")
	}
}

// TestExpiredSweepsInsteadOfSpawningTimers is the pin for the goroutine problem.
//
// Every started game used to spawn a goroutine that slept for the timeout and then acted,
// plus one per announcement to delete it later. None took a context, so after shutdown they
// woke against a closed session, and the count was bounded only by how often people played.
// One sweep replaces all of them.
func TestExpiredSweepsInsteadOfSpawningTimers(t *testing.T) {
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), testOpts())

	now := time.Now()
	m.now = func() time.Time { return now }

	if _, err := m.Start("a"); err != nil {
		t.Fatalf("Start a: %v", err)
	}
	if _, err := m.Start("b"); err != nil {
		t.Fatalf("Start b: %v", err)
	}
	if got := m.Expired(); len(got) != 0 {
		t.Fatalf("games expired before their timeout: %d", len(got))
	}

	now = now.Add(2 * time.Minute)
	expired := m.Expired()
	if len(expired) != 2 {
		t.Fatalf("%d games expired, want 2", len(expired))
	}
	// Sorted, so the caller's announcements do not follow Go's randomized map iteration.
	if expired[0].ChannelID != "a" || expired[1].ChannelID != "b" {
		t.Errorf("expired games are not in a deterministic order: %v, %v",
			expired[0].ChannelID, expired[1].ChannelID)
	}
	if m.Active() != 0 {
		t.Error("expired games are still live")
	}
	if got := m.Expired(); len(got) != 0 {
		t.Error("Expired returned the same games twice, so the caller would announce twice")
	}
}

func TestDueDeletionsAreSweptOnce(t *testing.T) {
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), testOpts())
	now := time.Now()
	m.now = func() time.Time { return now }

	m.DeleteLater("chan", "msg1")
	m.DeleteLater("chan", "") // no message: nothing to delete, must not be queued

	if got := m.DueDeletions(); len(got) != 0 {
		t.Fatalf("%d deletions came due immediately", len(got))
	}

	now = now.Add(time.Minute)
	due := m.DueDeletions()
	if len(due) != 1 || due[0].MessageID != "msg1" {
		t.Fatalf("due deletions = %v, want just msg1", due)
	}
	if got := m.DueDeletions(); len(got) != 0 {
		t.Error("a deletion came due twice, so the bot would try to delete a gone message")
	}
}

// TestAnnounceTTLZeroKeepsAnnouncements, so an operator who wants the win messages to stay
// can have that rather than being forced into a cleanup they did not ask for.
func TestAnnounceTTLZeroKeepsAnnouncements(t *testing.T) {
	o := testOpts()
	o.AnnounceTTL = 0
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), o)

	m.DeleteLater("chan", "msg1")
	if got := m.DueDeletions(); len(got) != 0 {
		t.Error("a deletion was queued with the TTL disabled")
	}
}

// TestNoteRequiresTheActivityThreshold. A quiet channel should not get a puzzle nobody is
// around to solve.
func TestMaybeStartRequiresTheActivityThreshold(t *testing.T) {
	o := testOpts()
	o.ActivityThreshold = 3
	c := counting(2) // one short
	m := NewManager(dictOf("peregrine"), seeded(1, 2), c, o)

	if m.MaybeStart("chan") {
		t.Error("a game was requested in a channel below the activity threshold")
	}
	c.n = 3
	if !m.MaybeStart("chan") {
		t.Error("the threshold was reached and no game was requested (TriggerChance is 1)")
	}
}

// TestMaybeStartAsksAboutItsOwnWindow. The counting moved to internal/activity, which
// keeps timestamps precisely so each consumer can bring its own window: word games want
// "is it busy right now", aggro wants "who is around". If the Manager stopped passing
// ActivityWindow, PEREGRINE_WORDGAME_ACTIVITY_WINDOW would quietly stop meaning anything.
func TestMaybeStartAsksAboutItsOwnWindow(t *testing.T) {
	o := testOpts()
	o.ActivityWindow = 90 * time.Second
	o.ActivityThreshold = 1
	c := counting(5)
	m := NewManager(dictOf("peregrine"), seeded(1, 2), c, o)

	m.MaybeStart("chan")
	if c.asked != 90*time.Second {
		t.Errorf("the Manager asked about a window of %s, want its own ActivityWindow of 90s", c.asked)
	}
}

// TestALiveGameSkipsTheCounterEntirely. Cheapest gate first: a channel already playing
// cannot start another game, so there is no reason to ask how busy it is.
func TestALiveGameSkipsTheCounterEntirely(t *testing.T) {
	o := testOpts()
	o.ActivityThreshold = 1
	c := counting(100)
	m := NewManager(dictOf("peregrine"), seeded(1, 2), c, o)

	if _, err := m.Start("chan"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	c.called = 0
	if m.MaybeStart("chan") {
		t.Error("a second game was requested in a channel already playing one")
	}
	if c.called != 0 {
		t.Errorf("the counter was consulted %d times for a channel already playing", c.called)
	}
}

// TestACooldownFollowsAGame. Starting a game used to clear the channel's activity count so
// the trigger had to build up again. The count is a shared observer now and zeroing it
// would lie to the aggro and autonomous-post features about how busy the channel is, so
// the Manager keeps its own cooldown of one ActivityWindow, which reproduces the same
// behaviour without touching anyone else's data.
func TestACooldownFollowsAGame(t *testing.T) {
	o := testOpts()
	o.ActivityThreshold = 1
	o.ActivityWindow = 5 * time.Minute
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(50), o)

	now := time.Now()
	m.now = func() time.Time { return now }

	if !m.MaybeStart("chan") {
		t.Fatal("a busy channel with no game was refused one")
	}
	if _, err := m.Start("chan"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := m.Guess("chan", "peregrine"); !ok {
		t.Fatal("could not solve")
	}

	// Solved, so no game is live, but the cooldown still holds.
	if m.MaybeStart("chan") {
		t.Error("a game was requested immediately after the previous one ended")
	}
	now = now.Add(5*time.Minute + time.Second)
	if !m.MaybeStart("chan") {
		t.Error("the cooldown never expired")
	}
}

// TestTheCooldownMapDoesNotAccumulateChannelsForever. It is keyed by channel, so it grows
// with every guild the bot joins. Same slow leak the conversation memory had before M7b,
// and the same kind a test using one channel would never reveal.
func TestTheCooldownMapDoesNotAccumulateChannelsForever(t *testing.T) {
	o := testOpts()
	o.MaxChannels = 5
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), o)

	for i := range 50 {
		if _, err := m.Start("chan" + string(rune('a'+i%26)) + string(rune('a'+i/26))); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}

	m.mu.Lock()
	n := len(m.started)
	m.mu.Unlock()
	if n > o.MaxChannels {
		t.Errorf("tracking %d cooldowns against a cap of %d", n, o.MaxChannels)
	}
}

// TestNoCounterMeansNoGames. A nil Counter is a wiring mistake, and it fails in the quiet
// direction: no way to tell whether a channel is busy is a reason to start no games, never
// a reason to start one on every message.
func TestNoCounterMeansNoGames(t *testing.T) {
	o := testOpts()
	o.ActivityThreshold = 1
	m := NewManager(dictOf("peregrine"), seeded(1, 2), nil, o)

	for range 20 {
		if m.MaybeStart("chan") {
			t.Fatal("a game was requested with no counter wired up")
		}
	}
}

// ---------------------------------------------------------------- the leaderboard

// TestLeaderboardResetsOnANewWeekAndCatchesUp is the pin for finding 17.
//
// The old check asked pool.ntp.org what day it was and reset only when the answer was
// Monday between 00:00 and 00:59 UTC. A failed query in that one-hour window skipped the
// reset for a week, and downtime across Monday morning skipped it entirely. Comparing week
// boundaries catches up instead: a bot that was off all Monday resets on its first tick
// back.
func TestLeaderboardResetsOnANewWeekAndCatchesUp(t *testing.T) {
	// A Wednesday.
	start := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)
	l := NewLeaderboard(start)
	l.AddWin("u1", "alice")

	if l.MaybeReset(start.Add(time.Hour)) {
		t.Error("reset within the same week")
	}
	if len(l.Entries()) != 1 {
		t.Error("scores were cleared without a reset")
	}

	// The following THURSDAY: well past Monday, so the old code would have missed the
	// window entirely and left the board stale for another week.
	next := start.AddDate(0, 0, 8)
	if !l.MaybeReset(next) {
		t.Fatal("a new week did not reset the board. The old NTP check only fired during " +
			"one hour on Monday, so downtime or a failed query skipped a whole week")
	}
	if len(l.Entries()) != 0 {
		t.Error("the board was not cleared")
	}
	if got := l.WeekStart(); got != StartOfWeekUTC(next) {
		t.Errorf("week start = %v, want %v", got, StartOfWeekUTC(next))
	}

	// Idempotent: a second tick in the same week must not reset again, or a win recorded
	// between ticks would vanish.
	l.AddWin("u2", "bob")
	if l.MaybeReset(next.Add(time.Hour)) {
		t.Error("reset twice in one week, discarding a win")
	}
	if len(l.Entries()) != 1 {
		t.Error("the win recorded after the reset was lost")
	}
}

func TestStartOfWeekUTCIsMondayMidnight(t *testing.T) {
	cases := map[string]time.Time{
		// A Monday stays where it is, at midnight.
		"2024-06-03T00:00:00Z": time.Date(2024, 6, 3, 0, 0, 0, 0, time.UTC),
		"2024-06-03T23:59:59Z": time.Date(2024, 6, 3, 0, 0, 0, 0, time.UTC),
		// A Sunday belongs to the week that began six days earlier, which is the case a
		// naive weekday subtraction gets wrong because Sunday is 0.
		"2024-06-09T12:00:00Z": time.Date(2024, 6, 3, 0, 0, 0, 0, time.UTC),
		"2024-06-05T12:00:00Z": time.Date(2024, 6, 3, 0, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		at, err := time.Parse(time.RFC3339, in)
		if err != nil {
			t.Fatal(err)
		}
		if got := StartOfWeekUTC(at); !got.Equal(want) {
			t.Errorf("StartOfWeekUTC(%s) = %v, want %v", in, got, want)
		}
	}
}

// TestLeaderboardMarshalsUnderTheLock is the pin for the data race.
//
// The mutex used to be an EXPORTED field of the struct that gets marshalled, and the
// marshalling ran outside the lock while AddWin held it: concurrent map read and write,
// which in Go is a fatal runtime error rather than a recoverable panic. This test is here
// mostly for the race detector in CI.
func TestLeaderboardMarshalsUnderTheLock(t *testing.T) {
	l := NewLeaderboard(time.Now())

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.AddWin("u"+string(rune('a'+i%26)), "player")
		}(i)
	}
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := json.Marshal(l); err != nil {
				t.Errorf("Marshal: %v", err)
			}
		}()
	}
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.Format(time.Now())
			_ = l.Entries()
		}()
	}
	wg.Wait()
}

// TestLeaderboardRoundTripsAndReadsTheOldFormat. The leaderboard is not derivable from
// anything else, unlike the corpus, so a format change must not discard a week of wins.
func TestLeaderboardRoundTripsAndReadsTheOldFormat(t *testing.T) {
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)

	original := NewLeaderboard(now)
	original.AddWin("u1", "alice")
	original.AddWin("u1", "alice")
	original.AddWin("u2", "bob")

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), "Mutex") {
		t.Error("the mutex was serialized")
	}

	var restored Leaderboard
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := restored.Entries(); len(got) != 2 || got[0].Wins != 2 || got[0].Username != "alice" {
		t.Errorf("round trip lost data: %+v", got)
	}
	if got := restored.WeekStart(); got != StartOfWeekUTC(now) {
		t.Errorf("week start = %v, want %v", got, StartOfWeekUTC(now))
	}

	// The pre-M11 shape, with week_start_date and last_reset.
	old := `{"week_start_date":"2024-06-03T00:00:00Z","last_reset":"2024-06-03T00:00:00Z",` +
		`"scores":{"u9":{"user_id":"u9","username":"carol","wins":7}}}`
	var legacyBoard Leaderboard
	if err := json.Unmarshal([]byte(old), &legacyBoard); err != nil {
		t.Fatalf("Unmarshal old format: %v", err)
	}
	got := legacyBoard.Entries()
	if len(got) != 1 || got[0].Wins != 7 {
		t.Errorf("the pre-M11 format lost its scores: %+v. Unlike the corpus, a week of "+
			"wins cannot be rebuilt from anything", got)
	}
	if legacyBoard.WeekStart() != time.Date(2024, 6, 3, 0, 0, 0, 0, time.UTC) {
		t.Errorf("the old week start was not carried over: %v", legacyBoard.WeekStart())
	}
}

func TestLeaderboardEntriesAreStablyOrdered(t *testing.T) {
	l := NewLeaderboard(time.Now())
	l.AddWin("u1", "zoe")
	l.AddWin("u2", "adam")
	l.AddWin("u3", "carol")
	l.AddWin("u3", "carol")

	for range 20 {
		got := l.Entries()
		if got[0].Username != "carol" {
			t.Fatalf("top entry = %q, want carol with 2 wins", got[0].Username)
		}
		// Equal wins break on username, so the order does not shuffle between calls.
		if got[1].Username != "adam" || got[2].Username != "zoe" {
			t.Fatalf("tied entries are not stably ordered: %+v", got)
		}
	}
}

func TestFormatIsReadableWhenEmptyAndWhenFull(t *testing.T) {
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)

	empty := NewLeaderboard(now).Format(now)
	if !strings.Contains(empty, "empty") {
		t.Errorf("empty board does not say so: %q", empty)
	}

	l := NewLeaderboard(now)
	for i := range 15 {
		id := "u" + string(rune('a'+i))
		for range 15 - i {
			l.AddWin(id, "player"+string(rune('a'+i)))
		}
	}
	full := l.Format(now)
	if strings.Count(full, "\n") > 20 {
		t.Errorf("the board shows more than the top ten:\n%s", full)
	}
	// The next reset is derived from the same boundary MaybeReset uses, so the promise and
	// the behaviour cannot disagree.
	nextReset := StartOfWeekUTC(now).AddDate(0, 0, 7)
	if !strings.Contains(full, itoa(nextReset.Unix())) {
		t.Errorf("the board does not show the same reset time the reset uses:\n%s", full)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

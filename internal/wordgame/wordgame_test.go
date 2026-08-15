package wordgame

import (
	"encoding/json"
	"errors"
	"fmt"
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
//
// It carries the DEFAULT options rather than a zero value, because those are what a planted
// word is held to: a zero MaxLength would refuse every word, so Usable would look correct for
// the wrong reason.
func dictOf(words ...string) *Dictionary {
	return &Dictionary{words: words, opts: DictionaryOptions{}.withDefaults()}
}

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

// ---------------------------------------------------------------- hints, M21b

// TestAHintIsSweptOnceAndOnlyWhenItIsDue.
//
// A hint is not a fourth timer. It is another thing the sweep already running notices, which is
// what keeps "one sweep, not a goroutine per game" true as this feature grows: the alternative
// is a goroutine per puzzle again, and that is the bug the sweep exists to have removed.
func TestAHintIsSweptOnceAndOnlyWhenItIsDue(t *testing.T) {
	o := testOpts()
	o.HintAfter = 20 * time.Second
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), o)

	now := time.Now()
	m.now = func() time.Time { return now }

	if _, err := m.Start("chan"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Announced("chan", "msg1")

	if got := m.DueHints(); len(got) != 0 {
		t.Fatalf("a hint was due immediately: %d", len(got))
	}

	now = now.Add(30 * time.Second)
	due := m.DueHints()
	if len(due) != 1 || due[0].ChannelID != "chan" {
		t.Fatalf("due hints = %v, want one for chan", due)
	}
	if got := due[0].Letters(); got != len("peregrine") {
		t.Errorf("Letters() = %d, want %d", got, len("peregrine"))
	}

	// The rung is still due until the caller says it landed, which is M25's split: DueHints no
	// longer advances the level, because a repost the guard refuses must not charge the winner
	// for a hint nobody saw.
	if got := m.DueHints(); len(got) != 1 {
		t.Errorf("an unacknowledged hint stopped being due (%d), so a refused repost would be "+
			"silently skipped rather than retried", len(got))
	}
	m.HintDelivered("chan", 1)

	// Acknowledged, so the same rung must not come round again: it would repost the identical
	// card and delete the one people are looking at, for nothing.
	if got := m.DueHints(); len(got) != 0 {
		t.Error("the same hint came due twice after being acknowledged")
	}
	if got := due[0].Mask(); !strings.HasPrefix(got, "p ") {
		t.Errorf("Mask() = %q, want it to open on the revealed first letter", got)
	}
}

// TestAHintIsNotDueForAGameThatWasSolved. Solving removes the game, so there is nothing left to
// hint at, and a hint arriving after the answer has been announced is the one visible way this
// could be wrong.
func TestAHintIsNotDueForAGameThatWasSolved(t *testing.T) {
	o := testOpts()
	o.HintAfter = 20 * time.Second
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), o)

	now := time.Now()
	m.now = func() time.Time { return now }

	if _, err := m.Start("chan"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Announced("chan", "msg1")
	if _, ok := m.Guess("chan", "peregrine"); !ok {
		t.Fatal("the game was not solved")
	}

	now = now.Add(30 * time.Second)
	if got := m.DueHints(); len(got) != 0 {
		t.Errorf("a solved game produced a hint: %v", got)
	}
}

// TestAGameWithNoAnnouncementIsNotHinted. The caller EDITS the announcement to add the hint, and
// there is nothing to edit when the guard refused the original send: a paused bot or an ignored
// channel. Returning it anyway would produce an edit against an empty message ID every tick.
func TestAGameWithNoAnnouncementIsNotHinted(t *testing.T) {
	o := testOpts()
	o.HintAfter = 20 * time.Second
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), o)

	now := time.Now()
	m.now = func() time.Time { return now }

	if _, err := m.Start("chan"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// No Announced call: the send was refused.

	now = now.Add(30 * time.Second)
	if got := m.DueHints(); len(got) != 0 {
		t.Errorf("an unannounced game produced a hint to edit into nothing: %v", got)
	}
}

// TestHintsOffProducesNoHints, which is what HintAfter zero has to mean.
func TestHintsOffProducesNoHints(t *testing.T) {
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), testOpts()) // HintAfter unset
	now := time.Now()
	m.now = func() time.Time { return now }

	if _, err := m.Start("chan"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Announced("chan", "msg1")

	now = now.Add(time.Hour)
	if got := m.DueHints(); len(got) != 0 {
		t.Errorf("hints are off and one fired anyway: %v", got)
	}
}

// TestAHintDueAtOrPastTheDeadlineIsTurnedOff rather than clamped.
//
// A hint that lands after the puzzle has ended is a knob wired to nothing, which is worse than
// no knob: the operator tunes it, nothing happens, and the feature gets blamed for ignoring
// configuration. Config refuses this outright; withDefaults is the second line, for a caller
// that builds Options directly.
func TestAHintDueAtOrPastTheDeadlineIsTurnedOff(t *testing.T) {
	for _, after := range []time.Duration{-time.Second, time.Minute, 2 * time.Minute} {
		o := testOpts() // Timeout is a minute
		o.HintAfter = after
		if got := o.withDefaults().HintAfter; got != 0 {
			t.Errorf("HintAfter %v against a one-minute timeout became %v, want 0", after, got)
		}
	}
}

// ---------------------------------------------------------------- planted words, M21b

// TestAPlantedWordIsHeldToTheDictionarysOwnRules.
//
// LoadDictionary excludes words with fewer than two distinct letters SPECIFICALLY because
// scramble used to recurse forever on them, and scramble's own bound is documented as a belt to
// those braces. A word arriving by a route that skipped the loader would leave the belt doing
// that work alone, which is exactly the arrangement that produced the original stack overflow.
func TestAPlantedWordIsHeldToTheDictionarysOwnRules(t *testing.T) {
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), testOpts())

	for _, word := range []string{
		"aaaaa",                        // one distinct letter: what made scramble recurse
		"hi",                           // below the minimum length
		"antidisestablishmentarianism", // above the maximum
		"two words",                    // not letters
		"b4nana",                       // not letters
	} {
		if _, err := m.StartWord("chan", word); !errors.Is(err, ErrUnusableWord) {
			t.Errorf("StartWord(%q) error = %v, want ErrUnusableWord", word, err)
		}
		if m.Active() != 0 {
			t.Fatalf("a refused word %q started a game anyway", word)
		}
	}

	g, err := m.StartWord("chan", "banana")
	if err != nil {
		t.Fatalf("StartWord on a usable word: %v", err)
	}
	if g.Word != "banana" {
		t.Errorf("word = %q, want the planted one", g.Word)
	}
	if !g.Planted {
		t.Error("a chosen word was not marked as planted, so the log cannot tell the two apart")
	}
	if g.Scrambled == g.Word {
		t.Error("the puzzle prints its own answer")
	}
}

// TestADrawnWordIsNotMarkedPlanted, which is the other half of the same flag.
func TestADrawnWordIsNotMarkedPlanted(t *testing.T) {
	m := NewManager(dictOf("peregrine"), seeded(1, 2), counting(0), testOpts())
	g, err := m.Start("chan")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if g.Planted {
		t.Error("a word drawn from the dictionary was marked as planted")
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
	l.AddWin("u1", "alice", time.Second, 1)

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
	l.AddWin("u2", "bob", time.Second, 1)
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
			l.AddWin("u"+string(rune('a'+i%26)), "player", time.Second, 1)
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
			_ = l.Scores()
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
	original.AddWin("u1", "alice", time.Second, 1)
	original.AddWin("u1", "alice", time.Second, 1)
	original.AddWin("u2", "bob", time.Second, 1)

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

	// M21b's fields default to zero rather than failing the load, which is what omitempty buys.
	// A board persisted before this milestone has no streaks and no records, and saying so is
	// correct; refusing to load it would throw away a week nothing else can rebuild.
	if e := got[0]; e.Streak != 0 || e.Best != 0 || e.FastestMS != 0 {
		t.Errorf("an M11-era entry arrived with invented records: %+v", e)
	}
	if _, ok := legacyBoard.Fastest(); ok {
		t.Error("a board with no recorded solve times claimed a fastest solve")
	}
	if _, ok := legacyBoard.Streak(); ok {
		t.Error("a board with no last winner claimed a streak")
	}
}

// ---------------------------------------------------------------- streaks and records, M21b

// TestAStreakBelongsToTheCurrentWinnerOnly.
//
// Streak is only ever read for whoever won last, which is why AddWin does not clear the previous
// holder's number: it is stale the moment lastWinner changes, and clearing it would mean
// touching an entry this win has nothing to do with.
func TestAStreakBelongsToTheCurrentWinnerOnly(t *testing.T) {
	l := NewLeaderboard(time.Now())

	l.AddWin("u1", "alice", 2*time.Second, 1)
	if _, ok := l.Streak(); ok {
		t.Error("one win was reported as a streak. That line would appear after every game")
	}

	l.AddWin("u1", "alice", 3*time.Second, 1)
	e, ok := l.Streak()
	if !ok || e.UserID != "u1" || e.Streak != 2 {
		t.Errorf("streak = %+v, %v, want alice on 2", e, ok)
	}

	// Somebody else winning ends it rather than shortening it.
	l.AddWin("u2", "bob", 4*time.Second, 1)
	if _, ok := l.Streak(); ok {
		t.Error("alice's streak survived bob winning")
	}

	// And the best is kept even though the current run is over.
	for _, entry := range l.Entries() {
		if entry.UserID == "u1" && entry.Best != 2 {
			t.Errorf("alice's best streak = %d, want 2", entry.Best)
		}
	}
}

// TestFastestKeepsThePersonalBestAndTheWeeklyRecord.
func TestFastestKeepsThePersonalBestAndTheWeeklyRecord(t *testing.T) {
	l := NewLeaderboard(time.Now())

	l.AddWin("u1", "alice", 5*time.Second, 1)
	l.AddWin("u2", "bob", 2*time.Second, 1)
	l.AddWin("u1", "alice", 3*time.Second, 1) // alice improves, but not past bob

	e, ok := l.Fastest()
	if !ok || e.UserID != "u2" || e.FastestMS != 2000 {
		t.Errorf("fastest = %+v, %v, want bob at 2000ms", e, ok)
	}
	for _, entry := range l.Entries() {
		if entry.UserID == "u1" && entry.FastestMS != 3000 {
			t.Errorf("alice's personal best = %dms, want 3000: a slower later solve overwrote it",
				entry.FastestMS)
		}
	}

	// A win with no measured time records no record, rather than claiming an instant solve.
	l2 := NewLeaderboard(time.Now())
	l2.AddWin("u1", "alice", 0, 1)
	if _, ok := l2.Fastest(); ok {
		t.Error("a win with no solve time was recorded as the fastest of the week")
	}
}

// TestTheWeeklyResetClearsTheStreakToo. Everything else goes; a run that survived the reset
// would let somebody hold a streak against a board that no longer has their wins on it.
func TestTheWeeklyResetClearsTheStreakToo(t *testing.T) {
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC)
	l := NewLeaderboard(now)
	l.AddWin("u1", "alice", time.Second, 1)
	l.AddWin("u1", "alice", time.Second, 1)
	if _, ok := l.Streak(); !ok {
		t.Fatal("no streak to reset")
	}

	if !l.MaybeReset(now.AddDate(0, 0, 7)) {
		t.Fatal("the week did not turn over")
	}
	if _, ok := l.Streak(); ok {
		t.Error("a streak survived the weekly reset")
	}
	if _, ok := l.Fastest(); ok {
		t.Error("a record survived the weekly reset")
	}
}

func TestLeaderboardEntriesAreStablyOrdered(t *testing.T) {
	l := NewLeaderboard(time.Now())
	l.AddWin("u1", "zoe", time.Second, 1)
	l.AddWin("u2", "adam", time.Second, 1)
	l.AddWin("u3", "carol", time.Second, 1)
	l.AddWin("u3", "carol", time.Second, 1)

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

// Ranking has to work from SCORES ALONE, with no name resolution anywhere in it. That is what
// took the leaderboard command from one Discord request per weekly talker down to at most
// eleven per board: a rank is one plus the number of people strictly ahead, and nobody's
// nickname is part of that sum.
func TestRankNeedsNoNamesAndUsesCompetitionRanking(t *testing.T) {
	scores := map[string]int{
		"a": 10,
		"b": 7,
		"c": 7, // tied with b
		"d": 3,
		"e": 0, // no score at all, so not a player
	}

	board := Rank(scores, "", 10)

	if board.Players != 4 {
		t.Errorf("Players = %d, want 4: a zero score is not a placing", board.Players)
	}
	if len(board.Top) != 4 {
		t.Fatalf("Top has %d rows, want 4: %+v", len(board.Top), board.Top)
	}

	wantRanks := map[string]int{"a": 1, "b": 2, "c": 2, "d": 4}
	for _, row := range board.Top {
		if row.Rank != wantRanks[row.UserID] {
			t.Errorf("%s ranked %d, want %d: equal scores share a rank and the next one skips",
				row.UserID, row.Rank, wantRanks[row.UserID])
		}
		if row.Name != "" {
			t.Errorf("%s came out of Rank with a name already set; ranking must not resolve one",
				row.UserID)
		}
	}
}

// THE ELEVENTH SLOT. Somebody outside the top ten gets their own row with their real rank, so
// they can see where they stand without the board having to reach that far.
func TestABoardCarriesTheViewersOwnRowWhenTheyAreOutsideTheTop(t *testing.T) {
	scores := map[string]int{}
	for i := range 30 {
		// Descending scores, so user 00 is first and user 29 is last.
		scores[fmt.Sprintf("u%02d", i)] = 100 - i
	}

	board := Rank(scores, "u17", 10)

	if len(board.Top) != 10 {
		t.Fatalf("Top has %d rows, want 10", len(board.Top))
	}
	if board.You == nil {
		t.Fatal("the viewer sits at 18th and got no row of their own")
	}
	if board.You.Rank != 18 {
		t.Errorf("viewer rank = %d, want 18", board.You.Rank)
	}
	if board.You.UserID != "u17" {
		t.Errorf("viewer row is %s, want u17", board.You.UserID)
	}
	if board.Unranked {
		t.Error("a viewer with a score was reported as unranked")
	}
}

// Somebody already on the board does not get a duplicate row, because they can see themselves.
func TestAViewerInsideTheTopGetsNoSecondRow(t *testing.T) {
	scores := map[string]int{"a": 10, "b": 9, "c": 8}

	board := Rank(scores, "b", 10)

	if board.You != nil {
		t.Errorf("a viewer already shown at rank %d got a duplicate row", board.You.Rank)
	}
	if board.Unranked {
		t.Error("a viewer on the board was reported as unranked")
	}
}

// A viewer with nothing this week is REPORTED rather than omitted. A missing row is
// indistinguishable from a bug, and "you have none" is a real answer to what was asked.
func TestAViewerWithNoScoreIsReportedAsUnranked(t *testing.T) {
	board := Rank(map[string]int{"a": 3}, "nobody", 10)

	if !board.Unranked {
		t.Error("a viewer with no score was not reported as unranked")
	}
	if board.You != nil {
		t.Error("a viewer with no score got a row")
	}

	// And with no viewer at all, neither is claimed.
	anonymous := Rank(map[string]int{"a": 3}, "", 10)
	if anonymous.Unranked || anonymous.You != nil {
		t.Error("a board with no viewer claimed something about one")
	}
}

// NamesNeeded is the whole performance argument in one method: at most eleven lookups per
// board rather than one per person who spoke this week.
func TestNamesNeededIsBoundedByWhatIsRendered(t *testing.T) {
	scores := map[string]int{}
	for i := range 200 {
		scores[fmt.Sprintf("u%03d", i)] = 200 - i
	}

	board := Rank(scores, "u150", 10)
	needed := board.NamesNeeded()

	if len(needed) != 11 {
		t.Errorf("NamesNeeded returned %d ids for a 200-player board, want 11 (ten rows plus "+
			"the viewer). Resolving more than this is the cost M21a exists to remove", len(needed))
	}

	// And filling them in touches nothing else.
	calls := 0
	named := board.WithNames(func(id string) string {
		calls++
		return "name-" + id
	})
	if calls != 11 {
		t.Errorf("WithNames resolved %d names, want 11", calls)
	}
	if named.Top[0].Name == "" || named.You == nil || named.You.Name == "" {
		t.Error("WithNames left a rendered row without a name")
	}
	// The original is untouched, so a caller cannot be surprised by an aliased slice.
	if board.Top[0].Name != "" {
		t.Error("WithNames mutated the board it was called on")
	}
}

// Ties have to order the same way twice. An unstable order made the ranking shuffle between
// two identical invocations, which reads as the bot being wrong about who is winning.
func TestTiedRowsOrderStably(t *testing.T) {
	scores := map[string]int{"z": 5, "a": 5, "m": 5, "q": 5}

	first := Rank(scores, "", 10)
	for range 20 {
		again := Rank(scores, "", 10)
		for i := range first.Top {
			if first.Top[i].UserID != again.Top[i].UserID {
				t.Fatalf("tied rows shuffled between calls: %v then %v",
					ids(first.Top), ids(again.Top))
			}
		}
	}
}

func ids(rows []Row) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.UserID)
	}
	return out
}

// The promised reset and the reset that happens come from one boundary, so they cannot
// disagree. They were two calculations before, one from the host clock and one from NTP.
func TestNextResetAgreesWithTheResetThatHappens(t *testing.T) {
	now := time.Date(2024, 6, 5, 12, 0, 0, 0, time.UTC) // a Wednesday
	l := NewLeaderboard(now)

	next := l.NextReset(now)
	if next != StartOfWeekUTC(now).AddDate(0, 0, 7) {
		t.Fatalf("NextReset = %v, which is not one week past the boundary MaybeReset compares", next)
	}

	// One second before the promised reset, nothing happens. One second after, it does.
	if l.MaybeReset(next.Add(-time.Second)) {
		t.Error("the board reset before the moment it promised")
	}
	if !l.MaybeReset(next.Add(time.Second)) {
		t.Error("the board did not reset at the moment it promised")
	}
}

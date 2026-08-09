package aggro

import (
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/storage"
)

func snowflake(n int) string {
	return strconv.FormatUint((uint64(n)<<22)|1, 10)
}

type fakeGuard struct {
	mu       sync.Mutex
	reacts   []string
	unreacts []string
}

func (g *fakeGuard) React(_, messageID, _ string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reacts = append(g.reacts, messageID)
	return true
}

func (g *fakeGuard) Unreact(_, messageID, _, userID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.unreacts = append(g.unreacts, messageID+"/"+userID)
	return true
}

func (g *fakeGuard) counts() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.reacts), len(g.unreacts)
}

// fakeActivity is a fixed candidate list.
type fakeActivity []string

func (f fakeActivity) RecentAuthors(time.Duration) []string {
	return append([]string(nil), f...)
}

func opts() Options {
	return Options{Chance: 1, Duration: time.Hour, Tick: time.Minute, Emoji: "\U0001F426", Window: 6 * time.Hour}
}

func fixture(t *testing.T, act Activity, o Options) (*Service, *storage.Store, *fakeGuard) {
	t.Helper()
	store := dbtest.Store(t)
	guard := &fakeGuard{}
	s := New(store, guard, act, o)
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s, store, guard
}

// TestTriggerPicksAndPersistsATarget.
func TestTriggerPicksAndPersistsATarget(t *testing.T) {
	s, store, _ := fixture(t, fakeActivity{snowflake(42)}, opts())
	s.SetBotID(func() string { return snowflake(1) })

	s.maybeTrigger()

	target, until := s.Target()
	if target != snowflake(42) {
		t.Errorf("target = %q, want the only recently active user", target)
	}
	if until.Before(time.Now()) {
		t.Errorf("the aggro ends at %s, which is already past", until)
	}

	// Persisted, so a restart does not forget who it was bothering.
	var state State
	if err := store.View(func(r *storage.Reader) error {
		v, err := r.GetBlob(storage.BlobConfig, "aggroState")
		if err != nil || v == nil {
			t.Fatalf("no aggro state was persisted: %v", err)
		}
		return json.Unmarshal(v, &state)
	}); err != nil {
		t.Fatal(err)
	}
	if state.TargetID != snowflake(42) {
		t.Errorf("persisted target = %q, want the live one", state.TargetID)
	}
}

// TestTriggerNeverPicksTheBot.
//
// It cannot happen today, because the gateway handler drops bot messages before anything is
// recorded, and that is exactly why the filter is worth having: aggro on our own output would be
// the bot reacting to itself in a loop, and the thing preventing it lives in another package.
func TestTriggerNeverPicksTheBot(t *testing.T) {
	me := snowflake(1)
	s, _, _ := fixture(t, fakeActivity{me}, opts())
	s.SetBotID(func() string { return me })

	s.maybeTrigger()
	if target, _ := s.Target(); target != "" {
		t.Errorf("target = %q, want empty: the bot must not be a target", target)
	}
}

// TestTriggerDoesNothingWithNobodyAround. The candidates are people this bot has SEEN, so for the
// first minutes after a restart there are none. That is the right answer for a feature whose
// point is poking somebody who is present, and it replaced a six-hour history walk that could
// pick someone long gone.
func TestTriggerDoesNothingWithNobodyAround(t *testing.T) {
	s, _, _ := fixture(t, fakeActivity{}, opts())
	s.SetBotID(func() string { return snowflake(1) })

	s.maybeTrigger()
	if target, _ := s.Target(); target != "" {
		t.Errorf("target = %q with nobody active, want empty", target)
	}
}

// TestTriggerRespectsTheChance, so an operator setting it to zero gets no aggro at all.
func TestTriggerRespectsTheChance(t *testing.T) {
	o := opts()
	o.Chance = 0
	s, _, _ := fixture(t, fakeActivity{snowflake(42)}, o)
	s.SetBotID(func() string { return snowflake(1) })

	for range 50 {
		s.maybeTrigger()
	}
	if target, _ := s.Target(); target != "" {
		t.Errorf("target = %q with a chance of 0", target)
	}
}

// TestTriggerDoesNotReplaceALiveAggro, so a target gets the duration they were given rather than
// being swapped out on the next tick.
func TestTriggerDoesNotReplaceALiveAggro(t *testing.T) {
	s, _, _ := fixture(t, fakeActivity{snowflake(42), snowflake(43)}, opts())
	s.SetBotID(func() string { return snowflake(1) })

	s.maybeTrigger()
	first, _ := s.Target()
	for range 20 {
		s.maybeTrigger()
	}
	got, _ := s.Target()
	if got != first {
		t.Errorf("target changed from %q to %q while the first aggro was still live", first, got)
	}
}

// TestHandleReactsToTheTargetAndNobodyElse.
func TestHandleReactsToTheTargetAndNobodyElse(t *testing.T) {
	s, _, guard := fixture(t, fakeActivity{snowflake(42)}, opts())
	s.SetBotID(func() string { return snowflake(1) })
	s.maybeTrigger()

	s.Handle("c1", snowflake(500), snowflake(42))
	if reacts, _ := guard.counts(); reacts != 1 {
		t.Errorf("reacted %d times to the target's message, want 1", reacts)
	}

	s.Handle("c1", snowflake(501), snowflake(99))
	if reacts, _ := guard.counts(); reacts != 1 {
		t.Errorf("reacted %d times after a non-target's message, want still 1", reacts)
	}
}

// TestAnExpiredAggroIsReleasedAndTakesItsReactionBack.
//
// Unreact is deliberately not pause-gated in the guard: withdrawing a reaction is the one thing
// an operator would want to still work during an incident, because refusing it would leave the
// bot's mark on somebody's message with no way to remove it.
func TestAnExpiredAggroIsReleasedAndTakesItsReactionBack(t *testing.T) {
	o := opts()
	o.Duration = time.Millisecond
	s, store, guard := fixture(t, fakeActivity{snowflake(42)}, o)
	s.SetBotID(func() string { return snowflake(1) })
	s.maybeTrigger()

	time.Sleep(25 * time.Millisecond)
	s.Handle("c1", snowflake(502), snowflake(42))

	reacts, unreacts := guard.counts()
	if reacts != 0 {
		t.Errorf("reacted %d times to an expired target", reacts)
	}
	if unreacts != 1 {
		t.Errorf("unreacted %d times, want 1: the bot's mark must come off", unreacts)
	}
	if target, _ := s.Target(); target != "" {
		t.Errorf("target = %q after expiry, want cleared", target)
	}

	// And the cleared state is persisted, so a restart does not resurrect it.
	var state State
	if err := store.View(func(r *storage.Reader) error {
		v, err := r.GetBlob(storage.BlobConfig, "aggroState")
		if err != nil || v == nil {
			return err
		}
		return json.Unmarshal(v, &state)
	}); err != nil {
		t.Fatal(err)
	}
	if state.TargetID != "" {
		t.Errorf("persisted target = %q after expiry, want cleared", state.TargetID)
	}
}

// TestALiveAggroSurvivesARestart, because the state is persisted and Init reads it.
func TestALiveAggroSurvivesARestart(t *testing.T) {
	store := dbtest.Store(t)
	state := State{TargetID: snowflake(42), EndTime: time.Now().Add(time.Hour)}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(w *storage.Writer) error {
		return w.PutBlob(storage.BlobConfig, "aggroState", encoded)
	}); err != nil {
		t.Fatal(err)
	}

	s := New(store, &fakeGuard{}, fakeActivity{}, opts())
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if target, _ := s.Target(); target != snowflake(42) {
		t.Errorf("target after a restart = %q, want the persisted one", target)
	}
}

// TestAnExpiredAggroIsClearedAtStartup rather than left to the first message from a target the
// bot is no longer bothering.
func TestAnExpiredAggroIsClearedAtStartup(t *testing.T) {
	store := dbtest.Store(t)
	state := State{TargetID: snowflake(42), EndTime: time.Now().Add(-time.Hour)}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(w *storage.Writer) error {
		return w.PutBlob(storage.BlobConfig, "aggroState", encoded)
	}); err != nil {
		t.Fatal(err)
	}

	s := New(store, &fakeGuard{}, fakeActivity{}, opts())
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if target, _ := s.Target(); target != "" {
		t.Errorf("target = %q, want an expired aggro cleared at startup", target)
	}

	var reread State
	if err := store.View(func(r *storage.Reader) error {
		v, err := r.GetBlob(storage.BlobConfig, "aggroState")
		if err != nil || v == nil {
			return err
		}
		return json.Unmarshal(v, &reread)
	}); err != nil {
		t.Fatal(err)
	}
	if reread.TargetID != "" {
		t.Error("the expired state was cleared in memory but not on disk, so the next restart " +
			"would load it again")
	}
}

// TestANilBotIDDoesNotPanic. The reactor learns the bot's ID from READY and hands it over as a
// closure, so between construction and READY there is a window where it is not set.
func TestANilBotIDDoesNotPanic(t *testing.T) {
	s, _, guard := fixture(t, fakeActivity{snowflake(42)}, opts())
	// SetBotID deliberately not called.

	s.maybeTrigger()
	if target, _ := s.Target(); target != snowflake(42) {
		t.Errorf("target = %q, want the active user even with no bot ID yet", target)
	}
	s.Handle("c1", snowflake(503), snowflake(42))
	if reacts, _ := guard.counts(); reacts != 1 {
		t.Errorf("reacted %d times, want 1", reacts)
	}
}

// TestConcurrentHandleAndTrigger exists for CI's race detector. Handle runs on dispatcher
// workers and maybeTrigger on the aggro loop, and the state they share is what the mutex guards.
// It is also the shape that used to hold that mutex across hundreds of REST calls.
func TestConcurrentHandleAndTrigger(t *testing.T) {
	s, _, _ := fixture(t, fakeActivity{snowflake(42), snowflake(43), snowflake(44)}, opts())
	s.SetBotID(func() string { return snowflake(1) })

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := range 100 {
				s.Handle("c1", snowflake(600+i), snowflake(42+i%3))
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 100 {
				s.maybeTrigger()
				_, _ = s.Target()
			}
		})
	}
	wg.Wait()
}

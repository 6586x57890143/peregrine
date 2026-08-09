// Package aggro is the bird-aggro feature: the bot picks somebody and reacts to
// everything they say for a while.
//
// It is one of the engagement behaviours SPEC.md section 6 calls the product rather than
// cruft. What it needed was not fixing but owning: the state was two package-level
// variables behind a package-level mutex, and the target was chosen by paging hundreds of
// messages out of Discord's REST API.
package aggro

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// Guard is the send chokepoint. Declared as an interface so this package cannot reach a
// raw session: every reaction has to go through mention suppression and the pause switch.
type Guard interface {
	React(channelID, messageID, emoji string) bool
	Unreact(channelID, messageID, emoji, userID string) bool
}

// Activity answers who has spoken recently. internal/activity's Tracker satisfies it.
type Activity interface {
	RecentAuthors(window time.Duration) []string
}

// Options are the dials, all of them from the environment.
type Options struct {
	// Chance is the per-tick probability of starting an aggro when none is running.
	Chance float64

	// Duration is how long an aggro lasts.
	Duration time.Duration

	// Tick is how often a new one might start.
	Tick time.Duration

	// Emoji is the reaction.
	Emoji string

	// Window is how far back a user counts as around. It bounds the candidate list, and
	// the process's own uptime bounds it further: the candidates are people this bot has
	// seen, so for the first minutes after a restart there are none. That is the right
	// answer for a feature whose point is poking somebody who is present, and it replaced
	// a six-hour history walk that could pick someone long gone.
	Window time.Duration
}

// State is the persisted form. It is written through the opaque blob API rather than a
// bucket of its own, because the shape of this value belongs to the feature and not to
// storage: making storage hold a type definition for every scrap of state a feature
// persists is how it ends up importing the features it is meant to serve.
type State struct {
	TargetID string    `json:"target_id"`
	EndTime  time.Time `json:"end_time"`
}

// Service is the feature. It implements core.Service.
type Service struct {
	store    *storage.Store
	guard    Guard
	activity Activity
	opts     Options
	botID    func() string

	mu       sync.Mutex
	targetID string
	endTime  time.Time

	loops       sync.WaitGroup
	cancelLoops context.CancelFunc
	logger      *slog.Logger
}

// New builds the service.
func New(store *storage.Store, guard Guard, act Activity, opts Options) *Service {
	return &Service{store: store, guard: guard, activity: act, opts: opts}
}

// SetBotID supplies a function returning the bot's own user ID.
//
// A function rather than a value, because the ID is only knowable after READY and this is
// constructed before the session opens. It is needed for two things: taking a reaction back
// under the bot's own identity, and never picking the bot as a target.
func (s *Service) SetBotID(f func() string) { s.botID = f }

func (s *Service) Name() string { return "aggro" }

// Init restores the persisted target.
//
// A failure to load is a warning rather than an error: aggro is one optional behaviour, and
// the rule in this bot is that exactly one feature failing disables that one and never the
// process.
func (s *Service) Init(deps core.Deps) error {
	s.logger = deps.Logger

	var state State
	if err := s.store.View(func(r *storage.Reader) error {
		v, err := r.GetBlob(storage.BlobConfig, "aggroState")
		if err != nil || v == nil {
			return err
		}
		return json.Unmarshal(v, &state)
	}); err != nil {
		log.Printf("[WARN] Failed to load aggro state: %v", err)
		return nil
	}

	switch {
	case state.TargetID == "":
	case time.Now().Before(state.EndTime):
		s.mu.Lock()
		s.targetID, s.endTime = state.TargetID, state.EndTime
		s.mu.Unlock()
		log.Printf("[AGGRO] Loaded active aggro on %s until %s",
			state.TargetID, state.EndTime.Format(time.RFC3339))
	default:
		// Expired while the bot was down. Cleared here, synchronously, rather than in a
		// goroutine: Init is the one moment where a write is guaranteed not to race the
		// message handler, because the gateway is not connected yet.
		log.Printf("[AGGRO] Loaded expired aggro on %s, clearing.", state.TargetID)
		if err := s.persist(State{}); err != nil {
			log.Printf("[WARN] Failed to clear expired aggro state: %v", err)
		}
	}
	return nil
}

// Start launches the trigger loop.
func (s *Service) Start(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancelLoops = cancel

	core.RunLoop(loopCtx, &s.loops, s.logger, core.Loop{
		Name:  "aggro",
		Every: s.opts.Tick,
		Fn:    func(context.Context) { s.maybeTrigger() },
	})
	return nil
}

// Shutdown stops the loop and waits for it.
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
	}
	return nil
}

// Reaction is what the reactor should do about one message from one author.
type Reaction int

const (
	// Nothing: this author is not the target.
	Nothing Reaction = iota

	// Poke: react to this message.
	Poke

	// Release: the aggro has expired, so take the reaction back.
	Release
)

// Consider reports what to do about a message, and applies the state change.
//
// The lock is released before the caller does any I/O, which is the discipline the old
// version got right and worth keeping: the aggro mutex must never be held across a Discord
// call.
func (s *Service) Consider(authorID string) Reaction {
	s.mu.Lock()
	isTarget := authorID != "" && authorID == s.targetID
	expired := time.Now().After(s.endTime)
	if isTarget && expired {
		s.targetID, s.endTime = "", time.Time{}
	}
	s.mu.Unlock()

	switch {
	case isTarget && !expired:
		return Poke
	case isTarget && expired:
		if err := s.persist(State{}); err != nil {
			log.Printf("[AGGRO] failed to clear persisted aggro state: %v", err)
		}
		return Release
	}
	return Nothing
}

// Handle applies Consider's verdict through the guard.
//
// Through the guard so that PEREGRINE_PAUSE_ALL_WRITES stops the bot reacting as well as
// talking: a reaction is still the bot visibly participating, and an operator hitting the
// emergency stop is not asking it to keep poking someone. Unreact is deliberately not
// pause-gated in the guard, because withdrawing a reaction during an incident is the one
// thing an operator would want to still work.
func (s *Service) Handle(channelID, messageID, authorID string) {
	switch s.Consider(authorID) {
	case Poke:
		s.guard.React(channelID, messageID, s.opts.Emoji)
	case Release:
		log.Printf("[AGGRO] Aggro expired for %s, clearing.", authorID)
		me := ""
		if s.botID != nil {
			me = s.botID()
		}
		s.guard.Unreact(channelID, messageID, s.opts.Emoji, me)
	case Nothing:
	}
}

// Target reports the current target and when it ends, for the status line.
func (s *Service) Target() (string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.targetID, s.endTime
}

// maybeTrigger starts an aggro if none is running and the dice agree.
func (s *Service) maybeTrigger() {
	s.mu.Lock()
	busy := s.targetID != "" && !time.Now().After(s.endTime)
	s.mu.Unlock()
	if busy {
		return
	}
	if rand.Float64() >= s.opts.Chance {
		return
	}

	// Chosen outside the lock. Picking a target takes the activity tracker's mutex, and
	// holding one package's lock while acquiring another's is how a lock-ordering deadlock
	// gets built. The version this replaced did it while also making hundreds of REST
	// calls, so the aggro state was locked for the length of a Discord page walk.
	target := s.pick()
	if target == "" {
		return
	}

	s.mu.Lock()
	// Re-checked, because the state could have changed while the lock was released. There
	// is only one aggro loop so this cannot happen today, and that is exactly why it is
	// worth two lines: the next caller will not know.
	if s.targetID != "" && !time.Now().After(s.endTime) {
		s.mu.Unlock()
		return
	}
	s.targetID = target
	s.endTime = time.Now().Add(s.opts.Duration)
	state := State{TargetID: s.targetID, EndTime: s.endTime}
	s.mu.Unlock()

	log.Printf("[AGGRO] Bird aggro triggered on user %s for %v.", target, s.opts.Duration)
	if err := s.persist(state); err != nil {
		log.Printf("[ERR] Failed to persist aggro state: %v", err)
	}
}

// pick chooses a random recently active user who is not the bot.
func (s *Service) pick() string {
	candidates := s.activity.RecentAuthors(s.opts.Window)

	// Never the bot itself. It cannot reach here, because the gateway handler drops bot
	// messages before anything is recorded, and that is exactly why this is worth three
	// lines: aggro on our own output would be the bot reacting to itself in a loop, and
	// the thing preventing it lives in a different package.
	me := ""
	if s.botID != nil {
		me = s.botID()
	}
	filtered := candidates[:0]
	for _, id := range candidates {
		if id != me {
			filtered = append(filtered, id)
		}
	}
	if len(filtered) == 0 {
		log.Println("[AGGRO] No recently active users to pick a target from.")
		return ""
	}
	return filtered[rand.IntN(len(filtered))]
}

func (s *Service) persist(state State) error {
	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode aggro state: %w", err)
	}
	return s.store.Update(func(w *storage.Writer) error {
		return w.PutBlob(storage.BlobConfig, "aggroState", encoded)
	})
}

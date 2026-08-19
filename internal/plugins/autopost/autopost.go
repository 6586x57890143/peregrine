// Package autopost speaks unprompted: on a timer, the bot picks the liveliest channel it is
// allowed to speak in and says something.
//
// # This feature has been dead three separate times
//
// It is worth recording, because each one was a different kind of wiring failure and the
// third would have survived an operator carefully setting every variable the docs named.
//
//  1. The enable flag was a compile-time constant, set false.
//  2. The channel allowlist was empty, and an empty allowlist means no channels, so
//     flipping the constant alone produced nothing and no explanation. Both variables are
//     named in one startup error now.
//  3. The loop was gated on the WORD-GAME flag and paced by the word-game interval, with two
//     comments claiming it posted word games. It posts a Markov sentence and always has, so
//     PEREGRINE_ENABLE_AUTONOMOUS_POST=true produced nothing unless word games happened to be
//     on as well (SPEC.md section 8, finding 30).
//
// # The default is still false, deliberately
//
// There is no safe default for "where may the bot speak unprompted": that is the operator's
// sentence to write, and config refuses the feature with an empty allowlist rather than
// inventing a channel or reading empty as "anywhere". What the milestones owed was that it
// works when the operator turns it on.
package autopost

import (
	"context"
	"log"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/channels"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/generate"
	"github.com/6586x57890143/peregrine/internal/plugins/tuning"
)

// Guard is the send chokepoint.
//
// Going through it is a change in behaviour and not only in plumbing: until M10a this path
// reached Discord without passing CheckEmit at all, because the gate sat at the generation
// exit and this poster called a different send. An unprompted post is the output with the
// least human context around it, so it was the worst one to have been ungated.
type Guard interface {
	Send(channelID, content string) (*discordgo.Message, bool)
}

// Speaker produces a sentence, and says why when it does not. internal/generate's Generator
// satisfies it.
type Speaker interface {
	Sentence(req generate.Request) (string, generate.Outcome, error)
}

// Recorder is the tuning export. internal/plugins/tuning satisfies it, and a nil one is
// replaced by a no-op in New.
//
// An unprompted post is a DIFFERENT question from a reply and the export keeps them apart
// for that reason: one is answering somebody and one is speaking into a room, so a single
// average over both would average two distributions that should never have met. This path
// is also the one with the least human context around it, which makes it the most likely to
// read as a non-sequitur and therefore the most worth measuring.
type Recorder interface {
	Record(tuning.Generation)
}

// noRecorder is what a nil Recorder means. A null object rather than a nil check, matching
// the reactor and wordgame.noCounter.
type noRecorder struct{}

func (noRecorder) Record(tuning.Generation) {}

// Options are the dials.
type Options struct {
	Enabled bool

	// Tick is how often a post might happen, and SkipChance is the probability of passing on
	// any given tick, for pacing that does not look mechanical.
	//
	// Two dials rather than one because they answer different questions: the tick bounds how
	// often the bot COULD speak, and the skip makes the rhythm irregular. A single longer
	// tick would give the same rate with a metronome's regularity.
	Tick       time.Duration
	SkipChance float64

	// Channels is the allowlist. Never empty when Enabled is true; config refuses that.
	Channels []string

	// ActiveChannelWindow is how recent traffic must be for a channel to count.
	ActiveChannelWindow time.Duration
}

// Service is the feature.
type Service struct {
	guard    Guard
	speaker  Speaker
	memories *generate.Memories
	counter  channels.Counter
	resolver channels.Resolver
	emoji    generate.EmojiResolver
	recorder Recorder
	opts     Options

	loops       sync.WaitGroup
	cancelLoops context.CancelFunc
	logger      *slog.Logger
}

// New builds the service.
func New(guard Guard, speaker Speaker, memories *generate.Memories,
	counter channels.Counter, resolver channels.Resolver, emoji generate.EmojiResolver,
	recorder Recorder, opts Options) *Service {
	if recorder == nil {
		recorder = noRecorder{}
	}
	return &Service{
		guard: guard, speaker: speaker, memories: memories,
		counter: counter, resolver: resolver, emoji: emoji, recorder: recorder, opts: opts,
	}
}

func (s *Service) Name() string { return "autopost" }

// Init does nothing: this feature holds no persistent state.
func (s *Service) Init(deps core.Deps) error {
	s.logger = deps.Logger
	return nil
}

// Start launches the poster, if it is enabled.
func (s *Service) Start(ctx context.Context) error {
	if !s.opts.Enabled {
		return nil
	}
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancelLoops = cancel

	core.RunLoop(loopCtx, &s.loops, s.logger, core.Loop{
		Name:  "autonomous-post",
		Every: s.opts.Tick,
		Fn:    func(context.Context) { s.post() },
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

// post picks a channel and says something there.
func (s *Service) post() {
	start := time.Now()
	log.Println("[AUTONOMOUS] Starting autonomous post cycle...")

	channelID := channels.Busiest(s.counter, s.resolver, s.opts.ActiveChannelWindow, s.opts.Channels, "")
	if channelID == "" {
		log.Println("[AUTONOMOUS] No active channel found, skipping post.")
		return
	}

	// WHICH SERVER'S WORDS, M31. This loop picks a channel and then speaks from a corpus, and
	// before per-guild corpora that corpus was every server at once: the clearest cross-guild
	// quote path in the bot, because nothing here was ever addressed to anybody. The guild
	// comes off the state cache with the rest of the channel's details.
	info, ok := s.resolver.Channel(channelID)
	if !ok || info.GuildID == "" {
		log.Printf("[AUTONOMOUS] Channel %s is not in the state cache, so there is no guild to "+
			"speak from. Skipping.", channelID)
		return
	}

	if rand.Float64() < s.opts.SkipChance {
		log.Printf("[AUTONOMOUS] Skipping this cycle for natural pacing (chance %.2f).", s.opts.SkipChance)
		return
	}

	// Seeded with the CHANNEL'S OWN recent context. The old code passed the channel's
	// LastMessageID: a snowflake, tokenized into a meaningless integer and fed to the
	// generator as if somebody had said it.
	// The prompt is a fixed literal, so for THIS caller the channel's conversation memory is
	// the only context there is. That is what makes the graded recency in M19 matter most
	// here: an unprompted post steered by nothing is a non-sequitur by construction.
	var trace generate.Trace
	msg, outcome, err := s.speaker.Sentence(generate.Request{
		GuildID: info.GuildID,
		Prompt:  "autonomous thought",
		Memory:  s.memories.For(channelID),
		Emoji:   s.emoji,
		Trace:   &trace,
	})

	// One helper for all four exits, so a path cannot be added that records nothing. The
	// silent ones are the point rather than an afterthought: an unprompted post that
	// produced nothing is the clearest signal there is that the corpus or the
	// author-diversity threshold is the problem, and until now it left only a log line.
	record := func(sentID, text string, sent bool) {
		s.recorder.Record(tuning.Generation{
			ID:      sentID,
			Trigger: "autopost",
			Channel: channelID,
			Prompt:  "autonomous thought",
			Reply:   text,
			Outcome: outcome.String(),
			Sent:    sent,
			Took:    time.Since(start),
			Trace:   &trace,
		})
	}

	if err != nil {
		log.Println("[AUTONOMOUS] Error generating message:", err)
		record("", "", false)
		return
	}
	if msg == "" {
		// This path already said something; the reason is what it was missing.
		log.Printf("[AUTONOMOUS] Nothing to post, skipping: %s", outcome)
		record("", "", false)
		return
	}

	// A refused send records no text, matching the reply path and internal/safety's rule
	// that the offending content is never written down.
	if sent, ok := s.guard.Send(channelID, msg); ok {
		log.Printf("[AUTONOMOUS] Sent message in %s: %s (ID: %s)", channelID, msg, sent.ID)
		record(sent.ID, msg, true)
	} else {
		record("", "", false)
	}
	log.Printf("[AUTONOMOUS] Cycle finished in %s.", time.Since(start))
}

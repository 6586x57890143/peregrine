// Package ingest is the service that walks Discord's history on a timer.
//
// internal/ingest is the walk itself: it decides what to ask Discord for, tracks per-channel
// cursors and pages with afterID. This package is the three adapters that connect it to the rest
// of the bot, plus the loop that runs it.
//
// # The split is where the safety boundary is
//
// internal/ingest asks "what is new" and knows nothing about the corpus. This package hands what
// it finds to learn.Learner.Message, which is the same function the live message handler calls,
// and that is the whole point of A1's fix: CheckLearn is inside that function, so a backfilled
// message cannot bypass a filter the live path applies. This was the exact path that used to,
// and it was the worst finding in the review.
package ingest

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/ingest"
	"github.com/6586x57890143/peregrine/internal/learn"
	"github.com/6586x57890143/peregrine/internal/names"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// Options are the dials, all from the environment.
type Options struct {
	// Tick is how often a pass runs.
	Tick time.Duration

	// Lookback bounds the FIRST pass over a channel and never applies again, because later
	// passes resume from a stored cursor. It used to be a re-read window, which at the shipped
	// defaults meant roughly 144 passes over every message: the history bucket it relied on for
	// dedup is capped, so on a busy guild the older half of each window had already been evicted
	// and was learned again, counting its n-grams twice (finding 13). Changing what this means
	// reintroduces that.
	Lookback time.Duration

	// GuildConcurrency and ChannelConcurrency bound both fan-out levels. Unbounded, the walk was
	// one goroutine per channel per guild, which on a bot in several large guilds is hundreds of
	// concurrent REST calls that Discord answers with rate limits whose retries make it worse.
	GuildConcurrency   int
	ChannelConcurrency int

	// BatchDelay paces the pages.
	BatchDelay time.Duration
}

// Service is the feature.
type Service struct {
	session *discordgo.Session
	store   *storage.Store
	learner *learn.Learner
	opts    Options
	logger  *slog.Logger

	// members is the session the NAME resolver uses, wrapped so a guild member is fetched
	// once rather than once per mention per message.
	//
	// This is not the same object as session on purpose: the walk itself wants the real
	// session, and only names.Resolve makes the repeated call. discordgo's GuildMember is an
	// unconditional REST GET with no state-cache check, so the first bootstrap over a large
	// lookback pays one request per mention across the whole window. That was invisible while
	// the only long walk lived in another package with its own private cache.
	members names.Session

	loops       sync.WaitGroup
	cancelLoops context.CancelFunc
}

// New builds the service. It takes the Learner rather than building one, because the corpus
// writer is one per process and the reactor holds the same instance: two Learners would mean two
// bot IDs and two mention patterns, and only one of them would ever be told who the bot is.
func New(session *discordgo.Session, store *storage.Store, learner *learn.Learner, opts Options) *Service {
	return &Service{
		session: session,
		store:   store,
		learner: learner,
		opts:    opts,
		// A SHORTER TTL than a repair pass uses, because this is the live path: a nickname
		// change should show up in what the bot calls somebody within the hour, whereas a
		// walk over old messages is applying today's nicknames to old text regardless.
		members: names.NewCachedSession(session, memberCacheTTL, memberCacheSize),
	}
}

// The bounds on the member cache. Constants rather than configuration for the same reason the
// health dials are: no incident changes either, and a cache an operator can misconfigure into
// staleness is worse than one they cannot touch.
const (
	memberCacheTTL  = time.Hour
	memberCacheSize = 4096
)

func (s *Service) Name() string { return "ingest" }

// Init records the logger. Nothing to load: the cursors live in the corpus and are read per pass.
func (s *Service) Init(deps core.Deps) error {
	s.logger = deps.Logger
	return nil
}

// Start launches the pass loop.
func (s *Service) Start(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancelLoops = cancel

	core.RunLoop(loopCtx, &s.loops, s.logger, core.Loop{
		Name:  "ingest",
		Every: s.opts.Tick,
		// Immediate, because the original code ran one backfill before entering its loop and a
		// bot that has just started is the one most likely to have missed something.
		Immediate: true,
		Fn:        func(ctx context.Context) { s.Once(ctx) },
	})
	return nil
}

// Shutdown stops the loop and waits for a pass in flight.
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
		s.logger.Warn("shutdown deadline reached with an ingest pass still running")
	}
	return nil
}

// Once performs one pass.
func (s *Service) Once(ctx context.Context) {
	s.logger.Info("starting an ingest pass")
	start := time.Now()

	in := ingest.New(
		s.session,
		cursors{store: s.store},
		learner{session: s.members, store: s.store, learner: s.learner},
		s.logger,
		ingest.Options{
			Lookback:           s.opts.Lookback,
			GuildConcurrency:   s.opts.GuildConcurrency,
			ChannelConcurrency: s.opts.ChannelConcurrency,
			BatchDelay:         s.opts.BatchDelay,
		})

	stats, err := in.Run(ctx)
	if err != nil {
		// Failing to LIST guilds is the one fatal case, and internal/ingest returns it as an
		// error for that reason: there is nothing to walk, and a cheerful log line would hide a
		// revoked token. An unreadable individual guild is swallowed inside the walk, because
		// errgroup cancels its context on the first error returned and one guild the bot cannot
		// read must not abandon the ones it can.
		s.logger.Error("ingest pass failed", "err", err)
		return
	}
	s.logger.Info("ingest pass finished",
		"guilds", stats.Guilds, "channels", stats.Channels,
		"learned", stats.Learned, "skipped", stats.Skipped, "errors", stats.Errors,
		"took", time.Since(start))
}

// learner adapts the corpus writer to ingest.Learner.
type learner struct {
	session names.Session
	store   *storage.Store
	learner *learn.Learner
}

func (l learner) Learn(m *discordgo.Message, guildID string) error {
	mentioned := names.OfMessage(l.session, l.store, &discordgo.MessageCreate{Message: m}, guildID)

	// One answer to "what is this person called", shared with the live handler. This was
	// hand-built here and hand-built again in chat, and neither copy knew about GlobalName.
	author := names.Primary(m.Author, m.Member)

	return l.store.Update(func(w *storage.Writer) error {
		return l.learner.Message(w, m.Content, m.ID, author, mentioned)
	})
}

// cursors adapts the corpus to ingest.Cursors.
//
// One transaction per call rather than one per pass, and that is the right trade: the alternative
// is holding a write transaction open across every REST round trip of a pass, and bbolt has a
// single writer process-wide, so it would block all live learning for the length of the walk. A
// read to fetch a cursor and a write to advance it are both a handful of bytes.
type cursors struct{ store *storage.Store }

func (c cursors) Cursor(channelID string) (string, error) {
	var id string
	err := c.store.View(func(r *storage.Reader) error {
		id = r.Cursor(channelID)
		return nil
	})
	return id, err
}

func (c cursors) SetCursor(channelID, messageID string) error {
	return c.store.Update(func(w *storage.Writer) error {
		return w.SetCursor(channelID, messageID)
	})
}

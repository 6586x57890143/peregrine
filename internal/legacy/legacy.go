// Package legacy is what is left of peregrine's original single-file implementation.
//
// It is a holding pen, not a design. It existed because two main packages cannot share code:
// turning cmd/bot into the entrypoint required the 3,200 lines it was calling to live
// somewhere importable, and moving them verbatim was the only way to keep `go build ./...`
// green at every commit while ending at merlin's layout (SPEC.md section 9).
//
// As of M11c it is down to one subsystem plus two log lines: the ingestion pass, the corpus
// status line, and the Discord latency probe. Everything else has been lifted into a package
// that owns it. M13 takes these three as well and deletes this package.
//
// Nothing new goes in here.
package legacy

import (
	"context"
	"log"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/config"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/ingest"
	"github.com/6586x57890143/peregrine/internal/learn"
	"github.com/6586x57890143/peregrine/internal/names"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// Service is the remaining legacy behaviour, as one core.Service.
type Service struct {
	cfg     *config.Config
	store   *storage.Store
	session *discordgo.Session
	learner *learn.Learner
	logger  *slog.Logger

	// loops tracks the background tickers so Shutdown can wait for them. Only RunLoop ever
	// Adds to it, and only during Start, which is the invariant that makes the finding-4
	// panic impossible here as well as in the Dispatcher.
	loops       sync.WaitGroup
	cancelLoops context.CancelFunc
}

// New returns the legacy service. It does no work: everything that can fail happens in Init,
// where a failure aborts startup with an explanation.
//
// It takes the Learner rather than building one, because the corpus writer is one per process
// and the reactor holds the same instance: two Learners would mean two mention patterns and
// two bot IDs, and only one of them would ever be told who the bot is.
func New(learner *learn.Learner) *Service { return &Service{learner: learner} }

func (s *Service) Name() string { return "legacy" }

// Init captures the dependencies. No gateway or REST calls belong here: the session is not
// connected yet.
func (s *Service) Init(deps core.Deps) error {
	s.cfg = deps.Config
	s.store = deps.Store
	s.session = deps.Session
	s.logger = deps.Logger
	return nil
}

// Start launches the two remaining loops.
func (s *Service) Start(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancelLoops = cancel

	loops := []core.Loop{
		{
			Name:      "status",
			Every:     s.cfg.StatusTick,
			Immediate: true, // wanted at startup: it is the first sign of life
			Fn:        func(context.Context) { s.printStatus() },
		},
		{
			Name:      "ingest",
			Every:     s.cfg.IngestTick,
			Immediate: true, // the original code ran one backfill before the loop
			Fn: func(ctx context.Context) {
				log.Println("[AUTO] Starting autonomous ingestion...")
				s.runIngest(ctx)
				log.Println("[AUTO] Autonomous ingestion finished.")
			},
		},
	}
	for _, loop := range loops {
		core.RunLoop(loopCtx, &s.loops, s.logger, loop)
	}

	// Not a RunLoop: it jitters its first probe and reports on its own cadence. M13 turns it
	// into a real health service alongside the status line.
	s.loops.Add(1)
	go func() {
		defer s.loops.Done()
		s.monitorPerformance(loopCtx)
	}()

	log.Println("[INFO] Bot running")
	return nil
}

// Shutdown stops the loops and waits for them.
//
// The wait is what the old code got wrong. It closed a stop channel and called wg.Wait on a
// WaitGroup the message handler was also Adding to, so shutdown could panic, and when it did
// not, wg.Wait returning still did not mean handlers were done: they kept using the corpus
// and the session while shutdown moved on to closing both. Here the only Adds happened during
// Start, the wait is bounded by ctx, and cmd/bot closes the store strictly after this returns.
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
		log.Println("[INFO] Background loops finished.")
	case <-ctx.Done():
		// Reported rather than waited out. The container has a fixed budget before SIGKILL,
		// and losing an orderly close of the corpus to a stuck loop is the worse trade.
		log.Println("[WARN] Shutdown deadline reached with background loops still running.")
	}
	return nil
}

// ingestLearner adapts the learn path to ingest.Learner.
//
// The historical path and the live path therefore go through the same function, which is the
// whole point of A1's fix: CheckLearn is inside Learner.Message, so a backfilled message
// cannot bypass a filter the live handler applies. This was the exact path that used to, and
// it was the worst finding in the review.
type ingestLearner struct {
	session *discordgo.Session
	store   *storage.Store
	learner *learn.Learner
}

func (l ingestLearner) Learn(m *discordgo.Message, guildID string) error {
	mentioned := names.OfMessage(l.session, l.store, &discordgo.MessageCreate{Message: m}, guildID)

	author := names.User{
		Name:     m.Author.Username,
		UserID:   m.Author.ID,
		Username: m.Author.Username,
	}
	if m.Member != nil && m.Member.Nick != "" {
		author.Name = m.Member.Nick
	}

	return l.store.Update(func(w *storage.Writer) error {
		return l.learner.Message(w, m.Content, m.ID, author, mentioned)
	})
}

// storeCursors adapts the corpus to ingest.Cursors.
//
// One transaction per call rather than one per pass, and that is the right trade here: the
// alternative is holding a write transaction open across every REST round trip of an ingest
// pass, and bbolt has a single writer process-wide, so it would block all live learning for
// the length of the walk. A read to fetch a cursor and a write to advance it are both a
// handful of bytes.
type storeCursors struct{ store *storage.Store }

func (c storeCursors) Cursor(channelID string) (string, error) {
	var id string
	err := c.store.View(func(r *storage.Reader) error {
		id = r.Cursor(channelID)
		return nil
	})
	return id, err
}

func (c storeCursors) SetCursor(channelID, messageID string) error {
	return c.store.Update(func(w *storage.Writer) error {
		return w.SetCursor(channelID, messageID)
	})
}

// runIngest performs one pass.
func (s *Service) runIngest(ctx context.Context) {
	in := ingest.New(
		s.session,
		storeCursors{store: s.store},
		ingestLearner{session: s.session, store: s.store, learner: s.learner},
		s.logger,
		ingest.Options{
			Lookback:           s.cfg.IngestLookback,
			GuildConcurrency:   s.cfg.IngestGuildConcurrency,
			ChannelConcurrency: s.cfg.IngestChannelConcurrency,
			BatchDelay:         s.cfg.IngestBatchDelay,
		})
	if _, err := in.Run(ctx); err != nil {
		log.Printf("[ERR] ingest pass failed to start: %v", err)
	}
}

// printStatus logs the size of the corpus.
//
// On a ticker rather than on the message path, which is where it used to be: the size field
// was filled by a Bucket.Stats() call once per message, and Stats() walks every page in the
// bucket (SPEC.md section 8, finding 11).
func (s *Service) printStatus() {
	start := time.Now()
	if err := s.store.View(func(r *storage.Reader) error {
		st := r.Status()
		log.Printf(
			"Library status: ngrams=%d | author-entries=%d | topics=%d | topic-word=%d | "+
				"name-topic=%d | names=%d | history=%d | images=%d | total-learned=%d | checked in %s",
			st.Ngrams, st.AuthorEntries, st.Topics, st.TopicWords, st.NameTopics,
			st.Names, st.HistoryWindow, st.ImageCache, st.Learned, time.Since(start),
		)
		return nil
	}); err != nil {
		log.Printf("[ERR] checking library status: %v", err)
	}
}

// monitorPerformance periodically probes Discord and logs notable latency.
func (s *Service) monitorPerformance(ctx context.Context) {
	// A small startup jitter so multiple instances do not align. Interruptible, unlike the
	// bare time.Sleep this replaced: a sleeping goroutine cannot notice shutdown, so the
	// process waited out the whole delay before it could stop.
	select {
	case <-time.After(time.Duration(rand.IntN(1000)) * time.Millisecond):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	// Only log above this, or on error, to keep the line rare enough to mean something.
	const threshold = 500 * time.Millisecond

	for {
		select {
		case <-ticker.C:
			start := time.Now()
			if _, err := s.session.User("@me"); err != nil {
				log.Printf("[HEALTH] Discord API ping failed: %v", err)
				continue
			}
			if latency := time.Since(start); latency > threshold {
				log.Printf("[HEALTH] Discord API latency high: %s", latency)
			}
		case <-ctx.Done():
			log.Println("[INFO] Performance monitor stopped by shutdown signal.")
			return
		}
	}
}

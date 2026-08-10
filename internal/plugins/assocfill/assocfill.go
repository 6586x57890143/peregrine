// Package assocfill is the one-shot walk that repairs the association indexes.
//
// # Why this exists (SPEC.md section 8, finding 46)
//
// Finding 33 found that the historical backfill wrote NO associations at all: it passed no
// author, learn.associate returns early on an empty name set, and so every backfilled message
// wrote neither name_topic nor topic_word. M14 fixed the writer.
//
// Fixing the writer did not fix the data, and this is the general lesson worth carrying: a
// fix to a writer is not a fix to the data, and whether the data is re-derivable is a property
// of the LAYOUT that has to be checked before the fix is called done. Here it is not
// re-derivable. The corpus stores n-grams and counts and never message text, while an
// association needs the original word sequence with positions. Writer.SetCursor is monotonic,
// so the normal pass will never re-read history either. A corpus is mostly backfill, so on a
// bot that looked healthy the indexes behind four seed tiers, all of Jump, and the
// TopicGravity, NameTopic, NameAssoc and CurrentTopic logits were nearly empty.
//
// # Additive and time-bounded, not drop-and-rebuild
//
// The obvious design is to empty both buckets and rebuild them, mirroring
// Writer.RebuildKNIndexes. That is wrong here, and the difference is worth stating because the
// two look alike:
//
//   - RebuildKNIndexes is derivable from the corpus, deterministic, and completes or rolls
//     back inside ONE transaction. This walk spans hours and REST calls and cannot be atomic.
//   - An interruption after a drop leaves every unwalked channel with LESS than it started
//     with, because it loses the correct post-fix associations live traffic has been writing
//     since M14. The status quo is thin but never wrong.
//   - A drop also destroys associations from messages Discord can no longer return: deleted
//     messages, deleted channels, channels the bot has lost read access to. Those are not
//     re-derivable at all.
//   - It would make the bot visibly worse for the whole duration, in exactly the direction
//     this milestone exists to improve, during the hours an operator is watching.
//
// Only messages older than the instant the fix deployed lack associations. Walking exactly
// those double-counts nothing, needs no drop, and makes an interruption monotone progress.
// PEREGRINE_ASSOC_BACKFILL_BEFORE is that instant, and being wrong is cheap in both
// directions: too early double-counts a thin slice, too late leaves a thin slice unrepaired.
// Compare the drop design, where being interrupted costs the whole index.
//
// # What it reuses
//
// internal/ingest is already a seam: Session, Cursors and Learner are interfaces. So this is
// the same walk with two different adapters and a stop bound, rather than a second
// implementation of paging, and internal/plugins/ingest is the shape it mirrors.
package assocfill

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

// Options are the dials.
type Options struct {
	// Enabled gates the whole pass. Default false: it re-reads all of history, which is a
	// decision an operator makes rather than one a deploy makes for them.
	Enabled bool

	// Before is the instant the association fix deployed. Messages older than this were
	// learned without associations; everything newer already has them.
	Before time.Time

	// Concurrency and BatchDelay are deliberately gentler than the live ingest pass. This
	// walk has no deadline and the bot does, so it yields REST budget rather than competing
	// for it. bbolt serializes writers, but both passes write one small transaction per
	// message, so the contention that matters is Discord's rate limiter and not the file.
	GuildConcurrency   int
	ChannelConcurrency int
	BatchDelay         time.Duration

	// Retry is how often to look again after an interrupted pass. It is a RESUME timer, not
	// a schedule: a completed pass returns immediately on every tick thereafter.
	Retry time.Duration
}

// Service is the feature.
type Service struct {
	session *discordgo.Session
	store   *storage.Store
	learner *learn.Learner
	opts    Options
	logger  *slog.Logger

	loops       sync.WaitGroup
	cancelLoops context.CancelFunc
}

// New builds the service, taking the shared Learner for the same reason the ingest plugin
// does: the corpus writer is one per process, and a second one would have its own bot ID that
// nothing ever sets.
func New(session *discordgo.Session, store *storage.Store, learner *learn.Learner, opts Options) *Service {
	return &Service{session: session, store: store, learner: learner, opts: opts}
}

func (s *Service) Name() string { return "assocfill" }

// Init records the logger. State lives in the corpus and is read per pass.
func (s *Service) Init(deps core.Deps) error {
	s.logger = deps.Logger
	return nil
}

// Start launches the pass.
//
// core.RunLoop rather than a bare goroutine, even though this is a one-shot: it buys panic
// isolation for a walk that runs for hours, context binding, and the shutdown wait, all of
// which a hand-rolled goroutine would have to reimplement. RunLoop refuses a non-positive
// interval, so the retry is a real duration; it is a resume timer, and Once returns
// immediately once the corpus says the walk is done.
func (s *Service) Start(ctx context.Context) error {
	if !s.opts.Enabled {
		return nil
	}

	loopCtx, cancel := context.WithCancel(ctx)
	s.cancelLoops = cancel

	core.RunLoop(loopCtx, &s.loops, s.logger, core.Loop{
		Name:      "assoc-backfill",
		Every:     s.opts.Retry,
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
		// Expected rather than alarming: the walk is long by design and the shutdown budget
		// is eight seconds. Its cursor bucket is what makes an interrupted pass resumable, so
		// nothing is lost.
		s.logger.Info("shutdown deadline reached with the association backfill still running; " +
			"it resumes from its own cursor on the next start")
	}
	return nil
}

// Once performs one pass, or returns immediately if the walk is already finished.
func (s *Service) Once(ctx context.Context) {
	var state string
	if err := s.store.View(func(r *storage.Reader) error {
		state = r.AssocBackfillState()
		return nil
	}); err != nil {
		s.logger.Error("read association backfill state failed", "err", err)
		return
	}
	if state == storage.AssocBackfillDone {
		return
	}

	if state == storage.AssocBackfillPending {
		if err := s.store.Update(func(w *storage.Writer) error {
			return w.SetAssocBackfillState(storage.AssocBackfillRunning)
		}); err != nil {
			s.logger.Error("mark association backfill running failed", "err", err)
			return
		}
	}

	s.logger.Info("starting the association backfill",
		"before", s.opts.Before.Format(time.RFC3339),
		"why", "messages older than this were learned before the association fix (finding 46)")
	start := time.Now()

	// A member cache per pass. names.Resolve calls GuildMember, which discordgo issues as an
	// unconditional REST GET with no state-cache check, and this walk touches every message
	// in history: without a cache a corpus with half a million messages and a third of them
	// carrying a mention is six figures of avoidable requests. names.Session is a one-method
	// interface declared by names itself, so the cache belongs here rather than there.
	cache := newMemberCache(s.session)

	in := ingest.New(
		cache,
		cursors{store: s.store},
		learner{session: cache, store: s.store, learner: s.learner},
		s.logger,
		ingest.Options{
			// From the beginning of time. SnowflakeAt clamps below the Discord epoch, which
			// is what that clamp was written for, so an absurd lookback is the honest way to
			// say "everything" without inventing a second mechanism.
			Lookback:           100 * 365 * 24 * time.Hour,
			Until:              s.opts.Before,
			GuildConcurrency:   s.opts.GuildConcurrency,
			ChannelConcurrency: s.opts.ChannelConcurrency,
			BatchDelay:         s.opts.BatchDelay,
		})

	stats, err := in.Run(ctx)
	if err != nil {
		s.logger.Error("association backfill pass failed, will resume", "err", err)
		return
	}

	if ctx.Err() != nil {
		s.logger.Info("association backfill interrupted, will resume from its cursor",
			"repaired", stats.Learned, "took", time.Since(start))
		return
	}

	if err := s.store.Update(func(w *storage.Writer) error {
		return w.SetAssocBackfillState(storage.AssocBackfillDone)
	}); err != nil {
		s.logger.Error("mark association backfill done failed", "err", err)
		return
	}

	s.logger.Info("association backfill finished",
		"guilds", stats.Guilds, "channels", stats.Channels,
		"repaired", stats.Learned, "skipped", stats.Skipped, "errors", stats.Errors,
		"requests_saved", cache.hits(), "took", time.Since(start))
}

// learner adapts the corpus to ingest.Learner, calling Associations rather than Message.
//
// That is the whole difference between this pass and the live one, and it is why a second
// entry point on Learner exists: re-reading through Message would count every n-gram a second
// time, which is finding 13.
type learner struct {
	session names.Session
	store   *storage.Store
	learner *learn.Learner
}

func (l learner) Learn(m *discordgo.Message, guildID string) error {
	mentioned := names.OfMessage(l.session, l.store, &discordgo.MessageCreate{Message: m}, guildID)
	author := names.Primary(m.Author, m.Member)

	return l.store.Update(func(w *storage.Writer) error {
		return l.learner.Associations(w, m.Content, author, mentioned)
	})
}

// cursors adapts the corpus to ingest.Cursors, over the backfill's OWN bucket.
//
// Separate from the ingest cursor so the two passes cannot move each other's mark. They read
// the same channels for opposite reasons: ingest asks what is new and must never rewind, this
// asks what is old and finishes.
type cursors struct{ store *storage.Store }

func (c cursors) Cursor(channelID string) (string, error) {
	var id string
	err := c.store.View(func(r *storage.Reader) error {
		id = r.AssocCursor(channelID)
		return nil
	})
	return id, err
}

func (c cursors) SetCursor(channelID, messageID string) error {
	return c.store.Update(func(w *storage.Writer) error {
		return w.SetAssocCursor(channelID, messageID)
	})
}

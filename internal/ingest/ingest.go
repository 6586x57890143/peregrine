// Package ingest walks Discord history and decides what the bot has not yet read.
//
// It owns exactly one question, "what is new", and deliberately not the answer to "what
// should be done with it": that belongs to the Learner the caller supplies. The split
// matters because the two have different failure modes. Reading the wrong messages
// wastes API budget and corrupts counts; learning them wrongly is a safety question with
// a gate in front of it.
//
// # What was wrong, and why a cursor rather than a bigger dedup window
//
// The old loop re-scanned the trailing PEREGRINE_INGEST_LOOKBACK window every
// PEREGRINE_INGEST_TICK, so with the shipped defaults it re-read 24 hours of history
// every 10 minutes: about 144 passes over every message. It relied on the history bucket
// to recognise what it had already learned, and that bucket is capped at
// PEREGRINE_MAX_HISTORY entries.
//
// On a busy guild the cap is reached inside the lookback window, so the oldest entries
// have already been evicted by the time the next pass arrives, and those messages are
// learned AGAIN. Their n-grams are counted twice, or ten times, depending on how far the
// eviction has run. That is finding 13, and its effect is not merely wasted work: raw
// frequency is what generation samples on, so the model was quietly biased toward
// whatever happened to fall in the re-read band.
//
// Raising the history cap would not fix it, it would only move the corpus size at which
// it starts. The cursor changes the question from "what have I forgotten" to "what is
// new", and the answer to that does not depend on how much the bot remembers.
//
// # afterID, and the bootstrap problem it creates
//
// Discord's ChannelMessages takes an afterID and returns messages newer than it, which is
// exactly a resumable cursor. The catch is the first pass for a channel, where there is
// no cursor and "everything after nothing" is the channel's entire history.
//
// So a missing cursor is seeded from the lookback window instead, by synthesising a
// snowflake from a timestamp. Discord IDs put a millisecond timestamp in their high bits,
// so an ID can be constructed for any instant, and asking for messages after it is the
// same request the old code approximated by paging backwards and comparing timestamps.
// That makes the lookback a BOOTSTRAP bound rather than a re-read window, which is the
// whole behavioural change: it applies once per channel instead of on every pass.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"
	"golang.org/x/sync/errgroup"
)

// Session is the slice of discordgo this package reads. *discordgo.Session satisfies it.
//
// Read-only by construction: there is no send method here, so an ingest pass cannot post
// even by accident. That is not a hypothetical concern, because this package walks
// channels the bot may have no business speaking in.
type Session interface {
	UserGuilds(limit int, beforeID, afterID string, withCounts bool, options ...discordgo.RequestOption) ([]*discordgo.UserGuild, error)
	GuildChannels(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Channel, error)
	ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error)
}

// Cursors is the high-water mark store. *storage.Store satisfies it through a small
// adapter in the caller, because storage's own methods take a Reader or a Writer.
type Cursors interface {
	// Cursor returns the newest message ID ingested from a channel, or "".
	Cursor(channelID string) (string, error)

	// SetCursor advances the mark. Implementations must refuse to move it backwards.
	SetCursor(channelID, messageID string) error
}

// Learner consumes one historical message.
//
// An error is counted and logged, never fatal: one message that will not learn is not a
// reason to abandon a channel, and the commonest cause is content the safety gate
// refused, which is a success from the gate's point of view.
type Learner interface {
	Learn(m *discordgo.Message, guildID string) error
}

// Options are the tunables. Zero values get sane defaults so a caller cannot
// accidentally configure a pass that does nothing.
type Options struct {
	// Lookback bounds the FIRST pass over a channel. It is not a re-read window.
	Lookback time.Duration

	// GuildConcurrency and ChannelConcurrency bound the fan-out.
	//
	// Both were unbounded, which is finding 14: one goroutine per channel per guild,
	// each paging Discord. On a bot in several large guilds that is hundreds of
	// concurrent REST calls, which Discord answers with rate limits, and the retries
	// then make it worse. bbolt's single writer also serializes every learn, so past a
	// handful of workers the extra concurrency buys contention rather than throughput.
	GuildConcurrency   int
	ChannelConcurrency int

	// BatchDelay paces the paging loop within one channel.
	BatchDelay time.Duration

	// PageSize is how many messages to request per call, capped at Discord's 100.
	PageSize int
}

func (o Options) withDefaults() Options {
	if o.Lookback <= 0 {
		o.Lookback = 24 * time.Hour
	}
	if o.GuildConcurrency <= 0 {
		o.GuildConcurrency = 4
	}
	if o.ChannelConcurrency <= 0 {
		o.ChannelConcurrency = 4
	}
	if o.PageSize <= 0 || o.PageSize > 100 {
		o.PageSize = 100
	}
	return o
}

// Ingester walks history. Safe for concurrent use if its dependencies are.
type Ingester struct {
	session Session
	cursors Cursors
	learner Learner
	log     *slog.Logger
	opts    Options
}

func New(s Session, c Cursors, l Learner, log *slog.Logger, opts Options) *Ingester {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Ingester{session: s, cursors: c, learner: l, log: log, opts: opts.withDefaults()}
}

// Stats is what one pass did, for the log line and for tests.
type Stats struct {
	Guilds   int
	Channels int
	Learned  int
	Skipped  int
	Errors   int
}

// Run performs one ingest pass over every guild the bot is in.
//
// It returns an error only when the pass could not start. A guild or channel that fails
// is logged and skipped, because one unreadable channel must not stop the others: the
// commonest cause is a 403 on a channel the bot cannot see, which is not a fault.
func (in *Ingester) Run(ctx context.Context) (Stats, error) {
	start := time.Now()

	guilds, err := in.session.UserGuilds(100, "", "", false)
	if err != nil {
		return Stats{}, fmt.Errorf("fetch guilds: %w", err)
	}

	var total stats
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(in.opts.GuildConcurrency)

	for _, guild := range guilds {
		if guild == nil {
			continue
		}
		gid, gname := guild.ID, guild.Name
		g.Go(func() error {
			// The error is swallowed on purpose: errgroup would cancel every other
			// guild on the first failure, and a guild the bot cannot read is not a
			// reason to abandon the ones it can.
			s := in.guild(gctx, gid, gname)
			total.add(s)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return total.snapshot(), err
	}

	out := total.snapshot()
	out.Guilds = len(guilds)
	in.log.Info("ingest pass complete",
		"guilds", out.Guilds, "channels", out.Channels,
		"learned", out.Learned, "skipped", out.Skipped, "errors", out.Errors,
		"took", time.Since(start))
	return out, nil
}

// guild walks the text channels of one guild.
//
// There is no activity pre-scan any more, and its absence is half of finding 14. The old
// code paged every channel to COUNT recent messages, discarded the messages, and then
// paged the active ones again to fetch the same messages. With a cursor the count is
// unnecessary: a channel with nothing after its mark returns one empty page and costs one
// call, which is cheaper than the count ever was and needs no second pass.
func (in *Ingester) guild(ctx context.Context, guildID, guildName string) Stats {
	channels, err := in.session.GuildChannels(guildID)
	if err != nil {
		if !forbidden(err) {
			in.log.Warn("fetch channels failed", "guild", guildName, "err", err)
		}
		return Stats{}
	}

	var total stats
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(in.opts.ChannelConcurrency)

	for _, ch := range channels {
		if ch == nil || ch.Type != discordgo.ChannelTypeGuildText {
			continue
		}
		channel := ch
		g.Go(func() error {
			s := in.channel(gctx, channel, guildID)
			total.add(s)
			return nil
		})
	}
	_ = g.Wait() // channel workers never return an error, for the reason above

	out := total.snapshot()
	out.Channels = countedChannels(channels)
	return out
}

// channel reads everything after a channel's cursor and learns it.
func (in *Ingester) channel(ctx context.Context, ch *discordgo.Channel, guildID string) Stats {
	var st Stats

	after, err := in.cursors.Cursor(ch.ID)
	if err != nil {
		in.log.Warn("read cursor failed", "channel", ch.Name, "err", err)
		return st
	}
	bootstrapped := after == ""
	if bootstrapped {
		// First pass over this channel: start from the lookback bound rather than from
		// the beginning of time.
		after = SnowflakeAt(time.Now().Add(-in.opts.Lookback))
	}

	// The newest ID seen in this pass, which becomes the new mark. Tracked separately
	// from `after` because the mark must only advance once the batch has been handled:
	// advancing it first would lose the batch on a shutdown mid-pass.
	newest := ""

	for {
		if ctx.Err() != nil {
			in.log.Debug("channel ingest stopped by shutdown", "channel", ch.Name)
			break
		}

		// afterID paging returns messages NEWER than after, and Discord orders them
		// newest-first within the page regardless. Both facts matter: the loop advances
		// `after` to the newest of the batch, and the batch is reversed before learning
		// so a channel is learned in the order it was said.
		batch, err := in.session.ChannelMessages(ch.ID, in.opts.PageSize, "", after, "")
		if err != nil {
			if !forbidden(err) {
				in.log.Warn("fetch messages failed", "channel", ch.Name, "err", err)
				st.Errors++
			}
			break
		}
		if len(batch) == 0 {
			break
		}

		newestInBatch := newestID(batch)
		if newestInBatch == "" {
			break
		}

		for _, m := range reversed(batch) {
			if m.Author == nil || m.Author.Bot || m.Timestamp.IsZero() {
				st.Skipped++
				continue
			}
			if err := in.learner.Learn(m, guildID); err != nil {
				st.Errors++
				continue
			}
			st.Learned++
		}

		newest = newestInBatch
		after = newestInBatch

		if len(batch) < in.opts.PageSize {
			break
		}
		if in.opts.BatchDelay > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(in.opts.BatchDelay):
			}
		}
	}

	// The mark advances even when nothing was learned, as long as messages were SEEN.
	// A page of bot messages is progress: leaving the mark behind would mean re-reading
	// them on every pass forever, which is the shape of the bug this replaces.
	if newest != "" {
		if err := in.cursors.SetCursor(ch.ID, newest); err != nil {
			in.log.Warn("advance cursor failed", "channel", ch.Name, "err", err)
		}
	}

	if st.Learned > 0 || st.Errors > 0 {
		in.log.Info("channel ingested", "channel", ch.Name,
			"learned", st.Learned, "skipped", st.Skipped, "errors", st.Errors,
			"bootstrap", bootstrapped)
	}
	return st
}

// discordEpoch is the offset snowflake timestamps are measured from, 2015-01-01 UTC in
// milliseconds.
const discordEpoch = 1420070400000

// SnowflakeAt builds the smallest snowflake for an instant, so it can be used as an
// afterID meaning "everything since then".
//
// Only the timestamp bits are set and the worker, process and sequence bits are left
// zero, which is what makes it the smallest ID for that millisecond and therefore
// inclusive of every real message in it. Discord documents this construction for exactly
// this purpose.
//
// An instant before the Discord epoch clamps to 0 rather than wrapping into a huge
// unsigned value. That is reachable with a large enough lookback, and the failure it
// avoids is not subtle: a wrapped ID is a FUTURE snowflake, so the pass would ask for
// messages after the year 4000 and quietly ingest nothing at all, forever.
func SnowflakeAt(t time.Time) string {
	ms := t.UTC().UnixMilli() - discordEpoch
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%d", uint64(ms)<<22)
}

// forbidden reports whether an error is Discord refusing access.
//
// Logged at debug rather than warn, because a channel the bot cannot see is the normal
// state of most channels in most guilds, and warning about each one on every pass buries
// the failures that matter.
func forbidden(err error) bool {
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Response != nil {
		return restErr.Response.StatusCode == http.StatusForbidden ||
			restErr.Response.StatusCode == http.StatusNotFound
	}
	return false
}

// newestID returns the largest snowflake in a batch.
//
// Compared as numbers via length-then-lexicographic rather than trusting the order
// Discord returned, because the cursor must never move backwards and a wrong answer here
// would rewind it. Length first because these are decimal strings: "9999" is shorter than
// "10000" but smaller, so a plain string comparison gets it wrong exactly the way the old
// history keys did (finding 10).
func newestID(batch []*discordgo.Message) string {
	best := ""
	for _, m := range batch {
		if m == nil || m.ID == "" {
			continue
		}
		if best == "" || snowflakeLess(best, m.ID) {
			best = m.ID
		}
	}
	return best
}

// snowflakeLess reports whether a is chronologically before b.
func snowflakeLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// reversed returns the batch oldest-first.
//
// Discord returns a page newest-first, and learning order is not cosmetic: n-grams are
// learned per message so the order between messages does not change what is stored, but
// the cursor and the log both read as nonsense if a channel is processed backwards, and a
// future per-channel context window would be silently wrong.
func reversed(batch []*discordgo.Message) []*discordgo.Message {
	out := make([]*discordgo.Message, len(batch))
	for i, m := range batch {
		out[len(batch)-1-i] = m
	}
	return out
}

func countedChannels(channels []*discordgo.Channel) int {
	n := 0
	for _, ch := range channels {
		if ch != nil && ch.Type == discordgo.ChannelTypeGuildText {
			n++
		}
	}
	return n
}

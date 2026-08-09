package ingest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// fakeSession serves a fixed set of messages per channel and records what was asked for.
//
// It implements afterID paging the way Discord does, which is the whole point: the bug
// this milestone fixes is about WHICH messages get requested, so a fake that ignored
// afterID would make every test here vacuous.
type fakeSession struct {
	mu sync.Mutex

	guilds   []*discordgo.UserGuild
	channels map[string][]*discordgo.Channel // guild ID -> channels
	messages map[string][]*discordgo.Message // channel ID -> messages, oldest first

	// calls counts ChannelMessages requests per channel, which is how the tests measure
	// that the double scan is gone and that an idle channel is cheap.
	calls map[string]int

	guildsErr   error
	channelsErr error
	messagesErr error
}

func newFake() *fakeSession {
	return &fakeSession{
		channels: map[string][]*discordgo.Channel{},
		messages: map[string][]*discordgo.Message{},
		calls:    map[string]int{},
	}
}

func (f *fakeSession) UserGuilds(int, string, string, bool, ...discordgo.RequestOption) ([]*discordgo.UserGuild, error) {
	if f.guildsErr != nil {
		return nil, f.guildsErr
	}
	return f.guilds, nil
}

func (f *fakeSession) GuildChannels(guildID string, _ ...discordgo.RequestOption) ([]*discordgo.Channel, error) {
	if f.channelsErr != nil {
		return nil, f.channelsErr
	}
	return f.channels[guildID], nil
}

// ChannelMessages implements afterID paging: everything strictly newer than afterID,
// returned NEWEST FIRST, capped at limit. That ordering is Discord's and it is why the
// production code reverses each batch.
func (f *fakeSession) ChannelMessages(channelID string, limit int, _, afterID, _ string, _ ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	f.mu.Lock()
	f.calls[channelID]++
	f.mu.Unlock()

	if f.messagesErr != nil {
		return nil, f.messagesErr
	}

	var newer []*discordgo.Message
	for _, m := range f.messages[channelID] {
		if afterID == "" || snowflakeLess(afterID, m.ID) {
			newer = append(newer, m)
		}
	}
	if len(newer) > limit {
		newer = newer[:limit]
	}
	return reversed(newer), nil
}

func (f *fakeSession) callCount(channelID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[channelID]
}

// memCursors is an in-memory Cursors that enforces the same monotonic rule storage does.
type memCursors struct {
	mu sync.Mutex
	m  map[string]string

	readErr  error
	writeErr error
}

func newCursors() *memCursors { return &memCursors{m: map[string]string{}} }

func (c *memCursors) Cursor(channelID string) (string, error) {
	if c.readErr != nil {
		return "", c.readErr
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[channelID], nil
}

func (c *memCursors) SetCursor(channelID, messageID string) error {
	if c.writeErr != nil {
		return c.writeErr
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.m[channelID]; ok && !snowflakeLess(cur, messageID) {
		return nil // never backwards, matching storage
	}
	c.m[channelID] = messageID
	return nil
}

// recordingLearner counts every message it is handed, so a test can assert that a
// message was learned EXACTLY ONCE across passes.
type recordingLearner struct {
	mu    sync.Mutex
	seen  map[string]int
	order []string
	err   error
}

func newLearner() *recordingLearner {
	return &recordingLearner{seen: map[string]int{}}
}

func (l *recordingLearner) Learn(m *discordgo.Message, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.seen[m.ID]++
	l.order = append(l.order, m.ID)
	return nil
}

func (l *recordingLearner) count(id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen[id]
}

func (l *recordingLearner) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.seen {
		n += c
	}
	return n
}

// msg builds a message whose ID is a snowflake for the given time, so the fake's afterID
// comparison and the production bootstrap logic agree about what is recent.
func msg(at time.Time, seq int, content string) *discordgo.Message {
	id := snowflakeAtSeq(at, seq)
	return &discordgo.Message{
		ID:        id,
		Content:   content,
		Timestamp: at,
		Author:    &discordgo.User{ID: "u1", Username: "human"},
	}
}

// snowflakeAtSeq is SnowflakeAt with the sequence bits set, so two messages in the same
// millisecond get distinct increasing IDs.
func snowflakeAtSeq(t time.Time, seq int) string {
	ms := t.UTC().UnixMilli() - discordEpoch
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%d", uint64(ms)<<22|uint64(seq))
}

func botMsg(at time.Time, seq int) *discordgo.Message {
	m := msg(at, seq, "beep")
	m.Author.Bot = true
	return m
}

// fixture builds a one-guild, one-channel world with the given messages.
func fixture(msgs ...*discordgo.Message) (*fakeSession, *memCursors, *recordingLearner) {
	f := newFake()
	f.guilds = []*discordgo.UserGuild{{ID: "g1", Name: "guild"}}
	f.channels["g1"] = []*discordgo.Channel{{ID: "c1", Name: "general", Type: discordgo.ChannelTypeGuildText}}
	f.messages["c1"] = msgs
	return f, newCursors(), newLearner()
}

func opts() Options {
	return Options{Lookback: time.Hour, GuildConcurrency: 2, ChannelConcurrency: 2, PageSize: 100}
}

// TestASecondPassLearnsNothingNew is the pin for finding 13, and it is the whole point of
// this milestone.
//
// The old loop re-read the trailing lookback window on every tick and relied on the
// history bucket, capped at PEREGRINE_MAX_HISTORY, to notice what it had already learned.
// Once that cap evicted an entry the message was learned AGAIN and its n-grams counted
// twice, which biases generation because raw frequency is what the sampler reads.
//
// This test gives the Learner no memory at all, which is exactly the situation an evicted
// history entry creates. The cursor has to do the work.
func TestASecondPassLearnsNothingNew(t *testing.T) {
	now := time.Now()
	f, c, l := fixture(
		msg(now.Add(-30*time.Minute), 1, "first"),
		msg(now.Add(-20*time.Minute), 2, "second"),
		msg(now.Add(-10*time.Minute), 3, "third"),
	)
	in := New(f, c, l, nil, opts())

	first, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Learned != 3 {
		t.Fatalf("first pass learned %d, want 3", first.Learned)
	}

	second, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.Learned != 0 {
		t.Errorf("second pass learned %d messages, want 0. The cursor did not hold, so the "+
			"pass re-read what it had already ingested. With the history bucket evicting "+
			"older entries that means those n-grams get counted twice, which is finding 13 "+
			"and it biases the model toward whatever falls in the re-read band", second.Learned)
	}

	for _, id := range l.order {
		if got := l.count(id); got != 1 {
			t.Errorf("message %s learned %d times, want exactly 1", id, got)
		}
	}
}

// TestOnlyNewMessagesAreLearnedOnALaterPass is the other half: the cursor must not be so
// sticky that genuinely new messages are missed.
func TestOnlyNewMessagesAreLearnedOnALaterPass(t *testing.T) {
	now := time.Now()
	older := msg(now.Add(-30*time.Minute), 1, "first")
	f, c, l := fixture(older)
	in := New(f, c, l, nil, opts())

	if _, err := in.Run(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// Somebody says something new.
	newer := msg(now.Add(-1*time.Minute), 2, "second")
	f.messages["c1"] = append(f.messages["c1"], newer)

	second, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.Learned != 1 {
		t.Fatalf("second pass learned %d, want exactly the 1 new message", second.Learned)
	}
	if l.count(newer.ID) != 1 {
		t.Error("the new message was not learned")
	}
	if l.count(older.ID) != 1 {
		t.Errorf("the older message was learned %d times, want 1", l.count(older.ID))
	}
}

// TestTheFirstPassIsBoundedByLookback. Without a cursor, "everything after nothing" is
// the channel's entire history, which on a real server is millions of messages and an API
// budget nobody has. The lookback bounds the bootstrap.
func TestTheFirstPassIsBoundedByLookback(t *testing.T) {
	now := time.Now()
	ancient := msg(now.Add(-72*time.Hour), 1, "ancient history")
	recent := msg(now.Add(-10*time.Minute), 2, "recent")

	f, c, l := fixture(ancient, recent)
	o := opts()
	o.Lookback = time.Hour
	in := New(f, c, l, nil, o)

	if _, err := in.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if l.count(recent.ID) != 1 {
		t.Error("a message inside the lookback was not learned")
	}
	if l.count(ancient.ID) != 0 {
		t.Error("a message from before the lookback was learned on the bootstrap pass; the " +
			"lookback is what stops the first pass walking the whole channel history")
	}
}

// TestAnIdleChannelCostsOneCall is the pin for the other half of finding 14.
//
// The old code paged every channel to COUNT recent messages, threw the messages away, and
// then paged the active ones AGAIN to fetch the same messages. With a cursor there is
// nothing to count: a channel with nothing new answers one request with an empty page.
func TestAnIdleChannelCostsOneCall(t *testing.T) {
	now := time.Now()
	f, c, l := fixture(msg(now.Add(-10*time.Minute), 1, "hello"))
	in := New(f, c, l, nil, opts())

	if _, err := in.Run(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	callsAfterFirst := f.callCount("c1")

	if _, err := in.Run(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	idleCalls := f.callCount("c1") - callsAfterFirst

	if idleCalls != 1 {
		t.Errorf("an idle channel cost %d requests, want exactly 1. The count-then-fetch "+
			"double scan is what this replaces, so a second scan means it is back", idleCalls)
	}
}

// TestMessagesAreLearnedOldestFirst. Discord returns a page newest-first, so a pass that
// forgot to reverse would learn a channel backwards. That does not change what n-grams
// are stored today, but the cursor and the log both read as nonsense and a future
// per-channel context window would be silently wrong.
func TestMessagesAreLearnedOldestFirst(t *testing.T) {
	now := time.Now()
	a := msg(now.Add(-30*time.Minute), 1, "first")
	b := msg(now.Add(-20*time.Minute), 2, "second")
	cc := msg(now.Add(-10*time.Minute), 3, "third")

	f, cur, l := fixture(a, b, cc)
	if _, err := New(f, cur, l, nil, opts()).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	want := []string{a.ID, b.ID, cc.ID}
	if len(l.order) != len(want) {
		t.Fatalf("learned %d messages, want %d", len(l.order), len(want))
	}
	for i := range want {
		if l.order[i] != want[i] {
			t.Fatalf("learned in order %v, want %v (oldest first)", l.order, want)
		}
	}
}

// TestPagingWalksEveryMessage covers a channel with more messages than one page holds,
// which is where an afterID loop that failed to advance would spin forever.
func TestPagingWalksEveryMessage(t *testing.T) {
	now := time.Now()
	var msgs []*discordgo.Message
	for i := range 25 {
		msgs = append(msgs, msg(now.Add(-time.Duration(30-i)*time.Minute), i+1, "m"))
	}

	f, c, l := fixture(msgs...)
	o := opts()
	o.PageSize = 10 // forces three pages
	in := New(f, c, l, nil, o)

	st, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if st.Learned != 25 {
		t.Errorf("learned %d of 25 messages across pages", st.Learned)
	}
	for _, m := range msgs {
		if l.count(m.ID) != 1 {
			t.Errorf("message %s learned %d times", m.ID, l.count(m.ID))
		}
	}
	// Three full-or-partial pages: 10, 10, 5. The fourth is not needed because the third
	// came back short.
	if got := f.callCount("c1"); got != 3 {
		t.Errorf("made %d requests for 25 messages at page size 10, want 3", got)
	}
}

// TestTheCursorAdvancesPastBotMessagesToo. A page of nothing but bot messages is still
// progress. Leaving the mark behind would mean re-requesting them on every pass forever,
// which is the shape of the bug this replaces.
func TestTheCursorAdvancesPastBotMessagesToo(t *testing.T) {
	now := time.Now()
	f, c, l := fixture(
		botMsg(now.Add(-20*time.Minute), 1),
		botMsg(now.Add(-10*time.Minute), 2),
	)
	in := New(f, c, l, nil, opts())

	st, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if st.Learned != 0 {
		t.Errorf("learned %d bot messages, want 0", st.Learned)
	}
	if st.Skipped != 2 {
		t.Errorf("skipped %d, want 2", st.Skipped)
	}

	got, _ := c.Cursor("c1")
	if got == "" {
		t.Fatal("the cursor did not advance past a page of bot messages, so they will be " +
			"re-requested on every pass forever")
	}

	callsBefore := f.callCount("c1")
	if _, err := in.Run(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if f.callCount("c1")-callsBefore != 1 {
		t.Error("the second pass re-paged the bot messages")
	}
}

// TestCursorNeverGoesBackwards. Two things can hand the store an older ID: a batch
// processed out of order, and two passes overlapping on one channel. Either would rewind
// the mark and cause everything between to be re-learned, which is finding 13 by another
// route.
func TestCursorNeverGoesBackwards(t *testing.T) {
	c := newCursors()
	if err := c.SetCursor("c1", "2000"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCursor("c1", "1000"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Cursor("c1"); got != "2000" {
		t.Errorf("cursor moved backwards to %q", got)
	}
}

// TestSnowflakeAtIsInclusiveAndMonotonic. The bootstrap depends on being able to turn an
// instant into an afterID, so this checks the construction rather than trusting it.
func TestSnowflakeAtIsInclusiveAndMonotonic(t *testing.T) {
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	earlier := SnowflakeAt(base)
	later := SnowflakeAt(base.Add(time.Hour))
	if !snowflakeLess(earlier, later) {
		t.Errorf("SnowflakeAt is not monotonic: %s then %s", earlier, later)
	}

	// A real message in the same millisecond has the sequence bits set, so it must sort
	// AFTER the synthesized ID. That is what makes the bound inclusive: a message exactly
	// at the cutoff is still ingested.
	real := snowflakeAtSeq(base, 1)
	if !snowflakeLess(earlier, real) {
		t.Errorf("a real message at the same instant (%s) does not sort after the "+
			"synthesized bound (%s), so the bootstrap would skip it", real, earlier)
	}
}

// TestSnowflakeAtClampsBeforeTheDiscordEpoch is not a hypothetical. A large enough
// lookback puts the bound before 2015, and an unclamped subtraction wraps into a huge
// unsigned value: a FUTURE snowflake, so the pass asks for messages after the year 4000
// and quietly ingests nothing at all, forever.
func TestSnowflakeAtClampsBeforeTheDiscordEpoch(t *testing.T) {
	got := SnowflakeAt(time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC))
	if got != "0" {
		t.Errorf("SnowflakeAt before the Discord epoch = %q, want \"0\". An unclamped "+
			"value wraps to a future ID and the pass silently ingests nothing", got)
	}
}

// TestAForbiddenChannelIsSkippedQuietly. A channel the bot cannot see is the normal state
// of most channels in most guilds, so it must not be an error and must not stop the pass.
func TestAForbiddenChannelIsSkippedQuietly(t *testing.T) {
	now := time.Now()
	f, c, l := fixture(msg(now.Add(-5*time.Minute), 1, "hello"))
	f.messagesErr = &discordgo.RESTError{
		Response: &http.Response{StatusCode: http.StatusForbidden},
	}

	st, err := New(f, c, l, nil, opts()).Run(context.Background())
	if err != nil {
		t.Fatalf("a 403 must not fail the pass: %v", err)
	}
	if st.Errors != 0 {
		t.Errorf("a 403 counted as %d errors, want 0: it is a permission, not a fault", st.Errors)
	}
	if st.Learned != 0 {
		t.Errorf("learned %d from a forbidden channel", st.Learned)
	}
}

// TestAFailingChannelDoesNotStopTheOthers. errgroup cancels its context on the first
// error, so returning one from a channel worker would abandon every other channel. The
// workers swallow theirs deliberately.
func TestAFailingChannelDoesNotStopTheOthers(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.guilds = []*discordgo.UserGuild{{ID: "g1", Name: "guild"}}
	f.channels["g1"] = []*discordgo.Channel{
		{ID: "good", Name: "good", Type: discordgo.ChannelTypeGuildText},
		{ID: "bad", Name: "bad", Type: discordgo.ChannelTypeGuildText},
	}
	f.messages["good"] = []*discordgo.Message{msg(now.Add(-5*time.Minute), 1, "hello")}
	f.messages["bad"] = []*discordgo.Message{msg(now.Add(-5*time.Minute), 2, "unreadable")}

	l := newLearner()
	l.err = errors.New("gate refused")

	st, err := New(f, newCursors(), l, nil, opts()).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Both channels were attempted. A learner error is counted, not fatal: the commonest
	// cause is the safety gate refusing content, which is a success from its point of
	// view.
	if st.Errors != 2 {
		t.Errorf("counted %d learn errors, want 2 (one per channel)", st.Errors)
	}
	if f.callCount("good") == 0 || f.callCount("bad") == 0 {
		t.Error("one channel was never attempted, so a failure in the other abandoned it")
	}
}

// TestOnlyTextChannelsAreWalked. Voice and category channels have no messages, and asking
// for them earns a 400 per channel per pass.
func TestOnlyTextChannelsAreWalked(t *testing.T) {
	f := newFake()
	f.guilds = []*discordgo.UserGuild{{ID: "g1", Name: "guild"}}
	f.channels["g1"] = []*discordgo.Channel{
		{ID: "text", Name: "text", Type: discordgo.ChannelTypeGuildText},
		{ID: "voice", Name: "voice", Type: discordgo.ChannelTypeGuildVoice},
		{ID: "cat", Name: "cat", Type: discordgo.ChannelTypeGuildCategory},
		nil,
	}

	st, err := New(f, newCursors(), newLearner(), nil, opts()).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.callCount("voice") != 0 || f.callCount("cat") != 0 {
		t.Error("a non-text channel was paged")
	}
	if st.Channels != 1 {
		t.Errorf("counted %d text channels, want 1", st.Channels)
	}
}

// TestGuildFetchFailureIsTheOneFatalCase. Everything else degrades, but if the guild list
// cannot be read there is nothing to walk, and reporting success would hide a broken
// token or a revoked scope behind a cheerful log line.
func TestGuildFetchFailureIsReported(t *testing.T) {
	f := newFake()
	f.guildsErr = errors.New("unauthorized")

	if _, err := New(f, newCursors(), newLearner(), nil, opts()).Run(context.Background()); err == nil {
		t.Error("a failure to list guilds must be reported: there is nothing to walk, and " +
			"a silent success hides a broken token")
	}
}

// TestACancelledContextStopsThePass covers shutdown. The pass must not keep paging after
// the process has been asked to stop, and the ordered shutdown in internal/core gives it
// a bounded budget to notice.
func TestACancelledContextStopsThePass(t *testing.T) {
	now := time.Now()
	var msgs []*discordgo.Message
	for i := range 50 {
		msgs = append(msgs, msg(now.Add(-time.Duration(60-i)*time.Minute), i+1, "m"))
	}
	f, c, l := fixture(msgs...)
	o := opts()
	o.PageSize = 5

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	if _, err := New(f, c, l, nil, o).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if l.total() != 0 {
		t.Errorf("learned %d messages under an already-cancelled context", l.total())
	}
}

// TestCursorReadFailureSkipsTheChannel. A cursor that cannot be read is not a reason to
// ingest from the beginning: doing that is precisely the double-count this milestone
// exists to remove, so the safe answer is to skip and try again next pass.
func TestCursorReadFailureSkipsTheChannel(t *testing.T) {
	now := time.Now()
	f, c, l := fixture(msg(now.Add(-5*time.Minute), 1, "hello"))
	c.readErr = errors.New("corpus busy")

	if _, err := New(f, c, l, nil, opts()).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if l.total() != 0 {
		t.Error("a channel whose cursor could not be read was ingested anyway, which " +
			"re-learns from the lookback bound and double-counts")
	}
	if f.callCount("c1") != 0 {
		t.Error("the channel was paged despite an unreadable cursor")
	}
}

// TestOptionsDefaultsCannotProduceANoOpPass. A zero PageSize would ask Discord for zero
// messages and a zero concurrency would deadlock errgroup, so the defaults are load
// bearing rather than a convenience.
func TestOptionsDefaultsCannotProduceANoOpPass(t *testing.T) {
	got := Options{}.withDefaults()
	if got.PageSize <= 0 || got.PageSize > 100 {
		t.Errorf("PageSize = %d, want 1..100", got.PageSize)
	}
	if got.GuildConcurrency <= 0 || got.ChannelConcurrency <= 0 {
		t.Errorf("concurrency defaults are not positive: %+v", got)
	}
	if got.Lookback <= 0 {
		t.Errorf("Lookback = %v, want positive", got.Lookback)
	}

	// An over-large page size is clamped rather than passed through, because Discord
	// rejects more than 100 and the failure would look like a broken channel.
	if clamped := (Options{PageSize: 5000}).withDefaults(); clamped.PageSize != 100 {
		t.Errorf("PageSize 5000 clamped to %d, want 100", clamped.PageSize)
	}
}

func TestNewestIDComparesNumericallyNotLexically(t *testing.T) {
	batch := []*discordgo.Message{
		{ID: "9999999999999999"},   // 16 digits
		{ID: "10000000000000000"},  // 17 digits, larger
		{ID: "100000000000000000"}, // 18 digits, largest
		nil,
		{ID: ""},
	}
	if got := newestID(batch); got != "100000000000000000" {
		t.Errorf("newestID = %q, want the numerically largest. A plain string comparison "+
			"gets this wrong the same way the old decimal history keys did (finding 10)", got)
	}
}

// TestManyGuildsAndChannelsConcurrently exists mostly for the race detector, which only
// runs in CI because this repo's usual checkout has no C toolchain.
//
// The counters are shared across bounded-but-concurrent workers, so without the mutex in
// stats.go this is a data race, which is the class of bug M3 spent a whole milestone
// removing. It also checks the arithmetic adds up across both fan-out levels, which a
// single-channel test cannot: an accumulator that double-counted or dropped a guild would
// look fine with one of each.
func TestManyGuildsAndChannelsConcurrently(t *testing.T) {
	now := time.Now()
	f := newFake()

	const guilds, channels, msgs = 6, 5, 4
	for gi := range guilds {
		gid := fmt.Sprintf("g%d", gi)
		f.guilds = append(f.guilds, &discordgo.UserGuild{ID: gid, Name: gid})
		for ci := range channels {
			cid := fmt.Sprintf("%s-c%d", gid, ci)
			f.channels[gid] = append(f.channels[gid], &discordgo.Channel{
				ID: cid, Name: cid, Type: discordgo.ChannelTypeGuildText,
			})
			for mi := range msgs {
				// Unique sequence bits per message so IDs never collide across channels.
				seq := (gi*channels+ci)*msgs + mi + 1
				f.messages[cid] = append(f.messages[cid],
					msg(now.Add(-time.Duration(msgs-mi)*time.Minute), seq, "hello"))
			}
		}
	}

	l := newLearner()
	st, err := New(f, newCursors(), l, nil, opts()).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	wantMsgs := guilds * channels * msgs
	if st.Learned != wantMsgs {
		t.Errorf("learned %d, want %d", st.Learned, wantMsgs)
	}
	if st.Channels != guilds*channels {
		t.Errorf("counted %d channels, want %d", st.Channels, guilds*channels)
	}
	if st.Guilds != guilds {
		t.Errorf("counted %d guilds, want %d", st.Guilds, guilds)
	}
	if l.total() != wantMsgs {
		t.Errorf("learner saw %d messages, want %d", l.total(), wantMsgs)
	}
}

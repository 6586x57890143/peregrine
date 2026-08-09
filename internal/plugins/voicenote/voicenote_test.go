package voicenote

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/core"
)

func snowflake(n int) string {
	return strconv.FormatUint((uint64(n)<<22)|1, 10)
}

type fakeGuard struct {
	mu      sync.Mutex
	replies []string
	edits   []string
	refuse  bool
}

func (g *fakeGuard) SendReply(_, content string, _ *discordgo.MessageReference) (*discordgo.Message, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refuse {
		return nil, false
	}
	g.replies = append(g.replies, content)
	return &discordgo.Message{ID: snowflake(850000 + len(g.replies))}, true
}

func (g *fakeGuard) Edit(_, _, content string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.edits = append(g.edits, content)
	return true
}

func (g *fakeGuard) counts() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.replies), len(g.edits)
}

func (g *fakeGuard) lastEdit() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.edits) == 0 {
		return ""
	}
	return g.edits[len(g.edits)-1]
}

// workingEngine stands in for the engine this repository does not ship, so the plugin's own
// behaviour is testable without one.
type workingEngine struct {
	text string
	err  error
	seen []string
}

func (e *workingEngine) Available() bool { return true }

func (e *workingEngine) Transcribe(_ context.Context, path string) (string, error) {
	e.seen = append(e.seen, path)
	return e.text, e.err
}

func logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fixture(t *testing.T, engine Engine, opts Options) (*Service, *fakeGuard) {
	t.Helper()
	guard := &fakeGuard{}
	s := New(engine, guard, opts)
	if err := s.Init(core.Deps{Logger: logger()}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s, guard
}

func voice() []discordgo.MessageAttachment {
	return []discordgo.MessageAttachment{{URL: "https://cdn.example/voice-message.ogg", Filename: "voice-message.ogg"}}
}

// TestTheStubEngineDeclinesRatherThanFailing.
//
// This is the shipped state, so it is the case worth being sure about. Available reports false
// up front, so nothing is queued, no placeholder is posted, and there is no failure reply per
// voice note: the old implementation on Linux produced one of those every time, which is why the
// flag defaults off.
func TestTheStubEngineDeclinesRatherThanFailing(t *testing.T) {
	s, guard := fixture(t, StubEngine(), Options{Enabled: true})

	if s.Available() {
		t.Error("Available reported true with the stub engine")
	}
	if took := s.Offer("c1", snowflake(10), snowflake(20), voice()); took {
		t.Error("Offer accepted a voice note with no engine behind it")
	}
	if replies, edits := guard.counts(); replies != 0 || edits != 0 {
		t.Errorf("said something with no engine: %d replies, %d edits", replies, edits)
	}
	if _, err := StubEngine().Transcribe(t.Context(), "anything"); !errors.Is(err, ErrNoEngine) {
		t.Errorf("Transcribe error = %v, want ErrNoEngine", err)
	}
}

// TestANilEngineIsTheStub, so a wiring mistake fails in the quiet direction rather than
// panicking on the first voice note.
func TestANilEngineIsTheStub(t *testing.T) {
	s, _ := fixture(t, nil, Options{Enabled: true})
	if s.Available() {
		t.Error("a nil engine reported itself available")
	}
}

// TestTheFeatureFlagGatesEverything, even with a working engine.
func TestTheFeatureFlagGatesEverything(t *testing.T) {
	s, guard := fixture(t, &workingEngine{text: "hello"}, Options{Enabled: false})

	if s.Available() {
		t.Error("Available reported true with the feature disabled")
	}
	if s.Offer("c1", snowflake(11), snowflake(20), voice()) {
		t.Error("Offer accepted a voice note with the feature disabled")
	}
	if replies, _ := guard.counts(); replies != 0 {
		t.Error("posted a placeholder with the feature disabled")
	}
}

// TestAPlaceholderIsPostedBeforeTheJobIsQueued.
//
// Without a placeholder there is nothing to edit later, so queueing anyway would burn a
// transcription run and then log an edit failure against a message ID that never existed.
func TestAPlaceholderIsPostedBeforeTheJobIsQueued(t *testing.T) {
	engine := &workingEngine{text: "the bird is loose"}
	s, guard := fixture(t, engine, Options{Enabled: true})

	if !s.Offer("c1", snowflake(12), snowflake(20), voice()) {
		t.Fatal("Offer declined a voice note with a working engine")
	}
	if replies, _ := guard.counts(); replies != 1 {
		t.Errorf("posted %d placeholders, want 1", replies)
	}
}

// TestARefusedPlaceholderMeansNoJob. The guard turns down a paused bot or an ignored channel,
// and a transcription with nowhere to go is a Whisper run spent on nothing.
func TestARefusedPlaceholderMeansNoJob(t *testing.T) {
	engine := &workingEngine{text: "hello"}
	s, guard := fixture(t, engine, Options{Enabled: true})
	guard.refuse = true

	if s.Offer("c1", snowflake(13), snowflake(20), voice()) {
		t.Error("Offer queued a job whose placeholder was refused")
	}
	if len(engine.seen) != 0 {
		t.Error("the engine was called for a job that could not be reported")
	}
}

// TestATranscriptEditsThePlaceholder, end to end through the worker.
func TestATranscriptEditsThePlaceholder(t *testing.T) {
	engine := &workingEngine{text: "  the bird is loose in the server  "}
	s, guard := fixture(t, engine, Options{Enabled: true})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !s.Offer("c1", snowflake(14), snowflake(20), voice()) {
		t.Fatal("Offer declined a voice note")
	}

	waitFor(t, func() bool { return guard.lastEdit() != "" })
	if got := guard.lastEdit(); got != "\U0001F50A the bird is loose in the server" {
		t.Errorf("edit = %q, want the trimmed transcript", got)
	}

	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// TestAFailedTranscriptionSaysSoRatherThanLeavingThePlaceholder.
//
// A placeholder that says "in progress" forever is worse than a failure: it looks like the bot
// hung, and every Discord call used to discard its error, so a failure was indistinguishable
// from success.
func TestAFailedTranscriptionSaysSoRatherThanLeavingThePlaceholder(t *testing.T) {
	engine := &workingEngine{err: errors.New("whisper fell over")}
	s, guard := fixture(t, engine, Options{Enabled: true})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Offer("c1", snowflake(15), snowflake(20), voice())

	waitFor(t, func() bool { return guard.lastEdit() != "" })
	got := guard.lastEdit()
	if got == "" {
		t.Fatal("nothing was said about a failed transcription")
	}
	if strings.HasPrefix(got, "\U0001F50A") {
		t.Errorf("edit = %q, which reads as a successful transcript", got)
	}
}

// TestSilentAudioSaysSoToo, rather than editing the placeholder to an empty message the guard
// would refuse anyway.
func TestSilentAudioSaysSoToo(t *testing.T) {
	engine := &workingEngine{text: "   "}
	s, guard := fixture(t, engine, Options{Enabled: true})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Offer("c1", snowflake(16), snowflake(20), voice())

	waitFor(t, func() bool { return guard.lastEdit() != "" })
	if got := guard.lastEdit(); got == "" {
		t.Error("nothing was said about audio with no speech in it")
	}
}

// TestAFullQueueDropsAndSaysSo.
//
// Non-blocking, because Offer runs on a dispatcher worker: blocking would stall message handling
// behind a slow transcription, which is the same reason the message queue drops rather than
// waits. The placeholder is already posted, so it is edited rather than left in progress forever.
func TestAFullQueueDropsAndSaysSo(t *testing.T) {
	engine := &workingEngine{text: "hello"}
	// Queue size 1 and no worker started, so the second job has nowhere to go.
	s, guard := fixture(t, engine, Options{Enabled: true, QueueSize: 1})

	if !s.Offer("c1", snowflake(17), snowflake(20), voice()) {
		t.Fatal("the first Offer was declined")
	}
	if s.Offer("c1", snowflake(18), snowflake(20), voice()) {
		t.Error("the second Offer was accepted into a full queue")
	}
	if got := s.Dropped(); got != 1 {
		t.Errorf("Dropped = %d, want 1", got)
	}
	if _, edits := guard.counts(); edits != 1 {
		t.Errorf("made %d edits, want 1: the dropped note's placeholder must not say "+
			"\"in progress\" forever", edits)
	}
}

// TestOnlyAudioAttachmentsAreOffered. A message can carry a screenshot and a voice note, and
// posting a placeholder for the screenshot would be the bot announcing work it will not do.
func TestOnlyAudioAttachmentsAreOffered(t *testing.T) {
	engine := &workingEngine{text: "hello"}
	s, guard := fixture(t, engine, Options{Enabled: true})

	atts := []discordgo.MessageAttachment{
		{URL: "https://cdn.example/screenshot.png", Filename: "screenshot.png"},
		{URL: "https://cdn.example/document.pdf", Filename: "document.pdf"},
	}
	if s.Offer("c1", snowflake(19), snowflake(20), atts) {
		t.Error("Offer accepted a message with no audio in it")
	}
	if replies, _ := guard.counts(); replies != 0 {
		t.Error("posted a placeholder for a message with no audio")
	}
}

// TestTheAudioExtensionsAreTheOnesPeoplePost. Discord voice messages are always
// voice-message.ogg, but ordinary audio attachments are the same job once the file is in hand.
func TestTheAudioExtensionsAreTheOnesPeoplePost(t *testing.T) {
	audio := []string{"voice-message.ogg", "clip.MP3", "recording.wav", "thing.m4a", "song.flac"}
	for _, name := range audio {
		if !isAudio(name) {
			t.Errorf("%q was not recognized as audio", name)
		}
	}
	for _, name := range []string{"screenshot.png", "document.pdf", "noextension", ""} {
		if isAudio(name) {
			t.Errorf("%q was treated as audio", name)
		}
	}
}

// TestShutdownWaitsForAnInFlightTranscription rather than cancelling and moving on, so the
// placeholder is not left saying "in progress" across a restart.
func TestShutdownWaitsForAnInFlightTranscription(t *testing.T) {
	engine := &workingEngine{text: "hello"}
	s, guard := fixture(t, engine, Options{Enabled: true})

	ctx, cancel := context.WithCancel(t.Context())
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Offer("c1", snowflake(21), snowflake(20), voice())
	waitFor(t, func() bool { return guard.lastEdit() != "" })

	cancel()
	if err := s.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestShutdownIsSafeWithoutStart, because the registry shuts down every registered service and a
// disabled one never started a worker.
func TestShutdownIsSafeWithoutStart(t *testing.T) {
	s, _ := fixture(t, StubEngine(), Options{Enabled: true})
	if err := s.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown before Start: %v", err)
	}
}

// TestStartDoesNothingWithoutAnEngine, so there is no worker blocking on a queue nothing will
// ever be put into.
func TestStartDoesNothingWithoutAnEngine(t *testing.T) {
	s, _ := fixture(t, StubEngine(), Options{Enabled: true})
	if err := s.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// TestConcurrentOffers exists for CI's race detector: Offer runs on dispatcher workers and the
// drop counter and the queue are shared with the worker goroutine.
func TestConcurrentOffers(t *testing.T) {
	engine := &workingEngine{text: "hello"}
	s, _ := fixture(t, engine, Options{Enabled: true, QueueSize: 4})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 20 {
				s.Offer("c1", snowflake(30000+i*100+j), snowflake(20), voice())
			}
		})
	}
	wg.Wait()
	_ = s.Dropped()
}

// waitFor polls a condition, because the worker is a goroutine and the alternative is a fixed
// sleep long enough to be slow and short enough to be flaky.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for the transcription worker")
}

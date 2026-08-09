package autopost

import (
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/activity"
	"github.com/6586x57890143/peregrine/internal/channels"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/generate"
)

func snowflake(n int) string {
	return strconv.FormatUint((uint64(n)<<22)|1, 10)
}

type fakeGuard struct {
	mu   sync.Mutex
	sent []string
}

func (g *fakeGuard) Send(channelID, content string) (*discordgo.Message, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sent = append(g.sent, channelID+": "+content)
	return &discordgo.Message{ID: snowflake(860000 + len(g.sent))}, true
}

func (g *fakeGuard) posts() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.sent...)
}

type fakeSpeaker struct {
	reply   string
	outcome generate.Outcome
	prompts []string
	mems    []*generate.Memory
}

func (s *fakeSpeaker) Sentence(prompt string, _ bool, mem *generate.Memory, _ generate.EmojiResolver) (string, generate.Outcome, error) {
	s.prompts = append(s.prompts, prompt)
	s.mems = append(s.mems, mem)
	return s.reply, s.outcome, nil
}

type fakeChannels map[string]channels.Info

func (f fakeChannels) Channel(id string) (channels.Info, bool) {
	info, ok := f[id]
	return info, ok
}

func fixture(t *testing.T, o Options, traffic map[string]int) (*Service, *fakeGuard, *fakeSpeaker) {
	t.Helper()

	tracker := activity.New(activity.Options{})
	chans := fakeChannels{}
	for id, n := range traffic {
		chans[id] = channels.Info{ID: id, Name: id, Text: true}
		for range n {
			tracker.Note(id, snowflake(42))
		}
	}

	guard := &fakeGuard{}
	speaker := &fakeSpeaker{reply: "the bird is loose in the server"}
	s := New(guard, speaker, generate.NewMemories(0), tracker, chans, nil, o)
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s, guard, speaker
}

func always() Options {
	return Options{
		Enabled:             true,
		Tick:                10 * time.Minute,
		SkipChance:          0, // never skip, so a silence is a decision rather than a die roll
		ActiveChannelWindow: time.Hour,
	}
}

// TestItPostsInTheBusiestChannel.
func TestItPostsInTheBusiestChannel(t *testing.T) {
	s, guard, _ := fixture(t, always(), map[string]int{"quiet": 2, "loud": 20})

	s.post()

	posts := guard.posts()
	if len(posts) != 1 {
		t.Fatalf("posts = %v, want one", posts)
	}
	if got := posts[0]; got[:4] != "loud" {
		t.Errorf("posted in %q, want the busiest channel", got)
	}
}

// TestTheAllowlistRestrictsWhereItSpeaks.
//
// An empty allowlist with the feature enabled is a startup error rather than "anywhere", because
// a bot that speaks unprompted must never do so somewhere nobody named. That check is
// internal/config's; this covers the restriction being applied at all.
func TestTheAllowlistRestrictsWhereItSpeaks(t *testing.T) {
	o := always()
	o.Channels = []string{"allowed"}
	s, guard, _ := fixture(t, o, map[string]int{"allowed": 2, "busiest": 50})

	s.post()

	posts := guard.posts()
	if len(posts) != 1 || posts[0][:7] != "allowed" {
		t.Errorf("posts = %v, want the busiest ALLOWED channel", posts)
	}
}

// TestNothingIsPostedWithNowhereActive. The tracker is empty for the first window after a
// restart, and returning nothing is correct: this is the bot speaking unprompted, so "we do not
// know where people are" has to mean "not yet".
func TestNothingIsPostedWithNowhereActive(t *testing.T) {
	s, guard, _ := fixture(t, always(), nil)

	s.post()
	if posts := guard.posts(); len(posts) != 0 {
		t.Errorf("posts = %v with no active channel, want none", posts)
	}
}

// TestTheSkipChanceIsHonoured. Two dials rather than one because they answer different
// questions: the tick bounds how often the bot COULD speak, and the skip makes the rhythm
// irregular. A single longer tick would give the same rate with a metronome's regularity.
func TestTheSkipChanceIsHonoured(t *testing.T) {
	o := always()
	o.SkipChance = 1 // always skip
	s, guard, _ := fixture(t, o, map[string]int{"loud": 20})

	for range 20 {
		s.post()
	}
	if posts := guard.posts(); len(posts) != 0 {
		t.Errorf("posted %d times with a skip chance of 1", len(posts))
	}
}

// TestAnEmptyGenerationPostsNothing rather than an empty message. Returning empty is a normal
// outcome: an empty corpus, or a young one where the author-diversity gate refuses everything.
func TestAnEmptyGenerationPostsNothing(t *testing.T) {
	s, guard, speaker := fixture(t, always(), map[string]int{"loud": 20})
	speaker.reply = ""

	s.post()
	if posts := guard.posts(); len(posts) != 0 {
		t.Errorf("posts = %v for an empty generation, want silence", posts)
	}
}

// TestItGeneratesFromTheChannelsOwnMemory.
//
// The old code passed the channel's LastMessageID as the prompt seed: a snowflake, tokenized into
// a meaningless integer and fed to the generator as if somebody had said it.
func TestItGeneratesFromTheChannelsOwnMemory(t *testing.T) {
	s, _, speaker := fixture(t, always(), map[string]int{"loud": 20})

	s.post()

	if len(speaker.prompts) != 1 {
		t.Fatalf("the speaker was called %d times, want 1", len(speaker.prompts))
	}
	if speaker.prompts[0] == "" {
		t.Error("the prompt was empty")
	}
	if _, err := strconv.ParseUint(speaker.prompts[0], 10, 64); err == nil {
		t.Errorf("the prompt %q is a bare number, which is what passing a snowflake looked like",
			speaker.prompts[0])
	}
	if speaker.mems[0] == nil {
		t.Error("no conversation memory was passed, so the post has no channel context at all")
	}
}

// TestStartDoesNothingWhenDisabled, so the loop is not registered and the ticker never fires.
// This feature was dead three separate times and one of those was a flag that did not gate the
// loop it named, so it is worth asserting that this one does.
func TestStartDoesNothingWhenDisabled(t *testing.T) {
	o := always()
	o.Enabled = false
	s, guard, _ := fixture(t, o, map[string]int{"loud": 20})

	if err := s.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if posts := guard.posts(); len(posts) != 0 {
		t.Errorf("posts = %v with the feature disabled", posts)
	}
}

// TestShutdownIsSafeWithoutStart, because the registry shuts down every registered service and a
// disabled one never started a loop to cancel.
func TestShutdownIsSafeWithoutStart(t *testing.T) {
	s, _, _ := fixture(t, always(), nil)
	if err := s.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown before Start: %v", err)
	}
}

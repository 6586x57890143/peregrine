package core

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// fakeWatcher stands in for *discordgo.Session so both paths through watchReady
// are testable with no gateway connection.
type fakeWatcher struct {
	mu       sync.Mutex
	handler  func(*discordgo.Session, *discordgo.Ready)
	removed  bool
	addCalls int
}

func (f *fakeWatcher) AddHandler(handler any) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addCalls++
	if h, ok := handler.(func(*discordgo.Session, *discordgo.Ready)); ok {
		f.handler = h
	}
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.removed = true
	}
}

func (f *fakeWatcher) fire() {
	f.mu.Lock()
	h := f.handler
	f.mu.Unlock()
	if h != nil {
		h(nil, &discordgo.Ready{})
	}
}

func TestWatchReadyReturnsOnReady(t *testing.T) {
	f := &fakeWatcher{}
	wait := watchReady(f, 5*time.Second)

	// The handler must already be registered at this point. This is the property
	// that matters: discordgo starts dispatching inside Open, so a caller that
	// registered after Open could miss READY entirely and then fail startup on a
	// perfectly healthy connection.
	if f.addCalls != 1 {
		t.Fatalf("AddHandler called %d times before waiting, want 1", f.addCalls)
	}

	go f.fire()
	if err := wait(); err != nil {
		t.Fatalf("wait() = %v, want nil", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.removed {
		t.Error("the handler must be removed once the wait completes")
	}
}

// TestWatchReadyTimesOutWithADiagnosableError is the whole reason this exists.
// Open returns as soon as the identify has been sent, never that Discord accepted
// it, so a rejected identify leaves a process that looks healthy, holds no gateway
// connection, and silently does nothing.
func TestWatchReadyTimesOutWithADiagnosableError(t *testing.T) {
	f := &fakeWatcher{}
	wait := watchReady(f, 20*time.Millisecond)

	err := wait()
	if err == nil {
		t.Fatal("wait() must fail when READY never arrives")
	}
	// The error has to say what to do about it. "timeout" alone sends an operator
	// looking at their network when the cause is a checkbox in the Developer
	// Portal.
	for _, want := range []string{"MESSAGE CONTENT", "Developer Portal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q so it is actionable; got: %v", want, err)
		}
	}
}

// TestWatchReadyToleratesRepeatedReady covers reconnects. discordgo re-dispatches
// READY on every successful reconnect, and closing an already-closed channel
// panics, so this would be a crash on the first network blip rather than at
// startup.
func TestWatchReadyToleratesRepeatedReady(t *testing.T) {
	f := &fakeWatcher{}
	wait := watchReady(f, 5*time.Second)

	f.fire()
	f.fire()
	f.fire()

	if err := wait(); err != nil {
		t.Fatalf("wait() = %v, want nil", err)
	}
}

// TestNewSessionRequestsTheIntentsThatMatter pins all three. IntentsGuilds is the
// one that was missing: without it s.State.Guilds is always empty, so custom emote
// resolution had never once succeeded and the NSFW check fell back to a REST call
// on every message (SPEC.md section 8, finding 7).
func TestNewSessionRequestsTheIntentsThatMatter(t *testing.T) {
	s, err := NewSession("not-a-real-token")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	for _, want := range []struct {
		name   string
		intent discordgo.Intent
	}{
		{"IntentsGuilds", discordgo.IntentsGuilds},
		{"IntentsGuildMessages", discordgo.IntentsGuildMessages},
		{"IntentsMessageContent", discordgo.IntentsMessageContent},
	} {
		if s.Identify.Intents&want.intent == 0 {
			t.Errorf("%s is not requested", want.name)
		}
	}
}

// TestCustomEmotesResolveThroughTheStateCache is the other half of the emote story.
//
// internal/markov pins that the engine can EMIT a :shortcode:. What was never pinned is that
// the shortcode then becomes a real emote: SessionEmoji walks s.State.Guilds, and that slice was
// empty for the entire life of this bot because the session never requested IntentsGuilds, so
// the resolver had never once succeeded and peregrine had never spoken in the server's own
// emotes (SPEC.md section 8, finding 7). M3 requested the intent; nothing until M11c checked the
// code that benefits.
//
// In a meme server the server's own emotes are most of the register, which makes this the
// largest single improvement to how the output READS in the whole restructure, and it had no
// test at all.
func TestCustomEmotesResolveThroughTheStateCache(t *testing.T) {
	s := &discordgo.Session{State: discordgo.NewState()}
	if err := s.State.GuildAdd(&discordgo.Guild{
		ID: "g1",
		Emojis: []*discordgo.Emoji{
			{ID: "111111111111111111", Name: "peepohappy"},
			{ID: "222222222222222222", Name: "birdspin", Animated: true},
		},
	}); err != nil {
		t.Fatalf("GuildAdd: %v", err)
	}

	resolver := SessionEmoji(s)

	got, ok := resolver.ResolveEmoji("peepohappy")
	if !ok {
		t.Fatal("a static emote in the state cache did not resolve")
	}
	if got != "<:peepohappy:111111111111111111>" {
		t.Errorf("static emote resolved to %q", got)
	}

	got, ok = resolver.ResolveEmoji("birdspin")
	if !ok {
		t.Fatal("an animated emote in the state cache did not resolve")
	}
	if got != "<a:birdspin:222222222222222222>" {
		t.Errorf("animated emote resolved to %q, want the <a:...> form", got)
	}
}

// TestAnUnknownShortcodeIsNotResolved. A word between colons that no guild has an emote for is
// ordinary text, and the caller leaves it alone: mangling it would be worse than ignoring it,
// because the corpus is full of things people typed.
func TestAnUnknownShortcodeIsNotResolved(t *testing.T) {
	s := &discordgo.Session{State: discordgo.NewState()}
	if err := s.State.GuildAdd(&discordgo.Guild{ID: "g1"}); err != nil {
		t.Fatalf("GuildAdd: %v", err)
	}

	if got, ok := SessionEmoji(s).ResolveEmoji("notanemote"); ok {
		t.Errorf("resolved an unknown shortcode to %q", got)
	}
}

// TestTheEmojiResolverSurvivesAnEmptyState pins the fail-safe direction. A session with no state
// cache (before READY, or in a test) must report false rather than dereference nil: custom
// emotes are one optional flourish and losing them must not take a reply down.
func TestTheEmojiResolverSurvivesAnEmptyState(t *testing.T) {
	for name, s := range map[string]*discordgo.Session{
		"nil session": nil,
		"no state":    {},
		"empty state": {State: discordgo.NewState()},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := SessionEmoji(s).ResolveEmoji("peepohappy"); ok {
				t.Error("resolved an emote against a session that cannot know about one")
			}
		})
	}
}

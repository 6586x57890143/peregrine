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

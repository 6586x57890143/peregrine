package channels_test

import (
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/activity"
	"github.com/6586x57890143/peregrine/internal/channels"
)

// fakeResolver is a state cache with holes in it, because a miss is the case everything here
// has to fail closed on.
type fakeResolver map[string]channels.Info

func (f fakeResolver) Channel(id string) (channels.Info, bool) {
	info, ok := f[id]
	return info, ok
}

func text(id, name string) channels.Info {
	return channels.Info{ID: id, Name: name, Text: true}
}

func tracker(t *testing.T, traffic map[string]int) *activity.Tracker {
	t.Helper()
	tr := activity.New(activity.Options{})
	for id, n := range traffic {
		for range n {
			tr.Note("g1", id, "someone")
		}
	}
	return tr
}

// TestBusiestPicksTheChannelWithTheMostTraffic, from the tracker rather than from Discord. The
// version this replaces paged every text channel in every guild fifty messages at a time.
func TestBusiestPicksTheChannelWithTheMostTraffic(t *testing.T) {
	tr := tracker(t, map[string]int{"quiet": 2, "loud": 9})
	res := fakeResolver{"quiet": text("quiet", "quiet"), "loud": text("loud", "memes")}

	if got := channels.Busiest(tr, res, time.Hour, nil, ""); got != "loud" {
		t.Errorf("Busiest = %q, want loud", got)
	}
}

// TestBusiestFiltersWhileChoosing. The old code scored every channel, picked the winner, and
// then rejected it if it was not on the allowlist, so a bot whose busiest channel was not
// listed posted nothing and logged a rejection every single cycle.
func TestBusiestFiltersWhileChoosing(t *testing.T) {
	tr := tracker(t, map[string]int{"allowed": 3, "busiest": 30})
	res := fakeResolver{"allowed": text("allowed", "allowed"), "busiest": text("busiest", "busiest")}

	if got := channels.Busiest(tr, res, time.Hour, []string{"allowed"}, ""); got != "allowed" {
		t.Errorf("Busiest with an allowlist = %q, want the busiest ALLOWED channel", got)
	}
}

// TestBusiestSkipsWhatItCannotIdentify. This decides where the bot speaks unprompted, so a
// channel missing from the state cache has to mean "not here": the alternative is posting into
// a channel whose type and NSFW flag are unknown.
func TestBusiestSkipsWhatItCannotIdentify(t *testing.T) {
	tr := tracker(t, map[string]int{"unknown": 50, "voice": 50, "nsfw": 50, "known": 2})
	res := fakeResolver{
		// "unknown" is deliberately absent.
		"voice": {ID: "voice", Name: "voice", Text: false},
		"nsfw":  {ID: "nsfw", Name: "nsfw-memes", Text: true},
		"known": text("known", "memes"),
	}

	if got := channels.Busiest(tr, res, time.Hour, nil, ""); got != "known" {
		t.Errorf("Busiest = %q, want the only identifiable, text, safe-for-work channel", got)
	}
}

// TestBusiestIsQuietOnAColdStart. The tracker is empty for the first window after a restart.
// Returning "" is correct: falling back to the state cache's LastMessageID would offer recency
// without volume, so a channel whose last message was 59 minutes ago could decide where the bot
// starts talking unprompted.
func TestBusiestIsQuietOnAColdStart(t *testing.T) {
	tr := activity.New(activity.Options{})
	res := fakeResolver{"c1": text("c1", "general")}

	if got := channels.Busiest(tr, res, time.Hour, nil, ""); got != "" {
		t.Errorf("Busiest on a cold start = %q, want empty", got)
	}
}

// TestTheGeneralBonusIsApplied. It is a judgement about where a bot is welcome to speak
// unprompted rather than a measurement, and it lives in one place so both callers share it.
func TestTheGeneralBonusIsApplied(t *testing.T) {
	// "memes" has more traffic, but not 1.5x more.
	tr := tracker(t, map[string]int{"general": 10, "memes": 13})
	res := fakeResolver{"general": text("general", "general"), "memes": text("memes", "memes")}

	if got := channels.Busiest(tr, res, time.Hour, nil, ""); got != "general" {
		t.Errorf("Busiest = %q, want general: the bonus is 1.5x and 10*1.5 beats 13", got)
	}
}

// TestBusiestIgnoresTrafficOutsideTheWindow.
//
// Time is moved rather than slept through, and rather than shrinking the window to a
// nanosecond: Windows' clock resolution is coarse enough that timestamps recorded microseconds
// ago can compare equal, so a tiny window is not reliably in the past.
func TestBusiestIgnoresTrafficOutsideTheWindow(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tr := activity.New(activity.Options{Now: func() time.Time { return now }})
	for range 50 {
		tr.Note("g1", "stale", "someone")
	}
	res := fakeResolver{"stale": text("stale", "memes")}

	if got := channels.Busiest(tr, res, time.Hour, nil, ""); got != "stale" {
		t.Fatalf("Busiest = %q before the window moved, want stale", got)
	}
	now = now.Add(2 * time.Hour)
	if got := channels.Busiest(tr, res, time.Hour, nil, ""); got != "" {
		t.Errorf("Busiest = %q after the traffic aged out of the window, want empty", got)
	}
}

// TestAnEmptyAllowlistMeansNoRestriction, which is what an unset variable has to mean here: the
// restriction is applied by the feature that owns the allowlist, and an empty list reaching this
// function means the caller had nothing to restrict by.
func TestAnEmptyAllowlistMeansNoRestriction(t *testing.T) {
	tr := tracker(t, map[string]int{"c1": 5})
	res := fakeResolver{"c1": text("c1", "memes")}

	for _, allow := range [][]string{nil, {}, {""}} {
		if got := channels.Busiest(tr, res, time.Hour, allow, ""); got != "c1" {
			t.Errorf("Busiest with allow=%v = %q, want c1", allow, got)
		}
	}
}

// TestNotSafeForWorkChecksTheFlagAndTheName. A channel called "nsfw-memes" whose flag nobody
// set is still not somewhere to take media from or post it into.
func TestNotSafeForWorkChecksTheFlagAndTheName(t *testing.T) {
	cases := map[channels.Info]bool{
		{Name: "memes"}:                 false,
		{Name: "general"}:               false,
		{Name: "memes", NSFW: true}:     true,
		{Name: "nsfw"}:                  true,
		{Name: "NSFW-Memes"}:            true,
		{Name: "very-nsfw-stuff"}:       true,
		{Name: "not-safe", NSFW: false}: false,
	}
	for info, want := range cases {
		if got := info.NotSafeForWork(); got != want {
			t.Errorf("Info{Name:%q, NSFW:%v}.NotSafeForWork() = %v, want %v",
				info.Name, info.NSFW, got, want)
		}
	}
}

// TestTheStateResolverFailsClosed. A nil session, a session with no state cache, and a channel
// the cache has never seen all have to report "no", because both questions this package answers
// decide whether the bot publishes something.
func TestTheStateResolverFailsClosed(t *testing.T) {
	cases := map[string]*discordgo.Session{
		"nil session": nil,
		"no state":    {},
		"empty state": {State: discordgo.NewState()},
	}
	for name, session := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := channels.FromSession(session).Channel("c1"); ok {
				t.Error("resolved a channel that the state cache cannot know about")
			}
		})
	}
}

// TestTheStateResolverReadsTheCache, so the fail-closed test above is not passing because the
// resolver never works.
func TestTheStateResolverReadsTheCache(t *testing.T) {
	s := &discordgo.Session{State: discordgo.NewState()}
	if err := s.State.GuildAdd(&discordgo.Guild{ID: "g1"}); err != nil {
		t.Fatalf("GuildAdd: %v", err)
	}
	if err := s.State.ChannelAdd(&discordgo.Channel{
		ID: "c1", GuildID: "g1", Name: "memes", Type: discordgo.ChannelTypeGuildText, NSFW: true,
	}); err != nil {
		t.Fatalf("ChannelAdd: %v", err)
	}

	info, ok := channels.FromSession(s).Channel("c1")
	if !ok {
		t.Fatal("a channel in the state cache did not resolve")
	}
	if info.Name != "memes" || !info.Text || !info.NSFW {
		t.Errorf("info = %+v, want the cached channel's name, type and NSFW flag", info)
	}
}

package activity_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/6586x57890143/peregrine/internal/activity"
)

// clock is a settable time source, so the window tests move time rather than sleeping.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)}
}

func TestCountIsWindowed(t *testing.T) {
	c := newClock()
	tr := activity.New(activity.Options{Now: c.Now})

	for range 5 {
		tr.Note("chan", "author")
		c.advance(time.Minute)
	}

	if got := tr.Count("chan", 10*time.Minute); got != 5 {
		t.Errorf("Count over 10m = %d, want 5", got)
	}
	// Five messages a minute apart, then the clock ends one minute past the last, so
	// the three-minute cutoff sits exactly on the third message and excludes it: two
	// remain. The boundary being exclusive is the point of checking a number rather
	// than "some".
	if got := tr.Count("chan", 3*time.Minute); got != 2 {
		t.Errorf("Count over 3m = %d, want 2", got)
	}
	c.advance(time.Hour)
	if got := tr.Count("chan", 10*time.Minute); got != 0 {
		t.Errorf("Count over 10m after an hour of silence = %d, want 0", got)
	}
}

// TestEachConsumerBringsItsOwnWindow is why the tracker keeps timestamps rather than a
// single counter. Word games want "is it busy right now" and aggro wants "who is
// around", and those are different questions about the same stream. One counter with
// one window would have forced them to agree.
func TestEachConsumerBringsItsOwnWindow(t *testing.T) {
	c := newClock()
	tr := activity.New(activity.Options{Now: c.Now})

	tr.Note("chan", "old-timer")
	c.advance(2 * time.Hour)
	tr.Note("chan", "just-now")

	if got := tr.Count("chan", 5*time.Minute); got != 1 {
		t.Errorf("short window sees %d messages, want 1", got)
	}
	if got := tr.Count("chan", 6*time.Hour); got != 2 {
		t.Errorf("long window sees %d messages, want 2", got)
	}
	if got := tr.RecentAuthors(5 * time.Minute); len(got) != 1 || got[0] != "just-now" {
		t.Errorf("short window authors = %v, want just-now", got)
	}
	if got := tr.RecentAuthors(6 * time.Hour); len(got) != 2 {
		t.Errorf("long window authors = %v, want both", got)
	}
}

// TestBusiestIsSortedAndDeterministic. The code this replaces accumulated into a slice
// from a goroutine per channel, so the order was whichever REST call returned first,
// and one caller picked by index: the bot's choice of where to speak depended on
// network timing.
func TestBusiestIsSortedAndDeterministic(t *testing.T) {
	tr := activity.New(activity.Options{Now: newClock().Now})

	for range 3 {
		tr.Note("quiet", "u1")
	}
	for range 9 {
		tr.Note("loud", "u1")
	}
	for range 3 {
		// Same count as "quiet", so the tie has to break somewhere predictable.
		tr.Note("also-quiet", "u1")
	}

	got := tr.Busiest(time.Hour)
	want := []activity.Channel{
		{ID: "loud", Count: 9},
		{ID: "also-quiet", Count: 3},
		{ID: "quiet", Count: 3},
	}
	if len(got) != len(want) {
		t.Fatalf("Busiest = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Busiest[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestBusiestSkipsChannelsOutsideTheWindow(t *testing.T) {
	c := newClock()
	tr := activity.New(activity.Options{Now: c.Now})

	tr.Note("stale", "u1")
	c.advance(time.Hour)
	tr.Note("fresh", "u1")

	got := tr.Busiest(10 * time.Minute)
	if len(got) != 1 || got[0].ID != "fresh" {
		t.Errorf("Busiest over 10m = %v, want only fresh", got)
	}
}

// TestChannelMapIsBounded. The map is keyed by channel and grows with every guild the
// bot joins, which is the leak the conversation memory and the word-game manager each
// had to be fixed for. A test using one channel would never reveal it.
func TestChannelMapIsBounded(t *testing.T) {
	c := newClock()
	tr := activity.New(activity.Options{MaxChannels: 10, Now: c.Now})

	for i := range 100 {
		tr.Note(fmt.Sprintf("chan-%03d", i), "u1")
		c.advance(time.Second)
	}

	if got := tr.Channels(); got > 10 {
		t.Errorf("tracking %d channels against a bound of 10", got)
	}
	// The quietest go first, so the most recent survives.
	if got := tr.Count("chan-099", time.Hour); got != 1 {
		t.Errorf("the channel noted last was evicted; Count = %d, want 1", got)
	}
	if got := tr.Count("chan-000", time.Hour); got != 0 {
		t.Errorf("the oldest channel survived 100 newer ones; Count = %d, want 0", got)
	}
}

func TestAuthorMapIsBounded(t *testing.T) {
	c := newClock()
	tr := activity.New(activity.Options{MaxAuthors: 10, Now: c.Now})

	for i := range 100 {
		tr.Note("chan", fmt.Sprintf("user-%03d", i))
		c.advance(time.Second)
	}

	if got := tr.Authors(); got > 10 {
		t.Errorf("remembering %d authors against a bound of 10", got)
	}
	authors := tr.RecentAuthors(time.Hour)
	found := false
	for _, a := range authors {
		if a == "user-099" {
			found = true
		}
		if a == "user-000" {
			t.Error("the oldest author survived 100 newer ones")
		}
	}
	if !found {
		t.Errorf("the author seen last is not in %v", authors)
	}
}

// TestRingSaturatesRatherThanGrowing pins the documented trade: the per-channel
// history is a fixed ring, so a count cannot exceed PerChannel and memory cannot grow
// with traffic.
func TestRingSaturatesRatherThanGrowing(t *testing.T) {
	tr := activity.New(activity.Options{PerChannel: 8, Now: newClock().Now})

	for range 1000 {
		tr.Note("busy", "u1")
	}
	if got := tr.Count("busy", time.Hour); got != 8 {
		t.Errorf("Count = %d, want it saturated at PerChannel (8)", got)
	}
}

// TestAnEmptyAuthorIsNotACandidate. The bot's own output reaches the learn path with an
// empty author so it cannot bootstrap author diversity, and the same value arrives
// here. It must not become someone the bot can decide to bother.
func TestAnEmptyAuthorIsNotACandidate(t *testing.T) {
	tr := activity.New(activity.Options{Now: newClock().Now})

	tr.Note("chan", "")
	if got := tr.RecentAuthors(time.Hour); len(got) != 0 {
		t.Errorf("RecentAuthors = %v, want empty", got)
	}
	if got := tr.Count("chan", time.Hour); got != 1 {
		t.Errorf("the message itself should still count as traffic; Count = %d, want 1", got)
	}
}

func TestEmptyTrackerAnswersNothingRatherThanPanicking(t *testing.T) {
	tr := activity.New(activity.Options{})

	if got := tr.Count("nobody-here", time.Hour); got != 0 {
		t.Errorf("Count = %d, want 0", got)
	}
	if got := tr.Busiest(time.Hour); len(got) != 0 {
		t.Errorf("Busiest = %v, want empty", got)
	}
	if got := tr.RecentAuthors(time.Hour); len(got) != 0 {
		t.Errorf("RecentAuthors = %v, want empty", got)
	}
	tr.Note("", "someone") // an empty channel ID is ignored rather than tracked
	if got := tr.Channels(); got != 0 {
		t.Errorf("Channels = %d after noting an empty channel ID, want 0", got)
	}
}

// TestConcurrentNotesAndReads exists for CI's race detector. Every Note comes from a
// dispatcher worker and the reads come from background loops, so this is the real
// access pattern rather than a synthetic one.
func TestConcurrentNotesAndReads(t *testing.T) {
	tr := activity.New(activity.Options{MaxChannels: 16, MaxAuthors: 32})

	var wg sync.WaitGroup
	for w := range 24 {
		wg.Go(func() {
			for i := range 200 {
				tr.Note(fmt.Sprintf("chan-%d", (w+i)%40), fmt.Sprintf("user-%d", (w*i)%80))
			}
		})
	}
	for range 8 {
		wg.Go(func() {
			for range 200 {
				_ = tr.Busiest(time.Minute)
				_ = tr.RecentAuthors(time.Minute)
				_ = tr.Count("chan-1", time.Minute)
			}
		})
	}
	wg.Wait()

	if got := tr.Channels(); got > 16 {
		t.Errorf("bound broke under concurrency: tracking %d channels, want at most 16", got)
	}
	if got := tr.Authors(); got > 32 {
		t.Errorf("bound broke under concurrency: remembering %d authors, want at most 32", got)
	}
}

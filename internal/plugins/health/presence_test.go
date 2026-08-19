package health

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// fakePresence records what would have reached Discord, and can refuse like a gate would.
type fakePresence struct {
	lines  []string
	refuse func(string) bool
}

func (f *fakePresence) Presence(text string) bool {
	if f.refuse != nil && f.refuse(text) {
		return false
	}
	f.lines = append(f.lines, text)
	return true
}

type fixedTopic string

func (t fixedTopic) TopTopicWord() string { return string(t) }

func presenceFixture(t *testing.T, d Deps, p PresenceOptions) *Service {
	t.Helper()
	if d.Corpora == nil {
		d.Corpora = dbtest.Set(t)
	}
	if d.Queue == nil {
		d.Queue = &fakeQueue{}
	}
	if d.Gate == nil {
		d.Gate = &fakeGate{}
	}
	s := New(d, Options{StatusTick: time.Minute, LatencyTick: time.Minute}, p)
	var buf bytes.Buffer
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(&buf, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s
}

func populated() storage.Status {
	return storage.Status{
		Ngrams: 412033, Topics: 18442, Names: 312, NameTopics: 9021,
		ImageCache: 88, Learned: 204553,
	}
}

// The rotation cycles rather than drawing at random. A status line that shows the same fact
// twice in a row reads as a bot that has stopped updating, which is the opposite of what a
// line in the member list is for.
func TestThePresenceLineCyclesRatherThanRepeating(t *testing.T) {
	p := &fakePresence{}
	s := presenceFixture(t, Deps{Presence: p}, PresenceOptions{Enabled: true})

	st := populated()
	for range 6 {
		s.updatePresence(st)
	}

	if len(p.lines) != 6 {
		t.Fatalf("got %d presence lines, want 6: %v", len(p.lines), p.lines)
	}
	seen := map[string]bool{}
	for _, line := range p.lines {
		if seen[line] {
			t.Errorf("the rotation repeated %q inside one cycle: %v", line, p.lines)
		}
		seen[line] = true
	}

	// And it wraps rather than running out.
	s.updatePresence(st)
	if p.lines[6] != p.lines[0] {
		t.Errorf("the rotation did not wrap: %q then %q", p.lines[0], p.lines[6])
	}
}

// Numbers are grouped, because a corpus reaches seven figures and an unbroken run of digits is
// the one thing in a status line that is genuinely hard to read at a glance.
func TestThePresenceLineGroupsItsNumbers(t *testing.T) {
	p := &fakePresence{}
	s := presenceFixture(t, Deps{Presence: p}, PresenceOptions{Enabled: true})

	s.updatePresence(populated())

	if len(p.lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(p.lines))
	}
	if !strings.Contains(p.lines[0], "412,033") {
		t.Errorf("the number is not grouped: %q", p.lines[0])
	}
}

// A fact with nothing behind it is skipped, because "knows 0 people" on a fresh deploy reads
// as broken rather than as new, and an entirely empty corpus says so honestly.
func TestAnEmptyCorpusSaysSoInsteadOfReportingZeros(t *testing.T) {
	p := &fakePresence{}
	s := presenceFixture(t, Deps{Presence: p}, PresenceOptions{Enabled: true})

	for range 3 {
		s.updatePresence(storage.Status{})
	}

	for _, line := range p.lines {
		if strings.Contains(line, " 0 ") {
			t.Errorf("a zero fact was shown: %q", line)
		}
		if !strings.Contains(line, "empty corpus") {
			t.Errorf("an empty corpus did not say so: %q", line)
		}
	}

	// One real number is enough to switch back to reporting it.
	p.lines = nil
	s.updatePresence(storage.Status{Names: 3})
	if len(p.lines) != 1 || !strings.Contains(p.lines[0], "3 people") {
		t.Errorf("the one non-zero fact was not shown: %v", p.lines)
	}
}

// A corpus word on public display is user-typed text with no human context around it, so it
// goes through the emit gate like any other emission. When the gate refuses it, the line falls
// back to a count rather than freezing: a stale status is how a running bot comes to look dead.
func TestARefusedCorpusWordFallsBackToACount(t *testing.T) {
	p := &fakePresence{refuse: func(text string) bool { return strings.Contains(text, "NOPE") }}
	s := presenceFixture(t, Deps{Presence: p, Topics: fixedTopic("NOPE")},
		PresenceOptions{Enabled: true, CorpusWordChance: 1})

	s.updatePresence(populated())

	if len(p.lines) != 1 {
		t.Fatalf("got %d lines, want the count fallback: %v", len(p.lines), p.lines)
	}
	if strings.Contains(p.lines[0], "NOPE") {
		t.Errorf("the refused word was set anyway: %q", p.lines[0])
	}
	if !strings.Contains(p.lines[0], "412,033") {
		t.Errorf("the fallback is not a count: %q", p.lines[0])
	}
}

// With the chance at zero the corpus is never quoted, which is the setting for an operator who
// does not want the bot's status to be user-derived at all.
func TestAZeroChanceNeverQuotesTheCorpus(t *testing.T) {
	p := &fakePresence{}
	s := presenceFixture(t, Deps{Presence: p, Topics: fixedTopic("ratio")},
		PresenceOptions{Enabled: true, CorpusWordChance: 0})

	for range 20 {
		s.updatePresence(populated())
	}
	for _, line := range p.lines {
		if strings.Contains(line, "ratio") {
			t.Fatalf("a corpus word appeared with the chance at zero: %q", line)
		}
	}
}

// Disabled means nothing is sent at all, rather than a blank line being set.
func TestPresenceDisabledSetsNothing(t *testing.T) {
	p := &fakePresence{}
	s := presenceFixture(t, Deps{Presence: p}, PresenceOptions{Enabled: false})

	s.updatePresence(populated())

	if len(p.lines) != 0 {
		t.Errorf("presence was set with the feature off: %v", p.lines)
	}
}

// The topic source must not return a stop word, a short token, or something said twice. Any of
// those in a public status line reads as the bot quoting noise.
func TestTheCorpusTopicSourceFiltersWhatItOffers(t *testing.T) {
	set := dbtest.Set(t)
	store := dbtest.Guild(t, set, "111")

	if err := store.Update(func(w *storage.Writer) error {
		// A stop word said constantly, a short token said constantly, a rare real word, and
		// one word that should actually win.
		for range 50 {
			if err := w.IncTopic("that"); err != nil {
				return err
			}
			if err := w.IncTopic("lol"); err != nil {
				return err
			}
			if err := w.IncTopic("ratio"); err != nil {
				return err
			}
		}
		return w.IncTopic("rarely")
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	topics := CorpusTopics(set)

	// Enough draws to visit the whole (tiny) vocabulary from several random starting letters.
	got := map[string]int{}
	for range 200 {
		if word := topics.TopTopicWord(); word != "" {
			got[word]++
		}
	}

	if got["that"] > 0 {
		t.Error("a stop word was offered for the status line")
	}
	if got["lol"] > 0 {
		t.Error("a three-letter token was offered; it carries no sense on its own")
	}
	if got["rarely"] > 0 {
		t.Error("a word said once was offered; that is not what the server is about")
	}
	if got["ratio"] == 0 {
		t.Error("the one qualifying word was never offered")
	}
}

// A nil store answers nothing rather than panicking, which is what makes the whole feature
// optional.
func TestTheCorpusTopicSourceToleratesNoStore(t *testing.T) {
	if word := CorpusTopics(nil).TopTopicWord(); word != "" {
		t.Errorf("a nil store produced %q", word)
	}
}

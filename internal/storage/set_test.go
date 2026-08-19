package storage_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/6586x57890143/peregrine/internal/storage"
)

// M31: one corpus per guild.

func newSet(t *testing.T, max int) *storage.Set {
	t.Helper()
	set, err := storage.OpenSet(filepath.Join(t.TempDir(), "corpora"), max, 3)
	if err != nil {
		t.Fatalf("OpenSet: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })
	return set
}

func learn(t *testing.T, set *storage.Set, guildID, prefix, next, author string) {
	t.Helper()
	store, err := set.For(guildID)
	if err != nil {
		t.Fatalf("For(%q): %v", guildID, err)
	}
	if err := store.Update(func(w *storage.Writer) error {
		return w.LearnNgram(prefix, next, author)
	}); err != nil {
		t.Fatalf("LearnNgram: %v", err)
	}
}

// TestOneGuildsWordsNeverReachAnother is the regression test for this entire milestone.
//
// Everything else here is a property of the mechanism; this is the behaviour the mechanism
// exists for. Before M31 one corpus served every guild, so a phrase learned in one server was a
// generation candidate in somebody else's.
func TestOneGuildsWordsNeverReachAnother(t *testing.T) {
	set := newSet(t, 8)
	learn(t, set, "111", "the bird", "screams", "alice")

	other, err := set.For("222")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if err := other.View(func(r *storage.Reader) error {
		if !r.CorpusEmpty() {
			t.Error("the second guild's corpus is not empty, so learning crossed guilds")
		}
		if r.HasSuccessors("the bird") {
			t.Error("a phrase learned in one guild is generatable in another, which is the leak " +
				"M31 exists to close")
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	// And the first guild really did learn it, so the test above is not passing because nothing
	// was written anywhere.
	first, err := set.For("111")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if err := first.View(func(r *storage.Reader) error {
		if !r.HasSuccessors("the bird") {
			t.Error("the guild that was taught the phrase cannot generate it")
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestAGuildGetsItsOwnFile, which is what makes the isolation above structural rather than a
// rule somebody has to remember.
func TestAGuildGetsItsOwnFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "corpora")
	set, err := storage.OpenSet(dir, 8, 3)
	if err != nil {
		t.Fatalf("OpenSet: %v", err)
	}
	defer func() { _ = set.Close() }()

	for _, id := range []string{"111", "222"} {
		if _, err := set.For(id); err != nil {
			t.Fatalf("For(%s): %v", id, err)
		}
	}

	for _, want := range []string{"111.db", "222.db"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("no corpus file %s: %v", want, err)
		}
	}
}

// TestTheSameGuildGetsTheSameStore. bbolt takes an exclusive flock per file, so opening a
// second handle for a guild would not merely waste memory: it would block on itself for five
// seconds and then fail.
func TestTheSameGuildGetsTheSameStore(t *testing.T) {
	set := newSet(t, 8)

	first, err := set.For("111")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	second, err := set.For("111")
	if err != nil {
		t.Fatalf("For again: %v", err)
	}
	if first != second {
		t.Error("a second call opened a second handle, which would deadlock on bbolt's flock")
	}
}

// TestAMessageWithNoGuildHasNoCorpus. A DM is not learned, and there is deliberately no
// fallback file for it: a shared "no guild" corpus is the thing this split removes, and it
// would be where every threading mistake quietly drained.
//
// The mistake it guards is specific. internal/plugins/ingest and internal/plugins/repair wrap a
// REST message, whose GuildID is EMPTY, so anything reaching for m.GuildID instead of the
// threaded guild would file every backfilled message in the bot under one key.
func TestAMessageWithNoGuildHasNoCorpus(t *testing.T) {
	set := newSet(t, 8)

	if _, err := set.For(""); !errors.Is(err, storage.ErrNoGuild) {
		t.Errorf("For(\"\") = %v, want ErrNoGuild", err)
	}
}

// TestAGuildIDThatIsNotASnowflakeIsRefused. The guild ID becomes a path component, so this is a
// trust boundary and not a formality: a separator or a parent reference would put a corpus
// somewhere the operator never mounted.
func TestAGuildIDThatIsNotASnowflakeIsRefused(t *testing.T) {
	set := newSet(t, 8)

	for _, bad := range []string{"../escape", "a/b", "guild one", "12a", ".."} {
		if _, err := set.For(bad); err == nil {
			t.Errorf("For(%q) was allowed, which writes a corpus outside the directory", bad)
		}
	}
}

// TestTheCorpusCapRefusesRatherThanEvicts. This repo has shipped an unbounded per-key map
// twice, and an open corpus is far more expensive than a map entry: it is a file handle, an
// mmap and an exclusive lock. Refusing is right where evicting is not, because closing a
// corpus to make room would drop the flock on a file another goroutine may be writing.
func TestTheCorpusCapRefusesRatherThanEvicts(t *testing.T) {
	set := newSet(t, 2)

	for _, id := range []string{"111", "222"} {
		if _, err := set.For(id); err != nil {
			t.Fatalf("For(%s): %v", id, err)
		}
	}
	if _, err := set.For("333"); !errors.Is(err, storage.ErrTooManyCorpora) {
		t.Errorf("For past the cap = %v, want ErrTooManyCorpora", err)
	}
	// The ones already open still work: a refusal must not take the bot down with it.
	if _, err := set.For("111"); err != nil {
		t.Errorf("an already-open corpus stopped working after the cap was hit: %v", err)
	}
}

// TestGuildsListsCorporaOnDiskAsWellAsOpen. The fan-out loops run on tickers, and a corpus file
// is the record that the bot has learned from a guild: listing only what happens to be open
// would skip a guild until something touched it.
func TestGuildsListsCorporaOnDiskAsWellAsOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "corpora")
	set, err := storage.OpenSet(dir, 8, 3)
	if err != nil {
		t.Fatalf("OpenSet: %v", err)
	}
	if _, err := set.For("111"); err != nil {
		t.Fatalf("For: %v", err)
	}
	if err := set.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A fresh set over the same directory has nothing open at all.
	reopened, err := storage.OpenSet(dir, 8, 3)
	if err != nil {
		t.Fatalf("OpenSet: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	// Something that is not a corpus, because this directory is a mount and other files land in
	// mounts.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := reopened.Guilds()
	if len(got) != 1 || got[0] != "111" {
		t.Errorf("Guilds() = %v, want exactly [111]", got)
	}
}

// TestSingleResolvesEveryGuildToOneStore, which is what keeps the test diff of this milestone
// proportional to the behaviour it changes rather than to the number of tests that happen to
// need a corpus.
func TestSingleResolvesEveryGuildToOneStore(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "markov.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	corpora := storage.Single(store)
	first, err := corpora.For("111")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	second, err := corpora.For("222")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if first != store || second != store {
		t.Error("Single handed out something other than the store it was given")
	}
}

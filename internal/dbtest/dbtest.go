// Package dbtest gives tests a real corpus in a temporary directory.
//
// It is a normal package rather than a _test.go file, so any package's tests can
// import it, and it is imported by nothing outside _test.go files, so it never
// enters the binary (SPEC.md section 2).
//
// There is NO SKIP PATH here and there is not meant to be one. Merlin's test harness
// needs a Postgres service and skips itself when the server is unreachable, which
// means its database tests can silently not run; peregrine's corpus is an embedded
// pure-Go library, so a temp file always works and the tests run identically on a
// laptop and in CI. "These tests skipped and nobody noticed" is not a failure mode
// this package can have.
package dbtest

import (
	"path/filepath"
	"testing"

	"github.com/6586x57890143/peregrine/internal/storage"
)

// Store returns an open, empty corpus in t.TempDir, closed automatically when the
// test finishes.
//
// The temp directory is per-test, so parallel tests get separate files and cannot
// contend on bbolt's exclusive flock.
func Store(t *testing.T) *storage.Store {
	t.Helper()

	path := filepath.Join(t.TempDir(), "markov.db")
	s, err := storage.Open(path)
	if err != nil {
		t.Fatalf("open test corpus at %s: %v", path, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close test corpus: %v", err)
		}
	})
	return s
}

// Path returns a path in a temp directory for a corpus that is NOT yet created, for
// tests about Open itself.
func Path(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "markov.db")
}

// Learn records a sequence of (prefix, next, author) triples, so a test can set up a
// corpus in one call instead of six lines of transaction boilerplate.
type Learn struct {
	Prefix string
	Next   string
	Author string
}

// Seed applies every Learn in one write transaction.
func Seed(t *testing.T, s *storage.Store, entries ...Learn) {
	t.Helper()
	if err := s.Update(func(w *storage.Writer) error {
		for _, e := range entries {
			if err := w.LearnNgram(e.Prefix, e.Next, e.Author); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed corpus: %v", err)
	}
}

// Set is a directory of per-guild corpora in t.TempDir(), closed when the test ends.
//
// The per-guild counterpart of Store, and it exists for the same reason: a test that wants a
// corpus should say so in one line. Services that fan out over guilds (games, aggro, health,
// repair, backup) take the concrete *storage.Set, so they cannot be handed a single Store; a
// test that only cares about one guild's behaviour can still wrap one with storage.Single.
func Set(t *testing.T) *storage.Set {
	t.Helper()

	set, err := storage.OpenSet(filepath.Join(t.TempDir(), "corpora"), 16, 1)
	if err != nil {
		t.Fatalf("OpenSet: %v", err)
	}
	t.Cleanup(func() {
		if err := set.Close(); err != nil {
			t.Errorf("closing corpora: %v", err)
		}
	})
	return set
}

// Guild is one guild's corpus out of a Set, failing the test if it cannot be opened.
func Guild(t *testing.T, set *storage.Set, guildID string) *storage.Store {
	t.Helper()

	store, err := set.For(guildID)
	if err != nil {
		t.Fatalf("corpus for guild %s: %v", guildID, err)
	}
	return store
}

package learn

import (
	"fmt"
	"strings"
	"testing"

	"github.com/6586x57890143/peregrine/internal/storage"
)

func learned(t *testing.T, s *storage.Store) uint64 {
	t.Helper()
	var n uint64
	if err := s.View(func(r *storage.Reader) error { n = r.MessagesLearned(); return nil }); err != nil {
		t.Fatal(err)
	}
	return n
}

// invented is n words no corpus has seen, distinct per call.
func invented(seed, n int) string {
	w := make([]string, n)
	for i := range w {
		w[i] = fmt.Sprintf("zq%dx%d", seed, i)
	}
	return strings.Join(w, " ")
}

// TestAWallOfNewWordsIsNotLearned. Random-word walls grew the corpus by half a megabyte a
// message, past the container's memory and onto the disk every bot on the host shares.
func TestAWallOfNewWordsIsNotLearned(t *testing.T) {
	s, l := fixture(t)
	learnOne(t, s, l, "the bird is loose in the server again", snowflake(1), author(1))
	learnOne(t, s, l, invented(1, maxNewPerMessage+1), snowflake(2), author(1))
	if got := learned(t, s); got != 1 {
		t.Fatalf("learned %d messages, want the wall dropped", got)
	}
	learnOne(t, s, l, "the bird is back with "+invented(2, 3), snowflake(3), author(1))
	if got := learned(t, s); got != 2 {
		t.Fatalf("an ordinary message with a few new words was dropped")
	}
}

// TestAnAuthorsNewWordsAreBudgeted. In a mature corpus, one author brings at most
// maxNewPerDay new words a day; others are unaffected.
func TestAnAuthorsNewWordsAreBudgeted(t *testing.T) {
	old := matureCorpus
	matureCorpus = 0
	t.Cleanup(func() { matureCorpus = old })
	s, l := fixture(t)
	per := maxNewPerMessage
	sent := 0
	for i := range maxNewPerDay/per + 3 {
		learnOne(t, s, l, invented(10+i, per), snowflake(100+i), author(1))
		sent++
	}
	if got := learned(t, s); got != uint64(maxNewPerDay/per) {
		t.Fatalf("learned %d of %d, want %d within the day's allowance", got, sent, maxNewPerDay/per)
	}
	learnOne(t, s, l, invented(99, 5), snowflake(999), author(2))
	if got := learned(t, s); got != uint64(maxNewPerDay/per)+1 {
		t.Fatal("another author was held to the first one's allowance")
	}
}

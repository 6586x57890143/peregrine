package wordgame

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDrawExcludesPrevious100Games(t *testing.T) {
	words := make([]string, 140)
	for i := range words {
		words[i] = fmt.Sprintf("word%03d", i)
	}
	m := NewManager(dictOf(words...), seeded(1, 2), nil, testOpts())
	last := map[string]int{}
	for round := range 2000 {
		g, err := m.Start("channel")
		if err != nil {
			t.Fatal(err)
		}
		if previous, ok := last[g.Word]; ok && round-previous <= 100 {
			t.Fatalf("round %d repeats %q from round %d", round, g.Word, previous)
		}
		last[g.Word] = round
		if _, won := m.Guess("channel", g.Word); !won {
			t.Fatal("could not finish game")
		}
	}
	if len(m.recent["channel"]) != 100 {
		t.Fatalf("history has %d words, want 100", len(m.recent["channel"]))
	}
}

func TestSmallDictionariesReuseOldestWord(t *testing.T) {
	for _, words := range [][]string{
		{"banana"},
		{"banana", "orange", "cherry"},
		{"banana", "BANANA", "orange", "cherry"},
	} {
		t.Run(strings.Join(words, ","), func(t *testing.T) {
			m := NewManager(dictOf(words...), seeded(1, 2), nil, testOpts())
			last := map[string]int{}
			unique := map[string]bool{}
			for _, word := range words {
				unique[strings.ToLower(word)] = true
			}
			for round := range 300 {
				g, err := m.Start("channel")
				if err != nil {
					t.Fatal(err)
				}
				word := strings.ToLower(g.Word)
				if previous, ok := last[word]; ok && round-previous != len(unique) {
					t.Fatalf("round %d repeats %q from round %d before cycling dictionary", round, word, previous)
				}
				last[word] = round
				m.Guess("channel", word)
			}
		})
	}
}

func TestPlantedWordsCountTowardChannelHistory(t *testing.T) {
	m := NewManager(dictOf("banana", "orange"), seeded(1, 2), nil, testOpts())
	for range 2 {
		if _, err := m.StartWord("first", "BANANA"); err != nil {
			t.Fatal(err)
		}
		m.Guess("first", "banana")
	}
	g, err := m.Start("first")
	if err != nil {
		t.Fatal(err)
	}
	if g.Word != "orange" {
		t.Fatalf("drew %q after planted banana, want orange", g.Word)
	}
	if _, err := m.Start("first"); err != ErrGameInProgress {
		t.Fatalf("second start error = %v", err)
	}
	if len(m.recent["first"]) != 3 {
		t.Fatal("failed start changed history")
	}
	if len(m.recent["second"]) != 0 {
		t.Fatal("history leaked into another channel")
	}
}

func TestWordHistoryEvictedWithChannelCooldown(t *testing.T) {
	opts := testOpts()
	opts.MaxChannels = 2
	m := NewManager(dictOf("banana"), seeded(1, 2), nil, opts)
	now := time.Now()
	m.now = func() time.Time { return now }
	for _, channel := range []string{"first", "second", "third"} {
		if _, err := m.Start(channel); err != nil {
			t.Fatal(err)
		}
		m.Guess(channel, "banana")
		now = now.Add(time.Minute)
	}
	if len(m.recent) != 2 || m.recent["first"] != nil {
		t.Fatalf("history was not bounded with cooldowns: %v", m.recent)
	}
}

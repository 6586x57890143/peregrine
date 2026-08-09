// Package wordgame owns the scramble game and its leaderboard.
//
// It knows nothing about Discord. The Manager decides what should happen and when; the
// caller does the talking. That split is what makes the game testable without a gateway
// connection, and it is also what lets every announcement go through the send chokepoint
// in internal/discordguard rather than around it.
//
// # What this replaced, and why it needed replacing rather than fixing
//
// The old package was 220 lines with four defects that shared one cause: the game had no
// owner. State lived in a package-level map in legacy, the lifecycle lived in three
// hand-copied `go func` plus `time.Sleep` blocks, and the numbers lived wherever they
// were used.
//
//   - Every started game spawned up to THREE goroutines: one to expire it after 60
//     seconds, and one per announcement to delete it 30 seconds later. None took a
//     context, so after shutdown they woke against a closed session and logged failures
//     for a bot that had stopped. On a busy server that is an unbounded goroutine count
//     whose only bound is how often people play.
//   - `scramble` recursed when a shuffle happened to reproduce the original word, with no
//     depth limit. For a word whose letters are all the same that condition can never be
//     false, so it recursed until the stack died and took the process with it. Unreachable
//     with the shipped dictionary and reachable with an operator's, which is the worst
//     combination: it would only ever fire in production.
//   - The leaderboard's mutex was an exported field of a struct that gets JSON-marshalled
//     to be persisted, and the marshalling happened outside the lock while AddWin held
//     it. That is a data race on the map, which in Go is a fatal runtime error rather
//     than a recoverable panic.
//   - The weekly reset asked an NTP server what day it was (finding 17), so a network
//     hiccup during the one hour a week the check could fire skipped the reset entirely,
//     and the display computed the next reset from the host clock instead. Two sources of
//     truth for one date.
//
// So the lifecycle is one sweep rather than a goroutine per event, the numbers are
// configuration, the scramble cannot recurse, and the clock is the clock.
package wordgame

import (
	"bufio"
	"embed"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
)

// The dictionary is embedded rather than read from a path at runtime.
//
// It used to be loaded from the relative path "wordgames/dictionary.txt", so the bot only
// started when its working directory happened to be the repo root, and a failure was
// log.Fatalf: a missing 64 KB word list took down learning, generation, replies and
// everything else along with word games. In the container it is worse than fragile, since
// the distroless image ships only the binary and that path can never exist. Embedding
// removes the failure mode rather than handling it (SPEC.md section 8, finding 20).
//
//go:embed dictionary.txt
var embeddedDictionary embed.FS

// Dictionary is a loaded, filtered word list.
//
// A value rather than a package-level slice, which the old version used. The global was
// written by LoadDictionary and read by the game constructor, and it worked only because
// the load happened once before any reader existed: a second call at runtime would have
// been a data race. Owning it makes that structural instead of circumstantial.
type Dictionary struct {
	words []string
}

// DictionaryOptions bounds which words are usable.
//
// MinLength was a bare `len(word) > 4` inside the load loop, comparing BYTES rather than
// runes, so a five-letter word with an accented character counted as six. It matters more
// here than it looks: the same byte-versus-rune confusion in the leaderboard formatter was
// finding 26's neighbour and was fixed in M0.
type DictionaryOptions struct {
	MinLength int
	MaxLength int
}

func (o DictionaryOptions) withDefaults() DictionaryOptions {
	if o.MinLength < 3 {
		// Below three letters a scramble is not a puzzle: there are at most six
		// arrangements and one of them is the answer.
		o.MinLength = 5
	}
	if o.MaxLength < o.MinLength {
		// Long words are not harder, they are just tedious to type on a phone, and a
		// wrong guess costs the player nothing but time.
		o.MaxLength = 12
	}
	return o
}

// LoadDictionary reads the word list. An empty path uses the embedded copy, which is the
// normal case and cannot fail at runtime; a path overrides it so an operator can supply a
// custom list without a rebuild.
//
// Words are filtered on three properties, and the third is a correctness requirement
// rather than a preference:
//
//   - Length within the configured bounds, counted in RUNES.
//   - Letters only, so an entry with a space or punctuation cannot produce a puzzle whose
//     answer nobody can type.
//   - At least two DISTINCT letters. A word like "aaaaa" cannot be scrambled into
//     anything different from itself, and the old code's response to that was to recurse
//     forever. Excluding it here means the scrambler's bound is a belt to this braces
//     rather than the only thing standing between an operator's word list and a stack
//     overflow.
func LoadDictionary(path string, opts DictionaryOptions) (*Dictionary, error) {
	opts = opts.withDefaults()

	var (
		file io.ReadCloser
		err  error
	)
	if path == "" {
		file, err = embeddedDictionary.Open("dictionary.txt")
	} else {
		file, err = os.Open(path)
	}
	if err != nil {
		return nil, fmt.Errorf("open dictionary: %w", err)
	}
	defer func() { _ = file.Close() }()

	d := &Dictionary{}
	var rejected int

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		word := strings.TrimSpace(scanner.Text())
		if word == "" {
			continue
		}
		if !usable(word, opts) {
			rejected++
			continue
		}
		d.words = append(d.words, word)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read dictionary: %w", err)
	}
	if len(d.words) == 0 {
		return nil, fmt.Errorf("dictionary at %q yielded no usable words: %d entries were "+
			"rejected for length, non-letter characters, or having only one distinct letter",
			source(path), rejected)
	}
	return d, nil
}

// usable reports whether a word can be a puzzle.
func usable(word string, opts DictionaryOptions) bool {
	runes := []rune(word)
	if len(runes) < opts.MinLength || len(runes) > opts.MaxLength {
		return false
	}

	distinct := make(map[rune]struct{}, len(runes))
	for _, r := range runes {
		if !unicode.IsLetter(r) {
			return false
		}
		distinct[r] = struct{}{}
	}
	return len(distinct) >= 2
}

// Len reports how many usable words were loaded, for the startup log line.
func (d *Dictionary) Len() int {
	if d == nil {
		return 0
	}
	return len(d.words)
}

func source(path string) string {
	if path == "" {
		return "embedded"
	}
	return path
}

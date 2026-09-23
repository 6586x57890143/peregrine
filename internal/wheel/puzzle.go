package wheel

import (
	"bufio"
	"embed"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// The puzzles are embedded for the dictionary's reason: a relative path only resolves
// when the working directory is the repo root, and the distroless image has no repo root.
//
//go:embed puzzles.txt
var embeddedPuzzles embed.FS

// Puzzle is one category and one phrase. The phrase is uppercase with single spaces.
type Puzzle struct {
	Category string
	Phrase   string
}

// Puzzles is a loaded, validated puzzle list.
type Puzzles struct {
	all      []Puzzle
	bonus    []int // indexes into all that still hide three letters after RSTLNE
	rejected int
}

// Loader limits. maxWordLetters is the layout constraint: the renderer wraps the board at
// word boundaries into lines of at most that many cells, so a longer word would either
// overflow a phone screen or have to be split mid-word, and a split word reads as two.
const (
	maxCategoryRunes = 24
	maxPhraseRunes   = 52
	maxWordLetters   = 13
	minLetters       = 3
)

// bonusGiven are the letters the bonus round reveals for free.
const bonusGiven = "RSTLNE"

// LoadPuzzles reads `category|PHRASE` lines from path, or the embedded list when path is
// empty. Blank lines and lines starting with # are skipped.
//
// A malformed line is skipped and counted rather than failing the load, and Rejected
// reports the count so the operator's startup log says so. Only a list with no usable
// puzzle at all is an error, and the caller turns that into the feature being off rather
// than the bot being down, which is the dictionary's rule: one feature failing disables
// that feature.
func LoadPuzzles(path string) (*Puzzles, error) {
	var (
		r    io.ReadCloser
		err  error
		name = path
	)
	if path == "" {
		name = "embedded puzzles.txt"
		r, err = embeddedPuzzles.Open("puzzles.txt")
	} else {
		r, err = os.Open(path)
	}
	if err != nil {
		return nil, fmt.Errorf("wheel: open puzzles %s: %w", name, err)
	}
	defer func() { _ = r.Close() }()

	p, err := parse(r)
	if err != nil {
		return nil, fmt.Errorf("wheel: read puzzles %s: %w", name, err)
	}
	if len(p.all) == 0 {
		return nil, fmt.Errorf("wheel: %s has no usable puzzles (%d rejected)", name, p.rejected)
	}
	return p, nil
}

// parse reads the lines, keeping what validates and counting what does not.
func parse(r io.Reader) (*Puzzles, error) {
	p := &Puzzles{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pz, ok := parseLine(line)
		if !ok {
			p.rejected++
			continue
		}
		if bonusEligible(pz.Phrase) {
			p.bonus = append(p.bonus, len(p.all))
		}
		p.all = append(p.all, pz)
	}
	return p, sc.Err()
}

// Len is the number of usable puzzles. It is nil-safe.
func (p *Puzzles) Len() int {
	if p == nil {
		return 0
	}
	return len(p.all)
}

// Rejected is the number of lines skipped as malformed. It is nil-safe.
func (p *Puzzles) Rejected() int {
	if p == nil {
		return 0
	}
	return p.rejected
}

// parseLine validates one line. The phrase is uppercased and its spaces collapsed, so the
// file can be written naturally.
func parseLine(line string) (Puzzle, bool) {
	cat, phrase, ok := strings.Cut(line, "|")
	if !ok || strings.Contains(phrase, "|") {
		return Puzzle{}, false
	}
	cat = strings.Join(strings.Fields(cat), " ")
	if cat == "" || utf8.RuneCountInString(cat) > maxCategoryRunes {
		return Puzzle{}, false
	}
	for _, r := range strings.ToUpper(cat) {
		if !allowed(r) {
			return Puzzle{}, false
		}
	}

	words := strings.Fields(strings.ToUpper(phrase))
	phrase = strings.Join(words, " ")
	if phrase == "" || utf8.RuneCountInString(phrase) > maxPhraseRunes {
		return Puzzle{}, false
	}
	letters, consonant := 0, false
	for _, w := range words {
		n := 0
		for _, r := range w {
			if !allowed(r) {
				return Puzzle{}, false
			}
			if isLetter(r) {
				n++
				consonant = consonant || !isVowel(r)
			}
		}
		if n > maxWordLetters {
			return Puzzle{}, false
		}
		letters += n
	}
	// Without a consonant the wheel has nothing to pay for, and a spin would be refused on
	// the first turn of the round.
	if letters < minLetters || !consonant {
		return Puzzle{}, false
	}
	return Puzzle{Category: cat, Phrase: phrase}, true
}

// allowed is the phrase alphabet. Everything except A-Z is shown from the start. An
// underscore is deliberately absent, because the board renders a hidden cell as one.
func allowed(r rune) bool {
	return isLetter(r) || (r >= '0' && r <= '9') || strings.ContainsRune(" '-&.,!?:", r)
}

func isLetter(r rune) bool { return r >= 'A' && r <= 'Z' }

func isVowel(r rune) bool { return strings.ContainsRune("AEIOU", r) }

// bonusEligible reports whether a phrase still hides at least three distinct letters once
// RSTLNE are given. With fewer, the bonus round is solved before it is played.
func bonusEligible(phrase string) bool {
	hidden := map[rune]bool{}
	for _, r := range phrase {
		if isLetter(r) && !strings.ContainsRune(bonusGiven, r) {
			hidden[r] = true
		}
	}
	return len(hidden) >= 3
}

// draw picks a puzzle this match has not used, from the bonus-eligible set when bonus is
// true. When every candidate has been used it falls back to any candidate rather than
// failing, so a short operator list repeats instead of hanging.
func (p *Puzzles) draw(src Source, used map[int]bool, bonus bool) int {
	pool := p.bonus
	if !bonus || len(pool) == 0 {
		pool = make([]int, len(p.all))
		for i := range pool {
			pool[i] = i
		}
	}
	fresh := make([]int, 0, len(pool))
	for _, i := range pool {
		if !used[i] {
			fresh = append(fresh, i)
		}
	}
	if len(fresh) == 0 {
		fresh = pool
	}
	return fresh[src.IntN(len(fresh))]
}

// normalizeSolve reduces a guess or a phrase to its letters and digits, uppercased. Both
// sides go through it, so case, spacing, hyphens and apostrophes never decide a solve,
// including the curly apostrophe a phone keyboard substitutes as you type.
func normalizeSolve(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if isLetter(r) || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// parseLetter accepts exactly one letter, in either case, with surrounding space.
func parseLetter(s string) (rune, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if utf8.RuneCountInString(s) != 1 {
		return 0, false
	}
	r, _ := utf8.DecodeRuneInString(s)
	return r, isLetter(r)
}

package wordgame

import "strings"

// The hint ladder.
//
// M21b's hint was one rung: the first letter, plus the letter count. That helps a stuck channel
// and does nothing else, because a single reveal cannot express degrees of help. An ordered
// ladder can, and once the rungs are ordered they are also a price: the caller pays fewer points
// the further down the ladder a solve lands, which is the only decision this game has ever
// offered a player. internal/plugins/games is where that price is applied.
//
// Nothing here does I/O or knows what a point is, in keeping with the rest of the package.

// hidden is what an unrevealed letter renders as.
//
// Escaped, because an underscore is italic markup in Discord and a mask is mostly underscores.
// Spaced runs happen not to trigger it today, which is exactly the kind of reading this repo
// declines to depend on: the escape costs one byte and does not depend on how a parser treats
// whitespace.
const hidden = `\_`

// revealOrder is the order positions are uncovered in, with the opening letter always first.
//
// Computed ONCE per game and stored on it, and that is what makes rung k a strict superset of
// rung k-1. Drawing positions afresh at each rung would let a letter shown at rung 2 disappear
// again at rung 3, and a hint that takes something back is worse than no hint at all.
//
// The opening letter leads because it is the one people use. It is what M21b's single hint
// revealed, and knowing a word starts with "s" prunes the search in a way that knowing its
// seventh letter does not.
func revealOrder(src Source, n int) []int {
	if n <= 0 {
		return nil
	}
	rest := make([]int, 0, n-1)
	for i := 1; i < n; i++ {
		rest = append(rest, i)
	}
	src.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	return append([]int{0}, rest...)
}

// ladder is how many letters each rung reveals, ascending and strictly increasing.
//
// ceil(length*k / 2*levels), so the top rung of a long word shows about half of it, and a rung
// shows more of a long word than of a short one at the same height. That scaling is the point:
// a twelve-letter word is harder than a five-letter one and needs a bigger concession to stay
// solvable, and this is how one configured rung count serves both.
//
// Two bounds, and both exist because a rung nobody gains from should not be charged for. At
// least two letters stay hidden however far the ladder goes, so the last rung is never the
// answer; and a count that does not beat the rung below it is dropped, so a short word gets a
// SHORTER ladder rather than rungs that reveal nothing. A game's real rung count is therefore
// len(ladder) rather than the configured number, which is why the schedule divides by that.
func ladder(length, levels int) []int {
	if length <= 0 || levels <= 0 {
		return nil
	}
	most := length - 2
	if most < 1 {
		return nil
	}

	var out []int
	for k := 1; k <= levels; k++ {
		// Integer ceiling of length*k / (2*levels).
		n := min((length*k+2*levels-1)/(2*levels), most)
		if len(out) > 0 && n <= out[len(out)-1] {
			continue
		}
		out = append(out, n)
	}
	return out
}

// mask renders the word with only the first count positions of the reveal order showing.
//
// Spaced, because an unspaced run of underscores is uncountable at a glance and the letter count
// is half of what a hint conveys: "s _ _ o _ a _ _ _ _" says the word has ten letters as clearly
// as it says which two are known.
func mask(word string, reveal []int, count int) string {
	runes := []rune(word)
	if len(runes) == 0 {
		return ""
	}

	show := make([]bool, len(runes))
	for i := 0; i < count && i < len(reveal); i++ {
		if p := reveal[i]; p >= 0 && p < len(show) {
			show[p] = true
		}
	}

	parts := make([]string, len(runes))
	for i, r := range runes {
		if show[i] {
			parts[i] = string(r)
			continue
		}
		parts[i] = hidden
	}
	return strings.Join(parts, " ")
}

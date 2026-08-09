package wordgame

import "math/rand/v2"

// Source is the randomness the game draws on, matching internal/markov's convention so
// the two packages do not disagree about how to be testable.
//
// A parameter rather than a package-level generator, for the reason recorded in M3: there
// used to be a shared *rand.Rand seeded in init and reached from the per-message handler,
// so two players triggering a game at once was a data race on the generator's internal
// state (SPEC.md finding 3). DefaultSource holds nothing, so production has nothing to
// race on, and a test can inject a seeded generator and get the same puzzle twice.
type Source interface {
	IntN(n int) int
	Shuffle(n int, swap func(i, j int))
}

// DefaultSource is the production randomness: a stateless adapter over math/rand/v2's
// goroutine-safe top-level functions.
type DefaultSource struct{}

func (DefaultSource) IntN(n int) int                     { return rand.IntN(n) }
func (DefaultSource) Shuffle(n int, swap func(i, j int)) { rand.Shuffle(n, swap) }

// maxScrambleAttempts bounds the search for an arrangement that differs from the original.
//
// The old implementation RECURSED on a shuffle that happened to reproduce the word, with
// no depth limit, so a word whose letters are all identical recursed until the stack died
// and took the process with it. LoadDictionary now excludes such words, which makes this
// bound belt to that braces: two independent reasons the crash cannot happen, because one
// of them depends on the operator's word list and the other does not.
//
// Ten is generous. For a word with at least two distinct letters the chance of a shuffle
// reproducing it exactly is at most one in two, so ten attempts fail with probability
// under one in a thousand, and the fallback below is still a valid puzzle.
const maxScrambleAttempts = 10

// scramble returns the letters of a word in a different order.
//
// If every attempt reproduces the original, it returns a deterministic rotation instead of
// giving up or looping. A rotation of a word with two distinct letters is ALWAYS different
// from the original, which is what makes this a real fallback rather than a shrug: it
// cannot fail for any input LoadDictionary admits.
func scramble(src Source, word string) string {
	runes := []rune(word)
	if len(runes) < 2 {
		return word
	}

	for range maxScrambleAttempts {
		shuffled := make([]rune, len(runes))
		copy(shuffled, runes)
		src.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		if string(shuffled) != word {
			return string(shuffled)
		}
	}

	return rotate(runes)
}

// rotate returns the word with its letters shifted by the smallest amount that changes it.
//
// It tries every offset rather than assuming one works, because a word like "abab" is
// unchanged by a rotation of two. Returning the input unchanged is only possible for a
// word with a single distinct letter, which LoadDictionary refuses.
func rotate(runes []rune) string {
	original := string(runes)
	for offset := 1; offset < len(runes); offset++ {
		rotated := make([]rune, 0, len(runes))
		rotated = append(rotated, runes[offset:]...)
		rotated = append(rotated, runes[:offset]...)
		if string(rotated) != original {
			return string(rotated)
		}
	}
	return original
}

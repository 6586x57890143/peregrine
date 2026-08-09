package markov

import "math/rand/v2"

// DefaultSource is the production randomness: a stateless adapter over
// math/rand/v2's top-level functions, which are goroutine-safe and auto-seeded.
//
// Stateless is the point. The previous engine shared one *rand.Rand across every
// message goroutine, the aggro ticker, the autonomous poster and the image reposter,
// which is a data race on the hot path (SPEC.md finding 3). M3 fixed it by having no
// shared generator rather than by locking one, and this type keeps that true while
// still letting tests inject a seeded generator for reproducible golden samples. A
// zero value works, so there is nothing to construct and nothing to seed.
type DefaultSource struct{}

func (DefaultSource) Float64() float64 { return rand.Float64() }
func (DefaultSource) IntN(n int) int   { return rand.IntN(n) }

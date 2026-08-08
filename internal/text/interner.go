package text

// Interner maps tokens to small integer ids and back.
//
// It exists because the generation code keys its candidate maps and cluster
// membership sets by int rather than string, which is worth doing: those maps are
// rebuilt for every token of every generated sentence, and int keys avoid both
// the hashing cost and the allocation.
//
// What it replaces is the reason it is a type at all. There used to be a
// package-level `vocab map[string]int` plus a `revVocab []string`, written by
// every per-message goroutine through a helper with no synchronization at all.
// Two consequences, one fatal:
//
//   - Concurrent map write is not a panic in Go, it is a runtime fatal error.
//     No recover, no unwind, no deferred database close, process gone. Under load
//     this was the single most likely way for the bot to die (SPEC.md section 8,
//     finding 2).
//   - It was never pruned, so it grew for the life of the process, holding a copy
//     of every distinct token the bot had ever seen.
//
// An Interner is created per generation call and discarded with it, which fixes
// both at once and needs no lock: ids only have to be consistent within the one
// call that uses them, because nothing persists them.
//
// That last point is worth being explicit about, because it looks like a
// limitation and is not. Nothing may persist an Interner id. Ids depend on
// insertion order, so an id written to the database is meaningful only to the
// process that wrote it, and stops being meaningful on the next restart. The
// clustering package already learned this the hard way: it converts ids back to
// strings before writing (see SPEC.md section 8, finding 27 for what happens when
// the two ends of that conversion disagree).
//
// Not safe for concurrent use, deliberately. Sharing one is the bug this type
// exists to prevent, so it carries no mutex to make sharing look supported. Give
// each goroutine its own.
type Interner struct {
	ids   map[string]int
	words []string
}

// NewInterner returns an empty Interner. The zero value is not usable.
func NewInterner() *Interner {
	return &Interner{ids: make(map[string]int)}
}

// Intern returns the id for a token, assigning one if it is new.
func (in *Interner) Intern(token string) int {
	if id, ok := in.ids[token]; ok {
		return id
	}
	id := len(in.words)
	in.ids[token] = id
	in.words = append(in.words, token)
	return id
}

// ID returns an existing id without assigning one. The bool reports whether the
// token was known.
//
// Separate from Intern because the callers that look a prompt word up in a
// cluster want to know whether it is present, and interning it there would insert
// a token the caller then has to treat as absent anyway.
func (in *Interner) ID(token string) (int, bool) {
	id, ok := in.ids[token]
	return id, ok
}

// Word returns the token for an id. It returns the empty string for an id this
// Interner never issued, rather than panicking on an out-of-range index, because
// the alternative is a bot that crashes on a stale id instead of emitting one
// slightly worse sentence.
func (in *Interner) Word(id int) string {
	if id < 0 || id >= len(in.words) {
		return ""
	}
	return in.words[id]
}

// Len reports how many distinct tokens have been interned.
func (in *Interner) Len() int { return len(in.words) }

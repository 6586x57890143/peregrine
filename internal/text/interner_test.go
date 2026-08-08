package text

import (
	"sync"
	"testing"
)

func TestInternerAssignsStableIDs(t *testing.T) {
	in := NewInterner()

	a := in.Intern("bird")
	b := in.Intern("cheese")
	again := in.Intern("bird")

	if a != again {
		t.Errorf("Intern is not stable: %d then %d for the same token", a, again)
	}
	if a == b {
		t.Errorf("distinct tokens got the same id %d", a)
	}
	if in.Len() != 2 {
		t.Errorf("Len() = %d, want 2", in.Len())
	}
	if got := in.Word(a); got != "bird" {
		t.Errorf("Word(%d) = %q, want %q", a, got, "bird")
	}
}

// TestInternerIDDoesNotInsert is the reason ID exists separately from Intern. The
// callers that look a prompt word up in a cluster want to know whether it is
// present; interning it there would insert a token the caller must then treat as
// absent anyway, and would grow the interner with every miss.
func TestInternerIDDoesNotInsert(t *testing.T) {
	in := NewInterner()

	if _, ok := in.ID("unseen"); ok {
		t.Error("ID reported a token the interner never saw")
	}
	if in.Len() != 0 {
		t.Errorf("ID inserted a token: Len() = %d, want 0", in.Len())
	}

	in.Intern("seen")
	if id, ok := in.ID("seen"); !ok || in.Word(id) != "seen" {
		t.Errorf("ID(%q) = %d, %v after interning", "seen", id, ok)
	}
}

// TestInternerWordOutOfRange pins that a stale id degrades to an empty string
// rather than panicking. The alternative is a bot that crashes on a bad index
// instead of emitting one slightly worse sentence.
func TestInternerWordOutOfRange(t *testing.T) {
	in := NewInterner()
	in.Intern("only")

	for _, id := range []int{-1, 1, 99999} {
		if got := in.Word(id); got != "" {
			t.Errorf("Word(%d) = %q, want the empty string", id, got)
		}
	}
}

// TestInternersAreIndependent is the property that makes per-call interning safe.
// Two interners assign ids independently, which is exactly why an id must never be
// persisted: written to the database by one process it means something else to the
// next. See the comment on Interner, and finding 27 for what happened when the two
// ends of that conversion disagreed.
func TestInternersAreIndependent(t *testing.T) {
	first := NewInterner()
	second := NewInterner()

	first.Intern("alpha")
	idInFirst := first.Intern("beta")

	// second sees the same tokens in the opposite order.
	second.Intern("beta")
	second.Intern("alpha")

	if first.Word(idInFirst) != "beta" {
		t.Fatal("setup wrong")
	}
	if second.Word(idInFirst) == "beta" {
		t.Error("ids happened to agree across interners; the test needs a stronger case, " +
			"but the guarantee being documented is that they need not agree")
	}
}

// TestInternerPerGoroutineIsRaceFree is the regression pin for finding 2, and it
// only means anything under -race, which CI runs.
//
// The bug it replaces was a package-level map written by every per-message
// goroutine with no synchronization. A concurrent map write in Go is a runtime
// FATAL ERROR, not a panic: no recover, no unwind, no deferred database close, the
// process is simply gone. This asserts the shape that prevents it, which is that
// each goroutine owns its own Interner.
func TestInternerPerGoroutineIsRaceFree(t *testing.T) {
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			in := NewInterner() // one per goroutine: the whole point
			for i := range 500 {
				in.Intern(string(rune('a'+g)) + string(rune('0'+i%10)))
			}
			if in.Len() == 0 {
				t.Error("interner recorded nothing")
			}
		}(g)
	}
	wg.Wait()
}

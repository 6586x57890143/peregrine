package legacy

import (
	"log"

	"github.com/6586x57890143/peregrine/internal/filter"
)

// These are thin wrappers over internal/filter, kept so the call sites in this
// package did not all have to change in the same commit that moved the logic out.
// They exist for exactly one reason beyond the rename: internal/filter is a leaf
// and does no logging, returning a reason string instead, and the log lines these
// produce are the ones an operator already recognizes. M5 replaces all of them
// with safety.CheckLearn and safety.CheckEmit at the two chokepoints, and then
// these go away.

func isSpammyContent(s string) bool {
	spam, reason := filter.Spam(s)
	if spam {
		log.Printf("[FILTER] Message blocked as spam: %s", reason)
	}
	return spam
}

func filterIllegalContent(content string) bool {
	blocked, category := filter.Illegal(content)
	if blocked {
		log.Printf("[FILTER] Message BLOCKED due to sensitive content category: %s", category)
	}
	return blocked
}

func filterSlurs(content string) string { return filter.ReplaceSlurs(content) }

func containsSlur(content string) bool { return filter.ContainsSlur(content) }

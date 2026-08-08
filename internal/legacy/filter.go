package legacy

import (
	"log"

	"github.com/6586x57890143/peregrine/internal/filter"
)

// What remains here is what the safety gate does not cover, and the shrinkage is
// the point.
//
// M4 left four thin wrappers over internal/filter. M5 replaced two of them:
// filterIllegalContent and filterSlurs had no callers left once CheckLearn ran
// inside learnMessage and CheckEmit ran at the generation exit, and the linter
// reporting them as unused is how that was confirmed rather than assumed. The
// laundering wrapper in particular is gone for good: replacement must never touch
// the learning path, and after the gate exists a launder before the gate would
// actively defeat it (SPEC.md section 4, A5).
//
// The two survivors are both called from -clean-db, which runs against the corpus
// with no Service, no config and no gate, so it cannot use safety.CheckLearn. M6
// replaces that pass entirely.

// isSpammyContent is used by the -clean-db pass, and by the trim in learnMessage's
// caller. internal/filter does no logging, so the log line lives here.
func isSpammyContent(s string) bool {
	spam, reason := filter.Spam(s)
	if spam {
		log.Printf("[FILTER] Message blocked as spam: %s", reason)
	}
	return spam
}

// containsSlur is used by the -clean-db pass to decide which existing keys to
// remove. It matches raw text, which is adequate here and nowhere else: it is
// cleaning a corpus that was written before the normalizer existed, so the entries
// it finds are in whatever form they were learned in.
func containsSlur(content string) bool { return filter.ContainsSlur(content) }

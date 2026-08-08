// Package maintenance holds the modes that operate on the corpus and never touch
// Discord.
//
// Each takes an already-open *storage.Store rather than a path, so cmd/bot owns the
// open and the close and there is exactly one place that decides where the corpus
// lives. The previous arrangement had -clean-db resolve PEREGRINE_DB_PATH through its
// own helper, which meant two code paths independently decided that, and nothing
// guaranteed they agreed. Cleaning the wrong database succeeds, reports a tidy
// summary, and changes nothing the operator cares about, which is the most misleading
// outcome available.
package maintenance

import (
	"fmt"
	"log/slog"

	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/filter"
	"github.com/6586x57890143/peregrine/internal/safety"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// CleanResult summarizes a pass.
type CleanResult struct {
	Scanned int
	Removed int
}

// Clean removes n-grams whose prefix or continuation trips the filters.
//
// It is the retroactive remedy: the gates stop new content entering the corpus, and
// this removes what got in before they existed or before a pattern was added to the
// blocklist. Both are needed, because adding a blocklist pattern does not retroact.
//
// Matching happens against the NORMALIZED form, unlike the pre-M6 version which
// matched raw text. That matters more here than on the live path: this is cleaning a
// corpus written before the normalizer existed, so the entries in it are in whatever
// spelling they were learned in, including the evaded ones.
//
// The KN indexes are rebuilt at the end rather than decremented per deletion.
// Decrementing N1+(. token) correctly requires knowing whether any other context
// still precedes that token, which is a scan per deleted key; one rebuild is both
// cheaper and provably consistent.
func Clean(store *storage.Store, gate *safety.Gate, log *slog.Logger) (CleanResult, error) {
	var res CleanResult

	// Collect first, delete after. bbolt forbids structural mutation of a bucket
	// while a cursor over it is live.
	type victim struct{ prefix, next string }
	var victims []victim

	if err := store.View(func(r *storage.Reader) error {
		return r.ForEachNgram(func(prefix, next string, _ corpus.Successor) error {
			res.Scanned++
			if suspect(gate, prefix) || suspect(gate, next) {
				victims = append(victims, victim{prefix, next})
			}
			return nil
		})
	}); err != nil {
		return res, fmt.Errorf("scan corpus: %w", err)
	}

	log.Info("clean pass scanned corpus", "ngrams", res.Scanned, "to_remove", len(victims))
	if len(victims) == 0 {
		return res, nil
	}

	if err := store.Update(func(w *storage.Writer) error {
		for _, v := range victims {
			if err := w.DeleteNgram(v.prefix, v.next); err != nil {
				return fmt.Errorf("delete %q -> %q: %w", v.prefix, v.next, err)
			}
			res.Removed++
		}
		// Consistency is not optional here: leaving the distinct counts describing
		// n-grams that no longer exist would make the Kneser-Ney lambda term wrong for
		// every prefix that lost a successor, which silently skews generation rather
		// than failing.
		return w.RebuildKNIndexes()
	}); err != nil {
		return res, fmt.Errorf("clean corpus: %w", err)
	}

	log.Info("clean pass complete", "scanned", res.Scanned, "removed", res.Removed)
	return res, nil
}

// suspect reports whether a stored fragment should be removed.
//
// It uses the emit-side view deliberately. A corpus entry is a thing the bot might
// SAY, so the question is whether emitting it would be acceptable, not whether it
// would have been acceptable to learn. The spam-shape check is skipped for the same
// reason: a two-word n-gram fragment fails a length-and-character-ratio test designed
// for whole messages, so applying it here would delete most of the corpus.
func suspect(gate *safety.Gate, fragment string) bool {
	if fragment == "" {
		return false
	}
	normalized := safety.Normalize(fragment)
	if filter.ContainsSlur(normalized) {
		return true
	}
	if illegal, _ := filter.Illegal(normalized); illegal {
		return true
	}
	if gate == nil {
		return false
	}
	return gate.BlocklistMatches(normalized)
}

// PurgeAuthor removes one author's contribution to author-diversity counts.
//
// The surgical alternative to discarding the corpus when one actor has poisoned it.
// See storage.Writer.PurgeAuthor for exactly what it does and does not remove.
func PurgeAuthor(store *storage.Store, authorID string, log *slog.Logger) (int, error) {
	var removed int
	if err := store.Update(func(w *storage.Writer) error {
		var err error
		removed, err = w.PurgeAuthor(authorID)
		return err
	}); err != nil {
		return removed, fmt.Errorf("purge author %s: %w", authorID, err)
	}
	log.Info("purged author from diversity counts", "author", authorID, "entries", removed)
	return removed, nil
}

// Compact copies the corpus into a fresh file, reclaiming free pages.
//
// It exists because bbolt's file never shrinks: deleting keys frees pages for reuse
// but does not return them to the filesystem. So a corpus that grew large through the
// old layout's write amplification stays large after Clean removes most of it, and
// this is the only way to get the space back.
//
// It writes to a new path rather than in place, because compaction is a copy and an
// in-place version would mean deleting the original before the copy is known good.
// The operator moves it into place, which keeps the rollback trivial.
func Compact(store *storage.Store, destination string, log *slog.Logger) error {
	if err := store.Compact(destination); err != nil {
		return err
	}
	log.Info("compacted corpus written", "source", store.Path(), "destination", destination,
		"next", "stop the bot, replace the original with this file, and restart")
	return nil
}

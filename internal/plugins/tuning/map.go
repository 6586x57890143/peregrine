package tuning

import (
	"strings"

	"github.com/6586x57890143/peregrine/internal/markov"
	"github.com/6586x57890143/peregrine/internal/tuning"
)

// The mapping from the engine's types onto the wire format lives here, in one file, and it
// is deliberately mechanical.
//
// internal/tuning's package comment explains why the wire types are duplicates rather than
// embeds: an archive has to stay readable after the engine has moved on, so a field
// disappearing from markov.Weights must be a decision made here rather than a silent change
// to the shape of files nobody can regenerate. The cost is this file. The alternative cost
// is not noticing.

// flatten turns a generation trace into the wire form, or nil.
func flatten(t *markov.Trace) *tuning.Trace {
	if t == nil {
		return nil
	}
	return &tuning.Trace{
		SeedTier:    t.SeedTier,
		SeedKey:     t.SeedKey,
		Attempts:    t.Attempts,
		Steps:       t.Steps,
		Jumps:       t.Jumps,
		DeadEnds:    t.DeadEnds,
		GateRefused: t.GateRefused,
		Starved:     t.Starved,
		MinOrder:    t.MinOrder,
		// Scaled to an integer so the wire format carries no float, which keeps a hand-read
		// line legible and a diff between two archives exact.
		CandidatesX100: int(t.MeanCandidates() * 100),
		ChoseEnd:       t.ChoseEnd,
	}
}

// params is the operator's half of the configuration: everything settable from the
// environment that generation reads.
//
// A map rather than a struct, because these are read by an analysis that compares two
// archives key by key and a struct would need a matching type on the reading side for
// every version that ever shipped.
func (s *Service) params() map[string]float64 {
	d := s.opts.Dials
	return map[string]float64{
		"max_ngram":            float64(d.MaxNGram),
		"min_words":            float64(d.MinWords),
		"max_words":            float64(d.MaxWords),
		"temperature":          d.Temperature,
		"top_k":                float64(d.TopK),
		"top_p":                d.TopP,
		"kn_discount":          d.KNDiscount,
		"kn_raw_mix":           d.KNRawMix,
		"min_distinct_authors": float64(d.MinDistinctAuthors),
		"prompt_relevance":     d.PromptRelevance,
		"roast_chance":         d.RoastChance,
	}
}

// weights is the half an operator CANNOT see from a .env, which is why exporting it matters
// more than exporting the params above.
//
// The logit coefficients are constants in internal/markov on purpose: they are only
// meaningful relative to each other and an operator has no instrument to judge them. That
// makes them invisible to anybody reading a deployment, so an archive that did not carry
// them would record output whose cause cannot be reconstructed. This is the file where the
// numbers SPEC.md section 10 calls "a considered first guess, not a measurement" finally sit
// next to the output they produced.
func (s *Service) weights() map[string]float64 {
	w := markov.DefaultWeights()
	return map[string]float64{
		"topic_gravity":     w.TopicGravity,
		"name_topic":        w.NameTopic,
		"name_assoc":        w.NameAssoc,
		"current_topic":     w.CurrentTopic,
		"significance":      w.Significance,
		"is_name":           w.IsName,
		"prompt_name":       w.PromptName,
		"persona":           w.Persona,
		"recent_context":    w.RecentContext,
		"repetition":        w.Repetition,
		"repetition_cap":    w.RepetitionCap,
		"immediate_repeat":  w.ImmediateRepeat,
		"bigram_repeat":     w.BigramRepeat,
		"trigram_repeat":    w.TrigramRepeat,
		"end_early":         w.EndEarly,
		"end_late":          w.EndLate,
		"end_late_cap":      w.EndLateCap,
		"connective":        w.Connective,
		"style_chance":      w.StyleChance,
		"style_chance_name": w.StyleChanceName,
	}
}

// countWords is the same tokenization the length model is judged by: whitespace-separated
// words, which is what SPEC.md section 5.5 criterion 1 counts.
func countWords(s string) int { return len(strings.Fields(s)) }

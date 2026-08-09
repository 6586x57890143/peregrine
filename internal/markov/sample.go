package markov

import (
	"math"
	"sort"
)

// sample turns logits into a token: temperature, then top-k, then nucleus, then a
// weighted draw.
//
// The order is not interchangeable and it is the reason a high temperature is usable
// here at all. Temperature first, because truncation has to act on the distribution
// the operator actually asked for; then top-k to cut the long tail of one-count
// continuations that a sparse corpus is mostly made of; then top-p to cut whatever
// tail survived a generous k. Sampling hot from a truncated head is surprising.
// Sampling hot from the full tail is word salad, and in a corpus where nearly every
// high-order n-gram has count 1 the tail is almost all of the mass.
func (g *Generator) sample(cands []candidate) string {
	if len(cands) == 0 {
		return ""
	}
	if len(cands) == 1 {
		return cands[0].token
	}

	// Highest logit first, with the token as a deterministic tie-break so equal
	// logits cannot make the result depend on the incoming slice order.
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].logit != cands[j].logit {
			return cands[i].logit > cands[j].logit
		}
		return cands[i].token < cands[j].token
	})

	t := g.params.Temperature
	if t <= 0 {
		// Zero temperature is argmax, which is the degenerate end of the dial rather
		// than an error. It is also exactly what the old engine did by accident, so
		// it is worth being reachable on purpose for comparison.
		return cands[0].token
	}

	// Subtract the maximum before exponentiating. Without this, logits from a large
	// corpus overflow to +Inf or underflow to 0 and the whole distribution becomes
	// NaN or empty; with it, the largest weight is exactly 1 and the arithmetic is
	// unconditionally safe. Cheap, and the failure it prevents is total.
	maxLogit := cands[0].logit / t
	weights := make([]float64, len(cands))
	var total float64
	for i := range cands {
		w := math.Exp(cands[i].logit/t - maxLogit)
		weights[i] = w
		total += w
	}
	if total <= 0 || math.IsNaN(total) || math.IsInf(total, 0) {
		return cands[0].token
	}

	// Top-k.
	n := len(cands)
	if k := g.params.TopK; k > 0 && k < n {
		n = k
	}

	// Nucleus: keep the shortest leading run whose probability mass reaches TopP.
	// The candidate that crosses the threshold is kept, so a p of 0 still leaves one
	// candidate rather than none.
	if p := g.params.TopP; p > 0 && p < 1 {
		var cum float64
		cut := n
		for i := 0; i < n; i++ {
			cum += weights[i] / total
			if cum >= p {
				cut = i + 1
				break
			}
		}
		n = cut
	}

	// Renormalize over the survivors, because the draw below has to be against the
	// truncated mass and not the original.
	var kept float64
	for i := 0; i < n; i++ {
		kept += weights[i]
	}
	if kept <= 0 {
		return cands[0].token
	}

	r := g.src.Float64() * kept
	for i := 0; i < n; i++ {
		r -= weights[i]
		if r <= 0 {
			return cands[i].token
		}
	}
	// Floating-point residue only.
	return cands[n-1].token
}

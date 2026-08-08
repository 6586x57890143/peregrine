package safety

import (
	"log/slog"
	"sync/atomic"

	"github.com/6586x57890143/peregrine/internal/filter"
)

// Verdict is what a gate returns.
type Verdict struct {
	// Allowed is false when the text must not be learned or must not be sent.
	Allowed bool

	// Reason is a short description for the log. Never shown to a user.
	Reason string

	// Category is set when a blocklist rule matched.
	Category Category

	// Source is the blocklist entry that matched, as file:line, when one did.
	Source string

	// Alert is true when the operator should be paged rather than merely logged.
	// Set for the illegal category, where the exposure is legal rather than
	// reputational.
	Alert bool
}

func allow() Verdict { return Verdict{Allowed: true} }

// Gate is the single place both directions are decided.
//
// One gate with two entry points rather than two gates, so the normalizer and the
// ruleset cannot drift apart. Both directions share Normalize and share the
// Blocklist; what differs is which categories apply and what happens on a match.
type Gate struct {
	blocklist *Blocklist
	log       *slog.Logger

	// paused refuses every emit process-wide. This is the lever for the moment the
	// bot is actively saying something awful and waiting for a deploy is not
	// acceptable. It deliberately does NOT stop learning: the corpus is not the
	// problem during an incident, the output is, and stopping ingestion would also
	// stop the bot noticing what is being said to it.
	paused atomic.Bool

	// Counters, surfaced in the status line. Attempted poisoning should be visible
	// rather than inferred from the bot behaving oddly.
	learnRejected atomic.Uint64
	emitRejected  atomic.Uint64
}

// NewGate builds the gate. blocklist may be nil only in tests; production wiring
// treats a failed load as fatal, because an empty ruleset is indistinguishable
// from a working one.
func NewGate(blocklist *Blocklist, log *slog.Logger, pauseAllWrites bool) *Gate {
	g := &Gate{blocklist: blocklist, log: log}
	g.paused.Store(pauseAllWrites)
	return g
}

// SetPaused toggles the process-wide mute at runtime. Exists so a Discord-reachable
// kill switch can be added later without another lever; today only the environment
// variable sets it, at startup.
func (g *Gate) SetPaused(paused bool) { g.paused.Store(paused) }

// Paused reports whether outbound messages are currently refused.
func (g *Gate) Paused() bool { return g.paused.Load() }

// CheckLearn decides whether text may enter the corpus.
//
// This is called INSIDE learnMessage, not at any of its callers, and that
// placement is the whole fix for the highest-value finding in the review. The live
// message handler filtered properly, but learnMessage had four callers and the
// other three passed content through raw: the historical backfill, self-learning,
// and voice transcripts. Since the backfill re-read the trailing 24 hours every ten
// minutes, a message the live path blocked was learned anyway, unfiltered, minutes
// later, which defeated the live filter entirely (SPEC.md section 4, A1).
//
// Putting the check at the funnel means a fifth caller is covered without anyone
// remembering to cover it. That is the point, and it is why this must not be
// "helpfully" hoisted to the call sites for performance.
//
// REJECT, NEVER LAUNDER. The old slur filter replaced matches, so a slur-bearing
// message was still learned with its structure intact and the replacement token
// injected into the corpus in the slur's grammatical position: the bot had been
// taught the sentence and merely said "ninja" where the slur went. On this path the
// verdict is always to drop the whole message.
func (g *Gate) CheckLearn(text string) Verdict {
	// Shape checks first: they are the cheapest and they reject the most.
	if spam, reason := filter.Spam(text); spam {
		return g.rejectLearn(Verdict{Reason: "spam shape: " + reason})
	}

	normalized := Normalize(text)

	// The in-source baseline, which holds even when the operator's list is thin.
	if filter.ContainsSlur(normalized) {
		return g.rejectLearn(Verdict{Reason: "matched the built-in slur list", Category: CategorySlur})
	}
	if illegal, category := filter.Illegal(normalized); illegal {
		return g.rejectLearn(Verdict{Reason: "matched the built-in illegal list: " + category, Category: CategoryIllegal, Alert: true})
	}

	// The operator's list. Every category applies on the way in, including spam:
	// there is no reason to learn invite spam even though generating something that
	// resembles it is harmless.
	if rule, ok := g.blocklist.Match(normalized); ok {
		return g.rejectLearn(Verdict{
			Reason:   "matched blocklist rule",
			Category: rule.Category,
			Source:   rule.Source,
			Alert:    rule.Category == CategoryIllegal,
		})
	}

	return allow()
}

// CheckEmit decides whether text may be sent.
//
// A second gate is not redundant with the first, and this is the architectural
// point that makes it mandatory. A Markov chain composes novel sequences from
// n-grams that were learned separately, so fragments that were each innocuous can
// join into something the operator has to answer for. No amount of input hygiene
// prevents that: input filtering lowers the rate, only an output gate bounds the
// result (SPEC.md section 4, A3).
//
// On rejection the caller must STAY SILENT rather than substituting a fallback.
// Silence is always safe; a fallback string is a new output that has to be reasoned
// about, and in a chaos bot an unexplained silence is indistinguishable from the
// bot choosing not to reply.
//
// The spam category is deliberately not applied here. Those rules are nuisance
// patterns rather than harm, and the bot generating a phrase that resembles
// advertising is not an incident worth suppressing output over.
func (g *Gate) CheckEmit(text string) Verdict {
	if g.paused.Load() {
		return g.rejectEmit(Verdict{Reason: "PEREGRINE_PAUSE_ALL_WRITES is set: every outbound message is refused"})
	}

	if spam, reason := filter.Spam(text); spam {
		return g.rejectEmit(Verdict{Reason: "spam shape: " + reason})
	}

	normalized := Normalize(text)

	if filter.ContainsSlur(normalized) {
		return g.rejectEmit(Verdict{Reason: "matched the built-in slur list", Category: CategorySlur})
	}
	if illegal, category := filter.Illegal(normalized); illegal {
		return g.rejectEmit(Verdict{Reason: "matched the built-in illegal list: " + category, Category: CategoryIllegal, Alert: true})
	}
	if rule, ok := g.blocklist.Match(normalized, CategorySlur, CategoryIllegal); ok {
		return g.rejectEmit(Verdict{
			Reason:   "matched blocklist rule",
			Category: rule.Category,
			Source:   rule.Source,
			Alert:    rule.Category == CategoryIllegal,
		})
	}

	return allow()
}

// rejectLearn and rejectEmit exist so counting and logging happen in exactly one
// place per direction, rather than at each return.
//
// Neither logs the offending text. The verdict, the category and the rule that
// matched are what an operator needs, and writing the content into the log would
// put the thing being blocked into a second place it has to be cleaned out of.
func (g *Gate) rejectLearn(v Verdict) Verdict {
	v.Allowed = false
	g.learnRejected.Add(1)
	if g.log != nil {
		g.log.Info("learn rejected", "reason", v.Reason, "category", v.Category, "rule", v.Source)
	}
	return v
}

func (g *Gate) rejectEmit(v Verdict) Verdict {
	v.Allowed = false
	g.emitRejected.Add(1)
	if g.log != nil {
		// Warn rather than Info: something reached the generator that should not
		// have, which means either the corpus is already contaminated or the
		// composition problem in A3 just happened. Either way an operator wants to
		// see it.
		g.log.Warn("emit rejected, staying silent", "reason", v.Reason, "category", v.Category, "rule", v.Source)
		if v.Alert {
			g.log.Error("OPERATOR ALERT: illegal-category content was about to be emitted",
				"rule", v.Source, "reason", v.Reason)
		}
	}
	return v
}

// LearnRejected and EmitRejected report the running counts for the status line.
func (g *Gate) LearnRejected() uint64 { return g.learnRejected.Load() }
func (g *Gate) EmitRejected() uint64  { return g.emitRejected.Load() }

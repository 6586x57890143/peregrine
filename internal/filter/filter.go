// Package filter is the cheap, deterministic content checks: spam shape, an
// illegal-content pattern list, and a slur list.
//
// It is a leaf and it does no logging. Every function returns a reason string
// alongside its verdict and the caller decides what to record, which keeps the
// package testable and stops a hot-path filter from writing to the log on a
// message the caller was going to discard silently anyway.
//
// This package is NOT the safety gate. It is the mechanism the gate uses. The gate
// itself is internal/safety (M5), which owns the normalizer, loads the real
// blocklist from disk as data, and sits at the two chokepoints (CheckLearn inside
// learnMessage, CheckEmit inside the send guard) where no call site can opt out.
// The distinction matters because these patterns match on RAW text and are
// therefore evadable by design: intra-word spacing, combining marks, zero-width
// characters and homoglyphs all walk straight through a \b-anchored pattern. Do
// not add evasion variants here; M5's normalizer removes the need for them, and
// enumerating them by hand is a game the defender loses.
package filter

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Spam shape limits. Named because they were bare numbers in the middle of the
// function and each one is a policy decision someone may want to argue with.
const (
	maxRunes        = 2000 // Discord's own message cap, so anything longer is malformed
	maxRepeatedRune = 20   // "aaaaaaaa..." and box-drawing walls
	minAllowedRatio = 0.80 // see the comment on AllowedRune
)

// Spam reports whether content has the shape of spam, and why.
//
// Shape only: it never looks at meaning. That is what makes it safe to run on
// every message on the hot path, and it is why it cannot be the only check.
func Spam(s string) (bool, string) {
	runeCount := utf8.RuneCountInString(s)
	if runeCount > maxRunes {
		return true, fmt.Sprintf("excessive length: %d runes", runeCount)
	}

	// Repeated-character walls. Spaces are exempt because ordinary text contains
	// runs of them (indentation pasted from elsewhere) and they are not what this
	// is trying to catch.
	var lastRune rune
	repeatCount := 0
	for _, r := range s {
		if r == lastRune && !unicode.IsSpace(r) {
			repeatCount++
		} else {
			repeatCount = 1
			lastRune = r
		}
		if repeatCount > maxRepeatedRune {
			return true, fmt.Sprintf("repeated character spam (%q)", r)
		}
	}

	// The ratio below is meaningless on an empty string, and dividing by zero
	// would make every empty message spam.
	if runeCount == 0 {
		return false, ""
	}
	allowed := 0
	for _, r := range s {
		if AllowedRune(r) {
			allowed++
		}
	}
	ratio := float64(allowed) / float64(runeCount)
	if ratio < minAllowedRatio {
		return true, fmt.Sprintf("low allowed character ratio: %.2f", ratio)
	}
	return false, ""
}

// AllowedRune defines what counts as ordinary conversation.
//
// This is a real policy decision with a real cost, and it is recorded in SPEC.md
// section 4.3 so it can be argued with rather than discovered. Only Latin script
// is permitted, so a message written predominantly in Cyrillic, Greek, CJK, Arabic
// or Devanagari fails the 0.80 ratio above and is dropped as spam. That is a
// deliberate trade in a server that speaks English and gets kaomoji and
// symbol-wall spam, and it would be the wrong trade in a multilingual server.
//
// It is exported because the safety normalizer in M5 needs the same notion of
// "ordinary" to decide what to fold, and two independent definitions of that would
// drift.
func AllowedRune(r rune) bool {
	return unicode.In(r, unicode.Latin) ||
		unicode.IsDigit(r) ||
		unicode.IsSpace(r) ||
		Emoji(r) ||
		strings.ContainsRune(`.,'?!@#$%^&*()_+-=[]{}|\\;:"/<>~:`, r)
}

// Emoji reports whether a rune is in one of the common emoji blocks.
//
// Deliberately a range check rather than a property lookup: Go's unicode tables
// have no single "is emoji" predicate, and the alternative is a dependency for a
// question this answers well enough. Emoji are permitted because in this server
// they carry meaning, so excluding them would classify normal messages as spam.
func Emoji(r rune) bool {
	switch {
	case r >= 0x1F600 && r <= 0x1F64F: // Emoticons
		return true
	case r >= 0x1F300 && r <= 0x1F5FF: // Misc Symbols and Pictographs
		return true
	case r >= 0x1F680 && r <= 0x1F6FF: // Transport and Map
		return true
	case r >= 0x2600 && r <= 0x26FF: // Misc symbols
		return true
	case r >= 0x2700 && r <= 0x27BF: // Dingbats
		return true
	case r >= 0xFE00 && r <= 0xFE0F: // Variation Selectors
		return true
	case r >= 0x1F900 && r <= 0x1F9FF: // Supplemental Symbols and Pictographs
		return true
	case r >= 0x1F1E6 && r <= 0x1F1FF: // Regional Indicator Symbols
		return true
	default:
		return false
	}
}

// rule pairs a compiled pattern with what it is for. A slice, not a map, and this
// is the whole point of the type.
//
// These were maps from pattern string to replacement, iterated to build the
// result. Go randomizes map iteration order, so when two patterns could both match
// overlapping text the output depended on which one the runtime happened to yield
// first: the same input produced different output on different runs, and the
// corpus recorded whichever one won that time (SPEC.md section 8, finding 22).
// A slice makes the order explicit, reviewable and stable.
type rule struct {
	re          *regexp.Regexp
	replacement string
	label       string
}

// illegalRules is a PLACEHOLDER and says so, because a comment claiming otherwise
// would be worse than the gap. The real patterns are operator data, not source:
// committing an explicit list of threat and CSAM-adjacent terms would make this
// repository a searchable copy of one and turn every addition into a rebuild and a
// public diff instead of an edit made mid-incident. M5 loads them from
// PEREGRINE_BLOCKLIST_PATH, which fails closed if the file is missing.
//
// Until then this blocks essentially nothing (SPEC.md section 4, A4). The two
// patterns below are demonstrations of the mechanism, kept so the wiring is
// exercised rather than untested.
var illegalRules = []rule{
	{
		re:    regexp.MustCompile(`(?i)\b(threaten|harm|attack|kill)\s+(someone|people|a group)\b`),
		label: "violent threat",
	},
	{
		re:    regexp.MustCompile(`(?i)\b(how to build|create|make)\s+(a|an)\s+(dangerous item|weapon)\b`),
		label: "illegal activities",
	},
}

// slurRules is a slice so the order is explicit, reviewable and stable. Every
// pattern here is \b-anchored on both ends, so as it happens none of them can both
// match the same word and the order does not change today's output; what the slice
// buys is that a future addition which DOES overlap behaves predictably instead of
// depending on which key Go's randomized map iteration yielded first.
//
// Kept in source rather than moved to the operator blocklist because these are the
// baseline that has to hold even when the blocklist file has not been written yet,
// and because they are already in this repository's history.
//
// The leet classes here ([i1], [a@], [o0]) are exactly the evasions someone
// enumerated by hand, and enumeration is the losing strategy: intra-word spacing,
// zero-width joiners, combining marks and Cyrillic homoglyphs all pass. M5
// normalizes first and matches against the normalized form, which is what makes
// these patterns adequate rather than a sieve. Do not add more variants here.
var slurRules = buildSlurRules()

func buildSlurRules() []rule {
	// Pattern, replacement. Order is significant and preserved.
	raw := [][2]string{
		{`\bn[i1]gg(a|er|ar)s?\b`, "ninja"},
		{`\bf[a@]gg?[o0]ts?\b`, "magician"},
		{`\btrann(y|ie)s?\b`, "transformer"},
		{`\bk[i1]ke\b`, "wizard"},
		{`\bredsk[i1]n\b`, "redwood"},
		{`\bwetb[a@]ck\b`, "waveback"},
		{`\bkaffir\b`, "kangaroo"},
		{`\bch[i1]nk\b`, "chime"},
		{`\bcrack(a|er)\b`, "cruncher"},
		{`\bcoolie\b`, "cookie"},
		{`\bsambo\b`, "samba"},
		{`\bhonky\b`, "honey"},
		{`\bsp[i1]c\b`, "spark"},
		{`\bgook\b`, "goofy"},
		{`\bpaki\b`, "pinky"},
		{`\bboche\b`, "bouncer"},
		{`\bc[o0]{2}n\b`, "doomer"},
		{`\byid\b`, "yard"},
		{`\bheeb\b`, "healer"},
		{`\bwop\b`, "whopper"},
		{`\bmick\b`, "mickey"},
		{`\bchug\b`, "chugger"},
		{`\bdago\b`, "dango"},
	}
	rules := make([]rule, 0, len(raw))
	for _, r := range raw {
		rules = append(rules, rule{
			re:          regexp.MustCompile(`(?i)` + r[0]),
			replacement: r[1],
		})
	}
	return rules
}

// Illegal reports whether content matches the illegal-content list, and which
// category matched.
func Illegal(s string) (bool, string) {
	for _, r := range illegalRules {
		if r.re.MatchString(s) {
			return true, r.label
		}
	}
	return false, ""
}

// ReplaceSlurs substitutes matches with harmless words, in rule order.
//
// Replacement is a DISPLAY operation and must never be used on the learning path.
// A slur-bearing message that has been laundered this way still carries its
// structure, and learning it injects the replacement token into the corpus in the
// slur's grammatical position, so the bot has been taught the sentence and merely
// says "ninja" where the slur went. On the learning path the verdict has to be
// "drop the whole message", which is M5's job (SPEC.md section 4, A5).
func ReplaceSlurs(s string) string {
	for _, r := range slurRules {
		s = r.re.ReplaceAllString(s, r.replacement)
	}
	return s
}

// ContainsSlur reports whether content matches any slur rule.
//
// Implemented directly rather than as ReplaceSlurs(s) != s, which is how it used
// to work. That version allocated a full rewritten copy of the string to answer a
// yes/no question, ran every rule even after the first match, and would have
// silently started returning false for any rule whose replacement happened to
// equal its own match.
func ContainsSlur(s string) bool {
	for _, r := range slurRules {
		if r.re.MatchString(s) {
			return true
		}
	}
	return false
}

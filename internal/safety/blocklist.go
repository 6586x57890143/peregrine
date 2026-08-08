package safety

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// Category says what a rule is for, which decides what happens when it matches.
type Category string

const (
	// CategorySlur is dropped on learn and blocked on emit. The bulk of the list.
	CategorySlur Category = "slur"

	// CategoryIllegal is dropped on learn, blocked on emit, and additionally
	// alerts the operator, because the exposure here is legal rather than
	// reputational. Keep it narrow: every entry pages a human.
	CategoryIllegal Category = "illegal"

	// CategorySpam is dropped on learn only. Not blocked on emit, because these
	// are nuisance patterns (invite spam, free-nitro bait) rather than harm, and
	// the bot generating a phrase that resembles advertising is not an incident.
	CategorySpam Category = "spam"
)

// Rule is one loaded pattern.
type Rule struct {
	Category Category
	Pattern  *regexp.Regexp

	// Source is the file and line the rule came from, so a match can be traced
	// back to the entry that caused it without grepping the list.
	Source string
}

// Blocklist is the loaded ruleset. Immutable after loading.
type Blocklist struct {
	rules []Rule
}

// ErrNoBlocklist is returned when no path is configured. Callers must treat this
// as fatal at startup rather than continuing with an empty ruleset.
var ErrNoBlocklist = errors.New("no blocklist path configured")

// LoadBlocklist reads the ruleset from path.
//
// It fails CLOSED, and that is the entire point of this function's error
// behaviour. A missing file, an unreadable file, a malformed line, an
// uncompilable pattern and an empty file are all errors. None of them degrade to
// "run with fewer rules", because an empty ruleset is indistinguishable from a
// working one right up until the worst possible moment, and the moment it becomes
// distinguishable is the moment the bot has already posted something the operator
// has to answer for.
//
// The list is data rather than source (SPEC.md section 4.2). Committing an
// explicit slur and threat list would make this repository a searchable copy of
// one, and would turn every addition into a rebuild and a public diff instead of
// an edit the operator makes mid-incident. Only blocklist.example.txt is
// committed.
func LoadBlocklist(path string) (*Blocklist, error) {
	if strings.TrimSpace(path) == "" {
		return nil, ErrNoBlocklist
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open blocklist %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	bl, err := parseBlocklist(f, path)
	if err != nil {
		return nil, err
	}
	return bl, nil
}

// parseBlocklist is split out so the format is testable without touching the
// filesystem.
func parseBlocklist(r io.Reader, name string) (*Blocklist, error) {
	var (
		rules []Rule
		errs  []error
	)

	scanner := bufio.NewScanner(r)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}

		category, pattern, ok := strings.Cut(text, " ")
		if !ok {
			// Tab-separated is the documented form and the one an editor produces,
			// so try it before giving up.
			category, pattern, ok = strings.Cut(text, "\t")
		}
		if !ok {
			errs = append(errs, fmt.Errorf("%s:%d: expected \"<category> <pattern>\", got %q", name, line, text))
			continue
		}
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			errs = append(errs, fmt.Errorf("%s:%d: empty pattern", name, line))
			continue
		}

		cat := Category(strings.ToLower(strings.TrimSpace(category)))
		switch cat {
		case CategorySlur, CategoryIllegal, CategorySpam:
		default:
			errs = append(errs, fmt.Errorf("%s:%d: unknown category %q, want one of slur, illegal, spam", name, line, category))
			continue
		}

		// Case-insensitive because the normalizer already lowercases, so a pattern
		// author writing an uppercase letter has made a mistake this absorbs rather
		// than a rule that silently never fires.
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s:%d: pattern %q does not compile: %w", name, line, pattern, err))
			continue
		}

		rules = append(rules, Rule{
			Category: cat,
			Pattern:  re,
			Source:   fmt.Sprintf("%s:%d", name, line),
		})
	}
	if err := scanner.Err(); err != nil {
		errs = append(errs, fmt.Errorf("read %s: %w", name, err))
	}

	// Every problem at once, so an operator editing the list under pressure gets
	// one pass rather than one restart per typo.
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	// An empty file is an error, not an empty ruleset. Someone who created the file
	// and has not filled it in yet is in exactly the state this must not silently
	// permit.
	if len(rules) == 0 {
		return nil, fmt.Errorf("%s contains no rules: an empty blocklist is indistinguishable "+
			"from a working one until the bot posts something it should not have. Populate it, "+
			"or unset PEREGRINE_BLOCKLIST_PATH deliberately", name)
	}

	return &Blocklist{rules: rules}, nil
}

// Match returns the first rule matching the already-normalized text, in the
// categories given.
//
// The text MUST already be normalized; this does not normalize for you. That is
// deliberate: the caller normalizes once and matches against several rulesets,
// and a helper that quietly re-normalized would make double normalization
// invisible. Normalize is idempotent, so a mistake here is not a correctness bug,
// but it is wasted work on the hot path.
func (b *Blocklist) Match(normalized string, categories ...Category) (Rule, bool) {
	if b == nil {
		return Rule{}, false
	}
	for _, r := range b.rules {
		if !categoryIn(r.Category, categories) {
			continue
		}
		if r.Pattern.MatchString(normalized) {
			return r, true
		}
	}
	return Rule{}, false
}

// categoryIn treats an empty list as "every category", which is what makes
// Match(normalized) mean "all rules" on the learning path and
// Match(normalized, CategorySlur, CategoryIllegal) mean a subset on the emit path.
func categoryIn(c Category, categories []Category) bool {
	if len(categories) == 0 {
		return true
	}
	return slices.Contains(categories, c)
}

// Len reports how many rules loaded, for the startup log line. Worth logging: it
// is the only way an operator can tell that the file they edited is the file the
// bot read.
func (b *Blocklist) Len() int {
	if b == nil {
		return 0
	}
	return len(b.rules)
}

// CountByCategory reports the rule count per category, also for the startup log.
// A list that is all slur and no illegal is a fine state to be in, but it should
// be a known one.
func (b *Blocklist) CountByCategory() map[Category]int {
	out := make(map[Category]int, 3)
	if b == nil {
		return out
	}
	for _, r := range b.rules {
		out[r.Category]++
	}
	return out
}

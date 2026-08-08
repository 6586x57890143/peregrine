package safety

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBlocklist(t *testing.T) {
	const list = `
# a comment
slur      \bexampleslur\b

illegal   \bkill\s+you\b
spam	\bfree\s+nitro\b
`
	bl, err := parseBlocklist(strings.NewReader(list), "test")
	if err != nil {
		t.Fatalf("parseBlocklist: %v", err)
	}
	if bl.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", bl.Len())
	}
	counts := bl.CountByCategory()
	for cat, want := range map[Category]int{CategorySlur: 1, CategoryIllegal: 1, CategorySpam: 1} {
		if counts[cat] != want {
			t.Errorf("CountByCategory()[%s] = %d, want %d", cat, counts[cat], want)
		}
	}

	// A rule must carry where it came from, so an operator can find the entry that
	// caused a block without grepping the list.
	rule, ok := bl.Match("that is an exampleslur here")
	if !ok {
		t.Fatal("a slur rule did not match")
	}
	if rule.Source != "test:3" {
		t.Errorf("Source = %q, want %q", rule.Source, "test:3")
	}
}

// TestBlocklistFailsClosed is the most important test in this file. Every one of
// these inputs must be an error, because the alternative is running with fewer
// rules than the operator believes, and an incomplete ruleset is indistinguishable
// from a working one until the bot posts something it should not have.
func TestBlocklistFailsClosed(t *testing.T) {
	cases := map[string]string{
		"empty file":           "",
		"only comments":        "# nothing here\n# still nothing\n",
		"only blank lines":     "\n\n\n",
		"unknown category":     "whoops \\bpattern\\b",
		"missing pattern":      "slur",
		"empty pattern":        "slur   ",
		"uncompilable pattern": "slur \\b(unclosed",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseBlocklist(strings.NewReader(content), "test"); err == nil {
				t.Error("expected an error; a partial or empty ruleset must never load quietly")
			}
		})
	}
}

// TestBlocklistReportsEveryProblem matters because the operator editing this file
// is frequently doing it mid-incident. One pass beats one restart per typo.
func TestBlocklistReportsEveryProblem(t *testing.T) {
	const list = `
slur      \bfine\b
nonsense  \bbad category\b
slur      \b(unclosed
badline
`
	_, err := parseBlocklist(strings.NewReader(list), "test")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"test:3", "test:4", "test:5", "unknown category", "does not compile"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must mention %q; got:\n%s", want, msg)
		}
	}
}

func TestLoadBlocklistMissingFileIsAnError(t *testing.T) {
	_, err := LoadBlocklist(filepath.Join(t.TempDir(), "does-not-exist.txt"))
	if err == nil {
		t.Fatal("a missing blocklist must be an error, not an empty ruleset")
	}
}

func TestLoadBlocklistEmptyPath(t *testing.T) {
	_, err := LoadBlocklist("")
	if !errors.Is(err, ErrNoBlocklist) {
		t.Errorf("LoadBlocklist(\"\") = %v, want ErrNoBlocklist", err)
	}
	if _, err := LoadBlocklist("   "); !errors.Is(err, ErrNoBlocklist) {
		t.Errorf("a whitespace-only path must also be ErrNoBlocklist, got %v", err)
	}
}

func TestLoadBlocklistFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocklist.txt")
	if err := os.WriteFile(path, []byte("slur \\bexampleslur\\b\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	bl, err := LoadBlocklist(path)
	if err != nil {
		t.Fatalf("LoadBlocklist: %v", err)
	}
	if bl.Len() != 1 {
		t.Errorf("Len() = %d, want 1", bl.Len())
	}
}

// TestCommittedExampleParses guards the one file that IS committed. It is what an
// operator copies to start their real list, so a format error in it would send
// someone debugging their own typing.
func TestCommittedExampleParses(t *testing.T) {
	bl, err := LoadBlocklist(filepath.Join("..", "..", "blocklist.example.txt"))
	if err != nil {
		t.Fatalf("blocklist.example.txt does not parse: %v", err)
	}
	counts := bl.CountByCategory()
	// The example must demonstrate every category, or a category's format goes
	// undocumented by example.
	for _, cat := range []Category{CategorySlur, CategoryIllegal, CategorySpam} {
		if counts[cat] == 0 {
			t.Errorf("blocklist.example.txt has no %s entries, so that category is undemonstrated", cat)
		}
	}
}

func TestMatchCategoryFiltering(t *testing.T) {
	bl, err := parseBlocklist(strings.NewReader("spam \\bfree nitro\\b\nslur \\bexampleslur\\b\n"), "test")
	if err != nil {
		t.Fatalf("parseBlocklist: %v", err)
	}

	// No categories means all of them, which is the learning path.
	if _, ok := bl.Match("get free nitro now"); !ok {
		t.Error("with no category filter, a spam rule must match")
	}
	// The emit path asks only for the harmful categories, because generating
	// something that resembles advertising is not an incident.
	if _, ok := bl.Match("get free nitro now", CategorySlur, CategoryIllegal); ok {
		t.Error("a spam rule must not match when only slur and illegal were requested")
	}
	if _, ok := bl.Match("an exampleslur", CategorySlur, CategoryIllegal); !ok {
		t.Error("a slur rule must match when slur was requested")
	}
}

// TestNilBlocklistDoesNotPanic covers the test-only construction path and any
// future caller that gets its error handling wrong. It must be inert, never
// permissive-by-crash.
func TestNilBlocklistIsInert(t *testing.T) {
	var bl *Blocklist
	if _, ok := bl.Match("anything"); ok {
		t.Error("a nil blocklist must match nothing")
	}
	if bl.Len() != 0 {
		t.Error("a nil blocklist must report zero rules")
	}
	if len(bl.CountByCategory()) != 0 {
		t.Error("a nil blocklist must report no categories")
	}
}

// TestPatternsMatchAgainstNormalizedForm is the contract between this file and the
// normalizer, and it is what lets the example file say "do not enumerate leet
// variants here". A rule written in plain form must catch the evaded spellings once
// the input has been normalized.
func TestPatternsMatchAgainstNormalizedForm(t *testing.T) {
	bl, err := parseBlocklist(strings.NewReader("slur \\bexampleslur\\b\n"), "test")
	if err != nil {
		t.Fatalf("parseBlocklist: %v", err)
	}

	evaded := []string{
		"EXAMPLESLUR",
		"3xampl3slur",
		"exampleslur" + string(rune(0x200D)),
		"e x a m p l e s l u r",
		"exampl" + string(rune(0x0435)) + "slur", // Cyrillic e
		"examplesluuuur",                         // repeat collapse gets this to ...luur, not ...lur
	}
	for _, in := range evaded {
		normalized := Normalize(in)
		if _, ok := bl.Match(normalized); !ok {
			// The last case is expected to miss: collapsing to two rather than one
			// means "sluuuur" becomes "sluur", which the plain pattern does not
			// match. Recorded rather than hidden, because it tells a pattern author
			// that a doubled letter is the one variant still worth allowing for.
			if in == "examplesluuuur" {
				t.Logf("known limit: %q normalizes to %q, which a plain pattern misses; "+
					"allow for one doubling in patterns where it matters", in, normalized)
				continue
			}
			t.Errorf("pattern missed %q, which normalizes to %q", in, normalized)
		}
	}
}

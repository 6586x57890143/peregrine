package legacy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/6586x57890143/peregrine/internal/config"
	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/safety"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// This file is the regression pin for SPEC.md section 4, A1: the highest-value
// finding in the review, and the one most likely to silently come back.
//
// The bug was that learnMessage had four callers and only one of them filtered.
// The live message handler ran the filters; the historical backfill, self-learning
// and voice transcripts did not. Since the backfill re-read the trailing 24 hours
// every ten minutes, a message the live path blocked was learned anyway,
// unfiltered, minutes later, which defeated the live filter entirely.
//
// It is tested from two directions on purpose. The behavioural tests prove the gate
// works. The structural test proves it is in the right PLACE, which is the part
// that regresses: a well-meaning change that moves the check to the call sites for
// performance, or adds a fifth caller, reintroduces exactly the original bug while
// every behavioural test still passes.

// gateFixture wires up the package globals learnMessage depends on, with a corpus
// in a temp directory and a blocklist containing one known pattern.
func gateFixture(t *testing.T) *storage.Store {
	t.Helper()

	s := dbtest.Store(t)

	bl, err := safety.LoadBlocklist(writeBlocklist(t))
	if err != nil {
		t.Fatalf("LoadBlocklist: %v", err)
	}

	// Save and restore the globals, so these tests do not leak into each other.
	oldStore, oldCfg, oldGate := store, cfg, gate
	t.Cleanup(func() { store, cfg, gate = oldStore, oldCfg, oldGate })

	store = s
	// The generation dials carry the shipped defaults rather than zero values, so these
	// tests exercise the configuration production actually runs. A zero Temperature
	// makes the sampler argmax and a zero TopP keeps only the single best candidate,
	// which would quietly turn every test here into a deterministic-path test and hide
	// anything to do with sampling.
	//
	// MinDistinctAuthors is the one deliberate exception, at 0. These fixtures are
	// single-author by nature, so the shipped default of 2 would make generation
	// correctly produce nothing and every assertion below would be testing the gate
	// instead of what it says it tests. The gate has its own coverage in
	// internal/markov and in TestGenerationHonoursTheConfiguredAuthorGate.
	cfg = &config.Config{
		MaxNGram:             3,
		MaxHistory:           1000,
		Temperature:          1.0,
		TopK:                 40,
		TopP:                 0.95,
		KNDiscount:           0.75,
		KNRawMix:             0.25,
		MinDistinctAuthors:   0,
		PromptRelevanceBoost: 0.6,
	}
	gate = safety.NewGate(bl, slog.New(slog.NewTextHandler(io.Discard, nil)), false)

	return s
}

func writeBlocklist(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blocklist.txt")
	const content = "slur \\bexampleslur\\b\nillegal \\bkill you\\b\n"
	if err := writeFile(path, content); err != nil {
		t.Fatalf("write blocklist: %v", err)
	}
	return path
}

// learnedNgrams returns every stored continuation as "prefix -> next", which is how
// these tests tell "learned" from "dropped".
func learnedNgrams(t *testing.T, s *storage.Store) []string {
	t.Helper()
	var out []string
	if err := s.View(func(r *storage.Reader) error {
		return r.ForEachNgram(func(prefix, next string, _ corpus.Successor) error {
			out = append(out, prefix+" -> "+next)
			return nil
		})
	}); err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	return out
}

// snowflake makes a syntactically valid Discord ID. Message IDs are stored as
// fixed-width big-endian integers now, so "m1" is not a message ID any more: it is
// an error, which is the correct answer for a caller that invented one.
func snowflake(n int) string {
	const base = 1000000000000000000
	return itoa(base + n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func learn(t *testing.T, s *storage.Store, msg, msgID string) {
	t.Helper()
	if err := s.Update(func(w *storage.Writer) error {
		return learnMessage(w, msg, msgID, "999", MentionedUser{Name: "tester", UserID: "1", Username: "tester"}, nil)
	}); err != nil {
		t.Fatalf("learnMessage: %v", err)
	}
}

func TestLearnMessageLearnsOrdinaryText(t *testing.T) {
	s := gateFixture(t)

	learn(t, s, "the bird is on the roof again", snowflake(1))

	if len(learnedNgrams(t, s)) == 0 {
		t.Fatal("ordinary text was not learned, so the rest of this file proves nothing")
	}
}

// TestLearnMessageDropsBlockedContent is the behavioural half. Note that it calls
// learnMessage DIRECTLY, with no handler in front of it: that is exactly the path
// the backfill, self-learning and transcripts take, and under the old arrangement
// this content would have been learned.
func TestLearnMessageDropsBlockedContent(t *testing.T) {
	cases := map[string]string{
		"blocklist slur":        "you are an exampleslur honestly",
		"blocklist illegal":     "i will kill you tomorrow",
		"built-in slur":         "you absolute wop",
		"spam shape":            strings.Repeat("a", 400),
		"evaded via spacing":    "you are an e x a m p l e s l u r",
		"evaded via leet":       "you are an 3xampl3slur",
		"evaded via homoglyph":  "you are an exampl" + string(rune(0x0435)) + "slur",
		"evaded via zero width": "you are an example" + string(rune(0x200D)) + "slur",
	}

	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			s := gateFixture(t)
			learn(t, s, msg, snowflake(1))

			if got := learnedNgrams(t, s); len(got) != 0 {
				t.Errorf("blocked content was learned: %d n-grams written, first %q", len(got), got[0])
			}
		})
	}
}

// TestLearnMessageRejectsWholeMessageNotJustTheWord is the reject-never-launder
// rule. The old filter replaced the match, so the message was learned with its
// structure intact and a harmless token in the offending word's grammatical
// position: the bot had been taught the sentence.
//
// This asserts that NONE of the surrounding words made it in, not merely that the
// slur did not.
func TestLearnMessageRejectsWholeMessageNotJustTheWord(t *testing.T) {
	s := gateFixture(t)

	learn(t, s, "i think that exampleslur is why the bird left the roof", snowflake(1))

	got := learnedNgrams(t, s)
	if len(got) != 0 {
		t.Fatalf("expected nothing learned, got %d n-grams", len(got))
	}
	// And specifically not the innocuous fragments, which is what laundering would
	// have preserved.
	for _, k := range got {
		for _, innocuous := range []string{"bird", "roof", "why the"} {
			if strings.Contains(k, innocuous) {
				t.Errorf("the message was laundered rather than dropped: %q survived", k)
			}
		}
	}
}

// TestLearnMessageWritesNoEmptyPrefix is finding 5's pin, at the layer that used to
// produce it.
//
// The ingestion loop ran n from MaxNGram down to 1, and at n == 1 the prefix is
// empty, so every unigram in the corpus accumulated into one key holding a map of the
// whole vocabulary. Writer.LearnNgram refuses an empty prefix, so a regression here
// is an error rather than silent write amplification, and this asserts learning still
// succeeds rather than failing on that error.
func TestLearnMessageWritesNoEmptyPrefix(t *testing.T) {
	s := gateFixture(t)

	learn(t, s, "the bird is on the roof", snowflake(1))

	for _, ng := range learnedNgrams(t, s) {
		if strings.HasPrefix(ng, " -> ") {
			t.Errorf("an empty-prefix n-gram was written: %q. Unigram frequency belongs in the "+
				"topic index, one key per word (SPEC.md section 8, finding 5)", ng)
		}
	}
}

// TestLearnMessageExcludesTheBotFromAuthorDiversity is A6's pin on the write path.
//
// Self-learning feeds the bot's own replies back into the corpus. If the bot counted
// as a distinct author, anything it said once would carry a diversity count of one
// from the moment it was said, which is half of what M7's eligibility gate asks for,
// bootstrapped by the bot itself rather than by people.
func TestLearnMessageExcludesTheBotFromAuthorDiversity(t *testing.T) {
	s := gateFixture(t)

	const botUserID = "999"
	botAuthor := MentionedUser{Name: "peregrine", UserID: botUserID, Username: "peregrine"}
	if err := s.Update(func(w *storage.Writer) error {
		return learnMessage(w, "the bird is loose", snowflake(1), botUserID, botAuthor, nil)
	}); err != nil {
		t.Fatalf("learnMessage: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		succ, ok, err := r.Successor("the", "bird")
		if err != nil || !ok {
			t.Fatalf("the bot's own reply was not learned at all: ok=%v err=%v", ok, err)
		}
		if succ.Authors != 0 {
			t.Errorf("Authors = %d after learning the bot's own output, want 0: self-learning "+
				"must not contribute to author diversity (SPEC.md section 4, A6)", succ.Authors)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestLearnMessageDedupesByMessageID pins the window that stops the backfill
// double-counting. The backfill re-reads recent history every ten minutes, so
// without this every pass would add the same n-grams again (finding 13).
func TestLearnMessageDedupesByMessageID(t *testing.T) {
	s := gateFixture(t)

	id := snowflake(1)
	learn(t, s, "the bird is loose", id)
	learn(t, s, "the bird is loose", id)

	if err := s.View(func(r *storage.Reader) error {
		succ, ok, err := r.Successor("the", "bird")
		if err != nil || !ok {
			t.Fatalf("nothing learned: ok=%v err=%v", ok, err)
		}
		if succ.Count != 1 {
			t.Errorf("Count = %d after learning the same message ID twice, want 1", succ.Count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestGateIsInsideLearnMessageNotAtCallSites is the structural half, and it is the
// one that catches the regression that matters.
//
// Design principle 3 in SPEC.md: a check at one of four call sites is not a check.
// This parses the package and asserts the gate call is inside learnMessage's own
// body. If someone hoists it to the callers for performance, or adds a fifth caller
// that forgets it, every behavioural test above still passes and this fails.
func TestGateIsInsideLearnMessageNotAtCallSites(t *testing.T) {
	learnFn := findFunc(t, "learnMessage")

	if !containsCall(learnFn.Body, "gate", "CheckLearn") {
		t.Error("learnMessage does not call gate.CheckLearn. That check must live INSIDE this " +
			"function, because it has multiple callers and a check at one call site is not a " +
			"check. See SPEC.md section 4, A1.")
	}
}

// TestLearnMessageCallerCountIsKnown makes a new caller a deliberate act.
//
// The count itself is not the protection: the gate being inside learnMessage is.
// But a change to this number means someone added a path into the corpus, and that
// is worth a moment's thought and a look at whether the new path also needs a
// CheckEmit counterpart.
func TestLearnMessageCallerCountIsKnown(t *testing.T) {
	// Counted from the AST rather than by grepping the source. A regexp attempt at
	// this counted five, because the function's own declaration looks exactly like a
	// call to a pattern that is not parsing Go.
	calls := 0
	ast.Inspect(parseLegacy(t), func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "learnMessage" {
			calls++
		}
		return true
	})

	const known = 4 // backfill, self-learning, live handler, voice transcript
	if calls != known {
		t.Errorf("learnMessage has %d call sites, expected %d.\n\n"+
			"If you added one: it is already covered, because the gate is inside "+
			"learnMessage rather than at the call sites, which is the entire point of "+
			"A1's fix. Update this number.\n\n"+
			"If you removed one: also fine, update this number.\n\n"+
			"If this dropped to zero, learning is disconnected.", calls, known)
	}
}

// TestThisPackageCannotReachBbolt is M6b's structural pin, and it is the same kind of
// test as the gate one above: it asserts a PLACEMENT rather than a behavior.
//
// internal/storage is the only package that may know a bucket exists. This one held
// twelve bucket-name constants and reached into buckets at roughly sixty sites, which
// is what made the nested-transaction deadlock writable in the first place: a function
// holding a *bbolt.Tx can open another transaction, and an outer read plus a writer
// waiting to remap plus an inner read is an unrecoverable hang (SPEC.md section 8,
// finding 1).
//
// The check is on the IMPORT rather than on the text, and the import is the exact
// invariant: the bbolt API is unreachable without it, so if no file in this package
// imports bbolt then no file in this package can name a bucket, start a transaction,
// or hold a handle. It scans every file rather than just legacy.go, so a new file
// cannot reintroduce the dependency next to a test that only looks at the old one.
func TestThisPackageCannotReachBbolt(t *testing.T) {
	// Files are globbed and parsed one at a time rather than with parser.ParseDir,
	// which is deprecated: it does not consider build tags when associating files
	// with packages. Every .go file in the directory is what this test wants anyway,
	// tags or not.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found, so this test proves nothing")
	}

	fset := token.NewFileSet()
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range file.Imports {
			if strings.Contains(imp.Path.Value, "bbolt") {
				t.Errorf("%s imports %s. Only internal/storage may reach bbolt; everything "+
					"here goes through storage.Reader and storage.Writer, neither of which "+
					"has a method that starts a transaction. See SPEC.md section 8, finding 1.",
					name, imp.Path.Value)
			}
		}
	}
}

// parseLegacy parses legacy.go for the structural tests.
func parseLegacy(t *testing.T) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "legacy.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse legacy.go: %v", err)
	}
	return file
}

func findFunc(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()
	for _, decl := range parseLegacy(t).Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("%s not found; if it was renamed, update this test rather than deleting it", name)
	return nil
}

// containsCall reports whether body contains a call of the form recv.method(...).
func containsCall(body *ast.BlockStmt, recv, method string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if ident.Name == recv && sel.Sel.Name == method {
			found = true
			return false
		}
		return true
	})
	return found
}

// writeFile is thin on purpose. Its readFile counterpart went away when the caller
// count moved from grepping the source to walking the AST.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

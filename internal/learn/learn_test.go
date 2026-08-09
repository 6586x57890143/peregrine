package learn

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/names"
	"github.com/6586x57890143/peregrine/internal/safety"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// This file is the regression pin for SPEC.md section 4, A1: the highest-value finding in the
// review, and the one most likely to silently come back.
//
// The bug was that the learn path had four callers and only one of them filtered. The live
// message handler ran the filters; the historical backfill, self-learning and voice
// transcripts did not. Since the backfill re-read the trailing 24 hours every ten minutes, a
// message the live path blocked was learned anyway, unfiltered, minutes later, which defeated
// the live filter entirely.
//
// It is tested from two directions on purpose. The behavioural tests prove the gate works.
// The structural test proves it is in the right PLACE, which is the part that regresses: a
// well-meaning change that moves the check to the call sites for performance, or adds a fifth
// caller, reintroduces exactly the original bug while every behavioural test still passes.

func snowflake(n int) string {
	return strconv.FormatUint((uint64(n)<<22)|1, 10)
}

// fixture returns a corpus and a Learner over a blocklist with one known pattern.
func fixture(t *testing.T) (*storage.Store, *Learner) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "blocklist.txt")
	const content = "slur \\bexampleslur\\b\nillegal \\bkill you\\b\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write blocklist: %v", err)
	}
	bl, err := safety.LoadBlocklist(path)
	if err != nil {
		t.Fatalf("LoadBlocklist: %v", err)
	}
	gate := safety.NewGate(bl, slog.New(slog.NewTextHandler(io.Discard, nil)), false)

	l := New(gate, Options{MaxNGram: 3, MaxHistory: 1000, CooccurrenceWindow: 5})
	return dbtest.Store(t), l
}

func author(id int) names.User {
	return names.User{Name: "u" + strconv.Itoa(id), UserID: snowflake(id), Username: "u" + strconv.Itoa(id)}
}

func learnOne(t *testing.T, s *storage.Store, l *Learner, text, msgID string, who names.User) {
	t.Helper()
	if err := s.Update(func(w *storage.Writer) error {
		return l.Message(w, text, msgID, who, []names.User{who})
	}); err != nil {
		t.Fatalf("Message: %v", err)
	}
}

func successors(t *testing.T, s *storage.Store, prefix string) map[string]corpus.Successor {
	t.Helper()
	out := map[string]corpus.Successor{}
	if err := s.View(func(r *storage.Reader) error {
		got, err := r.Successors(prefix)
		if err != nil {
			return err
		}
		for _, sc := range got {
			out[sc.Token] = sc
		}
		return nil
	}); err != nil {
		t.Fatalf("Successors(%q): %v", prefix, err)
	}
	return out
}

func TestOrdinaryTextIsLearned(t *testing.T) {
	s, l := fixture(t)
	learnOne(t, s, l, "the bird is loose again", snowflake(1), author(7))

	if got := successors(t, s, "the bird"); len(got) == 0 {
		t.Error("nothing was learned from an ordinary message")
	}
}

// TestBlockedContentWritesNothing is the behavioural half of A1, exercised through the path
// the backfill takes: calling the learn path directly, with no handler in front of it.
func TestBlockedContentWritesNothing(t *testing.T) {
	s, l := fixture(t)
	learnOne(t, s, l, "the bird said exampleslur loudly", snowflake(2), author(7))

	if got := successors(t, s, "the bird"); len(got) != 0 {
		t.Errorf("blocked content reached the corpus: %v", got)
	}
	var learned uint64
	if err := s.View(func(r *storage.Reader) error {
		learned = r.Status().Learned
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if learned != 0 {
		t.Errorf("the learned counter moved to %d for a blocked message", learned)
	}
}

// TestRejectionIsWholeMessageNotJustTheWord pins reject-never-launder.
//
// The old code replaced the match in place and learned the message anyway, so the corpus
// gained the sentence with a harmless token in the slur's grammatical position: the bot had
// been taught the sentence and merely said "ninja" where the slur went (A5). safety.Verdict
// has no field for rewritten text, which makes laundering unexpressible rather than merely
// discouraged, and this asserts the surrounding words are gone too.
func TestRejectionIsWholeMessageNotJustTheWord(t *testing.T) {
	s, l := fixture(t)
	learnOne(t, s, l, "peregrine thinks exampleslur about everything", snowflake(3), author(7))

	for _, prefix := range []string{"peregrine thinks", "thinks exampleslur", "about everything"} {
		if got := successors(t, s, prefix); len(got) != 0 {
			t.Errorf("prefix %q survived a blocked message: %v", prefix, got)
		}
	}
}

// TestNoEmptyPrefixIsWritten is finding 5. The old ingestion loop descended to n == 1, where
// the prefix is empty, so the entire vocabulary accumulated into one bbolt key holding a JSON
// map that nothing ever read.
func TestNoEmptyPrefixIsWritten(t *testing.T) {
	s, l := fixture(t)
	learnOne(t, s, l, "one two three four five", snowflake(4), author(7))

	if got := successors(t, s, ""); len(got) != 0 {
		t.Errorf("the empty prefix holds %d successors; that key is the finding-5 hot key", len(got))
	}
}

// TestTheBotIsExcludedFromAuthorDiversity is the anti-poisoning exclusion (A6).
//
// Self-learning feeds the bot's replies back into the corpus. Without this, anything it said
// once would carry a diversity count of one from the moment it said it, bootstrapped by the
// bot rather than by people, and the author-diversity gate is the strongest single control
// against poisoning.
//
// Verified by reverting: with the botID comparison removed from ngrams, the count becomes 1
// and this fails.
func TestTheBotIsExcludedFromAuthorDiversity(t *testing.T) {
	s, l := fixture(t)
	const botID = "424242424242424242"
	l.SetBotID(botID)

	bot := names.User{Name: "peregrine", UserID: botID, Username: "peregrine"}
	learnOne(t, s, l, "bird says the thing", snowflake(5), bot)

	got := successors(t, s, "bird says")["the"]
	if got.Count == 0 {
		t.Fatal("the bot's own output was not learned at all; self-learning is the point of this path")
	}
	if got.Authors != 0 {
		t.Errorf("the bot contributed %d to author diversity, want 0: its own output must not "+
			"bootstrap a phrase into eligibility (SPEC.md section 4, A6)", got.Authors)
	}
}

// TestDedupeByMessageID is finding 13's other half. The backfill re-reads recent history, and
// without the dedup window every pass would re-learn the same messages and double-count their
// n-grams.
func TestDedupeByMessageID(t *testing.T) {
	s, l := fixture(t)
	const id = "1700000000000000001"
	learnOne(t, s, l, "the bird flew away", id, author(7))
	learnOne(t, s, l, "the bird flew away", id, author(8))

	if got := successors(t, s, "the bird")["flew"]; got.Count != 1 {
		t.Errorf("count = %d after learning the same message ID twice, want 1", got.Count)
	}
}

// TestTheBotsOwnMentionIsStrippedBeforeLearning, so the corpus does not fill with the bot's
// user ID as a token.
func TestTheBotsOwnMentionIsStrippedBeforeLearning(t *testing.T) {
	s, l := fixture(t)
	const botID = "424242424242424242"
	l.SetBotID(botID)

	learnOne(t, s, l, "<@"+botID+"> the bird is loose", snowflake(6), author(7))

	if got := successors(t, s, "<@"+botID+">"); len(got) != 0 {
		t.Errorf("the bot's own mention was learned as a token: %v", got)
	}
	if got := successors(t, s, "the bird"); len(got) == 0 {
		t.Error("stripping the mention also lost the rest of the message")
	}
}

// TestAMessageThatIsOnlyAMentionLearnsNothing, because after stripping there is no content,
// and learning an empty message would mark its ID seen for no benefit.
func TestAMessageThatIsOnlyAMentionLearnsNothing(t *testing.T) {
	s, l := fixture(t)
	const botID = "424242424242424242"
	l.SetBotID(botID)

	if err := s.Update(func(w *storage.Writer) error {
		return l.Message(w, "<@"+botID+">", snowflake(7), author(7), nil)
	}); err != nil {
		t.Fatalf("Message: %v", err)
	}
	var learned uint64
	if err := s.View(func(r *storage.Reader) error {
		learned = r.Status().Learned
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if learned != 0 {
		t.Errorf("a message with nothing left after stripping was counted as learned (%d)", learned)
	}
}

// TestCooccurrenceIsWindowed is the remaining half of finding 12.
//
// The loop was all-pairs and therefore quadratic in message length, running inside the single
// write transaction that serializes every other write in the process. The window is also the
// more defensible model: "co-occurs anywhere in the same message" gets weaker the longer the
// message is.
func TestCooccurrenceIsWindowed(t *testing.T) {
	s, _ := fixture(t)
	gate := safety.NewGate(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	l := New(gate, Options{MaxNGram: 3, MaxHistory: 1000, CooccurrenceWindow: 2})

	// A named person is required, because both association indexes are gated on one.
	who := author(7)
	if err := s.Update(func(w *storage.Writer) error {
		return l.Message(w, "alpha bravo charlie delta echo foxtrot golf", snowflake(8), who, []names.User{who})
	}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		assoc, err := r.TopicWordsFor("alpha")
		if err != nil {
			return err
		}
		// Window 2, so alpha reaches bravo and charlie and nothing further.
		for _, near := range []string{"bravo", "charlie"} {
			if _, ok := assoc[near]; !ok {
				t.Errorf("%q is within the window of alpha but was not associated", near)
			}
		}
		for _, far := range []string{"echo", "foxtrot", "golf"} {
			if _, ok := assoc[far]; ok {
				t.Errorf("%q is outside the window of alpha but was associated anyway", far)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCooccurrenceWindowZeroIsUnbounded, which is the escape hatch for an operator who wants
// the old behaviour and knows what it costs.
func TestCooccurrenceWindowZeroIsUnbounded(t *testing.T) {
	s, _ := fixture(t)
	gate := safety.NewGate(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	l := New(gate, Options{MaxNGram: 3, MaxHistory: 1000, CooccurrenceWindow: 0})

	who := author(7)
	if err := s.Update(func(w *storage.Writer) error {
		return l.Message(w, "alpha bravo charlie delta echo foxtrot golf", snowflake(9), who, []names.User{who})
	}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		assoc, err := r.TopicWordsFor("alpha")
		if err != nil {
			return err
		}
		if _, ok := assoc["golf"]; !ok {
			t.Error("with a window of 0 every pair should be recorded, but the furthest word was not")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCooccurrenceRecordsBothDirections. The index stores the position of the ASSOCIATE, so
// (a, b) and (b, a) carry different position sums and the positional heuristics read them.
func TestCooccurrenceRecordsBothDirections(t *testing.T) {
	s, _ := fixture(t)
	gate := safety.NewGate(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	l := New(gate, Options{MaxNGram: 3, MaxHistory: 1000, CooccurrenceWindow: 5})

	who := author(7)
	if err := s.Update(func(w *storage.Writer) error {
		return l.Message(w, "alpha bravo", snowflake(10), who, []names.User{who})
	}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		forward, err := r.TopicWordsFor("alpha")
		if err != nil {
			return err
		}
		back, err := r.TopicWordsFor("bravo")
		if err != nil {
			return err
		}
		if _, ok := forward["bravo"]; !ok {
			t.Error("alpha -> bravo was not recorded")
		}
		if _, ok := back["alpha"]; !ok {
			t.Error("bravo -> alpha was not recorded; the index is direction-sensitive")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestStopWordsAreExcludedFromAssociations. Without this, "the" is the top association of
// every word in the corpus and the topic heuristics measure nothing. It is also half the
// reason clustering collapsed (finding 29).
func TestStopWordsAreExcludedFromAssociations(t *testing.T) {
	s, _ := fixture(t)
	gate := safety.NewGate(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	l := New(gate, Options{MaxNGram: 3, MaxHistory: 1000, CooccurrenceWindow: 5})

	who := author(7)
	if err := s.Update(func(w *storage.Writer) error {
		return l.Message(w, "peregrine and the bird", snowflake(11), who, []names.User{who})
	}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		assoc, err := r.TopicWordsFor("peregrine")
		if err != nil {
			return err
		}
		for _, stop := range []string{"and", "the"} {
			if _, ok := assoc[stop]; ok {
				t.Errorf("stop word %q was recorded as an association", stop)
			}
		}
		if _, ok := assoc["bird"]; !ok {
			t.Error("a real word was excluded along with the stop words")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- structural

// TestTheGateIsInsideMessageNotAtItsCallers is the structural half of A1, and it is the one
// that catches the regression that matters.
//
// Design principle 3 in SPEC.md: a check at one of four call sites is not a check. This parses
// the package and asserts the gate call is inside Learner.Message's own body. If someone
// hoists it to the callers for performance, or adds a fifth caller that forgets it, every
// behavioural test above still passes and this fails.
func TestTheGateIsInsideMessageNotAtItsCallers(t *testing.T) {
	fn := findMethod(t, "Message")
	if !containsCall(fn.Body, "CheckLearn") {
		t.Error("Learner.Message does not call CheckLearn. That check must live INSIDE this " +
			"function, because it has multiple callers and a check at one call site is not a " +
			"check. See SPEC.md section 4, A1.")
	}
}

// TestVerdictHasNoFieldForRewrittenText pins reject-never-launder at the type level.
//
// The learn path must not be able to express "here is a cleaned-up version of this message",
// because a rewritten message is still learned with its structure intact. This is checked
// structurally because it is a property of the API, not of any one caller.
func TestVerdictHasNoFieldForRewrittenText(t *testing.T) {
	// A DENYLIST rather than an allowlist, deliberately. An allowlist of the fields that
	// exist today would fail every time Verdict gains an innocuous one, which trains whoever
	// hits it to update the list without reading why it is there. The invariant is narrow:
	// nothing on this type may carry a cleaned-up copy of the message.
	forbidden := []string{"clean", "text", "rewrit", "replac", "sanit", "content", "filtered"}

	for _, name := range structFields(t, "safety", "Verdict") {
		lower := strings.ToLower(name)
		for _, bad := range forbidden {
			if strings.Contains(lower, bad) {
				t.Errorf("safety.Verdict has a field %q, which looks like it carries rewritten "+
					"text. It must not: a laundered message is still learned, with its structure "+
					"intact and a harmless token in the offending word's grammatical position, so "+
					"the bot has been taught the sentence (SPEC.md section 4, A5). Rejection on "+
					"the learn path means dropping the message whole", name)
			}
		}
	}
}

// findMethod returns the named method declaration from this package.
func findMethod(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()
	for _, file := range parsePackage(t) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Name.Name == name && fn.Recv != nil {
				return fn
			}
		}
	}
	t.Fatalf("method %s not found in this package", name)
	return nil
}

// parsePackage parses every non-test file here.
//
// Every file, not one named file: a structural check scoped to a single file stops being a
// check on the package the moment the package grows a second one.
func parsePackage(t *testing.T) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(".", "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	fset := token.NewFileSet()
	var files []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("parsed no files; the glob is wrong and this test proves nothing")
	}
	return files
}

// containsCall reports whether body calls a method with the given name on anything.
func containsCall(body *ast.BlockStmt, method string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
			found = true
		}
		return true
	})
	return found
}

// structFields returns the field names of a struct in another package of this module.
func structFields(t *testing.T, pkg, typeName string) []string {
	t.Helper()
	dir := filepath.Join("..", pkg)
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	var out []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || spec.Name.Name != typeName {
				return true
			}
			st, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				for _, ident := range field.Names {
					out = append(out, ident.Name)
				}
			}
			return false
		})
	}
	if len(out) == 0 {
		t.Fatalf("found no fields on %s.%s; this test proves nothing", pkg, typeName)
	}
	return out
}

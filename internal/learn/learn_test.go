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

// learnOne learns a message THE WAY THE BACKFILL DOES, with a nil mentioned list.
//
// It used to pass []names.User{who}, mirroring the live chat handler, which appended the author
// to that slice before calling. The backfill did not, and associate returns early on an empty
// name set, so every backfilled message learned no associations at all. Passing the author here
// is what hid that for a whole milestone: the shape every test exercised was the only shape that
// worked.
//
// Nil is now the interesting case, because Message merges the author in itself. Tests that want
// the other shape pass a list explicitly.
func learnOne(t *testing.T, s *storage.Store, l *Learner, text, msgID string, who names.User) {
	t.Helper()
	if err := s.Update(func(w *storage.Writer) error {
		return l.Message(w, text, msgID, who, nil)
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
		return l.Message(w, "alpha bravo charlie delta echo foxtrot golf", snowflake(8), who, nil)
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
		return l.Message(w, "alpha bravo charlie delta echo foxtrot golf", snowflake(9), who, nil)
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
		return l.Message(w, "alpha bravo", snowflake(10), who, nil)
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

// TestTheAuthorIsAlwaysAName is the regression pin for the backfill learning no associations.
//
// The live chat handler appended the author to its mentioned slice before calling Message; the
// backfill passed only the @mentions, which for most messages is nothing. associate returns
// early on an empty name set, so a backfilled message wrote NEITHER index: not name_topic, which
// every name-aware seed tier reads, and not topic_word, which tiers 3 and 6 and all of Jump
// read. A corpus is mostly backfill, so both stayed nearly empty while the bot looked like it
// was learning fine, because n-grams are written outside that guard.
//
// This is A1's shape one subsystem over: one entry point, two callers, one of them doing an
// extra step. Message merges the author in itself now, so a third caller cannot get it wrong.
//
// Verified by reverting: drop the authorName merge in Message and both halves of this fail, as
// do the four co-occurrence tests above, which now learn the way the backfill does.
func TestTheAuthorIsAlwaysAName(t *testing.T) {
	s, _ := fixture(t)
	gate := safety.NewGate(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	l := New(gate, Options{MaxNGram: 3, MaxHistory: 1000, CooccurrenceWindow: 5})

	// Nil, exactly as ingest.learner.Learn passes it for a message that mentions nobody.
	who := author(7)
	if err := s.Update(func(w *storage.Writer) error {
		return l.Message(w, "gigi posted another cursed image", snowflake(12), who, nil)
	}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		topics, err := r.NameTopicsFor(who.Username)
		if err != nil {
			return err
		}
		if len(topics) == 0 {
			t.Error("the author has no name-topic associations, so every name-aware seed tier " +
				"has nothing to read for anybody who was not @mentioned")
		}
		if _, ok := topics["cursed"]; !ok {
			t.Errorf("the author is not associated with their own vocabulary: %v", topics)
		}

		assoc, err := r.TopicWordsFor("cursed")
		if err != nil {
			return err
		}
		if len(assoc) == 0 {
			t.Error("no word-to-word associations were written, so tiers 3 and 6 and Jump have " +
				"nothing to read")
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
		return l.Message(w, "peregrine and the bird", snowflake(11), who, nil)
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
// TestEveryWriterEntryPointCallsCheckLearn.
//
// This began as a check that Learner.Message contains a CheckLearn call, which was the whole
// of A1's structural fix while Message was the only way into the corpus. M17 added
// Associations, and a test naming one method would have been blind to it: that is finding
// 31's shape exactly, a rule applied to one of two producers.
//
// So the claim is widened rather than duplicated. EVERY exported method on *Learner that
// takes a *storage.Writer must contain the call in its own body, which means a third entry
// point is covered by the rule that already exists rather than by somebody remembering.
//
// Deliberately literal about "in its own body". Routing both through a shared helper would
// satisfy a looser test and would put one hop between the entry point and the gate, which the
// next refactor turns into two.
func TestEveryWriterEntryPointCallsCheckLearn(t *testing.T) {
	entries := writerEntryPoints(t)
	if len(entries) < 2 {
		t.Fatalf("found %d writing entry points, expected at least Message and Associations: "+
			"if this test cannot see them it cannot enforce anything", len(entries))
	}

	for _, fn := range entries {
		if !containsCall(fn.Body, "CheckLearn") {
			t.Errorf("Learner.%s takes a *storage.Writer and does not call CheckLearn. That "+
				"check must live INSIDE every entry point, because a check at one of them is "+
				"not a check. See SPEC.md section 4, A1.", fn.Name.Name)
		}
	}
}

// writerEntryPoints returns every exported *Learner method that can write to the corpus.
func writerEntryPoints(t *testing.T) []*ast.FuncDecl {
	t.Helper()

	var out []*ast.FuncDecl
	for _, file := range parsePackage(t) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !fn.Name.IsExported() || fn.Body == nil {
				continue
			}
			if takesWriter(fn) {
				out = append(out, fn)
			}
		}
	}
	return out
}

// takesWriter reports whether a function has a *storage.Writer parameter.
func takesWriter(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, param := range fn.Type.Params.List {
		star, ok := param.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "storage" && sel.Sel.Name == "Writer" {
			return true
		}
	}
	return false
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

// TestAssociationsWritesOnlyTheTwoAssociationIndexes is the pin for the whole safety argument
// behind a second entry point (SPEC.md section 8, finding 46).
//
// The association re-walk re-reads history that has ALREADY been learned. If this method
// touched anything else, every message it repaired would have its n-grams counted twice, its
// author's weekly stat bumped again, or a live dedup entry evicted from a capped window.
// Asserting "associations changed" is the easy half; asserting nothing else moved is the half
// that matters, so every other counter is checked explicitly.
func TestAssociationsWritesOnlyTheTwoAssociationIndexes(t *testing.T) {
	s, l := fixture(t)
	who := author(7)

	// A message learned the normal way first, which is the state the re-walk finds: n-grams
	// present, associations missing because the old backfill wrote none.
	learnOne(t, s, l, "greg is coping about the queue", snowflake(400), who)

	type snapshot struct {
		ngrams  map[string]corpus.Successor
		learned uint64
		history uint64
		topics  uint64
		stat    corpus.WeeklyStat
		name    corpus.Name
	}
	take := func() snapshot {
		var snap snapshot
		if err := s.View(func(r *storage.Reader) error {
			snap.ngrams = map[string]corpus.Successor{}
			got, err := r.Successors("greg is")
			if err != nil {
				return err
			}
			for _, sc := range got {
				snap.ngrams[sc.Token] = sc
			}
			snap.learned = r.MessagesLearned()
			snap.history = r.HistoryCount()
			snap.topics = r.TotalTopicCount()
			snap.stat, _, _ = r.UserStat(who.UserID)
			snap.name, _, _ = r.Name(who.Username)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return snap
	}

	before := take()

	if err := s.Update(func(w *storage.Writer) error {
		return l.Associations(w, "greg is coping about the queue", who, nil)
	}); err != nil {
		t.Fatalf("Associations: %v", err)
	}

	after := take()

	if len(after.ngrams) != len(before.ngrams) {
		t.Errorf("the n-gram set changed size, %d to %d", len(before.ngrams), len(after.ngrams))
	}
	for tok, sc := range after.ngrams {
		if was := before.ngrams[tok]; sc.Count != was.Count {
			t.Errorf("n-gram %q count moved from %d to %d: re-reading history through this "+
				"method would double-count every message it repairs (finding 13)",
				tok, was.Count, sc.Count)
		}
	}
	if after.learned != before.learned {
		t.Errorf("messages-learned counter moved from %d to %d", before.learned, after.learned)
	}
	if after.history != before.history {
		t.Errorf("history count moved from %d to %d: filling the capped dedup window with old "+
			"message IDs would evict the live entries that stop real double-learning",
			before.history, after.history)
	}
	if after.topics != before.topics {
		t.Errorf("topic total moved from %d to %d; topic counts are written outside associate's "+
			"guard and were already correct", before.topics, after.topics)
	}
	if after.stat.Count != before.stat.Count {
		t.Errorf("the author's weekly count moved from %d to %d", before.stat.Count, after.stat.Count)
	}
	if after.name.Count != before.name.Count {
		t.Errorf("Name.Count moved from %d to %d: names.Record bumps it on every call, and the "+
			"historical pass already recorded these people", before.name.Count, after.name.Count)
	}

	// And the thing it is FOR actually happened.
	var assoc map[string]corpus.TopicAssoc
	if err := s.View(func(r *storage.Reader) error {
		var err error
		assoc, err = r.NameTopicsFor(who.Username)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(assoc) == 0 {
		t.Error("no name associations were written, so the pass repairs nothing")
	}
}

// TestAssociationsRefusesWhatTheGateRefuses, behaviourally, so the AST test is not the only
// thing holding the gate in place on this path.
func TestAssociationsRefusesWhatTheGateRefuses(t *testing.T) {
	s, l := fixture(t)
	who := author(7)

	if err := s.Update(func(w *storage.Writer) error {
		return l.Associations(w, "greg said exampleslur about the queue", who, nil)
	}); err != nil {
		t.Fatalf("Associations: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		for _, word := range []string{"greg", "exampleslur", "queue"} {
			got, err := r.TopicWordsFor(word)
			if err != nil {
				return err
			}
			if len(got) != 0 {
				t.Errorf("blocked content produced associations for %q: %v", word, got)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

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

	"go.etcd.io/bbolt"

	"github.com/6586x57890143/peregrine/internal/config"
	"github.com/6586x57890143/peregrine/internal/safety"
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
func gateFixture(t *testing.T) *bbolt.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "markov.db")
	store, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := EnsureBuckets(store); err != nil {
		t.Fatalf("EnsureBuckets: %v", err)
	}

	bl, err := safety.LoadBlocklist(writeBlocklist(t))
	if err != nil {
		t.Fatalf("LoadBlocklist: %v", err)
	}

	// Save and restore the globals, so these tests do not leak into each other or
	// into the cleanup tests in this package.
	oldDB, oldCfg, oldGate := db, cfg, gate
	t.Cleanup(func() { db, cfg, gate = oldDB, oldCfg, oldGate })

	db = store
	cfg = &config.Config{MaxNGram: 3, MaxHistory: 1000}
	gate = safety.NewGate(bl, slog.New(slog.NewTextHandler(io.Discard, nil)), false)

	return store
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

// markovKeys returns every key in the markov bucket, which is how these tests tell
// "learned" from "dropped" without depending on the key layout.
func markovKeys(t *testing.T, store *bbolt.DB) []string {
	t.Helper()
	var keys []string
	if err := store.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(MarkovBucket)).ForEach(func(k, _ []byte) error {
			keys = append(keys, string(k))
			return nil
		})
	}); err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	return keys
}

func learn(t *testing.T, store *bbolt.DB, msg, msgID string) {
	t.Helper()
	if err := store.Update(func(tx *bbolt.Tx) error {
		return learnMessage(tx, msg, msgID, "999", MentionedUser{Name: "tester", UserID: "1", Username: "tester"}, nil)
	}); err != nil {
		t.Fatalf("learnMessage: %v", err)
	}
}

func TestLearnMessageLearnsOrdinaryText(t *testing.T) {
	store := gateFixture(t)

	learn(t, store, "the bird is on the roof again", "m1")

	if len(markovKeys(t, store)) == 0 {
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
			store := gateFixture(t)
			learn(t, store, msg, "m1")

			if keys := markovKeys(t, store); len(keys) != 0 {
				t.Errorf("blocked content was learned: %d keys written, first %q", len(keys), keys[0])
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
	store := gateFixture(t)

	learn(t, store, "i think that exampleslur is why the bird left the roof", "m1")

	keys := markovKeys(t, store)
	if len(keys) != 0 {
		t.Fatalf("expected nothing learned, got %d keys", len(keys))
	}
	// And specifically not the innocuous fragments, which is what laundering would
	// have preserved.
	for _, k := range keys {
		for _, innocuous := range []string{"bird", "roof", "why the"} {
			if strings.Contains(k, innocuous) {
				t.Errorf("the message was laundered rather than dropped: key %q survived", k)
			}
		}
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
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "legacy.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse legacy.go: %v", err)
	}

	var learnFn *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "learnMessage" {
			learnFn = fn
			break
		}
	}
	if learnFn == nil {
		t.Fatal("learnMessage not found; if it was renamed, update this test rather than deleting it")
	}

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
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "legacy.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse legacy.go: %v", err)
	}

	// Counted from the AST rather than by grepping the source. A regexp attempt at
	// this counted five, because the function's own declaration looks exactly like a
	// call to a pattern that is not parsing Go.
	calls := 0
	ast.Inspect(file, func(n ast.Node) bool {
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

package discordguard_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNothingBypassesTheGuard is the structural pin for finding 8 and A3.
//
// A behavioural test cannot cover this. Mention suppression only matters once a request has
// been marshalled and sent to Discord, so a test that exercises a handler proves nothing about
// a call site it happens not to reach, and the whole problem was that fourteen sites existed
// and a rule at thirteen of them is not a rule. This package's other tests cover the wire
// form; this one's job is to prove that every send in the repository actually comes here.
//
// # It scans the whole module now, and that is the M11c change
//
// It used to scan internal/legacy, because that was where every send lived, and it permitted
// three adapter functions there by name. Both of those facts are gone: the sends are spread
// across internal/plugins and the adapters were deleted, so the scan covers every package and
// the allow list is EMPTY. That is the strongest form this check has taken: outside
// internal/discordguard, no function in this module may name a session method that speaks.
func TestNothingBypassesTheGuard(t *testing.T) {
	// Methods that send, edit, delete or react. Reads such as ChannelMessages and
	// ChannelMessage are deliberately absent: they cannot ping, and gating them would stop the
	// bot from learning, which is the asymmetry PAUSE_ALL_WRITES already has.
	//
	// A forbidden-call list is only as good as its enumeration. MessageReactionRemove was
	// added after the M10b split dropped a call to it and this test did not notice, because
	// the method was missing here. When the guard grows a method, this grows with it.
	forbidden := map[string]bool{
		"ChannelMessageSend":        true,
		"ChannelMessageSendComplex": true,
		"ChannelMessageSendReply":   true,
		"ChannelMessageSendEmbed":   true,
		"ChannelMessageEdit":        true,
		"ChannelMessageEditComplex": true,
		"ChannelMessageDelete":      true,
		"MessageReactionAdd":        true,
		"MessageReactionRemove":     true,

		// Interactions, added in M26 with the first slash command this bot has ever had.
		//
		// An interaction response is a SEND: it puts text in front of people, it can carry a
		// mention, and it is exactly as capable of saying something the operator has to answer
		// for as a channel message. Adding the handler without adding these would have opened a
		// speaking path that skips CheckEmit, PAUSE_ALL_WRITES and PEREGRINE_IGNORE_CHANNELS
		// all at once, which is the same gap the split left when MessageReactionRemove was
		// missing, on a bigger surface.
		//
		// Ephemeral does not exempt anything. "Only the person who asked can see it" narrows
		// who is harmed, not whether the bot said it, and the pause switch is about the bot
		// being quiet rather than about the audience.
		"InteractionRespond":      true,
		"InteractionResponseEdit": true,
		"FollowupMessageCreate":   true,

		// Registration is not speaking, and is here for the other reason Delete is in the
		// guard: every Discord write has one place that knows about it and logs it. A command
		// registered from somewhere else is a command nobody can find the source of.
		"ApplicationCommandCreate":        true,
		"ApplicationCommandBulkOverwrite": true,
	}

	// Empty, and that is the point. Every adapter that used to be permitted here has been
	// deleted, so a new entry means somebody is asking for an exception to the chokepoint and
	// a reviewer is looking at this list when they do.
	allowed := map[string]bool{}

	// The module root, from this package's directory.
	roots := []string{filepath.Join("..", ".."), filepath.Join("..", "..", "cmd")}
	seen := map[string]bool{}
	scanned := 0

	fset := token.NewFileSet()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "voicenotes", "node_modules":
					return fs.SkipDir
				}
				// This package is the one place these calls belong.
				if filepath.Base(path) == "discordguard" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			abs, aerr := filepath.Abs(path)
			if aerr == nil {
				if seen[abs] {
					return nil
				}
				seen[abs] = true
			}
			scanned++

			file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}

			// Walked function by function, so the enclosing name is known and an allow list
			// can be scoped to it rather than to the file.
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil || allowed[fn.Name.Name] {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || !forbidden[sel.Sel.Name] {
						return true
					}
					t.Errorf("%s calls %s directly (%s in %s). Every outbound Discord call goes "+
						"through internal/discordguard, which suppresses mentions, applies "+
						"CheckEmit and honours PAUSE_ALL_WRITES. Peregrine's output is Markov "+
						"text assembled from what users typed, so a learned mention is a corpus "+
						"token the generator will emit again (SPEC.md section 8, finding 8)",
						fn.Name.Name, sel.Sel.Name, fset.Position(sel.Pos()), path)
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	// A walk that found nothing would pass silently, which is the failure mode this whole
	// style of test has: it would look like an invariant holding rather than like a glob that
	// matched no files.
	if scanned < 20 {
		t.Fatalf("scanned only %d files; the walk is wrong and this test proves nothing", scanned)
	}
}

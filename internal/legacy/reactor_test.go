package legacy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/bwmarrin/discordgo"
)

// TestSelfLearnUsesTheReplysOwnMessageID is the pin for finding 6, and the bug it covers
// was silent DATA LOSS rather than an inefficiency.
//
// Self-learning called learnMessage with the USER's message ID, and so did the learn step.
// learnMessage dedupes on that ID through MarkSeen, so whichever transaction committed
// first marked the ID seen and the other became a no-op: either the user's message or the
// bot's reply was thrown away on every single interaction, and which one depended on a
// race between the self-learn goroutine and the main path.
//
// The test drives learnMessage directly with the two IDs, because that is where the dedup
// lives, and asserts that BOTH bodies of text end up in the corpus. With one shared ID
// only one of them does.
func TestSelfLearnUsesTheReplysOwnMessageID(t *testing.T) {
	s := gateFixture(t)

	const userID, replyID = 900, 901
	const userText = "userphrase alpha bravo charlie"
	const replyText = "replyphrase delta echo foxtrot"

	// The user's message, keyed by its own ID.
	if err := s.Update(func(w *storage.Writer) error {
		return learnMessage(w, userText, snowflake(userID), "999",
			MentionedUser{Name: "human", UserID: "1", Username: "human"}, nil)
	}); err != nil {
		t.Fatalf("learn user message: %v", err)
	}

	// The bot's reply, keyed by THE REPLY'S own ID. Under the old code this was
	// snowflake(userID) and the write below silently did nothing.
	if err := s.Update(func(w *storage.Writer) error {
		return learnMessage(w, replyText, snowflake(replyID), "999",
			MentionedUser{Name: "bot", UserID: "999", Username: "bot"}, nil)
	}); err != nil {
		t.Fatalf("learn bot reply: %v", err)
	}

	learned := strings.Join(learnedNgrams(t, s), " | ")
	if !strings.Contains(learned, "userphrase") {
		t.Error("the user's message is missing from the corpus")
	}
	if !strings.Contains(learned, "replyphrase") {
		t.Error("the bot's reply is missing from the corpus. Both were learned under one " +
			"message ID, so the dedup inside learnMessage discarded whichever committed " +
			"second: that is finding 6, and it lost one message per interaction")
	}
}

// TestSharedMessageIDLosesOneMessage demonstrates the bug the fix above avoids, so the
// test that pins the fix cannot pass for an unrelated reason.
//
// It asserts the OLD behaviour deliberately: given one ID for both, exactly one of the two
// bodies survives. If this ever stops being true the dedup has changed and the pin above
// needs rethinking rather than trusting.
func TestSharedMessageIDLosesOneMessage(t *testing.T) {
	s := gateFixture(t)

	const shared = 910
	if err := s.Update(func(w *storage.Writer) error {
		return learnMessage(w, "userphrase alpha bravo", snowflake(shared), "999",
			MentionedUser{Name: "human", UserID: "1", Username: "human"}, nil)
	}); err != nil {
		t.Fatalf("learn user message: %v", err)
	}
	if err := s.Update(func(w *storage.Writer) error {
		return learnMessage(w, "replyphrase delta echo", snowflake(shared), "999",
			MentionedUser{Name: "bot", UserID: "999", Username: "bot"}, nil)
	}); err != nil {
		t.Fatalf("learn bot reply: %v", err)
	}

	learned := strings.Join(learnedNgrams(t, s), " | ")
	hasUser := strings.Contains(learned, "userphrase")
	hasReply := strings.Contains(learned, "replyphrase")
	if hasUser && hasReply {
		t.Error("both messages survived a shared message ID, which means learnMessage is " +
			"no longer deduping on it and the reasoning behind finding 6 has changed")
	}
	if !hasUser && !hasReply {
		t.Error("neither message was learned, so this test is not measuring the dedup")
	}
}

// TestCommandsConsumeTheMessage is the pin for finding 9.
//
// The !leaderboard branch had no return, so after answering the command the handler
// carried on into the reply generator and the learn step: the bot replied to its own
// command as if it were conversation and then taught itself the string "!leaderboard".
//
// It calls stepCommands and asserts its RETURN VALUE, which is the thing that stops the
// run. The first version of this test called commandFor instead and therefore passed with
// the short-circuit removed, which made it a test whose name promised more than it
// checked. That was caught by reverting the fix, which is the whole reason for doing that.
func TestCommandsConsumeTheMessage(t *testing.T) {
	gateFixture(t)
	cfg.EnableWordGames = true
	wordGamesAvailable = true

	cases := map[string]bool{
		"!leaderboard":   true,
		"!LEADERBOARD":   true, // case-insensitive, as before
		"  !leaderboard": true, // and tolerant of surrounding space, which it was not before
		"!wordgame":      true,
		"hello there":    false,
		"":               false,
		// A command mentioned mid-sentence is conversation, not a command. Matching on a
		// prefix rather than the whole message would make talking ABOUT the command
		// impossible.
		"you should try !leaderboard sometime": false,
	}

	for content, wantConsumed := range cases {
		t.Run(content, func(t *testing.T) {
			r := &reaction{
				m:     &discordgo.MessageCreate{Message: &discordgo.Message{Content: content}},
				flags: map[string]bool{},
			}
			if got := stepCommands(r); got != wantConsumed {
				t.Errorf("stepCommands(%q) = %v, want %v. A command that does not consume "+
					"falls through into the reply and learn steps, so the bot answers its "+
					"own command and then learns it (finding 9)", content, got, wantConsumed)
			}
		})
	}
}

// TestCommandForMatchesTheWholeMessage covers the recognizer on its own, which is worth
// separating because the whole-message rule is a decision rather than an implementation
// detail: a prefix match would swallow any sentence that happens to mention a command.
func TestCommandForMatchesTheWholeMessage(t *testing.T) {
	cases := map[string]string{
		"!leaderboard":                         "!leaderboard",
		"!LEADERBOARD":                         "!leaderboard",
		"  !wordgame\t":                        "!wordgame",
		"!wordgame please":                     "",
		"you should try !leaderboard sometime": "",
		"leaderboard":                          "",
		"":                                     "",
	}
	for in, want := range cases {
		if got := commandFor(in); got != want {
			t.Errorf("commandFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestWordgameIsNotACommandWhenTheFeatureIsOff. With word games disabled, "!wordgame" is
// just a word somebody typed, so it must fall through and be treated as chat. Consuming it
// would mean the bot silently ignored a message for a feature that is not running.
func TestWordgameIsNotACommandWhenTheFeatureIsOff(t *testing.T) {
	gateFixture(t)
	cfg.EnableWordGames = false
	wordGamesAvailable = false

	r := &reaction{
		m:     &discordgo.MessageCreate{Message: &discordgo.Message{Content: "!wordgame"}},
		flags: map[string]bool{},
	}
	if stepCommands(r) {
		t.Error("!wordgame was consumed with word games disabled; it is not a command then, " +
			"and consuming it means the bot silently ignores the message")
	}
}

// TestStepsFormAValidPipeline checks the table itself: every step has a name and a
// function, names are unique, and the two orderings the doc comment calls load-bearing
// actually hold.
func TestStepsFormAValidPipeline(t *testing.T) {
	seen := map[string]int{}
	for i, st := range steps {
		if st.name == "" {
			t.Errorf("step %d has no name; the name is what the completion log line reports", i)
		}
		if st.fn == nil {
			t.Errorf("step %q has no function", st.name)
		}
		if prev, dup := seen[st.name]; dup {
			t.Errorf("step name %q is used at both %d and %d, so the log cannot say which ran",
				st.name, prev, i)
		}
		seen[st.name] = i
	}

	// The learn gate must be first, or the bot replies to and reacts to messages it
	// refuses to learn from.
	if len(steps) == 0 || steps[0].name != "learn-gate" {
		t.Error("the learn gate must run first")
	}

	// Commands must precede reply and learn, which is the whole point of the
	// short-circuit. This is finding 9 expressed as an ordering.
	cmd, reply, learn := seen["commands"], seen["reply"], seen["learn"]
	if cmd >= reply || cmd >= learn {
		t.Errorf("commands at %d must come before reply at %d and learn at %d, or answering "+
			"a command also replies to it and learns it", cmd, reply, learn)
	}
}

// TestOnlyCommandsConsume is the contract check, and it is a source check because it is a
// statement about intent that no fixture can express.
//
// The distinction that matters is between "I did something" and "this message was for me".
// The aggro step reacts and the reply step posts, and neither consumes, because a message
// that earns a reply is still conversation and must still be learned from. If a future
// step starts returning true, learning silently stops for whatever it matches, and no
// existing test would notice because they all assert about corpus contents for messages
// that reach the learn step.
func TestOnlyCommandsConsume(t *testing.T) {
	// The steps allowed to return a bare `true`. Everything else must only ever
	// `return false`.
	mayConsume := map[string]bool{
		"stepLearnGate": true, // a dropped message is not processed further, by design
		"stepCommands":  true, // finding 9: a command is not conversation
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "reactor.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse reactor.go: %v", err)
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !strings.HasPrefix(fn.Name.Name, "step") {
			continue
		}
		if mayConsume[fn.Name.Name] {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ret, ok := n.(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				return true
			}
			ident, ok := ret.Results[0].(*ast.Ident)
			if !ok || ident.Name != "true" {
				return true
			}
			t.Errorf("%s: %s returns true, consuming the message. Only a command may consume; "+
				"a step that merely acted must return false or the message stops being "+
				"learned from. If this is deliberate, add it to mayConsume in this test with "+
				"a reason.", fset.Position(ret.Pos()), fn.Name.Name)
			return true
		})
	}
}

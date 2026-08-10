package chat

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/generate"
	"github.com/6586x57890143/peregrine/internal/learn"
	"github.com/6586x57890143/peregrine/internal/names"
	"github.com/6586x57890143/peregrine/internal/plugins/images"
	"github.com/6586x57890143/peregrine/internal/safety"
	"github.com/6586x57890143/peregrine/internal/storage"
)

func snowflake(n int) string {
	return strconv.FormatUint((uint64(n)<<22)|1, 10)
}

// ---------------------------------------------------------------- fakes

// The features are interfaces this package declares, so the reactor is testable without five
// real plugins behind it. That is the reason for the seam, not a side effect of it.

type fakeGuard struct {
	mu      sync.Mutex
	replies []string
}

func (g *fakeGuard) SendReply(_, content string, _ *discordgo.MessageReference) (*discordgo.Message, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.replies = append(g.replies, content)
	return &discordgo.Message{ID: snowflake(990000 + len(g.replies))}, true
}

func (g *fakeGuard) sent() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.replies...)
}

type fakeActivity struct {
	mu    sync.Mutex
	notes []string
}

func (a *fakeActivity) Note(channelID, authorID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.notes = append(a.notes, channelID+"/"+authorID)
}

func (a *fakeActivity) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.notes)
}

type fakeAggro struct{ handled int }

func (a *fakeAggro) Handle(string, string, string) { a.handled++ }

type fakeImages struct {
	captured int
	reposts  int
	forgot   []string
}

func (i *fakeImages) Capture(string, string, string, string, []images.Attachment) { i.captured++ }
func (i *fakeImages) MaybeRepost(string, bool)                                    { i.reposts++ }
func (i *fakeImages) Forget(ids ...string)                                        { i.forgot = append(i.forgot, ids...) }

type fakeGames struct {
	guesses  int
	commands []string
	consume  bool
}

func (g *fakeGames) Guess(string, string, string, string, string) bool {
	g.guesses++
	return false
}

func (g *fakeGames) Command(cmd, _, _ string, _ func(string) string) bool {
	g.commands = append(g.commands, cmd)
	return g.consume
}

type fakeSpeaker struct {
	reply   string
	outcome generate.Outcome

	// What it was asked for, so the reply step's decisions are observable rather than
	// inferred from whether something was posted.
	prompts []string
	roasts  []bool

	// The whole Request, so the reply-chain rule is observable rather than inferred.
	reqs []generate.Request
}

func (s *fakeSpeaker) Sentence(req generate.Request) (string, generate.Outcome, error) {
	prompt, roast := req.Prompt, req.Roast
	_ = roast
	s.prompts = append(s.prompts, prompt)
	s.roasts = append(s.roasts, roast)
	s.reqs = append(s.reqs, req)
	return s.reply, s.outcome, nil
}

// fixture wires a reactor over fakes and a real corpus.
func fixture(t *testing.T) (*Service, *storage.Store, *fakeGuard, *fakeGames, *fakeActivity, *fakeImages) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "blocklist.txt")
	if err := os.WriteFile(path, []byte("slur \\bexampleslur\\b\n"), 0o600); err != nil {
		t.Fatalf("write blocklist: %v", err)
	}
	bl, err := safety.LoadBlocklist(path)
	if err != nil {
		t.Fatalf("LoadBlocklist: %v", err)
	}
	gate := safety.NewGate(bl, slog.New(slog.NewTextHandler(io.Discard, nil)), false)

	store := dbtest.Store(t)
	learner := learn.New(gate, learn.Options{MaxNGram: 3, MaxHistory: 1000, CooccurrenceWindow: 5})
	learner.SetBotID(snowflake(1))

	guard := &fakeGuard{}
	games := &fakeGames{}
	act := &fakeActivity{}
	imgs := &fakeImages{}

	s := New(Deps{
		Session:  nil, // no gateway: every step under test must work without one
		Store:    store,
		Gate:     gate,
		Guard:    guard,
		Learner:  learner,
		Speaker:  &fakeSpeaker{reply: "the bird is loose"},
		Memories: generate.NewMemories(0),
		Activity: act,
		Aggro:    &fakeAggro{},
		Images:   imgs,
		Games:    games,
		Options:  chatOptions(),
	})
	s.botID = snowflake(1)
	return s, store, guard, games, act, imgs
}

func chatOptions() Options {
	return Options{
		// The PRODUCTION default from internal/config, verbatim, so these tests exercise what
		// actually ships rather than a narrower pattern invented here. The fixture carried
		// `\bperegrine\b` until the keyword tests were written, which silently meant nothing
		// covered the "bird" half of the trigger at all.
		SelfMention:  regexp.MustCompile(`(?i)\b(peregrine|bird)\b`),
		RoastChance:  0.1,
		EnableImages: true,
		EnableVoice:  false,
	}
}

func message(content string) *reaction {
	return &reaction{
		m: &discordgo.MessageCreate{Message: &discordgo.Message{
			ID:        snowflake(4242),
			ChannelID: "c1",
			Content:   content,
			Author:    &discordgo.User{ID: snowflake(77), Username: "someone"},
		}},
		flags: map[string]bool{},
	}
}

// ---------------------------------------------------------------- finding 9

// TestCommandsConsumeTheMessage is the pin for finding 9.
//
// The !leaderboard branch had no return, so after answering the command the handler carried on
// into the reply generator and the learn step: the bot replied to its own command as if it were
// conversation and then taught itself the string "!leaderboard".
//
// It asserts stepCommands' RETURN VALUE, which is the thing that stops the run. An earlier
// version of this test called the recognizer instead and therefore passed with the
// short-circuit removed, which made it a test whose name promised more than it checked. That
// was caught by reverting the fix, which is the whole reason for doing that.
func TestCommandsConsumeTheMessage(t *testing.T) {
	s, _, _, games, _, _ := fixture(t)
	games.consume = true

	cases := map[string]bool{
		"!leaderboard":   true,
		"!LEADERBOARD":   true, // case-insensitive, as before
		"  !leaderboard": true, // and tolerant of surrounding space, which it was not before
		"!wordgame":      true,
		"hello there":    false,
		"":               false,
		// A command mentioned mid-sentence is conversation, not a command. Matching on a prefix
		// rather than the whole message would make talking ABOUT the command impossible.
		"you should try !leaderboard sometime": false,
	}

	for content, want := range cases {
		t.Run(content, func(t *testing.T) {
			if got := s.stepCommands(message(content)); got != want {
				t.Errorf("stepCommands(%q) = %v, want %v. A command that does not consume falls "+
					"through into the reply and learn steps, so the bot answers its own command "+
					"and then learns it (finding 9)", content, got, want)
			}
		})
	}
}

// TestACommandTheFeatureDeclinesIsNotConsumed. !wordgame with word games off is not a command,
// so it falls through and is treated as chat, which is what it is. Consuming it would mean the
// bot silently ignored a message for a feature that is not running.
func TestACommandTheFeatureDeclinesIsNotConsumed(t *testing.T) {
	s, _, _, games, _, _ := fixture(t)
	games.consume = false

	if s.stepCommands(message("!wordgame")) {
		t.Error("stepCommands consumed a command the feature declined to handle")
	}
	if len(games.commands) != 1 || games.commands[0] != "!wordgame" {
		t.Errorf("the command was not offered to the feature at all: %v", games.commands)
	}
}

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
	for content, want := range cases {
		if got := commandFor(content); got != want {
			t.Errorf("commandFor(%q) = %q, want %q", content, got, want)
		}
	}
}

// ---------------------------------------------------------------- finding 6

// TestSelfLearnUsesTheReplysOwnMessageID is the pin for finding 6, and the bug it covers was
// silent DATA LOSS rather than an inefficiency.
//
// Self-learning called the learn path with the USER's message ID, and so did the learn step.
// The learn path dedupes on that ID through MarkSeen, so whichever transaction committed first
// marked the ID seen and the other became a silent no-op: either the user's message or the
// bot's reply was thrown away on every single interaction, and which one depended on a race
// between the self-learn goroutine and the main path.
//
// The test drives the learn path directly with the two IDs, because that is where the dedup
// lives, and asserts that BOTH bodies of text end up in the corpus. With one shared ID only
// one of them does.
func TestSelfLearnUsesTheReplysOwnMessageID(t *testing.T) {
	_, store, _, _, _, _ := fixture(t)
	gate := safety.NewGate(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	learner := learn.New(gate, learn.Options{MaxNGram: 3, MaxHistory: 1000, CooccurrenceWindow: 5})

	const userText = "userphrase alpha bravo charlie"
	const replyText = "replyphrase delta echo foxtrot"
	who := names.User{Name: "u", UserID: snowflake(9), Username: "u"}

	if err := store.Update(func(w *storage.Writer) error {
		return learner.Message(w, userText, snowflake(900), who, nil)
	}); err != nil {
		t.Fatalf("learn the user's message: %v", err)
	}
	if err := store.Update(func(w *storage.Writer) error {
		return learner.Message(w, replyText, snowflake(901), who, nil)
	}); err != nil {
		t.Fatalf("learn the reply: %v", err)
	}

	for _, prefix := range []string{"userphrase alpha", "replyphrase delta"} {
		if !hasSuccessors(t, store, prefix) {
			t.Errorf("prefix %q is missing from the corpus; one of the two messages was "+
				"discarded by the dedup window (finding 6)", prefix)
		}
	}
}

// TestSharedMessageIDLosesOneMessage is the negative control. Without it, the test above could
// pass because the dedup window does nothing, and the finding would be unpinned.
func TestSharedMessageIDLosesOneMessage(t *testing.T) {
	_, store, _, _, _, _ := fixture(t)
	gate := safety.NewGate(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	learner := learn.New(gate, learn.Options{MaxNGram: 3, MaxHistory: 1000, CooccurrenceWindow: 5})

	const shared = "1500000000000000001"
	who := names.User{Name: "u", UserID: snowflake(9), Username: "u"}

	if err := store.Update(func(w *storage.Writer) error {
		return learner.Message(w, "userphrase alpha bravo", shared, who, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(w *storage.Writer) error {
		return learner.Message(w, "replyphrase delta echo", shared, who, nil)
	}); err != nil {
		t.Fatal(err)
	}

	if hasSuccessors(t, store, "replyphrase delta") {
		t.Error("both messages were learned under one ID, so the dedup window is not working " +
			"and the test above proves nothing about finding 6")
	}
}

func hasSuccessors(t *testing.T, s *storage.Store, prefix string) bool {
	t.Helper()
	found := false
	if err := s.View(func(r *storage.Reader) error {
		got, err := r.Successors(prefix)
		if err != nil {
			return err
		}
		found = len(got) > 0
		return nil
	}); err != nil {
		t.Fatalf("Successors(%q): %v", prefix, err)
	}
	return found
}

// ---------------------------------------------------------------- ordering and steps

// TestTheLearnGateRunsFirst. A message the bot will not learn from is also not replied to and
// not reacted to. That is a convenience rather than the protection (the protection is inside
// the learn path), but the ordering is still behaviour.
func TestTheLearnGateRunsFirst(t *testing.T) {
	if steps[0].name != "learn-gate" {
		t.Errorf("the first step is %q, want learn-gate: a message the corpus refuses should "+
			"not be replied to or reacted to either", steps[0].name)
	}
}

// TestActivityIsRecordedAfterTheLearnGate.
//
// The order is a decision, not an accident. That count decides where the bot speaks unprompted,
// which channels have earned a word game, and who gets aggro, so counting spam would let a
// flood advertise a channel as busy and pull the bot toward exactly the place it should be
// ignoring.
func TestActivityIsRecordedAfterTheLearnGate(t *testing.T) {
	gateIdx, actIdx := -1, -1
	for i, st := range steps {
		switch st.name {
		case "learn-gate":
			gateIdx = i
		case "activity":
			actIdx = i
		}
	}
	if gateIdx < 0 || actIdx < 0 {
		t.Fatal("expected both a learn-gate and an activity step")
	}
	if actIdx < gateIdx {
		t.Error("the activity step runs before the learn gate, so spam counts as activity")
	}

	// And behaviourally.
	s, _, _, _, act, _ := fixture(t)
	r := message("the bird should exampleslur")
	for _, st := range steps {
		if st.fn(s, r) {
			break
		}
	}
	if act.count() != 0 {
		t.Errorf("a blocked message counted as activity (%d recorded)", act.count())
	}
}

// TestCommandsRunBeforeReplyAndLearn is the ordering half of finding 9. Putting them after
// would answer the command and then also reply to it and learn it.
func TestCommandsRunBeforeReplyAndLearn(t *testing.T) {
	idx := map[string]int{}
	for i, st := range steps {
		idx[st.name] = i
	}
	cmd, ok := idx["commands"]
	if !ok {
		t.Fatal("there is no commands step")
	}
	for _, later := range []string{"reply", "learn"} {
		if i, ok := idx[later]; !ok || cmd >= i {
			t.Errorf("the commands step is not before %q; that ordering IS finding 9", later)
		}
	}
}

// TestOnlyCommandsConsume parses this package and fails if any step other than the command step
// can return true.
//
// This is the contract that closes finding 9, and it cannot be tested behaviourally: a step
// that starts consuming silently stops learning for whatever it matches, and every existing
// behavioural test still passes. Returning true means "this message was addressed to me", not
// "I did something": the aggro step reacts and the reply step posts, and neither consumes.
func TestOnlyCommandsConsume(t *testing.T) {
	// The steps allowed to return a bare true. Everything else must only ever return false,
	// and a new entry here is a deliberate edit with a reason attached.
	mayConsume := map[string]bool{
		"stepLearnGate": true, // a dropped message is not processed further, by design
		"stepCommands":  true, // finding 9: a command is not conversation
	}

	for _, file := range parsePackage(t) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "step") {
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
				t.Errorf("%s returns true, which consumes the message and stops the run (%s). "+
					"Only a command may consume: a message that earns a reply or a reaction is "+
					"still conversation and must still be learned from (finding 9)",
					fn.Name.Name, fset.Position(ret.Pos()))
				return true
			})
		}
	}
}

// TestEveryStepInTheTableExists, because a table entry naming a function that does not exist
// would not compile, but an entry MISSING from the table compiles fine and silently disables a
// step. This asserts every step function in the package is wired in.
func TestEveryStepInTheTableExists(t *testing.T) {
	inTable := map[string]bool{}
	for _, st := range steps {
		inTable[st.name] = true
	}

	declared := 0
	for _, file := range parsePackage(t) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(fn.Name.Name, "step") {
				continue
			}
			declared++
		}
	}
	if declared != len(steps) {
		t.Errorf("the package declares %d step functions but the table has %d entries. A step "+
			"missing from the table compiles and is silently never run", declared, len(steps))
	}
}

var fset = token.NewFileSet()

func parsePackage(t *testing.T) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(".", "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
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

// ---------------------------------------------------------------- steps in isolation

// TestBlockedContentIsDroppedBeforeAnythingElse.
func TestBlockedContentIsDroppedBeforeAnythingElse(t *testing.T) {
	s, _, guard, _, _, imgs := fixture(t)

	r := message("the bird should exampleslur")
	if !s.stepLearnGate(r) {
		t.Fatal("the learn gate allowed blocked content through")
	}
	if len(guard.sent()) != 0 {
		t.Errorf("something was sent for a blocked message: %v", guard.sent())
	}
	if imgs.captured != 0 {
		t.Error("a blocked message reached the image capture")
	}
}

// TestTheReplyStepOnlyFiresWhenAddressed. The bot is not a chatbot that answers everything;
// replying to every message would make it noise rather than chaos.
func TestTheReplyStepOnlyFiresWhenAddressed(t *testing.T) {
	s, _, guard, _, _, _ := fixture(t)

	r := message("just talking amongst ourselves")
	r.flags["TEXT"] = true
	if s.stepReply(r) {
		t.Error("the reply step consumed the message")
	}
	if len(guard.sent()) != 0 {
		t.Errorf("replied to a message that did not address the bot: %v", guard.sent())
	}

	r.flags["MENTIONED"] = true
	s.stepReply(r)
	if len(guard.sent()) != 1 {
		t.Errorf("did not reply to a message that mentioned the bot: %v", guard.sent())
	}
}

// TestAnEmptyGenerationPostsNothing. Returning empty is a normal outcome, not a failure: an
// empty corpus, a young one where the author gate refuses everything, or a dead-ended seed all
// produce it, and silence is what this bot does anyway when it decides not to answer.
func TestAnEmptyGenerationPostsNothing(t *testing.T) {
	s, _, guard, _, _, _ := fixture(t)
	s.speaker = &fakeSpeaker{reply: ""}

	r := message("hey peregrine")
	r.flags["TEXT"] = true
	r.flags["MENTIONED"] = true
	s.stepReply(r)

	if len(guard.sent()) != 0 {
		t.Errorf("posted %v for an empty generation, want silence", guard.sent())
	}
}

// TestTheLearnStepRecordsTheAuthorAndTheirMessage.
func TestTheLearnStepRecordsTheAuthorAndTheirMessage(t *testing.T) {
	s, store, _, _, _, _ := fixture(t)

	r := message("the bird is loose in the server")
	r.flags["TEXT"] = true
	// The name resolution needs a session; the reaction cache is primed instead, which is
	// exactly what the memoization exists for.
	r.mentioned, r.mentionedSet = nil, true

	if s.stepLearn(r) {
		t.Error("the learn step consumed the message")
	}
	if !hasSuccessors(t, store, "the bird") {
		t.Error("nothing was learned from an ordinary message")
	}

	var stats map[string]corpus.WeeklyStat
	if err := store.View(func(rd *storage.Reader) error {
		var err error
		stats, err = rd.AllUserStats()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := stats[snowflake(77)]; !ok {
		t.Error("the author's weekly count was not recorded, so the chat leaderboard is blind")
	}
}

// TestTheImageStepNeverConsumes, and captures before it reposts: the message being handled is a
// source of URLs whether or not the dice say to post one.
func TestTheImageStepNeverConsumes(t *testing.T) {
	s, _, _, _, _, imgs := fixture(t)

	if s.stepImages(message("look at this")) {
		t.Error("the image step consumed the message")
	}
	if imgs.captured != 1 {
		t.Errorf("Capture called %d times, want 1", imgs.captured)
	}
	if imgs.reposts != 1 {
		t.Errorf("MaybeRepost called %d times, want 1: the dice live in the feature, not here",
			imgs.reposts)
	}
}

// TestTheImageStepIsSkippedWhenTheFeatureIsOff, so nothing is cached for a consumer that does
// not exist. Continuing to store other people's media URLs in the operator's database with
// reposting off would be liability with no upside.
func TestTheImageStepIsSkippedWhenTheFeatureIsOff(t *testing.T) {
	s, _, _, _, _, imgs := fixture(t)
	s.opts.EnableImages = false

	s.stepImages(message("look at this"))
	if imgs.captured != 0 || imgs.reposts != 0 {
		t.Errorf("the image feature was used with the flag off: captured=%d reposts=%d",
			imgs.captured, imgs.reposts)
	}
}

// TestADeclinedReplyExplainsItself is the pin for finding 32.
//
// The bot was addressed, classified the message correctly (the flags line in the log proved
// it), decided it had nothing to say, and returned WITHOUT LOGGING ANYTHING. So a freshly
// deployed bot looked like a broken trigger, and telling the two apart meant reading the
// source. The autonomous poster logged the identical condition all along, which is what made
// the omission visible.
//
// The bot staying silent is the design. The operator being unable to tell why is the bug.
func TestADeclinedReplyExplainsItself(t *testing.T) {
	cases := map[generate.Outcome][]string{
		// Each reason has to point somewhere DIFFERENT, which is the whole reason the outcome
		// is typed rather than a bool: "corpus empty" sends an operator to ingestion and
		// "too short" sends them to the author gate.
		generate.CorpusEmpty: {"corpus is empty", "ingestion"},
		generate.TooShort:    {"word floor", "MIN_DISTINCT_AUTHORS"},
	}

	for outcome, wants := range cases {
		t.Run(outcome.String(), func(t *testing.T) {
			s, _, guard, _, _, _ := fixture(t)
			s.speaker = &fakeSpeaker{reply: "", outcome: outcome}

			var buf bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(previous) })

			r := message("hey peregrine")
			r.flags["TEXT"] = true
			r.flags["MENTIONED"] = true
			s.stepReply(r)

			if posts := guard.sent(); len(posts) != 0 {
				t.Errorf("posted %v for an empty generation, want silence", posts)
			}
			got := buf.String()
			if got == "" {
				t.Fatal("declined a reply and logged NOTHING; that is the whole finding")
			}
			for _, want := range wants {
				if !strings.Contains(got, want) {
					t.Errorf("the log does not mention %q, so it does not tell the operator "+
						"where to look:\n%s", want, got)
				}
			}
		})
	}
}

// TestTheKeywordTriggerRepliesWithoutAMention covers what the operator calls the main way to
// use the bot: saying "peregrine" or "bird" in ordinary conversation, with no @mention and no
// reply, and getting an answer.
//
// It had NO test until now, which is the gap worth closing rather than the behaviour. The path
// spans three files (config compiles the pattern, cmd/bot hands it to the reactor, the reactor
// classifies and then replies) and M11c moved the middle of it between packages. Nothing in the
// suite would have noticed if the flag had been dropped in that move: the reply step would just
// have stopped firing, which reads as an empty corpus.
func TestTheKeywordTriggerRepliesWithoutAMention(t *testing.T) {
	for _, word := range []string{"peregrine", "bird", "PEREGRINE", "the Bird is loose"} {
		t.Run(word, func(t *testing.T) {
			s, _, guard, _, _, _ := fixture(t)
			speaker := &fakeSpeaker{reply: "the server is doomed"}
			s.speaker = speaker

			r := message("has anyone seen " + word + " today")
			s.stepClassify(r)

			if !r.flags["SELF_MENTION_KEYWORD"] {
				t.Fatalf("%q did not set SELF_MENTION_KEYWORD; the pattern is not reaching the "+
					"reactor", word)
			}
			if r.flags["MENTIONED"] || r.flags["REPLY_TO_BOT"] {
				t.Fatal("the fixture accidentally mentioned the bot, so this proves nothing " +
					"about the keyword path on its own")
			}

			if s.stepReply(r) {
				t.Error("the reply step consumed the message; only a command may consume")
			}
			if posts := guard.sent(); len(posts) != 1 {
				t.Fatalf("posts = %v, want one reply to a keyword mention", posts)
			}

			// Overheard rather than addressed, so it ALWAYS roasts. That is a decision, not a
			// chance: the bot is being talked about.
			if len(speaker.roasts) != 1 || !speaker.roasts[0] {
				t.Errorf("roast = %v, want true: an overheard self-mention always roasts",
					speaker.roasts)
			}

			// THE WHOLE MESSAGE REACHES THE GENERATOR. This branch used to replace the prompt
			// with the fixed string "<START> peregrine", which discarded every other word in
			// it, so "peregrine what is up with lachy" lost "lachy" and no name tier or topic
			// tier ever saw the thing the message was actually about. On the path the operator
			// calls the main way to use the bot.
			//
			// It is still self-referential, and by construction rather than by a substitution:
			// this branch only runs when the self-mention pattern matched, so the keyword is
			// already in the prompt. Asserted below so that a future change cannot satisfy this
			// test by passing something that has lost the keyword.
			content := "has anyone seen " + word + " today"
			if len(speaker.prompts) != 1 || speaker.prompts[0] != content {
				t.Errorf("prompt = %q, want the message itself, %q", speaker.prompts, content)
			}
			if len(speaker.prompts) == 1 && !s.opts.SelfMention.MatchString(speaker.prompts[0]) {
				t.Errorf("prompt %q no longer contains the self-mention keyword, so the reply "+
					"has stopped being self-referential", speaker.prompts[0])
			}
		})
	}
}

// TestOrdinaryChatDoesNotTriggerAReply is the negative control. Without it the test above
// would pass just as well against a reactor that replied to everything, which would be the
// opposite bug and a far more annoying one.
func TestOrdinaryChatDoesNotTriggerAReply(t *testing.T) {
	for _, content := range []string{
		"has anyone seen the cat today",
		"birdsong is nice actually", // no word boundary, so the pattern must not match
		"rebirth",
		"",
	} {
		t.Run(content, func(t *testing.T) {
			s, _, guard, _, _, _ := fixture(t)

			r := message(content)
			s.stepClassify(r)
			if r.flags["SELF_MENTION_KEYWORD"] {
				t.Errorf("%q set SELF_MENTION_KEYWORD; the pattern is too loose", content)
			}
			s.stepReply(r)
			if posts := guard.sent(); len(posts) != 0 {
				t.Errorf("replied to ordinary chat: %v", posts)
			}
		})
	}
}

// ---------------------------------------------------------------- context (M19)

// replyTo builds a message that replies to another, the way the gateway delivers one:
// MessageReference plus the referenced message inline.
func replyTo(content string, ref *discordgo.Message) *discordgo.MessageCreate {
	m := message(content).m
	m.MessageReference = &discordgo.MessageReference{
		MessageID: ref.ID,
		ChannelID: m.ChannelID,
	}
	m.ReferencedMessage = ref
	return m
}

// TestTheReferencedMessageSteersButIsNotThePrompt is the pin for the one rule context obeys.
//
// The referenced message says what we are talking about; the prompt says what was said to me.
// Only the prompt may seed, both may steer. If the referenced content leaked into Prompt the
// bot would be answering the wrong message, and generation would be free to start a reply on a
// third party's phrasing.
func TestTheReferencedMessageSteersButIsNotThePrompt(t *testing.T) {
	s, _, _, _, _, _ := fixture(t)
	speaker := &fakeSpeaker{reply: "sure"}
	s.speaker = speaker

	ref := &discordgo.Message{
		ID:      snowflake(50),
		Content: "greg is coping about the queue",
		Author:  &discordgo.User{ID: snowflake(9), Username: "carol"},
	}
	m := replyTo("<@"+s.botID+"> what do you think", ref)

	s.handle(m)

	if len(speaker.reqs) != 1 {
		t.Fatalf("generation was asked %d times, want 1", len(speaker.reqs))
	}
	req := speaker.reqs[0]

	if strings.Contains(req.Prompt, "coping") {
		t.Errorf("the referenced message leaked into the prompt: %q", req.Prompt)
	}
	if !strings.Contains(req.Context, "coping") {
		t.Errorf("the referenced message did not reach Context: %q", req.Context)
	}
	if len(req.ContextNames) == 0 {
		t.Error("the referenced message's author was not offered as a context name")
	}
}

// TestTheBotsOwnMessageIsNotContext.
//
// Its output already re-enters the corpus through selfLearn. Feeding it back as context too is
// a loop that makes each reply more like the last one.
func TestTheBotsOwnMessageIsNotContext(t *testing.T) {
	s, _, _, _, _, _ := fixture(t)
	speaker := &fakeSpeaker{reply: "sure"}
	s.speaker = speaker

	ref := &discordgo.Message{
		ID:      snowflake(60),
		Content: "the bird is loose again",
		Author:  &discordgo.User{ID: s.botID, Username: "peregrine"},
	}
	s.handle(replyTo("<@"+s.botID+"> ok", ref))

	if len(speaker.reqs) != 1 {
		t.Fatalf("generation was asked %d times, want 1", len(speaker.reqs))
	}
	if speaker.reqs[0].Context != "" {
		t.Errorf("the bot's own message was used as context: %q", speaker.reqs[0].Context)
	}
}

// TestAForwardIsNotAReply.
//
// MessageReference.Type distinguishes a forward from a reply, and the old check ignored it, so
// a forwarded message triggered a REST fetch that could never identify a bot author. A forward
// also carries snapshots rather than a referenced message, so there is nothing here to answer.
func TestAForwardIsNotAReply(t *testing.T) {
	s, _, _, _, _, _ := fixture(t)
	speaker := &fakeSpeaker{reply: "sure"}
	s.speaker = speaker

	m := message("<@" + s.botID + "> look at this").m
	m.MessageReference = &discordgo.MessageReference{
		Type:      discordgo.MessageReferenceTypeForward,
		MessageID: snowflake(69),
		ChannelID: m.ChannelID,
	}
	m.ReferencedMessage = &discordgo.Message{
		ID:      snowflake(69),
		Content: "something forwarded",
		Author:  &discordgo.User{ID: snowflake(8), Username: "dave"},
	}

	s.handle(m)

	if len(speaker.reqs) != 1 {
		t.Fatalf("generation was asked %d times, want 1", len(speaker.reqs))
	}
	if speaker.reqs[0].Context != "" {
		t.Errorf("a forwarded message was treated as a reply chain: %q", speaker.reqs[0].Context)
	}
}

// TestTheBotHearsItself.
//
// selfLearn wrote the reply to the corpus and never added it to conversation memory, so the one
// participant present in every exchange was the only one missing from the channel's record of
// what was being discussed.
func TestTheBotHearsItself(t *testing.T) {
	s, _, guard, _, _, _ := fixture(t)
	s.speaker = &fakeSpeaker{reply: "absolutely cooked honestly"}

	m := message("<@" + s.botID + "> hello").m
	s.handle(m)

	if len(guard.replies) == 0 {
		t.Fatal("nothing was posted, so there is no reply to remember")
	}

	w := s.memories.For(m.ChannelID).Weights()
	if w["cooked"] == 0 {
		t.Errorf("the bot's own reply is absent from conversation memory: %v", w)
	}
}

// TestMemoryStoresNamesRatherThanMentionMarkup.
//
// stepReply and stepLearn both substitute mentions; memory was the one path that did not, so it
// stored <@123> blobs where a name belongs and those reached the recent seed tier as tokens
// that match nothing.
func TestMemoryStoresNamesRatherThanMentionMarkup(t *testing.T) {
	s, _, _, _, _, _ := fixture(t)

	m := message("hello <@" + snowflake(7) + "> how are you").m
	// Resolvable, because Substitute deliberately leaves an ID it does not know exactly as it
	// was: an unresolved mention is still a word in the middle of a sentence, and dropping it
	// would cost the structure around it.
	m.Mentions = []*discordgo.User{{ID: snowflake(7), Username: "greg", GlobalName: "greg"}}
	s.handle(m)

	for token := range s.memories.For(m.ChannelID).Weights() {
		if strings.HasPrefix(token, "<@") {
			t.Errorf("conversation memory stored raw mention markup: %q", token)
		}
	}
}

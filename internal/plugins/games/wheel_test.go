package games

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/activity"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/wheel"
	"github.com/6586x57890143/peregrine/internal/wordgame"
)

// M35: the wheel through the service, driven by presses and form submits the way Discord
// delivers them. The engine's rules are pinned in internal/wheel; these tests are about what
// reaches the channel, who is told what privately, and where the gold lands.

// steady is a wheel Source that always lands on 600 (wedge 1), never shuffles the seats, and
// draws the second candidate where it can, so a match is predictable without reaching into
// the engine.
type steady struct{}

func (steady) IntN(n int) int              { return 1 % n }
func (steady) Shuffle(int, func(i, j int)) {}

// fakeCounter says how busy a channel has been, for the repost decision. A fake rather than
// the real tracker, because the window is "since the card went up", and on a coarse clock a
// card posted and a message noted in the same tick would make the real one flaky.
type fakeCounter struct{ n int }

func (fakeCounter) Busiest(time.Duration) []activity.Channel { return nil }
func (c *fakeCounter) Count(string, time.Duration) int       { return c.n }

const (
	alice = "1001"
	bob   = "1002"
)

type wheelRig struct {
	t       *testing.T
	s       *Service
	guard   *fakeGuard
	counter *fakeCounter
}

func wheelFixture(t *testing.T, wopts wheel.Options, puzzles ...string) *wheelRig {
	t.Helper()
	if len(puzzles) == 0 {
		puzzles = []string{"phrase|hello world"}
	}
	path := filepath.Join(t.TempDir(), "puzzles.txt")
	if err := os.WriteFile(path, []byte(strings.Join(puzzles, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := wheel.LoadPuzzles(path)
	if err != nil {
		t.Fatal(err)
	}
	if wopts.Rounds == 0 {
		wopts.Rounds = 1
	}
	dict, err := wordgame.LoadDictionary("", wordgame.DictionaryOptions{MinLength: 5, MaxLength: 12})
	if err != nil {
		t.Fatal(err)
	}
	counter := &fakeCounter{}
	manager := wordgame.NewManager(dict, nil, counter, wordgame.Options{})
	guard := &fakeGuard{}
	opts := enabled()
	opts.Wheel = true
	s := New(dbtest.Set(t), guard, manager, wheel.NewManager(p, steady{}, wopts), counter,
		fakeChannels{"c1": {ID: "c1", Name: "memes", Text: true, GuildID: testGuild}}, nil, opts)
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatal(err)
	}
	return &wheelRig{t: t, s: s, guard: guard, counter: counter}
}

func member(userID string) *discordgo.Member {
	return &discordgo.Member{User: &discordgo.User{ID: userID, Username: "user" + userID}}
}

func wheelCommand(userID, name string) *discordgo.Interaction {
	return &discordgo.Interaction{
		Type:      discordgo.InteractionApplicationCommand,
		ChannelID: "c1",
		GuildID:   testGuild,
		Member:    member(userID),
		Data:      discordgo.ApplicationCommandInteractionData{Name: name},
	}
}

func (r *wheelRig) view() wheel.View {
	r.t.Helper()
	v, ok := r.s.wheels.Snapshot("c1")
	if !ok {
		r.t.Fatal("no live match")
	}
	return v
}

// press presses a button on the live card with the card's current turn token.
func (r *wheelRig) press(userID, action string) {
	r.t.Helper()
	v := r.view()
	r.pressID(userID, wheelID(action, v.Turn), v.MessageID)
}

func (r *wheelRig) pressID(userID, customID, messageID string) {
	r.s.onInteraction(nil, &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type:      discordgo.InteractionMessageComponent,
		ChannelID: "c1",
		GuildID:   testGuild,
		Member:    member(userID),
		Message:   &discordgo.Message{ID: messageID},
		Data:      discordgo.MessageComponentInteractionData{CustomID: customID},
	}})
}

// submit sends a form answer, shaped the way discordgo decodes one: pointer rows and inputs.
func (r *wheelRig) submit(userID, customID, text string) {
	r.s.onInteraction(nil, &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type:      discordgo.InteractionModalSubmit,
		ChannelID: "c1",
		GuildID:   testGuild,
		Member:    member(userID),
		Data: discordgo.ModalSubmitInteractionData{
			CustomID: customID,
			Components: []discordgo.MessageComponent{&discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{&discordgo.TextInput{CustomID: wheelInputID, Value: text}},
			}},
		},
	}})
}

// typed presses a button that opens a form, then submits the form it opened.
func (r *wheelRig) typed(userID, action, text string) {
	r.t.Helper()
	before := len(r.guard.modals)
	r.press(userID, action)
	if len(r.guard.modals) != before+1 {
		r.t.Fatalf("pressing %s opened no form; responses %v", action, r.guard.responded())
	}
	r.submit(userID, r.guard.modals[len(r.guard.modals)-1].customID, text)
}

func (r *wheelRig) lastResponse() response {
	r.t.Helper()
	rs := r.guard.responded()
	if len(rs) == 0 {
		r.t.Fatal("nothing was answered")
	}
	return rs[len(rs)-1]
}

func (r *wheelRig) wallet() map[string]int {
	r.t.Helper()
	store, err := r.s.corpora.For(testGuild)
	if err != nil {
		r.t.Fatal(err)
	}
	var w map[string]int
	if err := store.View(func(rd *storage.Reader) error {
		var err error
		w, err = readWallet(rd)
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return w
}

// playSolo takes alice through a one-round match and the bonus, winning both: 3 Ls at 600,
// then the bonus. steady draws the second bonus prize, 7,500.
func (r *wheelRig) playSolo() {
	r.t.Helper()
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	r.press(alice, actStart)
	r.press(alice, actSpin)
	r.typed(alice, actConsonant, "l")
	r.typed(alice, actSolve, "Hello, world!")
	r.typed(alice, actPick, "h d w o")
	r.typed(alice, actSolve, "hello world")
}

const soloGold = 1800 + 7500

func TestAFullMatchPaysGoldExactlyOnce(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.playSolo()

	if r.s.wheels.Active() != 0 {
		t.Fatal("the match is still live after the bonus")
	}
	if got := r.s.board(testGuild).Golds()[alice]; got != soloGold {
		t.Fatalf("weekly gold %d, want %d", got, soloGold)
	}
	if got := r.wallet()[alice]; got != soloGold {
		t.Fatalf("wallet %d, want %d", got, soloGold)
	}
	if !strings.Contains(strings.Join(r.guard.posts(), "\n"), "wins the wheel with 9,300 gold") {
		t.Errorf("the winner was not crowned in the channel:\n%s", strings.Join(r.guard.posts(), "\n---\n"))
	}
	// The final card lost its buttons: an empty row, not nil, so the update clears them.
	if comps := r.guard.lastComponents(); comps == nil || len(comps) != 0 {
		t.Errorf("the finished card kept components %v", comps)
	}

	posts := len(r.guard.posts())
	r.s.wheelSweep()
	r.s.wheelSweep()
	if r.s.board(testGuild).Golds()[alice] != soloGold || r.wallet()[alice] != soloGold || len(r.guard.posts()) != posts {
		t.Fatal("the sweep paid or announced a finished match a second time")
	}
}

// Board and wallet both reach the corpus, so a restart reads them back.
func TestAResultPersistsBoardAndWalletInOneWrite(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.playSolo()

	restarted := New(r.s.corpora, &fakeGuard{}, r.s.manager, nil, r.counter, r.s.resolver, nil, r.s.opts)
	if err := restarted.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatal(err)
	}
	if got := restarted.board(testGuild).Golds()[alice]; got != soloGold {
		t.Fatalf("after a restart the weekly board has %d gold, want %d", got, soloGold)
	}
}

func TestTheWalletSurvivesTheWeeklyReset(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.playSolo()
	r.s.board(testGuild).MaybeReset(time.Now().AddDate(0, 0, 8))

	r.s.handleWallet(wheelCommand(alice, walletCommandName))
	got := r.lastResponse()
	if !got.ephemeral || !strings.Contains(got.content, "9,300 gold in this server") || !strings.Contains(got.content, "0 this week") {
		t.Fatalf("wallet answer %+v", got)
	}
}

func TestANonCurrentPlayerGetsAnEphemeralNotYourTurn(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	r.press(bob, actJoin)
	r.press(alice, actStart)

	for _, action := range []string{actSolve, actSpin} {
		modals := len(r.guard.modals)
		r.press(bob, action)
		if got := r.lastResponse(); !got.ephemeral || got.content != "not your turn" {
			t.Fatalf("%s: %+v", action, got)
		}
		if len(r.guard.modals) != modals {
			t.Fatalf("%s opened a form for somebody whose turn it is not", action)
		}
	}
}

// The failure mode is an answer typed into a form while the turn timed out, landing on the
// same player's NEXT turn in a solo match.
func TestAStaleTokenIsAnsweredEphemerally(t *testing.T) {
	r := wheelFixture(t, wheel.Options{TurnTimeout: 20 * time.Millisecond})
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	r.press(alice, actStart)
	r.press(alice, actSolve)
	form := r.guard.modals[len(r.guard.modals)-1].customID

	time.Sleep(40 * time.Millisecond)
	r.s.wheelSweep()
	r.submit(alice, form, "hello world")
	if got := r.lastResponse(); !got.ephemeral || !strings.Contains(got.content, "too slow") {
		t.Fatalf("a stale answer got %+v", got)
	}
	if r.s.wheels.Active() != 1 {
		t.Fatal("the stale answer solved the round")
	}
}

// Every error the engine can return has a sentence. A new one without a row would reach the
// player as "something went wrong".
func TestEveryEngineRefusalHasAnEphemeralAnswer(t *testing.T) {
	for _, err := range []error{
		wheel.ErrNoPuzzles, wheel.ErrMatchInProgress, wheel.ErrTooManyMatches, wheel.ErrNoMatch,
		wheel.ErrWrongPhase, wheel.ErrLobbyFull, wheel.ErrAlreadyJoined, wheel.ErrNotJoined,
		wheel.ErrNotHost, wheel.ErrTooFewPlayers, wheel.ErrNotYourTurn, wheel.ErrStaleTurn,
		wheel.ErrSpinFirst, wheel.ErrMustCallConsonant, wheel.ErrBadLetter, wheel.ErrNoConsonantsLeft,
		wheel.ErrNoVowelsLeft, wheel.ErrCannotAffordVowel, wheel.ErrEmptySolve, wheel.ErrBadBonusPick,
	} {
		if _, known := wheelRefusal(err); !known {
			t.Errorf("no answer for %v", err)
		}
	}
	r := wheelFixture(t, wheel.Options{})
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	r.press(alice, actJoin)
	if got := r.lastResponse(); !got.ephemeral || got.content != "you're already in" {
		t.Fatalf("got %+v", got)
	}
}

// A card from before a restart: its buttons heal on first touch instead of failing forever.
func TestAPressOnADeadMatchReplacesItWithAnEndedCard(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.pressID(alice, wheelID(actSpin, 3), "99999")
	embeds := r.guard.posted()
	if r.guard.updates != 1 || len(embeds) != 1 || !strings.Contains(embeds[0].Description, "has ended") {
		t.Fatalf("updates %d embeds %v", r.guard.updates, embeds)
	}
	if comps := r.guard.lastComponents(); comps == nil || len(comps) != 0 {
		t.Errorf("the ended card kept components %v", comps)
	}
}

func TestARefusedLobbyPostAbandonsTheMatch(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.guard.refuseEmbed = true
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	if r.s.wheels.Active() != 0 {
		t.Fatal("an invisible lobby is still holding the channel")
	}
	if got := r.lastResponse(); !got.ephemeral || !strings.Contains(got.content, "could not post there") {
		t.Fatalf("got %+v", got)
	}
}

// A card that cannot be repainted (deleted, or the channel gone) is abandoned after a few
// tries rather than retried every second until the time limit.
func TestAFailedRepaintAbandonsTheMatch(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	if _, err := r.s.wheels.Act("c1", wheel.Action{Kind: wheel.Join, UserID: bob, Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	r.guard.refuseEdit = true
	for range maxPaintFailures - 1 {
		r.s.wheelSweep()
	}
	if r.s.wheels.Active() != 1 {
		t.Fatal("abandoned before the failure bound")
	}
	r.s.wheelSweep()
	if r.s.wheels.Active() != 0 {
		t.Fatal("still live after repeated failed repaints")
	}
	if len(r.s.wheelPostedAt) != 0 || len(r.s.wheelPaintFails) != 0 {
		t.Fatal("bookkeeping for an abandoned match leaked")
	}
}

func TestTheSweepRepaintsOnlyStaleMatches(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	r.press(bob, actJoin) // painted by the press itself
	r.s.wheelSweep()
	if len(r.guard.cardEdits) != 0 {
		t.Fatalf("the sweep repainted a card that was current: %d edits", len(r.guard.cardEdits))
	}

	// A change nobody pressed for, like a timeout.
	if _, err := r.s.wheels.Act("c1", wheel.Action{Kind: wheel.Leave, UserID: bob}); err != nil {
		t.Fatal(err)
	}
	r.s.wheelSweep()
	r.s.wheelSweep()
	if len(r.guard.cardEdits) != 1 {
		t.Fatalf("%d edits, want exactly one repaint", len(r.guard.cardEdits))
	}
	if e := r.guard.cardEdits[0]; e.messageID != r.view().MessageID {
		t.Fatalf("repainted %s, not the card", e.messageID)
	}
}

// One blocklisted guess must not make every later repaint fail the emit gate, so a wrong
// answer is never quoted anywhere the bot writes.
func TestAWrongSolveIsNotEchoed(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	r.press(alice, actStart)
	r.typed(alice, actSolve, "exampleslur nonsense")

	var all []string
	for _, e := range r.guard.posted() {
		all = append(all, flatten(e))
	}
	all = append(all, r.guard.posts()...)
	if joined := strings.Join(all, "\n"); strings.Contains(joined, "exampleslur") {
		t.Fatalf("the wrong guess was rendered:\n%s", joined)
	}
	if !strings.Contains(flatten(r.guard.posted()[len(r.guard.posted())-1]), "guessed wrong") {
		t.Error("the card does not say the guess was wrong")
	}
}

// A new round moves the card to the bottom only once the channel has moved on from it, and
// the new card goes up before the old one comes down.
func TestARoundStartRepostsOnlyAfterTraffic(t *testing.T) {
	for _, busy := range []bool{false, true} {
		r := wheelFixture(t, wheel.Options{Rounds: 2}, "phrase|hello world", "thing|good job")
		r.s.handleWheel(wheelCommand(alice, wheelCommandName))
		r.press(alice, actStart)
		first := r.view().MessageID
		if busy {
			r.counter.n = repostAfter
		}
		r.typed(alice, actSolve, r.solution())

		cards := len(r.guard.posted()) - r.guard.updates
		switch {
		case !busy && (r.view().MessageID != first || len(r.guard.deleted()) != 0):
			t.Fatal("a quiet channel had its card reposted")
		case busy && (r.view().MessageID == first || cards != 2):
			t.Fatalf("a busy channel kept its old card: %d cards", cards)
		case busy && (len(r.guard.deleted()) != 1 || r.guard.deleted()[0] != first):
			t.Fatalf("deleted %v, want the old card", r.guard.deleted())
		}
	}
}

// solution reads the live puzzle off the engine's own board once revealed, which a test can
// only do by revealing it: the phrase is not in the view until the match is over. steady
// draws the second puzzle for round one when there are two.
func (r *wheelRig) solution() string {
	if strings.Contains(r.view().Category, "thing") {
		return "good job"
	}
	return "hello world"
}

func TestWheelComponentsDoNotReachTheBoardHandler(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	before := len(r.guard.posted())
	r.press(bob, actJoin)
	if v := r.view(); len(v.Players) != 2 {
		t.Fatalf("the join did not reach the wheel: %+v", v.Players)
	}
	for _, e := range r.guard.posted()[before:] {
		if strings.Contains(e.Title, "leaderboard") {
			t.Fatal("a wheel press rendered a leaderboard")
		}
	}

	// And the reverse: a board press does not touch the match.
	r.s.board(testGuild).AddWin(alice, "a", time.Second, 5)
	r.pressID(alice, buttonID(scopeLocal, 1), "board")
	if last := r.guard.posted()[len(r.guard.posted())-1]; !strings.Contains(last.Title, "leaderboard") {
		t.Fatalf("a board press did not reach the board: %q", last.Title)
	}
	if len(r.view().Players) != 2 {
		t.Fatal("a board press changed the match")
	}
}

func TestWheelIsNotRegisteredWhenDisabledOrUnavailable(t *testing.T) {
	has := func(defs []*discordgo.ApplicationCommand, name string) bool {
		for _, d := range defs {
			if d.Name == name {
				return true
			}
		}
		return false
	}
	if defs := definitions(false, false); has(defs, wheelCommandName) || has(defs, walletCommandName) {
		t.Fatal("the wheel's commands were registered with the wheel off")
	}
	if defs := definitions(false, true); !has(defs, wheelCommandName) || !has(defs, walletCommandName) || !has(defs, boardCommandName) {
		t.Fatal("the wheel's commands are missing with the wheel on")
	}

	r := wheelFixture(t, wheel.Options{})
	r.s.wheels = wheel.NewManager(nil, nil, wheel.Options{})
	if r.s.wheelOn() {
		t.Fatal("the wheel reports on with no puzzles")
	}
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	if got := r.lastResponse(); !got.ephemeral || got.content != "the wheel is off right now" {
		t.Fatalf("got %+v", got)
	}
}

func TestWheelRefusesADMAndADisallowedChannel(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	dm := wheelCommand(alice, wheelCommandName)
	dm.GuildID, dm.Member, dm.User = "", nil, &discordgo.User{ID: alice}
	r.s.handleWheel(dm)
	if got := r.lastResponse(); !got.ephemeral || !strings.Contains(got.content, "server channel") {
		t.Fatalf("DM: %+v", got)
	}

	r.s.opts.AllowChannels = []string{"c2"}
	r.s.guilds = map[string]*guildState{} // settings seed from opts on first load
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	if got := r.lastResponse(); !got.ephemeral || !strings.Contains(got.content, "another channel") {
		t.Fatalf("disallowed channel: %+v", got)
	}
	if r.s.wheels.Active() != 0 {
		t.Fatal("a lobby opened where games may not run")
	}
}

func TestASecondWheelInTheChannelGetsAJumpLink(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	r.s.handleWheel(wheelCommand(bob, wheelCommandName))
	want := "https://discord.com/channels/" + testGuild + "/c1/" + r.view().MessageID
	if got := r.lastResponse(); !got.ephemeral || !strings.Contains(got.content, want) {
		t.Fatalf("got %+v, want a link to %s", got, want)
	}
}

// discordgo's own accessor type-asserts and panics when the interaction type and its data
// disagree, which are two independent fields off the wire.
func TestAMalformedModalPayloadIsIgnoredNotPanicked(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.s.onInteraction(nil, &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type:      discordgo.InteractionModalSubmit,
		ChannelID: "c1",
		GuildID:   testGuild,
		Member:    member(alice),
		Data:      discordgo.MessageComponentInteractionData{CustomID: "wof:solve:1"},
	}})
	r.submit(alice, "lb:local:1", "x")
	r.submit(alice, wheelID(actSpin, 1), "x") // a form the wheel never opens
	if len(r.guard.responded()) != 0 {
		t.Fatalf("a malformed or foreign form was answered: %v", r.guard.responded())
	}

	// Value-typed components, as a hand-built payload has them, read the same as pointers.
	id, text := modalText(&discordgo.Interaction{Data: discordgo.ModalSubmitInteractionData{
		CustomID: "wof:cons:2",
		Components: []discordgo.MessageComponent{discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{discordgo.TextInput{CustomID: wheelInputID, Value: "l"}},
		}},
	}})
	if id != "wof:cons:2" || text != "l" {
		t.Fatalf("modalText = %q %q", id, text)
	}
}

func TestAWheelIDRoundTripsAndRejectsForeignShapes(t *testing.T) {
	if a, turn, ok := parseWheelID(wheelID(actVowel, 12)); !ok || a != actVowel || turn != 12 {
		t.Fatalf("round trip: %q %d %v", a, turn, ok)
	}
	for _, bad := range []string{"", "lb:local:1", "wof:spin", "wof:dance:1", "wof:spin:x", "wof:spin:1:2"} {
		if _, _, ok := parseWheelID(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

// The board wraps at word boundaries into lines a phone shows without scrolling.
func TestTheBoardWrapsAtWordBoundaries(t *testing.T) {
	lines := wrapBoard("A RACCOON IN A TRENCH COAT")
	// "A RACCOON IN" is exactly 12 cells counting a cell per word gap, so it shares a line.
	want := []string{"A   R A C C O O N   I N", "A   T R E N C H   C O A T"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("wrapped %q, want %q", lines, want)
	}
	// Letters plus word gaps fit in 13 cells, and a line renders at most 2 x 13 - 1 characters
	// whatever the mix of words, because a word gap costs three characters and one cell.
	for _, l := range wrapBoard("_____________ ____ ___ __ _ A B C D E F G H") {
		if len(l) > 2*boardCells-1 {
			t.Errorf("line %q is wider than a phone", l)
		}
	}
}

// The press, not a sweep, repaints: one update per press and nothing left stale.
func TestAPressIsAnsweredWithTheRepaint(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	r.press(bob, actJoin)
	if r.guard.updates != 1 {
		t.Fatalf("%d updates for one press", r.guard.updates)
	}
	if stale := r.s.wheels.Stale(); len(stale) != 0 {
		t.Fatalf("a press left the card stale: %+v", stale)
	}
	card := r.guard.posted()[len(r.guard.posted())-1]
	if !strings.Contains(card.Fields[0].Value, "user"+bob) {
		t.Errorf("the repaint does not show the new player:\n%s", card.Fields[0].Value)
	}
}

// A payout reaches the wallet as a JSON map, which is what an operator reading the blob by
// hand after a dispute will find.
func TestTheWalletIsAPlainJSONMap(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.playSolo()
	store, _ := r.s.corpora.For(testGuild)
	_ = store.View(func(rd *storage.Reader) error {
		raw, err := rd.GetBlob(storage.BlobLeaderboard, walletKey)
		var m map[string]int
		if err != nil || json.Unmarshal(raw, &m) != nil || m[alice] != soloGold {
			t.Fatalf("wallet blob %s, err %v", raw, err)
		}
		return nil
	})
}

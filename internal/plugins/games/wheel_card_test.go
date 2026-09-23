package games

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/wheel"
	"github.com/6586x57890143/peregrine/internal/wheelart"
)

// M35f: the card's look, the recap between rounds, and reposts driven by conversation.

func isIndicator(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

// Two regional indicators side by side render as a flag, so HE would become a country. Every
// tile is followed by a separator, and this is the test that says so.
func TestTheBoardIsEmojiTilesThatNeverFormFlags(t *testing.T) {
	out := tiles("HE__O WORLD")
	var prev rune
	for _, r := range out {
		if isIndicator(r) && isIndicator(prev) {
			t.Fatalf("two indicators touch in %q", out)
		}
		prev = r
	}
	if !strings.Contains(out, hidden) || !strings.Contains(out, "🇭") || !strings.Contains(out, wordGap) {
		t.Fatalf("board %q", out)
	}
	if got := tiles("NO. 1 FAN!"); !strings.Contains(got, "1️⃣") || !strings.Contains(got, "❗") || !strings.Contains(got, "**.**") {
		t.Fatalf("punctuation and digits rendered as %q", got)
	}
}

// Eleven tiles to a line, counting a tile for each word gap, so a phone never wraps a word.
func TestTheBoardWrapsAtElevenTiles(t *testing.T) {
	lines := strings.Split(tiles("A RACCOON IN A TRENCH COAT"), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines: %q", len(lines), lines)
	}
	for _, l := range lines {
		cells := 0
		for _, r := range l {
			if isIndicator(r) || string(r) == hidden || string(r) == wordGap {
				cells++
			}
		}
		if cells > boardCells {
			t.Errorf("line %q is %d cells", l, cells)
		}
	}
}

func TestTheLetterTrackerDotsOutCalledLetters(t *testing.T) {
	f := lettersField([]rune("AEZ"))
	want := "`· B C D · F G H I J K L M`\n`N O P Q R S T U V W X Y ·`"
	if f.Value != want {
		t.Fatalf("letters:\n%s\nwant:\n%s", f.Value, want)
	}
}

func playView() wheel.View {
	return wheel.View{
		Phase: wheel.Round, Round: 1, Rounds: 3, Category: "phrase", Board: "HE__O WOR_D",
		Current: "a", Turn: 4, LastWedge: 7, CanSpin: true, Deadline: time.Unix(1_900_000_000, 0),
		Players: []wheel.PlayerView{
			{UserID: "a", Name: "alice", Round: 1800, Bank: 600},
			{UserID: "b", Name: "bob", Left: true, Bank: 250},
			{UserID: "c", Name: "carol"},
		},
	}
}

func TestTheScoreboardIsOneInlineFieldPerPlayer(t *testing.T) {
	e, _ := wheelCard(playView(), "")
	if len(e.Fields) != 4 || e.Fields[0].Name != "🔤 LETTERS LEFT" {
		t.Fatalf("fields %+v", e.Fields)
	}
	a, b, c := e.Fields[1], e.Fields[2], e.Fields[3]
	if !a.Inline || a.Name != "▶ alice" || !strings.Contains(a.Value, "**1,800**") || !strings.Contains(a.Value, "600") {
		t.Errorf("current player %+v", a)
	}
	if b.Name != "🚪 bob" || !strings.Contains(b.Value, "left") {
		t.Errorf("player who left %+v", b)
	}
	if c.Name != "carol" {
		t.Errorf("waiting player %+v", c)
	}
	if !strings.Contains(e.Description, "❯ **alice** to play") || !strings.Contains(e.Description, "<t:1900000000:R>") {
		t.Errorf("turn block:\n%s", e.Description)
	}
	if e.Author == nil || e.Author.Name != brand || e.Footer == nil || !strings.Contains(e.Title, "ROUND 1") {
		t.Errorf("chrome %+v %+v %q", e.Author, e.Footer, e.Title)
	}
}

// The strip under a spin shows where the wheel landed; everywhere else it is plain.
func TestTheStripHighlightsTheLastSpin(t *testing.T) {
	const base = "https://example.test/wheel/"
	v := playView()
	v.Last = []wheel.Event{{Kind: wheel.Spun, UserID: "a", Name: "alice", Wedge: wheel.Wedge{Kind: wheel.Bankrupt}}}
	if e, _ := wheelCard(v, base); e.Image == nil || e.Image.URL != base+wheelart.Name(7) {
		t.Fatalf("after a spin the image is %+v", e.Image)
	}
	v.Last = []wheel.Event{{Kind: wheel.WrongSolve, UserID: "a", Name: "alice"}}
	if e, _ := wheelCard(v, base); e.Image.URL != base+wheelart.Plain {
		t.Fatalf("without a spin the image is %q", e.Image.URL)
	}
}

func TestNoAssetURLMeansNoImage(t *testing.T) {
	if e, _ := wheelCard(playView(), ""); e.Image != nil {
		t.Fatalf("image %+v with no asset URL", e.Image)
	}
}

// A long puzzle, ten players with long names and a busy event line still fit Discord's limits,
// which reject the whole message rather than truncating it.
func TestTheCardStaysInsideDiscordLimits(t *testing.T) {
	v := playView()
	v.Board = "YOU CAN'T WIN THEM ALL, BUT TRY ANYWAY OK? 123"
	v.Players = nil
	for i := range 10 {
		v.Players = append(v.Players, wheel.PlayerView{
			UserID: strings.Repeat("9", 18), Name: strings.Repeat("W", 32) + string(rune('a'+i)), Round: 99999, Bank: 999999,
		})
	}
	v.Last = []wheel.Event{
		{Kind: wheel.Spun, Name: strings.Repeat("W", 32), Amount: 2500},
		{Kind: wheel.LetterHit, Name: strings.Repeat("W", 32), Letter: 'T', Count: 9, Amount: 22500},
	}
	for _, phase := range []wheel.Phase{wheel.Lobby, wheel.Round, wheel.Intermission, wheel.BonusPick, wheel.Done} {
		v.Phase = phase
		v.MaxPlayers = 10
		v.Recap = &wheel.Recap{Round: 1, Category: "phrase", Phrase: v.Board, SolverName: strings.Repeat("W", 32), Gold: 999999}
		e, _ := wheelCard(v, "https://example.test/")
		total := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description) + utf8.RuneCountInString(e.Footer.Text) + utf8.RuneCountInString(e.Author.Name)
		if utf8.RuneCountInString(e.Title) > 256 || utf8.RuneCountInString(e.Description) > 4096 || len(e.Fields) > 25 {
			t.Fatalf("phase %v: title %d description %d fields %d", phase, len(e.Title), len(e.Description), len(e.Fields))
		}
		for _, f := range e.Fields {
			if utf8.RuneCountInString(f.Name) > 256 || utf8.RuneCountInString(f.Value) > 1024 {
				t.Fatalf("phase %v: field %q is %d", phase, f.Name, len(f.Value))
			}
			total += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
		}
		if total > 6000 {
			t.Fatalf("phase %v: %d characters in total", phase, total)
		}
	}
}

// The recap is the answer and who got it, which the card used to skip straight past.
func TestTheRecapShowsTheSolvedPhraseAndTheSolver(t *testing.T) {
	r := wheelFixture(t, wheel.Options{Rounds: 2}, "phrase|hello world", "thing|good job")
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	r.press(alice, actStart)
	r.press(alice, actSpin)
	r.typed(alice, actConsonant, "g")
	r.typed(alice, actSolve, "good job")

	card := r.guard.posted()[len(r.guard.posted())-1]
	if !strings.Contains(card.Title, "ROUND 1 SOLVED") || !strings.Contains(card.Description, tiles("GOOD JOB")) ||
		!strings.Contains(card.Description, "**user"+alice+"** solved it") || !strings.Contains(card.Description, "**600**") {
		t.Fatalf("recap card:\n%s\n%s", card.Title, card.Description)
	}
	if card.Fields[0].Name != "🏆 STANDINGS" {
		t.Fatalf("recap fields %+v", card.Fields)
	}
	next := r.guard.lastComponents()[0].(discordgo.ActionsRow).Components[0].(discordgo.Button)
	if next.Label != "next round" {
		t.Fatalf("recap button %+v", next)
	}
	r.press(bob, actNext)
	if got := r.lastResponse(); got.content != "you're not in this game" {
		t.Fatalf("a stranger ended the recap: %+v", got)
	}
	r.press(alice, actNext)
	if v := r.view(); v.Phase != wheel.Round || v.Round != 2 {
		t.Fatalf("after next: %+v", v)
	}
}

// chat puts a live card up and ages it past the cooldown, as if people have been talking.
func (r *wheelRig) chat(messages int) string {
	r.t.Helper()
	r.s.mu.Lock()
	r.s.wheelPostedAt["c1"] = time.Now().Add(-time.Minute)
	r.s.mu.Unlock()
	r.counter.n = messages
	return r.view().MessageID
}

func TestChatterRepostsTheCard(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	r.press(alice, actStart)
	old := r.chat(defaultRepostAfter)
	r.s.wheelSweep()
	if v := r.view(); v.MessageID == old {
		t.Fatal("the card was not reposted")
	}
	if d := r.guard.deleted(); len(d) != 1 || d[0] != old {
		t.Fatalf("deleted %v, want the old card", d)
	}
	if st := r.s.wheels.Stale(); len(st) != 0 {
		t.Fatalf("the reposted card is stale: %+v", st)
	}
}

func TestAQuietChannelKeepsItsCard(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	old := r.chat(defaultRepostAfter - 1)
	r.s.wheelSweep()
	if r.view().MessageID != old || len(r.guard.deleted()) != 0 {
		t.Fatal("a card was reposted below the threshold")
	}
}

// However busy the channel, one card is reposted at most once per cooldown.
func TestRepostsAreRateLimited(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	r.chat(50)
	r.s.wheelSweep()
	first := r.view().MessageID
	r.s.wheelSweep()
	if r.view().MessageID != first || len(r.guard.deleted()) != 1 {
		t.Fatal("reposted twice inside the cooldown")
	}
}

// A thumb already on the old card when it was reposted: the press counts, it is acknowledged
// rather than answered with a repaint of a deleted message, and the live card catches up.
func TestAPressOnAReplacedCardStillCounts(t *testing.T) {
	r := wheelFixture(t, wheel.Options{})
	r.s.handleWheel(wheelCommand(alice, wheelCommandName))
	old := r.chat(defaultRepostAfter)
	r.s.wheelSweep()
	updates := r.guard.updates

	r.pressID(bob, wheelID(actJoin, 0), old)
	if len(r.view().Players) != 2 {
		t.Fatal("a press on the replaced card was dropped")
	}
	if r.guard.acks != 1 || r.guard.updates != updates {
		t.Fatalf("acks %d updates %d->%d", r.guard.acks, updates, r.guard.updates)
	}
	edits := len(r.guard.cardEdits)
	r.s.wheelSweep()
	if len(r.guard.cardEdits) != edits+1 || r.guard.cardEdits[edits].messageID != r.view().MessageID {
		t.Fatal("the live card was not repainted after a press on the old one")
	}
}

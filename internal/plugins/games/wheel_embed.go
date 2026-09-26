package games

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/wheel"
	"github.com/6586x57890143/peregrine/internal/wheelart"
	"github.com/6586x57890143/peregrine/internal/wordgame"
)

// The wheel's card, M35.
//
// A CARD, which is the call M28 made in the other direction for scramble puzzles. A puzzle is
// something the bot drops into a conversation every few minutes, so a boxed notice kept
// interrupting; a wheel match is something a person asked for, that a handful of people play
// by pressing buttons on it for ten minutes. That is the leaderboard's category, a notice
// people act on, and the box is what holds the buttons and the board together.
//
// Every part of an embed is used for one job: the author line is the brand, the title is the
// phase, the description is the board and whose turn it is, the fields are the letters left
// and one tile per player, the footer is the rules, and the image is the wheel strip. The strip
// is also what pins the card's width, since an embed is otherwise only as wide as its widest
// line and the card would change shape as the board did (see internal/wheelart).
//
// Everything is rendered from a wheel.View, so this file is pure: no state, no I/O, and the
// only user-controlled text on the card is nicknames, which the guard gates on every repaint.
// A wrong solve is never quoted, and the engine does not even carry it, because one
// blocklisted guess would otherwise make every later repaint of the match fail the gate.

// boardCells is the widest line of the board, in tiles, counting a tile for each word gap.
// Eleven emoji tiles fit a phone's embed without the client wrapping mid-word.
const boardCells = 11

// Phase colours: the one thing on the card readable at a glance while scrolling.
const (
	colourLobby = 0x3498DB
	colourPlay  = 0xF1C40F
	colourRecap = 0x2ECC71
	colourBonus = 0x9B59B6
	colourOver  = 0x95A5A6
)

const (
	brand     = "🎡 WHEEL OF FORTUNE"
	divider   = "▬▬▬▬▬▬▬▬▬▬▬▬▬▬▬▬▬▬▬▬"
	bar       = " ┃ "
	wordGap   = "🟩" // the board's backing: word breaks and the padding either side of a row
	tileGap   = " "
	hidden    = "⬜"
	alphabet1 = "ABCDEFGHIJKLM"
	alphabet2 = "NOPQRSTUVWXYZ"
)

// wheelCard renders a view and the buttons under it. assets is the base URL of the strip
// images, ending in a slash, or empty for no image. A finished match gets an EMPTY component
// slice rather than nil, because both the edit and the interaction update omit or null a nil
// one, and a finished card must lose its buttons.
func wheelCard(v wheel.View, assets string) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	e := &discordgo.MessageEmbed{
		Author: &discordgo.MessageEmbedAuthor{Name: brand},
		Footer: &discordgo.MessageEmbedFooter{Text: rulesFooter()},
	}
	var b strings.Builder

	switch v.Phase {
	case wheel.Lobby:
		e.Title, e.Color = "✦ SIGN-UPS OPEN ✦", colourLobby
		fmt.Fprintf(&b, "Press **✋ join** to play%s**%s** + ⭐ bonus\n", bar, plural(v.Rounds, "round"))
		fmt.Fprintf(&b, "⏳ starts %s, or when the 👑 host presses **▶️ start**", relative(v))
		e.Fields = []*discordgo.MessageEmbedField{lobbyField(v)}

	case wheel.Round, wheel.BonusPick, wheel.BonusSolve:
		e.Title, e.Color = fmt.Sprintf("ROUND %d ⁄ %d%s📜 %s", v.Round, v.Rounds, bar, upper(v.Category)), colourPlay
		if v.Phase != wheel.Round {
			e.Title, e.Color = fmt.Sprintf("⭐ BONUS ROUND%s%s", bar, upper(v.Category)), colourBonus
		}
		b.WriteString(tiles(v.Board))
		b.WriteString("\n" + divider + "\n")
		b.WriteString(turnBlock(v))
		writeEvent(&b, v.Last)
		e.Fields = append([]*discordgo.MessageEmbedField{lettersField(v.Called)}, playerFields(v)...)

	case wheel.Intermission:
		r := v.Recap
		e.Title, e.Color = fmt.Sprintf("✅ ROUND %d SOLVED%s📜 %s", r.Round, bar, upper(r.Category)), colourRecap
		b.WriteString(tiles(r.Phrase))
		b.WriteString("\n" + divider + "\n")
		fmt.Fprintf(&b, "🎉 **%s** solved it and banks **%s** gold\n", name(r.SolverName, r.SolverID), commas(r.Gold))
		next := fmt.Sprintf("round %d", r.Round+1)
		if r.Round >= v.Rounds {
			next = "the ⭐ bonus round"
		}
		fmt.Fprintf(&b, "-# ⏭️ %s starts %s, or press **next**", next, relative(v))
		e.Fields = []*discordgo.MessageEmbedField{standingsField(v)}

	default:
		e.Title, e.Color = endTitle(v), colourOver
		if v.Board != "" {
			b.WriteString(tiles(v.Board))
			b.WriteString("\n" + divider + "\n")
		}
		if line := endLine(v); line != "" {
			b.WriteString(line + "\n")
		}
		writeEvent(&b, v.Last)
		e.Fields = []*discordgo.MessageEmbedField{standingsField(v)}
		e.Footer.Text = "thanks for playing" + bar + "/game wheel to play again"
	}

	e.Description = strings.TrimRight(b.String(), "\n")
	if assets != "" {
		e.Image = &discordgo.MessageEmbedImage{URL: assets + stripFor(v)}
	}
	return e, wheelButtons(v)
}

// tiles renders the board as the show's wall: every row is boardCells tiles wide, words sit
// centred on it, and everything that is not a letter is a green backing tile. A letter is a
// regional indicator and a hidden letter a white tile.
//
// The gaps are a TILE rather than whitespace (M36). A wide space was the word break before,
// and on some clients it rendered barely wider than the gap between two letters, so a board of
// white tiles read as one long word. A coloured tile is the same width on every client, and
// padding each row to the full width is what makes the rows line up as a wall on a phone as
// well as a desktop.
//
// Tiles are still joined with a space, which is what stops two regional indicators side by
// side from rendering as a flag.
func tiles(board string) string {
	var lines []string
	var row []string
	flush := func() {
		pad := max(boardCells-len(row), 0)
		cells := append(slices.Repeat([]string{wordGap}, pad/2), row...)
		cells = append(cells, slices.Repeat([]string{wordGap}, pad-pad/2)...)
		lines = append(lines, strings.Join(cells, tileGap))
		row = nil
	}
	for _, w := range strings.Fields(board) {
		n := len([]rune(w))
		if len(row) > 0 && len(row)+1+n > boardCells {
			flush()
		}
		if len(row) > 0 {
			row = append(row, wordGap)
		}
		for _, r := range w {
			row = append(row, tile(r))
		}
	}
	if len(row) > 0 {
		flush()
	}
	return strings.Join(lines, "\n")
}

func tile(r rune) string {
	switch {
	case r == '_':
		return hidden
	case r >= 'A' && r <= 'Z':
		return string(rune(0x1F1E6 + (r - 'A')))
	case r >= '0' && r <= '9':
		return string(r) + "️⃣" // keycap
	case r == '!':
		return "❗"
	case r == '?':
		return "❓"
	}
	return "**" + string(r) + "**"
}

// turnBlock is whose turn it is, until when, and what they can do.
func turnBlock(v wheel.View) string {
	who := "somebody"
	for _, p := range v.Players {
		if p.UserID == v.Current {
			who = displayName(p)
		}
	}
	head := fmt.Sprintf("❯ **%s** to play%s⏳ %s\n", who, bar, relative(v))
	switch {
	case v.Phase == wheel.BonusPick:
		return head + "⭐ pick **3 consonants** and **1 vowel**, R S T L N E are free"
	case v.Phase == wheel.BonusSolve:
		return head + "⭐ **one guess** at the bonus puzzle"
	case v.Pending != nil:
		return head + fmt.Sprintf("🎯 **%s** on the wheel%scall a **consonant**", commas(v.Pending.Value), bar)
	case !v.CanSpin:
		return head + "🔤 consonants are gone" + bar + "**buy a vowel** or **solve**"
	}
	return head + "🎡 **spin**" + bar + "🅰️ **buy a vowel**" + bar + "💡 **solve**"
}

func writeEvent(b *strings.Builder, evs []wheel.Event) {
	if line := eventLine(evs); line != "" {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\n')
		}
		b.WriteString("-# 💬 " + line)
	}
}

// lettersField is the alphabet with the called letters dotted out, so what is left to call
// reads at a glance. In a code span, so the two rows line up.
func lettersField(called []rune) *discordgo.MessageEmbedField {
	gone := map[rune]bool{}
	for _, r := range called {
		gone[r] = true
	}
	row := func(letters string) string {
		out := make([]string, 0, len(letters))
		for _, r := range letters {
			if gone[r] {
				out = append(out, "·")
			} else {
				out = append(out, string(r))
			}
		}
		return "`" + strings.Join(out, " ") + "`"
	}
	return &discordgo.MessageEmbedField{Name: "🔤 LETTERS LEFT", Value: row(alphabet1) + "\n" + row(alphabet2)}
}

// playerFields is one inline field per seat, which Discord lays out three to a row on a
// desktop: a scoreboard grid rather than a list.
func playerFields(v wheel.View) []*discordgo.MessageEmbedField {
	out := make([]*discordgo.MessageEmbedField, 0, len(v.Players))
	for _, p := range v.Players {
		f := &discordgo.MessageEmbedField{Inline: true}
		switch {
		case p.Left:
			f.Name = "🚪 " + displayName(p)
			f.Value = fmt.Sprintf("🏦 %s\n*left*", commas(p.Bank))
		default:
			f.Name = displayName(p)
			if p.UserID == v.Current {
				f.Name = "▶ " + f.Name
			}
			f.Value = fmt.Sprintf("💰 **%s**\n🏦 %s", commas(p.Round), commas(p.Bank))
		}
		out = append(out, f)
	}
	return out
}

func lobbyField(v wheel.View) *discordgo.MessageEmbedField {
	var b strings.Builder
	for _, p := range v.Players {
		b.WriteString("• **" + displayName(p) + "**")
		if p.UserID == v.HostID {
			b.WriteString(" 👑")
		}
		b.WriteByte('\n')
	}
	return &discordgo.MessageEmbedField{
		Name:  fmt.Sprintf("👥 PLAYERS %d ⁄ %d", len(v.Players), v.MaxPlayers),
		Value: strings.TrimRight(b.String(), "\n"),
	}
}

// standingsField ranks the players by what they banked, which is what gets paid, with a bar
// against the leader.
func standingsField(v wheel.View) *discordgo.MessageEmbedField {
	ps := ranked(v.Players)
	top := 0
	if len(ps) > 0 {
		top = ps[0].Bank
	}
	var b strings.Builder
	for i, p := range ps {
		rank := fmt.Sprintf("`%2d`", i+1)
		if i < len(medals) && p.Bank > 0 {
			rank = medals[i]
		}
		fmt.Fprintf(&b, "%s **%s**%s%s %s\n", rank, displayName(p), sep, meter(p.Bank, top), commas(p.Bank))
	}
	return &discordgo.MessageEmbedField{Name: "🏆 STANDINGS", Value: strings.TrimRight(b.String(), "\n")}
}

// ranked orders seats by bank, ties in seat order, which is the engine's own tie-break for
// the winner. An insertion sort: at most ten seats, and it is stable.
func ranked(players []wheel.PlayerView) []wheel.PlayerView {
	ps := append([]wheel.PlayerView(nil), players...)
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j].Bank > ps[j-1].Bank; j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
	return ps
}

// meter is a six-segment bar of n against the leader's top.
func meter(n, top int) string {
	const width = 6
	full := 0
	if top > 0 {
		full = (n*width + top - 1) / top
	}
	return strings.Repeat("▰", full) + strings.Repeat("▱", width-full)
}

// endTitle crowns the winner by the engine's rule: the top bank still playing, ties to the
// earlier seat, and nobody when nothing was banked.
func endTitle(v wheel.View) string {
	if v.Phase == wheel.Done {
		for _, p := range ranked(v.Players) {
			if !p.Left && p.Bank > 0 {
				return "🏆 " + upper(displayName(p)) + " WINS"
			}
		}
	}
	return "GAME OVER"
}

// endLine says how the match ended, when it was not simply played out.
func endLine(v wheel.View) string {
	if v.Phase == wheel.Aborted || v.Reason != wheel.Completed {
		if r := endReason(v.Reason); r != "" {
			return "🛑 " + r
		}
	}
	return ""
}

// stripFor is the banner file for a view: the landed wedge lit under a spin, plain otherwise.
func stripFor(v wheel.View) string {
	if v.LastWedge >= 0 && (v.Phase == wheel.Round) {
		for _, e := range v.Last {
			if e.Kind == wheel.Spun {
				return wheelart.Name(v.LastWedge)
			}
		}
	}
	return wheelart.Plain
}

func rulesFooter() string {
	return fmt.Sprintf("🅰️ vowels %d%s💥 BANKRUPT x2%s⏭️ LOSE A TURN x1%s⭐ bonus up to 25k",
		wheel.VowelCost, bar, bar, bar)
}

func relative(v wheel.View) string {
	return "<t:" + strconv.FormatInt(v.Deadline.Unix(), 10) + ":R>"
}

func upper(s string) string { return strings.ToUpper(s) }

func displayName(p wheel.PlayerView) string { return name(p.Name, p.UserID) }

func name(n, id string) string {
	if n == "" {
		n = id
	}
	return wordgame.TruncateRunes(n, nameWidth)
}

// eventLine says what the last change did, from the engine's structured events. Structured
// rather than pre-rendered, so a repaint from the sweep says the same thing the press did.
func eventLine(evs []wheel.Event) string {
	var parts []string
	for _, e := range evs {
		who := "**" + name(e.Name, e.UserID) + "**"
		var s string
		switch e.Kind {
		case wheel.Joined:
			s = who + " joined"
		case wheel.Left:
			s = who + " left"
		case wheel.Spun:
			switch e.Wedge.Kind {
			case wheel.Bankrupt:
				s = who + " hit 💥 **BANKRUPT**"
			case wheel.LoseTurn:
				s = who + " landed on ⏭️ **LOSE A TURN**"
			default:
				s = who + " spun **" + commas(e.Amount) + "**"
			}
		case wheel.LetterHit:
			if e.Amount < 0 {
				s = fmt.Sprintf("%s bought %s (x%d)", who, letterName(e.Letter), e.Count)
			} else {
				s = fmt.Sprintf("%s found **%d %c** (+%s)", who, e.Count, e.Letter, commas(e.Amount))
			}
		case wheel.LetterMiss:
			s = fmt.Sprintf("%s called **%c**: none there", who, e.Letter)
		case wheel.LetterRepeat:
			s = fmt.Sprintf("%s called **%c** again", who, e.Letter)
		case wheel.Solved:
			s = fmt.Sprintf("%s solved **%s** (+%s)", who, e.Text, commas(e.Amount))
		case wheel.WrongSolve:
			s = who + " guessed wrong"
		case wheel.TimedOut:
			s = who + " ran out of time"
		case wheel.StruckOut:
			s = who + " is out for idling"
		case wheel.BonusStart:
			s = who + " plays the ⭐ bonus round"
		case wheel.BonusPicked:
			s = who + " picked **" + strings.Join(strings.Split(e.Text, ""), " ") + "**"
		case wheel.BonusWon:
			s = fmt.Sprintf("%s won the bonus: **+%s**", who, commas(e.Amount))
		case wheel.BonusLost:
			s = fmt.Sprintf("%s missed the bonus, worth **%s**", who, commas(e.Amount))
		}
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, sep)
}

func letterName(r rune) string {
	if strings.ContainsRune("AEIOU", r) {
		return "an **" + string(r) + "**"
	}
	return "a **" + string(r) + "**"
}

func endReason(r wheel.Reason) string {
	switch r {
	case wheel.TimeLimit:
		return "out of time"
	case wheel.NoPlayers:
		return "everybody left"
	case wheel.TooFewPlayers:
		return "not enough players signed up"
	}
	return ""
}

// wheelButtons are the controls for the phase. Every id carries the turn token, so a press
// aimed at a turn that has since moved on is refused rather than applied to the next one.
// Labels and their emoji are this repository's constants, which is why the guard does not
// gate them.
func wheelButtons(v wheel.View) []discordgo.MessageComponent {
	btn := func(emoji, label, action string, style discordgo.ButtonStyle, disabled bool) discordgo.MessageComponent {
		return discordgo.Button{
			Label: label, Style: style, CustomID: wheelID(action, v.Turn), Disabled: disabled,
			Emoji: &discordgo.ComponentEmoji{Name: emoji},
		}
	}
	var row []discordgo.MessageComponent
	switch v.Phase {
	case wheel.Lobby:
		row = []discordgo.MessageComponent{
			btn("✋", "join", actJoin, discordgo.PrimaryButton, false),
			btn("▶️", "start", actStart, discordgo.SuccessButton, false),
			btn("🚪", "leave", actLeave, discordgo.SecondaryButton, false),
		}
	case wheel.Round:
		row = []discordgo.MessageComponent{
			btn("🎡", "spin", actSpin, discordgo.PrimaryButton, !v.CanSpin),
			btn("🔤", "consonant", actConsonant, discordgo.PrimaryButton, v.Pending == nil),
			btn("🅰️", "vowel", actVowel, discordgo.SecondaryButton, v.Pending != nil || !v.CanVowel),
			btn("💡", "solve", actSolve, discordgo.SuccessButton, v.Pending != nil),
			btn("🚪", "leave", actLeave, discordgo.DangerButton, false),
		}
	case wheel.Intermission:
		label := "next round"
		if v.Recap != nil && v.Recap.Round >= v.Rounds {
			label = "to the bonus"
		}
		row = []discordgo.MessageComponent{
			btn("⏭️", label, actNext, discordgo.SuccessButton, false),
			btn("🚪", "leave", actLeave, discordgo.SecondaryButton, false),
		}
	case wheel.BonusPick:
		row = []discordgo.MessageComponent{
			btn("⭐", "pick letters", actPick, discordgo.PrimaryButton, false),
			btn("🚪", "leave", actLeave, discordgo.DangerButton, false),
		}
	case wheel.BonusSolve:
		row = []discordgo.MessageComponent{
			btn("💡", "solve", actSolve, discordgo.SuccessButton, false),
			btn("🚪", "leave", actLeave, discordgo.DangerButton, false),
		}
	default:
		return []discordgo.MessageComponent{}
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: row}}
}

// endedCard replaces the buttons of a card for a match this process no longer holds: one that
// finished, or one that was live when the bot restarted. The dead buttons heal on first touch,
// so a restart needs no shutdown edit.
func endedCard() (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	return &discordgo.MessageEmbed{
		Author:      &discordgo.MessageEmbedAuthor{Name: brand},
		Title:       "GAME OVER",
		Description: "this game has ended" + bar + "**/game wheel** starts a new one",
		Color:       colourOver,
	}, []discordgo.MessageComponent{}
}

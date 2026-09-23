package games

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/wheel"
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
// Everything on it is rendered from a wheel.View, so this file is pure: no state, no I/O, and
// the only user-controlled text on the card is nicknames, which the guard gates on every
// repaint. A wrong solve is never quoted, and the engine does not even carry it, because one
// blocklisted guess would otherwise make every later repaint of the match fail the gate.

// boardCells is the widest line of the board, in letter cells. It is the engine's
// per-word limit, so a word never has to be split, and at two characters a cell it keeps
// a line inside a phone screen without horizontal scrolling.
const boardCells = 13

// Phase colours: the one thing on the card readable at a glance while scrolling.
const (
	colourLobby = 0x3498DB
	colourPlay  = 0xF1C40F
	colourBonus = 0x9B59B6
	colourOver  = 0x95A5A6
)

// wheelCard renders a view and the buttons under it. A finished match gets an EMPTY
// component slice rather than nil, because both the edit and the interaction update omit or
// null a nil one, and a finished card must lose its buttons.
func wheelCard(v wheel.View) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	e := &discordgo.MessageEmbed{Color: colourPlay}
	var b strings.Builder

	switch v.Phase {
	case wheel.Lobby:
		e.Title = "🎡 wheel of fortune"
		e.Color = colourLobby
		fmt.Fprintf(&b, "%s\n", subtext("sign-ups close <t:"+strconv.FormatInt(v.Deadline.Unix(), 10)+":R>"))
		fmt.Fprintf(&b, "press **join** to play%s%d rounds and a bonus", sep, v.Rounds)
	case wheel.Round, wheel.BonusPick, wheel.BonusSolve:
		e.Title = fmt.Sprintf("🎡 round %d/%d", v.Round, v.Rounds)
		if v.Phase != wheel.Round {
			e.Title, e.Color = "🎡 bonus round", colourBonus
		}
		writeBoard(&b, v)
		b.WriteString("\n" + turnLine(v))
	default:
		e.Title, e.Color = "🎡 game over", colourOver
		writeBoard(&b, v)
	}
	if line := eventLine(v.Last); line != "" {
		b.WriteString("\n" + subtext(line))
	}
	e.Description = b.String()
	e.Fields = []*discordgo.MessageEmbedField{{Name: "players", Value: playerList(v)}}
	return e, wheelButtons(v)
}

// writeBoard puts the category over the board, and the called letters under it.
func writeBoard(b *strings.Builder, v wheel.View) {
	fmt.Fprintf(b, "%s\n```\n%s\n```", subtext(strings.ToLower(v.Category)), strings.Join(wrapBoard(v.Board), "\n"))
	if len(v.Called) > 0 && v.Phase != wheel.Done && v.Phase != wheel.Aborted {
		letters := make([]string, len(v.Called))
		for i, r := range v.Called {
			letters[i] = string(r)
		}
		b.WriteString("\n" + subtext("called: "+strings.Join(letters, " ")))
	}
}

// wrapBoard lays the phrase out as cells, a space between letters and three between words,
// wrapped at word boundaries so no line is wider than boardCells.
func wrapBoard(board string) []string {
	var lines []string
	var cur []string
	width := 0
	for _, w := range strings.Fields(board) {
		n := len([]rune(w))
		if width > 0 && width+1+n > boardCells {
			lines = append(lines, strings.Join(cur, "   "))
			cur, width = nil, 0
		}
		if width > 0 {
			width++
		}
		width += n
		cur = append(cur, strings.Join(strings.Split(w, ""), " "))
	}
	if len(cur) > 0 {
		lines = append(lines, strings.Join(cur, "   "))
	}
	return lines
}

// turnLine says whose turn it is, what they can do, and until when. The deadline is a Discord
// relative timestamp, so the client counts it down and no edit is spent on a clock.
func turnLine(v wheel.View) string {
	name := "somebody"
	for _, p := range v.Players {
		if p.UserID == v.Current {
			name = displayName(p)
		}
	}
	var do string
	switch {
	case v.Phase == wheel.BonusPick:
		do = "pick 3 consonants and a vowel"
	case v.Phase == wheel.BonusSolve:
		do = "one guess at the bonus"
	case v.Pending != nil:
		do = fmt.Sprintf("%s on the wheel, call a consonant", commas(v.Pending.Value))
	case !v.CanSpin:
		do = "no consonants left: buy a vowel or solve"
	default:
		do = "spin, buy a vowel, or solve"
	}
	return fmt.Sprintf("▸ **%s**%s%s%s<t:%d:R>", name, sep, do, sep, v.Deadline.Unix())
}

// playerList is one line per seat. In a match it is the round bank and the match bank, and
// the one whose turn it is carries the marker.
func playerList(v wheel.View) string {
	if v.Phase == wheel.Done || v.Phase == wheel.Aborted {
		return finalStandings(v)
	}
	var b strings.Builder
	for _, p := range v.Players {
		name := displayName(p)
		switch {
		case v.Phase == wheel.Lobby:
			fmt.Fprintf(&b, "**%s**", name)
			if p.UserID == v.HostID {
				b.WriteString(sep + "host")
			}
		case p.Left:
			fmt.Fprintf(&b, "~~%s~~%s%s banked", name, sep, commas(p.Bank))
		default:
			// An ideographic space holds the column where the marker would be, because an
			// embed trims leading ASCII whitespace.
			marker := "　"
			if p.UserID == v.Current {
				marker = "▸"
			}
			fmt.Fprintf(&b, "%s **%s**%s%s this round%s%s banked", marker, name, sep, commas(p.Round), sep, commas(p.Bank))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// finalStandings ranks the players by what they banked, which is what gets paid.
func finalStandings(v wheel.View) string {
	ps := append([]wheel.PlayerView(nil), v.Players...)
	// A stable insertion sort: at most ten seats, and ties keep seat order, which is the
	// engine's own tie-break for the winner.
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j].Bank > ps[j-1].Bank; j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
	var b strings.Builder
	for i, p := range ps {
		rank := fmt.Sprintf("`%2d`", i+1)
		if i < len(medals) && p.Bank > 0 {
			rank = medals[i]
		}
		fmt.Fprintf(&b, "%s **%s**%s%s gold\n", rank, displayName(p), sep, commas(p.Bank))
	}
	return strings.TrimRight(b.String(), "\n")
}

func displayName(p wheel.PlayerView) string {
	if p.Name == "" {
		return wordgame.TruncateRunes(p.UserID, nameWidth)
	}
	return wordgame.TruncateRunes(p.Name, nameWidth)
}

// eventLine says what the last change did, from the engine's structured events. Structured
// rather than pre-rendered, so a repaint from the sweep says the same thing the press did.
func eventLine(evs []wheel.Event) string {
	var parts []string
	for _, e := range evs {
		name := "**" + wordgame.TruncateRunes(e.Name, nameWidth) + "**"
		var s string
		switch e.Kind {
		case wheel.Joined:
			s = name + " joined"
		case wheel.Left:
			s = name + " left"
		case wheel.Spun:
			switch e.Wedge.Kind {
			case wheel.Bankrupt:
				s = name + " hit BANKRUPT"
			case wheel.LoseTurn:
				s = name + " lost a turn"
			default:
				s = name + " spun " + commas(e.Amount)
			}
		case wheel.LetterHit:
			if e.Amount < 0 {
				s = fmt.Sprintf("%s bought %s (%d)", name, letterName(e.Letter), e.Count)
			} else {
				s = fmt.Sprintf("%s found %s (+%s)", name, plural(e.Count, string(e.Letter)), commas(e.Amount))
			}
		case wheel.LetterMiss:
			s = fmt.Sprintf("%s called %c: none there", name, e.Letter)
		case wheel.LetterRepeat:
			s = fmt.Sprintf("%s called %c again", name, e.Letter)
		case wheel.Solved:
			s = fmt.Sprintf("%s solved it (+%s)", name, commas(e.Amount))
		case wheel.WrongSolve:
			s = name + " guessed wrong"
		case wheel.TimedOut:
			s = name + " ran out of time"
		case wheel.StruckOut:
			s = name + " is out for idling"
		case wheel.BonusStart:
			s = name + " plays the bonus round"
		case wheel.BonusPicked:
			s = name + " picked " + strings.Join(strings.Split(e.Text, ""), " ")
		case wheel.BonusWon:
			s = fmt.Sprintf("%s won the bonus: +%s", name, commas(e.Amount))
		case wheel.BonusLost:
			s = fmt.Sprintf("the bonus was %s gold", commas(e.Amount))
		case wheel.Ended:
			s = endReason(e.Reason)
		}
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, sep)
}

func letterName(r rune) string {
	if strings.ContainsRune("AEIOU", r) {
		return "an " + string(r)
	}
	return "a " + string(r)
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
// Labels are this repository's constants, which is why the guard does not gate them.
func wheelButtons(v wheel.View) []discordgo.MessageComponent {
	btn := func(label, action string, style discordgo.ButtonStyle, disabled bool) discordgo.MessageComponent {
		return discordgo.Button{Label: label, Style: style, CustomID: wheelID(action, v.Turn), Disabled: disabled}
	}
	var row []discordgo.MessageComponent
	switch v.Phase {
	case wheel.Lobby:
		row = []discordgo.MessageComponent{
			btn("join", actJoin, discordgo.PrimaryButton, false),
			btn("leave", actLeave, discordgo.SecondaryButton, false),
			btn("start", actStart, discordgo.SuccessButton, false),
		}
	case wheel.Round:
		row = []discordgo.MessageComponent{
			btn("spin", actSpin, discordgo.PrimaryButton, !v.CanSpin),
			btn("consonant", actConsonant, discordgo.PrimaryButton, v.Pending == nil),
			btn("buy a vowel", actVowel, discordgo.SecondaryButton, v.Pending != nil || !v.CanVowel),
			btn("solve", actSolve, discordgo.SuccessButton, v.Pending != nil),
			btn("leave", actLeave, discordgo.DangerButton, false),
		}
	case wheel.BonusPick:
		row = []discordgo.MessageComponent{
			btn("pick letters", actPick, discordgo.PrimaryButton, false),
			btn("leave", actLeave, discordgo.DangerButton, false),
		}
	case wheel.BonusSolve:
		row = []discordgo.MessageComponent{
			btn("solve", actSolve, discordgo.SuccessButton, false),
			btn("leave", actLeave, discordgo.DangerButton, false),
		}
	default:
		return []discordgo.MessageComponent{}
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: row}}
}

// endedCard replaces the buttons of a match this process no longer holds: one that finished,
// or one that was live when the bot restarted. The dead buttons heal on first touch, so a
// restart needs no shutdown edit.
func endedCard() (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	return &discordgo.MessageEmbed{
		Title:       "🎡 wheel of fortune",
		Description: "this game has ended. /wheel starts a new one",
		Color:       colourOver,
	}, []discordgo.MessageComponent{}
}

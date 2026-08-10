package games

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/wordgame"
)

// The leaderboard's rendering.
//
// # Why an embed rather than the code block it replaces
//
// The old board was two fenced code blocks with a fixed-width Rank/Player/Wins table in each.
// Code blocks do not wrap on a phone, so the table was horizontally scrolled off screen for
// most of the people looking at it, and a monospace grid was doing the work a layout should.
// An embed gets a colour, a heading, two columns that stack on mobile, and a footer, none of
// which needed a column-alignment calculation.
//
// It also cost one Discord request per weekly talker to render ten rows, which is the part
// that made it slow. See postLeaderboard.
//
// # Everything here is user-controlled text
//
// Names come from Discord nicknames, so every string that lands in an embed field is
// untrusted by construction. That is why it goes through Guard.SendEmbed, which runs
// CheckEmit over EVERY text field rather than the description alone, and sets AllowedMentions
// exactly as a plain message does.
//
// And why names are rendered as plain text, never as <@id> mention markup. Discord documents
// mentions inside an embed as not notifying, which is very likely true and permanent, and is
// exactly the kind of undocumented-adjacent reading discordguard's allowedMentions comment
// refuses to lean on. Plain names cost nothing and depend on nothing.

// leaderboardRows is how many places each board shows before the viewer's own row.
//
// Ten because that is what a leaderboard means, and because an embed field caps at 1024
// characters: eleven short lines is comfortably inside it while a rank in the hundreds is not
// something anybody scrolls to.
const leaderboardRows = 10

// medals decorate the top three. Anything below gets its number.
var medals = []string{"🥇", "🥈", "🥉"}

// leaderboardEmbed renders both boards as one message.
func leaderboardEmbed(wins, chat wordgame.Board, nextReset time.Time, footer string) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title: "🏆 Weekly Leaderboard",
		// A Discord relative timestamp rather than a rendered duration, so it stays correct in
		// every reader's timezone and keeps counting down without the bot editing anything.
		Description: fmt.Sprintf("Resets <t:%d:R>", nextReset.Unix()),
		Color:       0xF1C40F,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:  "Word Scramble",
				Value: renderBoard(wins, "wins"),
				// Inline, so the two boards sit side by side on a desktop and stack on a
				// phone. That is the whole reason this is a field pair rather than one long
				// description.
				Inline: true,
			},
			{
				Name:   "Chat",
				Value:  renderBoard(chat, "messages"),
				Inline: true,
			},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: footer},
	}
}

// renderBoard turns one ranked board into an embed field value.
//
// unit names what the numbers are, because the two boards count different things and a column
// of bare integers under one heading invites the reading that they are comparable.
func renderBoard(b wordgame.Board, unit string) string {
	if len(b.Top) == 0 {
		return "_Nobody yet this week._"
	}

	var sb strings.Builder
	for _, row := range b.Top {
		sb.WriteString(line(row))
		sb.WriteByte('\n')
	}

	// THE ELEVENTH SLOT. Ranks 1 to 10 as usual, a divider, then where the person who asked
	// actually stands. Somebody at 18th can see 18th without the board having to go that far,
	// which is the entire feature.
	switch {
	case b.You != nil:
		sb.WriteString("　\n") // an ideographic space: a blank line an embed field will not trim
		sb.WriteString(line(*b.You))
		sb.WriteByte('\n')
	case b.Unranked:
		// Said rather than omitted. A missing row is indistinguishable from a bug, and "you
		// have none" is a real answer to the question that was asked.
		sb.WriteString("　\n")
		fmt.Fprintf(&sb, "_You have no %s this week._\n", unit)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// line renders one row: a medal or a rank, the name, and the score.
func line(r wordgame.Row) string {
	rank := fmt.Sprintf("`%2d.`", r.Rank)
	if r.Rank >= 1 && r.Rank <= len(medals) {
		rank = medals[r.Rank-1]
	}

	name := r.Name
	if name == "" {
		// The resolver falls back to the raw ID rather than failing, so this only happens when
		// a board was rendered without names at all. Showing the ID beats showing nothing.
		name = r.UserID
	}
	// Runes rather than bytes, because a nickname is full of emoji and accented characters and
	// byte slicing splits them into a replacement glyph (M0).
	return fmt.Sprintf("%s **%s** %s", rank, wordgame.TruncateRunes(name, 16), commas(r.Score))
}

// leaderboardFooter is the one-line summary under both boards.
func leaderboardFooter(wins, chat wordgame.Board) string {
	return fmt.Sprintf("%s playing, %s talking this week",
		plural(wins.Players, "player"), plural(chat.Players, "person", "people"))
}

func plural(n int, singular string, plural ...string) string {
	word := singular + "s"
	if len(plural) > 0 {
		word = plural[0]
	}
	if n == 1 {
		word = singular
	}
	return fmt.Sprintf("%d %s", n, word)
}

// commas groups a number for reading. The chat board reaches five figures on a busy week and
// an unbroken run of digits is the one thing in this layout that is genuinely hard to read.
func commas(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}

	var out strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		out.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if out.Len() > 0 {
			out.WriteByte(',')
		}
		out.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + out.String()
	}
	return out.String()
}

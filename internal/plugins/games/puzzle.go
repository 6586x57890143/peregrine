package games

import (
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/wordgame"
)

// The puzzle's rendering.
//
// # Why an embed rather than the plain strings it replaced
//
// The same argument embed.go makes about the leaderboard, plus one this feature has that the
// leaderboard does not: a puzzle has STATE. It is live, or it has been hinted, or it was solved,
// or it timed out, and a player scrolling back through a channel needs to know which at a glance
// rather than by reading. A colour answers that in no words at all, and a plain message has no
// colour to give.
//
// It also has a deadline, and "60 seconds" printed into a string is wrong the moment it is sent.
// A Discord relative timestamp counts down on its own, in the reader's own timezone, without the
// bot editing anything, which is the same reason the leaderboard renders its reset that way.
//
// # The M27 pass: a card is two lines, three when hinted
//
// The first version of this spent four embed slots (title, description, a field, footer) on what
// is really two facts, and read like a productivity app in a server whose bot talks in lowercase.
// It said "Unscramble this word:" above a scrambled word, titled itself "Word Scramble" on a card
// whose entire content is a word scramble, and closed with a three-clause footer.
//
// So: description only. The scramble is a header, the mask sits under it, and everything else
// collapses into ONE line of subtext. The four state colours are unchanged and are now doing the
// work the deleted title used to.
//
// The copy is deadpan lowercase, and that is the opposite of what "match the server's register"
// naively implies. This chrome repeats on EVERY puzzle, and a fixed joke stops being funny by the
// fourth time; peregrine's chaos belongs in the generated text, which varies. Furniture should be
// flat and get out of the way.
//
// # Everything here still goes through Guard.SendEmbed
//
// Which runs CheckEmit over EVERY text field rather than the description alone. The winner's
// nickname is interpolated into one of these, and a nickname is user-controlled text: even a
// message the bot composes itself is untrusted-input-shaped. Nothing here needed a new gate,
// which is precisely why this renders embeds and not Components V2, whose flag disables embeds
// and would have made a recursive component walker load-bearing safety code.

// Puzzle state colours. Discord's own palette, so they read as intentional next to every other
// bot in the server rather than as arbitrary hex.
const (
	colourLive    = 0x5865F2 // blurple: running, no help given
	colourHinted  = 0xFEE75C // yellow: the ladder has started, the prize is falling
	colourSolved  = 0x57F287 // green
	colourTimeout = 0xED4245 // red
)

// sep joins the parts of a subtext line.
//
// A middle dot rather than the pipe this used to use, because a pipe reads as a table that is not
// there. It is also not one of the four characters CI's prose check bans, which a real dash would
// have been.
const sep = " · "

// puzzleEmbed renders a live puzzle, at whatever rung its ladder has reached.
//
// One function for the announcement and every repost of it, because they are the same card at
// different moments and two builders would be two places for the round number or the stake to go
// stale.
func puzzleEmbed(g *wordgame.Game, points int) *discordgo.MessageEmbed {
	lines := []string{"## " + g.Scrambled}
	meta := []string{}

	if g.Rounds > 0 {
		meta = append(meta, fmt.Sprintf("round %d/%d", g.Round, g.Rounds))
	}

	colour := colourLive
	if g.HintLevel > 0 {
		colour = colourHinted
		// NOT wrapped in backticks, and that is a collision with existing code rather than a
		// preference. hidden in internal/wordgame/hint.go is an ESCAPED underscore, because an
		// underscore is italic markup and a mask is mostly underscores; inside an inline code
		// span a backslash is literal, so backticks would render "p \_ \_ e". Monospace
		// alignment is not worth either un-escaping that constant or growing a second Mask.
		lines = append(lines, g.Mask())
		meta = append(meta, fmt.Sprintf("hint %d/%d", g.HintLevel, g.Rungs()))
	} else {
		// The letter count earns its place only until a rung lands. After that the mask says the
		// same thing more directly, and dropping it is what keeps the subtext to one line as it
		// gains the round and hint counters.
		meta = append(meta, plural(g.Letters(), "letter"))
	}

	meta = append(meta,
		fmt.Sprintf("worth %d", points),
		fmt.Sprintf("ends <t:%d:R>", g.ExpiresAt.Unix()),
	)
	lines = append(lines, subtext(meta...))

	return &discordgo.MessageEmbed{
		Description: strings.Join(lines, "\n"),
		Color:       colour,
	}
}

// solvedEmbed announces a win.
func solvedEmbed(winner, word string, solveTime time.Duration, points int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Description: fmt.Sprintf("**%s** got **%s**\n%s",
			wordgame.TruncateRunes(winner, 32), word,
			subtext(fmt.Sprintf("%.2fs", solveTime.Seconds()), fmt.Sprintf("%d pts", points))),
		Color: colourSolved,
	}
}

// timeoutEmbed announces that nobody got it.
//
// One line, and no consolation. The colour says it went badly and the word says what it was.
func timeoutEmbed(word string) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Description: fmt.Sprintf("nobody got **%s**", word),
		Color:       colourTimeout,
	}
}

// gauntletDoneEmbed closes out a run.
//
// A run that just stops is indistinguishable from a run that broke, which is finding 32's shape:
// the bot going quiet is fine, a player being unable to tell whether that was the design is not.
// It carries no standings of its own, because a gauntlet win is an ordinary win and the weekly
// board is where wins are counted.
func gauntletDoneEmbed(rounds int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Description: fmt.Sprintf("that's all %s\n%s",
			plural(rounds, "round"), subtext("`!leaderboard` for the damage")),
		Color: colourSolved,
	}
}

// subtext renders one small grey line from its non-empty parts.
//
// "-#" is Discord's subtext markdown, which is the only way to get footer-sized text into a
// DESCRIPTION. It has to be the description rather than the actual footer, because the countdown
// lives on this line and Discord renders <t:N:R> in descriptions and field values only, never in
// a footer or a title. So the one part of the card that most wants to be small is also the one
// part that cannot use the small slot.
func subtext(parts ...string) string {
	kept := parts[:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return "-# " + strings.Join(kept, sep)
}

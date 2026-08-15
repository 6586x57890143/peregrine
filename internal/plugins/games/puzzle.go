package games

import (
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/wordgame"
)

// The puzzle's rendering.
//
// # Why an embed rather than the plain strings it replaces
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
// # Everything here still goes through Guard.SendEmbed
//
// Which runs CheckEmit over EVERY text field rather than the description alone. The winner's
// nickname is interpolated into one of these, and a nickname is user-controlled text: even a
// message the bot composes itself is untrusted-input-shaped. Nothing here needed a new gate,
// which is precisely why this milestone renders embeds and not Components V2, whose flag
// disables embeds and would have made a recursive component walker load-bearing safety code.

// Puzzle state colours. Discord's own palette, so they read as intentional next to every other
// bot in the server rather than as arbitrary hex.
const (
	colourLive    = 0x5865F2 // blurple: running, no help given
	colourHinted  = 0xFEE75C // yellow: the ladder has started, the prize is falling
	colourSolved  = 0x57F287 // green
	colourTimeout = 0xED4245 // red
)

// puzzleEmbed renders a live puzzle, at whatever rung its ladder has reached.
//
// One function for the announcement and every repost of it, because they are the same message
// at different moments and two builders would be two places for the round number or the stake to
// go stale.
func puzzleEmbed(g *wordgame.Game, points int) *discordgo.MessageEmbed {
	e := &discordgo.MessageEmbed{
		Title: puzzleTitle(g),
		// The scramble on its own line and nothing else, because it is the one thing being
		// asked and everything else on this card is context for it.
		Description: fmt.Sprintf("Unscramble this word:\n# %s\n\nEnds <t:%d:R>",
			g.Scrambled, g.ExpiresAt.Unix()),
		Color:  colourLive,
		Footer: &discordgo.MessageEmbedFooter{Text: puzzleFooter(g, points)},
	}

	if g.HintLevel > 0 {
		e.Color = colourHinted
		e.Fields = []*discordgo.MessageEmbedField{{
			Name:  fmt.Sprintf("💡 Hint %d of %d", g.HintLevel, g.Rungs()),
			Value: g.Mask(),
		}}
	}
	return e
}

// solvedEmbed announces a win.
func solvedEmbed(winner, word string, solveTime time.Duration, points int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title: "🎉 Solved",
		Description: fmt.Sprintf("**%s** got **%s** in %.2f seconds.",
			wordgame.TruncateRunes(winner, 32), word, solveTime.Seconds()),
		Color:  colourSolved,
		Footer: &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("%s earned", plural(points, "point"))},
	}
}

// timeoutEmbed announces that nobody got it.
func timeoutEmbed(word string) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:       "⏰ Time's up",
		Description: fmt.Sprintf("Nobody got it. The word was **%s**.", word),
		Color:       colourTimeout,
	}
}

// gauntletDoneEmbed closes out a run.
//
// A run that just stops is indistinguishable from a run that broke, which is finding 32's shape:
// the bot going quiet is fine, an operator or a player being unable to tell whether that was the
// design is not. It carries no standings of its own, because a gauntlet win is an ordinary win
// and the weekly board is where wins are counted.
func gauntletDoneEmbed(rounds int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:       "🏁 Gauntlet complete",
		Description: fmt.Sprintf("That was all %s. Check `!leaderboard` for the damage.", plural(rounds, "round")),
		Color:       colourSolved,
	}
}

// puzzleTitle names the puzzle and places it in a run when there is one.
func puzzleTitle(g *wordgame.Game) string {
	if g.Rounds > 0 {
		return fmt.Sprintf("🔤 Word Scramble - Round %d of %d", g.Round, g.Rounds)
	}
	return "🔤 Word Scramble"
}

// puzzleFooter is the stake and the shape of the answer.
//
// The stake is the entire reason the ladder exists, so it is on the card from the first moment
// rather than appearing once it starts falling: a player who cannot see what a puzzle is worth
// before the first hint has no decision to make about waiting for one.
func puzzleFooter(g *wordgame.Game, points int) string {
	s := fmt.Sprintf("Worth %s | %s", plural(points, "point"), plural(g.Letters(), "letter"))
	if g.HintLevel < g.Rungs() {
		return s + " | a hint costs a point"
	}
	return s
}

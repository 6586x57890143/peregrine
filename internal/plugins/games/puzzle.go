package games

import (
	"fmt"
	"strings"
	"time"

	"github.com/6586x57890143/peregrine/internal/wordgame"
)

// The puzzle's rendering: a plain chat message.
//
// # Why not an embed
//
// It was one for two milestones and it was wrong. An embed is a CARD: a coloured bar, a box, and
// a chunk of visual weight that says "this is a notice" every time it appears. A word game is not
// a notice, it is a thing somebody said in the channel, and a bot that posts a card every few
// minutes is a bot that keeps interrupting. Reverted on that feedback rather than on a theory.
//
// **The formatting survives the container.** Everything the embed carried in its description is
// markdown that a normal message renders identically, and two of those actually work BETTER
// outside an embed: headers and subtext are ordinary message markdown, and the relative timestamp
// has no footer or title to be excluded from any more. So this is the same three lines with the
// box taken off.
//
// # What is genuinely lost, and what replaced it
//
// The state colour. Four colours said live, hinted, solved and timed out with no words at all,
// and a plain message has no colour to give. It turned out to be near-redundant: the mask only
// exists on a hinted puzzle, "hint 1/3" only appears once a rung has landed, and a message that
// opens "nobody got" is not ambiguous about how it went. The one thing the colour did that the
// text does not is work at a glance while scrolling, and the H2 does that instead.
//
// # Which is why the header stays
//
// Dropping the box makes the header MORE load-bearing rather than less. An embed is findable in a
// scroll because it is a box; without it, the scramble being an H2 is the only thing that makes
// the puzzle catch the eye of somebody skimming. It is also the one element on the message that
// deserves weight, since it is the entire question being asked.
//
// # Everything here still goes through the guard
//
// Guard.Send applies CheckEmit to the whole message, which is if anything a wider check than the
// embed walk it replaces. The winner's nickname is interpolated into one of these and a nickname
// is user-controlled text: even a message the bot composes itself is untrusted-input-shaped.

// sep joins the parts of a subtext line.
//
// A middle dot rather than the pipe this used to use, because a pipe reads as a table that is not
// there. It is also not one of the four characters CI's prose check bans, which a real dash would
// have been.
const sep = " · "

// puzzleMessage renders a live puzzle, at whatever rung its ladder has reached.
//
// One function for the announcement and every repost of it, because they are the same message at
// different moments and two builders would be two places for the round number or the stake to go
// stale.
func puzzleMessage(g *wordgame.Game, points int) string {
	lines := []string{"## " + g.Scrambled}
	meta := []string{}

	if g.Rounds > 0 {
		meta = append(meta, fmt.Sprintf("round %d/%d", g.Round, g.Rounds))
	}

	if g.HintLevel > 0 {
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
	return strings.Join(append(lines, subtext(meta...)), "\n")
}

// solvedMessage announces a win.
func solvedMessage(winner, word string, solveTime time.Duration, points int) string {
	return fmt.Sprintf("**%s** got **%s**\n%s",
		wordgame.TruncateRunes(winner, 32), word,
		subtext(fmt.Sprintf("%.2fs", solveTime.Seconds()), fmt.Sprintf("%d pts", points)))
}

// timeoutMessage announces that nobody got it.
//
// One line, and no consolation.
func timeoutMessage(word string) string {
	return fmt.Sprintf("nobody got **%s**", word)
}

// gauntletDoneMessage closes out a run.
//
// A run that just stops is indistinguishable from a run that broke, which is finding 32's shape:
// the bot going quiet is fine, a player being unable to tell whether that was the design is not.
// It carries no standings of its own, because a gauntlet win is an ordinary win and the weekly
// board is where wins are counted.
func gauntletDoneMessage(rounds int) string {
	return fmt.Sprintf("that's all %s\n%s",
		plural(rounds, "round"), subtext("`!leaderboard` for the damage"))
}

// subtext renders one small grey line from its non-empty parts.
//
// "-#" is Discord's subtext markdown. It was load-bearing when these were embeds, because a
// relative timestamp renders in a description and not in a footer, so the one part of the card
// that most wanted to be small could not use the small slot. In a plain message it is simply the
// right size for metadata: the scramble is the message and this is the annotation on it.
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

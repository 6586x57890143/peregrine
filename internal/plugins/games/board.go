package games

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/names"
	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/wordgame"
)

// The leaderboard: one renderer, three ways in.
//
// !leaderboard posts page one of the local board, /leaderboard picks a scope, and a button
// press re-renders. All three build the same tally and go through the same render, because a
// board that looked different depending on which of them produced it would be two boards.
//
// # Local and global only mean something after M31
//
// Scores are per guild now, which is what makes "this server" a real answer and "everybody" a
// different one. Before the corpus split there was one blended board and no question to ask.
//
// Global deliberately does NOT move text between servers, which is what M31 was for: the only
// things that cross a guild boundary here are a user ID and an integer, never a word anybody
// typed.

// scope is which servers a board counts.
type scope string

const (
	// scopeLocal is this server. The default, because that is who the viewer is playing with.
	scopeLocal scope = "local"

	// scopeGlobal is every server the bot keeps a corpus for.
	scopeGlobal scope = "global"
)

// buttonPrefix marks a component this service owns. Discord delivers every component press to
// the same handler, so an ID that could belong to another feature's message is a press answered
// with the wrong thing.
const buttonPrefix = "lb"

// buttonID encodes where a press should take the reader.
//
// The state rides in the custom_id rather than in a map keyed by message: no TTL, nothing to
// leak, and a restart still answers a press on a board posted last week, which is the whole
// reason not to hold state for this.
//
// THE GUILD IS DELIBERATELY NOT IN HERE, unlike the milestone plan's sketch. A component
// interaction already carries the guild it was pressed in, so encoding a second copy creates a
// pair that can disagree, and the only way to resolve a disagreement is to render one server's
// board from a press in another. That is the exact leak M31 exists to make unwritable, so the
// ID carries what the interaction cannot supply and nothing else.
func buttonID(sc scope, page int) string {
	return fmt.Sprintf("%s:%s:%d", buttonPrefix, sc, page)
}

// parseButtonID reads one back, and reports whether it is ours at all.
func parseButtonID(customID string) (scope, int, bool) {
	parts := strings.Split(customID, ":")
	if len(parts) != 3 || parts[0] != buttonPrefix {
		return "", 0, false
	}
	page, err := strconv.Atoi(parts[2])
	if err != nil {
		return "", 0, false
	}
	// An unrecognized scope reads as local rather than being refused. A press is somebody
	// asking for a board, and the worst thing this path can do is show them nothing.
	sc := scope(parts[1])
	if sc != scopeGlobal {
		sc = scopeLocal
	}
	return sc, page, true
}

// tally is everything a rendered board needs, for either scope.
//
// A struct rather than five return values because global and local fill it from different
// numbers of corpora and the renderer must not be able to tell which: ONE renderer is what
// stops the two scopes drifting into two layouts.
type tally struct {
	// wins is word-game points by user ID, and chat is message counts by user ID.
	wins map[string]int
	chat map[string]int

	// fastest and streak are the week's records, nil when nobody qualifies. Merged across
	// guilds for a global board: the fastest solve anywhere is a real fact about the week,
	// where adding two streaks together would not be.
	fastest *wordgame.Entry
	streak  *wordgame.Entry

	// guilds is how many corpora went into this. Rendered only for the global board, where
	// "everybody" is otherwise a claim with no size attached.
	guilds int
}

// gather builds a tally for one scope.
//
// A guild whose corpus cannot be reached is SKIPPED for a global board and fatal for a local
// one, which is the same asymmetry maybeReset makes: one unreachable server must not empty
// everybody else's board, and a viewer asking about their own server has to be told.
func (s *Service) gather(sc scope, guildID string) (tally, error) {
	t := tally{wins: map[string]int{}, chat: map[string]int{}}

	guilds := []string{guildID}
	if sc == scopeGlobal {
		guilds = s.corpora.Guilds()
	}
	for _, g := range guilds {
		board := s.board(g)
		store, err := s.corpora.For(g)
		if board == nil || err != nil {
			if sc == scopeGlobal {
				log.Printf("[LEADERBOARD] skipping guild %s in the global board: %v", g, err)
				continue
			}
			return tally{}, fmt.Errorf("no corpus for guild %s: %w", g, err)
		}

		var chatScores map[string]int
		if err := store.View(func(r *storage.Reader) error {
			var err error
			chatScores, err = weeklyScores(r)
			return err
		}); err != nil {
			if sc == scopeGlobal {
				log.Printf("[LEADERBOARD] skipping guild %s in the global board: %v", g, err)
				continue
			}
			return tally{}, fmt.Errorf("user stats for guild %s: %w", g, err)
		}

		for id, points := range board.Scores() {
			t.wins[id] += points
		}
		for id, count := range chatScores {
			t.chat[id] += count
		}
		if e, ok := board.Fastest(); ok && (t.fastest == nil || e.FastestMS < t.fastest.FastestMS) {
			t.fastest = &e
		}
		if e, ok := board.Streak(); ok && (t.streak == nil || e.Streak > t.streak.Streak) {
			t.streak = &e
		}
		t.guilds++
	}
	return t, nil
}

// render turns a tally into the message: the embed and the buttons under it.
//
// viewerID is whoever is looking RIGHT NOW, which for a button press is the presser rather than
// whoever ran the command.
func (s *Service) render(t tally, guildID, viewerID string, sc scope, page int) (
	*discordgo.MessageEmbed, []discordgo.MessageComponent) {

	// RANK FIRST, RESOLVE NAMES SECOND, and that order is the M21a performance fix.
	//
	// This used to resolve a display name for EVERY user in the week's stats before sorting
	// anything, through an uncached GuildMember REST GET. On a server with two hundred weekly
	// talkers that was two hundred-odd sequential, rate-limited requests to render twenty rows.
	//
	// A rank is one plus the number of people strictly ahead, so it needs no names at all, and
	// only the rows actually displayed need one: at most eleven per board. A global board makes
	// that matter MORE rather than less, since it ranks people the viewer's guild has never had
	// as members, so each of them would fall through to a REST call.
	wins := wordgame.Rank(t.wins, viewerID, leaderboardRows, page)
	chat := wordgame.Rank(t.chat, viewerID, leaderboardRows, page)

	// Memoized across BOTH boards, so somebody on the word-game board and the chat board costs
	// one lookup rather than two.
	resolve := memoize(func(userID string) string {
		return names.Display(s.session, s.members, guildID, userID)
	})
	wins = wins.WithNames(resolve)
	chat = chat.WithNames(resolve)

	// The records go through the SAME memoized resolver, so a record held by somebody already
	// on a board costs nothing extra.
	footer := leaderboardFooter(t, wins, chat, resolve)
	nextReset := corpus.StartOfWeekUTC(time.Now()).AddDate(0, 0, 7)

	// One page number drives both columns, and pages is whichever board is longer. Two
	// independent page counters under one pair of buttons would be a control that means
	// different things on the left and the right of the same card.
	pages := max(wins.Pages, chat.Pages)

	return leaderboardEmbed(wins, chat, nextReset, footer, sc, t.guilds, page, pages),
		boardButtons(sc, page, pages)
}

// boardButtons is prev and next, disabled at the ends.
//
// Disabled rather than absent, so the row does not change width as somebody pages through it. A
// single-page board gets no row at all, because two dead buttons under a board that already
// fits are furniture.
func boardButtons(sc scope, page, pages int) []discordgo.MessageComponent {
	if pages <= 1 {
		return nil
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "prev",
				Style:    discordgo.SecondaryButton,
				CustomID: buttonID(sc, page-1),
				Disabled: page <= 1,
			},
			discordgo.Button{
				Label:    "next",
				Style:    discordgo.SecondaryButton,
				CustomID: buttonID(sc, page+1),
				Disabled: page >= pages,
			},
		},
	}}
}

// postLeaderboard answers !leaderboard with the word-game wins and the chat leaderboard.
//
// Deliberately NOT gated on the feature flag. The chat half reads the stats bucket, which is
// populated on every message regardless of whether the scramble game runs, so the command is
// useful with word games off.
//
// Local and page one, because a bang command carries no options. The buttons are what make that
// acceptable rather than a lesser version of /leaderboard.
func (s *Service) postLeaderboard(guildID, channelID, viewerID string) {
	// THIS server's board, from THIS server's corpus. A local board merged across guilds would
	// rank people against strangers whose messages they cannot see, which is the same leak as
	// generating one server's text into another, wearing a scoreboard.
	t, err := s.gather(scopeLocal, guildID)
	if err != nil {
		log.Printf("[LEADERBOARD] %v", err)
		s.guard.Send(channelID, "could not build the leaderboard")
		return
	}

	embed, buttons := s.render(t, guildID, viewerID, scopeLocal, 1)
	if _, ok := s.guard.SendEmbed(channelID, embed, buttons...); !ok {
		// The guard has already logged whether this was a refusal or a failure. Said here as
		// well because a command that produced nothing is a question the operator will be
		// asked, and silence on the reply path is the bug finding 32 was about.
		log.Printf("[LEADERBOARD] the board was not sent in %s", channelID)
	}
}

// handleLeaderboard is /leaderboard.
//
// PUBLIC rather than ephemeral, which is the opposite of every other answer this package gives
// an interaction: a leaderboard is the one thing here that the whole channel wants to see, and
// the bang command has always posted it. The failures stay private, for M21b's reason.
//
// Not gated on Authorized either, for postLeaderboard's reason: this is not an operator command.
func (s *Service) handleLeaderboard(i *discordgo.Interaction) {
	if i.GuildID == "" {
		// A DM has no board and no corpus to build one from. Answered rather than ignored,
		// because every exit from an interaction answers.
		s.guard.Respond(i, "there is no leaderboard in a DM", true)
		return
	}

	sc := scopeLocal
	for _, opt := range i.ApplicationCommandData().Options {
		if opt != nil && opt.Name == optScope && scope(opt.StringValue()) == scopeGlobal {
			sc = scopeGlobal
		}
	}

	t, err := s.gather(sc, i.GuildID)
	if err != nil {
		log.Printf("[LEADERBOARD] %v", err)
		s.guard.Respond(i, "could not build the leaderboard", true)
		return
	}

	who := interactionRequester(i)
	embed, buttons := s.render(t, i.GuildID, who.UserID, sc, 1)
	if !s.guard.RespondEmbed(i, embed, false, buttons...) {
		log.Printf("[LEADERBOARD] the board was not sent in %s", i.ChannelID)
	}
}

// handleBoardButton re-renders for whoever pressed.
//
// For the PRESSER rather than for whoever ran the command: the board is public, so anybody can
// page it, and the eleventh slot is the one row that is about the person looking. Restricting
// the buttons to the original caller would be more code and a worse answer.
func (s *Service) handleBoardButton(i *discordgo.Interaction) {
	sc, page, ok := parseButtonID(componentID(i))
	if !ok {
		// Somebody else's component, or a shape this build does not know. Ignored rather than
		// answered: this bot has no token for another application's interaction, and the
		// every-exit-answers rule is about commands it accepted.
		return
	}
	if i.GuildID == "" {
		s.guard.Respond(i, "there is no leaderboard in a DM", true)
		return
	}

	t, err := s.gather(sc, i.GuildID)
	if err != nil {
		log.Printf("[LEADERBOARD] %v", err)
		s.guard.Respond(i, "could not build the leaderboard", true)
		return
	}

	who := interactionRequester(i)
	embed, buttons := s.render(t, i.GuildID, who.UserID, sc, page)
	if !s.guard.UpdateEmbed(i, embed, buttons...) {
		log.Printf("[LEADERBOARD] the board was not updated in %s", i.ChannelID)
	}
}

// memoize wraps a name resolver so each user is looked up at most once.
//
// Not concurrency-safe, and does not need to be: one command invocation resolves its own board
// on one goroutine. A mutex here would be structure with no failure mode behind it.
func memoize(resolve func(userID string) string) func(string) string {
	cache := map[string]string{}
	return func(userID string) string {
		if name, ok := cache[userID]; ok {
			return name
		}
		name := resolve(userID)
		cache[userID] = name
		return name
	}
}

// weeklyScores returns each user's message count for the current week.
//
// It no longer skips keys that are not numeric, because the stats bucket no longer holds a
// non-user key: total_messages_learned is a meta counter. Anything that forgot to skip it used
// to decode an integer as a stat and count a phantom user.
func weeklyScores(r *storage.Reader) (map[string]int, error) {
	all, err := r.AllUserStats()
	if err != nil {
		return nil, err
	}
	start := corpus.StartOfWeekUTC(time.Now())
	scores := make(map[string]int, len(all))
	for userID, stat := range all {
		if !stat.LastTimestamp.Before(start) {
			scores[userID] = int(stat.Count)
		}
	}
	return scores, nil
}

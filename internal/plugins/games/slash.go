package games

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/wordgame"
)

// The slash command: the first application command peregrine has ever registered.
//
// SPEC.md section 10 has carried "whether it should be a real slash command, which is
// registration work peregrine has never done" as an open question since M11b. This settles half
// of it, on the feature that wanted it most rather than on the kill switch, because a word game
// is a safe place to learn the surface: the worst outcome of getting it wrong is a puzzle that
// does not start.
//
// # What the slash command buys that the bang command cannot
//
// An EPHEMERAL refusal. M21b had to make !wordgame refuse in silence, because answering a
// non-admin in the channel advertises that the command exists and that they are not allowed to
// use it, which is an invitation. The cost was that a legitimate operator whose command did
// nothing had to read the log to find out why, and the case that actually bit was theirs: with
// PEREGRINE_BOOTSTRAP_ADMIN_USER_ID unset, Authorized fails closed and refuses everybody
// including the person who deployed the bot. An ephemeral reply says no to the person who asked
// and to nobody else, which is the answer that dilemma never had.
//
// Permissions arrive already computed in the interaction payload, so this path needs no state
// cache lookup and no REST call at all. The bang path resolves the same Requester from the state
// cache; both feed the one Authorized.
//
// # The bang command is not removed
//
// A soft move. !wordgame still works, so nobody's muscle memory breaks and a server whose members
// have not refreshed their client is not locked out. The slash command is the blessed path, and
// the one that gets the ephemeral answers.

// commandName is the slash command, and it deliberately matches the bang command's name so the
// two are visibly the same thing rather than two features.
const commandName = "wordgame"

const (
	optWord  = "word"
	optCount = "count"
)

// definitions is what gets registered. One command with two optional options, mirroring the bang
// command's single argument rather than inventing a subcommand tree: "!wordgame banana" and
// "!wordgame 5" are one command with an argument, and the slash form should not be a different
// shape of the same feature.
func definitions() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{{
		Name:        commandName,
		Description: "Start a word scramble puzzle",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        optWord,
				Description: "Plant a specific word instead of drawing one",
				Required:    false,
			},
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        optCount,
				Description: "Run a gauntlet of this many puzzles, back to back",
				Required:    false,
				// Bounded by Discord as well as by the Manager, so the client refuses an absurd
				// number before it costs a round trip. The Manager still clamps, because this
				// bound is a courtesy and that one is the rule.
				MinValue: ptr(1.0),
				MaxValue: 50,
			},
		},
	}}
}

func ptr[T any](v T) *T { return &v }

// registerCommands publishes the command set, once, at Start.
//
// Start rather than Init, because it is a REST call and Init runs before the gateway is even
// open. It is also the first moment the bot's own application ID is knowable.
func (s *Service) registerCommands() {
	if s.session == nil || s.session.State == nil || s.session.State.User == nil {
		log.Println("[WORDGAME] No session identity, so /wordgame was not registered. The bang " +
			"command still works.")
		return
	}
	s.guard.RegisterCommands(s.session.State.User.ID, definitions())
}

// onInteraction is the gateway handler, registered in Init.
//
// Init rather than Start, for the reason chat.Init states about its own handlers: discordgo
// begins dispatching inside Open, and Open happens between Init and Start, so a handler
// registered in Start drops everything that arrived in that window.
//
// It rejects cheaply and then hands off to the dispatcher, matching tuning.onReaction. The
// dispatcher drops rather than blocking, which for an interaction means the person who ran the
// command sees Discord's own "did not respond" after three seconds. That is worse than for a
// reaction and still better than growing goroutines without bound: a queue full enough to drop
// interactions is a bot in trouble, and the drop count is reported.
func (s *Service) onInteraction(_ *discordgo.Session, ic *discordgo.InteractionCreate) {
	if ic == nil || ic.Interaction == nil {
		return
	}
	if ic.Type != discordgo.InteractionApplicationCommand {
		// Components and modals are not registered, so anything else is either another bot's
		// event or a shape this build does not know. Ignored rather than answered.
		return
	}
	if ic.ApplicationCommandData().Name != commandName {
		return
	}

	i := ic.Interaction
	if s.dispatcher == nil {
		// No dispatcher means Init never ran, which cannot happen in production. Handled
		// inline rather than dropped, so a test does not need one.
		s.handleInteraction(i)
		return
	}
	if !s.dispatcher.Submit(func(context.Context) { s.handleInteraction(i) }) {
		log.Printf("[WORDGAME] Dropped a /%s: the work queue is full.", commandName)
	}
}

// handleInteraction runs the command and answers the person who ran it.
//
// EVERY exit answers. An interaction that is never responded to shows the caller a red "the
// application did not respond" after three seconds, which is the slash-command version of
// finding 32: the bot's silence in a channel is a design decision, and a caller unable to tell
// whether their command worked is a bug. That is the opposite discipline from the bang command,
// which stays silent on purpose, and the difference is exactly that this answer is private.
func (s *Service) handleInteraction(i *discordgo.Interaction) {
	if !s.opts.Enabled || !s.manager.Available() {
		s.guard.Respond(i, "Word games are switched off right now.", true)
		return
	}

	who := interactionRequester(i)
	if !s.Authorized(who) {
		if s.opts.AdminUserID == "" {
			log.Printf("[WORDGAME] /%s in %s was refused because "+
				"PEREGRINE_BOOTSTRAP_ADMIN_USER_ID is unset and the caller is not a guild "+
				"administrator, so the check fails closed and refuses everyone.",
				commandName, i.ChannelID)
		} else {
			log.Printf("[WORDGAME] /%s in %s from %s was refused: not the configured admin and "+
				"not a guild administrator.", commandName, i.ChannelID, who.UserID)
		}
		// Ephemerally, which is the whole point: the person who asked learns why, and nobody
		// else learns that the command exists.
		s.guard.Respond(i, "You need Administrator in this server to start a word game.", true)
		return
	}

	word, count := interactionArgs(i)
	switch {
	case count > 0:
		s.startGauntletFor(i, count)
	default:
		s.startWordFor(i, word)
	}
}

// startWordFor is /wordgame, optionally with a planted word.
//
// The acknowledgement is ephemeral and the PUZZLE is a normal channel post, which is the split
// that makes a slash command better than a bang command here: the thing everybody is meant to
// see goes to the channel, and the thing only the operator needs goes to the operator. It also
// means the command does not leave its own invocation sitting above the puzzle.
func (s *Service) startWordFor(i *discordgo.Interaction, word string) {
	g, err := s.manager.StartWord(i.ChannelID, word)
	switch {
	case err == nil:
	case errors.Is(err, wordgame.ErrGameInProgress):
		s.guard.Respond(i, "A word game is already running in this channel.", true)
		return
	case errors.Is(err, wordgame.ErrUnusableWord):
		minLen, maxLen := s.manager.WordBounds()
		s.guard.Respond(i, fmt.Sprintf(
			"I cannot scramble that. A puzzle word has to be %d to %d letters, letters only, "+
				"and use at least two different ones.", minLen, maxLen), true)
		return
	default:
		log.Printf("[WORDGAME] /%s failed to start a game: %v", commandName, err)
		s.guard.Respond(i, "Something went wrong starting that puzzle.", true)
		return
	}

	if !s.announce(g) {
		s.manager.Abandon(i.ChannelID)
		// The guard refused the puzzle, so the operator is told rather than left watching an
		// empty channel. This is the case M21b could only put in the log.
		s.guard.Respond(i, "I could not post there. The channel is on the ignore list, or "+
			"writes are paused.", true)
		return
	}
	s.guard.Respond(i, "Puzzle posted.", true)
	log.Printf("[WORDGAME] Started a game in channel %s from /%s.", i.ChannelID, commandName)
}

// startGauntletFor is /wordgame count:<n>.
func (s *Service) startGauntletFor(i *discordgo.Interaction, n int) {
	queued, err := s.manager.Queue(i.ChannelID, n)
	switch {
	case err == nil:
	case errors.Is(err, wordgame.ErrGauntletInProgress):
		remaining, total := s.manager.Gauntlet(i.ChannelID)
		s.guard.Respond(i, fmt.Sprintf(
			"A gauntlet is already running here: %d of %d still to go.", remaining, total), true)
		return
	default:
		log.Printf("[WORDGAME] /%s failed to queue a gauntlet: %v", commandName, err)
		s.guard.Respond(i, "Something went wrong starting that gauntlet.", true)
		return
	}

	// Answered BEFORE the first puzzle goes up, because an interaction has three seconds and
	// starting a puzzle is a REST call of its own. The alternative is a deferred response, which
	// is a second shape of answer to maintain for no gain here.
	msg := fmt.Sprintf("Gauntlet of %d starting.", queued)
	if queued < n {
		// Said, rather than silently clamped. An operator who asked for fifty and got ten
		// should learn that from the bot rather than by counting.
		msg = fmt.Sprintf("Gauntlet of %d starting. (%d is the most I will queue at once.)",
			queued, queued)
	}
	s.guard.Respond(i, msg, true)

	s.start(i.ChannelID)
	log.Printf("[WORDGAME] Gauntlet of %d queued in channel %s from /%s.",
		queued, i.ChannelID, commandName)
}

// interactionRequester reads who ran the command and what they may do.
//
// Member.Permissions is Discord's own computed permission set for this user in this channel,
// shipped in the payload, so this path makes no REST call and consults no cache. The bang path
// has to ask the state cache for the same number; both feed the one Authorized.
//
// A DM has no Member and therefore no permissions, which leaves the zero value and fails closed.
// That is right rather than merely safe: there is no channel for a puzzle to be posted in.
func interactionRequester(i *discordgo.Interaction) Requester {
	if i == nil {
		return Requester{}
	}
	if i.Member != nil {
		who := Requester{Permissions: i.Member.Permissions}
		if i.Member.User != nil {
			who.UserID = i.Member.User.ID
		}
		return who
	}
	if i.User != nil {
		return Requester{UserID: i.User.ID}
	}
	return Requester{}
}

// interactionArgs pulls the two optional options out, with the same meanings the bang command's
// single argument has.
//
// A count wins over a word when somebody supplies both, because a gauntlet of planted words is
// not a thing this feature does and refusing the combination would mean an error message for a
// request that has an obvious reading.
func interactionArgs(i *discordgo.Interaction) (word string, count int) {
	if i == nil {
		return "", 0
	}
	for _, opt := range i.ApplicationCommandData().Options {
		if opt == nil {
			continue
		}
		switch opt.Name {
		case optWord:
			word = strings.ToLower(strings.TrimSpace(opt.StringValue()))
		case optCount:
			count = int(opt.IntValue())
		}
	}
	return word, count
}

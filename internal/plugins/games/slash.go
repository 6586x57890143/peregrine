package games

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

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

// configCommandName is the settings command. A SECOND command rather than a subcommand tree or
// more options on the first, because the two are different jobs with different audiences: one
// starts a puzzle everybody sees, the other changes where puzzles happen and answers only the
// operator. Overloading /wordgame would also have meant every start of a game carrying four
// options that have nothing to do with starting one.
//
// There is deliberately no bang equivalent. commandFor accepts exactly two tokens whose first is
// the command, which is what keeps "you should try !wordgame sometime" ordinary chat, and
// "mode interval" is three; and a settings answer wants to be ephemeral for the reason M21b
// states about refusals.
const configCommandName = "wordgame-config"

const (
	optWord  = "word"
	optCount = "count"

	optChannel  = "channel"
	optMode     = "mode"
	optInterval = "interval"
	optReset    = "reset"
)

// The channel option's three verbs. Verbs rather than a channel argument, because the answer is
// almost always "here": an operator types the command in the channel they want puzzles in, and a
// channel picker would let them bind a voice channel or a category the bot cannot post to.
const (
	channelBind     = "bind"
	channelUnbind   = "unbind"
	channelAnywhere = "anywhere"
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
	}, {
		Name:        configCommandName,
		Description: "Show or change where and how word games run",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        optChannel,
				Description: "Bind games to this channel, unbind it, or allow anywhere",
				Required:    false,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "bind this channel", Value: channelBind},
					{Name: "unbind this channel", Value: channelUnbind},
					{Name: "allow anywhere", Value: channelAnywhere},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        optMode,
				Description: "Start puzzles when a channel is busy, or on a timer",
				Required:    false,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "activity", Value: string(ModeActivity)},
					{Name: "interval", Value: string(ModeInterval)},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        optInterval,
				Description: "Minutes between puzzles in interval mode",
				Required:    false,
				// Discord's bound is a courtesy so the client refuses an absurd number before it
				// costs a round trip; clampInterval is the rule, for the same reason the gauntlet
				// count is bounded in both places.
				MinValue: ptr(float64(minInterval / time.Minute)),
				MaxValue: float64(maxInterval / time.Minute),
			},
			{
				Type:        discordgo.ApplicationCommandOptionBoolean,
				Name:        optReset,
				Description: "Write the environment's values back over the stored ones",
				Required:    false,
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
	name := ic.ApplicationCommandData().Name
	var handle func(*discordgo.Interaction)
	switch name {
	case commandName:
		handle = s.handleInteraction
	case configCommandName:
		handle = s.handleConfig
	default:
		return
	}

	i := ic.Interaction
	if s.dispatcher == nil {
		// No dispatcher means Init never ran, which cannot happen in production. Handled
		// inline rather than dropped, so a test does not need one.
		handle(i)
		return
	}
	if !s.dispatcher.Submit(func(context.Context) { handle(i) }) {
		log.Printf("[WORDGAME] Dropped a /%s: the work queue is full.", name)
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
		s.guard.Respond(i, "word games are off right now", true)
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
		s.guard.Respond(i, "you need Administrator here to start a word game", true)
		return
	}

	if !s.allowed(i.GuildID, i.ChannelID) {
		s.guard.Respond(i, "word games are restricted to another channel", true)
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
		s.guard.Respond(i, "a game is already running here", true)
		return
	case errors.Is(err, wordgame.ErrUnusableWord):
		minLen, maxLen := s.manager.WordBounds()
		s.guard.Respond(i, fmt.Sprintf(
			"cannot scramble that. a puzzle word is %d to %d letters, letters only, and "+
				"needs at least two different ones", minLen, maxLen), true)
		return
	default:
		log.Printf("[WORDGAME] /%s failed to start a game: %v", commandName, err)
		s.guard.Respond(i, "something went wrong starting that", true)
		return
	}

	if !s.announce(g) {
		s.manager.Abandon(i.ChannelID)
		// The guard refused the puzzle, so the operator is told rather than left watching an
		// empty channel. This is the case M21b could only put in the log.
		s.guard.Respond(i, "could not post there: the channel is on the ignore list, or "+
			"writes are paused", true)
		return
	}
	s.guard.Respond(i, "posted", true)
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
			"a gauntlet is already running here: %d of %d to go", remaining, total), true)
		return
	default:
		log.Printf("[WORDGAME] /%s failed to queue a gauntlet: %v", commandName, err)
		s.guard.Respond(i, "something went wrong starting that", true)
		return
	}

	// Answered BEFORE the first puzzle goes up, because an interaction has three seconds and
	// starting a puzzle is a REST call of its own. The alternative is a deferred response, which
	// is a second shape of answer to maintain for no gain here.
	msg := fmt.Sprintf("gauntlet of %d starting", queued)
	if queued < n {
		// Said, rather than silently clamped. An operator who asked for fifty and got ten
		// should learn that from the bot rather than by counting.
		msg = fmt.Sprintf("gauntlet of %d starting (%d is the most I queue at once)",
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

// handleConfig is /wordgame-config: show the settings, or change them.
//
// # It is NOT gated on allowed()
//
// Every other path here refuses a channel that is not on the allowlist, and this one must not:
// the command an operator runs to bind the channel they are standing in cannot require that
// channel to already be bound. That is the same trap as an authorization check that fails open on
// an empty string, in the other direction, and it would make the feature unrecoverable from
// Discord once somebody bound the wrong channel.
//
// Every exit answers, ephemerally, for the reason handleInteraction states: an interaction with no
// response shows the caller Discord's own red failure after three seconds, and a settings command
// that appears to do nothing is worse than one that says no.
func (s *Service) handleConfig(i *discordgo.Interaction) {
	who := interactionRequester(i)
	if !s.Authorized(who) {
		if s.opts.AdminUserID == "" {
			log.Printf("[WORDGAME] /%s in %s was refused because "+
				"PEREGRINE_BOOTSTRAP_ADMIN_USER_ID is unset and the caller is not a guild "+
				"administrator, so the check fails closed and refuses everyone.",
				configCommandName, i.ChannelID)
		} else {
			log.Printf("[WORDGAME] /%s in %s from %s was refused: not the configured admin and "+
				"not a guild administrator.", configCommandName, i.ChannelID, who.UserID)
		}
		s.guard.Respond(i, "you need Administrator here to change word-game settings", true)
		return
	}

	channel, mode, minutes, reset := configArgs(i)
	if channel == "" && mode == "" && minutes == 0 && !reset {
		// No options is a read. The command with nothing filled in is how an operator asks what
		// the bot is currently doing, which is the question the log line at startup answers once
		// and then scrolls away.
		s.guard.Respond(i, "word games: "+s.snapshot(i.GuildID).String(), true)
		return
	}

	var notes []string
	set := s.update(i.GuildID, func(set *settings) {
		if reset {
			// Applied FIRST, so an operator can reset and set something in one command rather
			// than watching their change be overwritten by the reset in the same call.
			*set = settings{
				Channels: s.opts.AllowChannels,
				Mode:     s.opts.Mode,
				Interval: s.opts.Interval,
			}
			notes = append(notes, "reset to the values this process started with")
		}
		switch channel {
		case channelBind:
			if !slices.Contains(set.Channels, i.ChannelID) {
				set.Channels = append(set.Channels, i.ChannelID)
			}
			notes = append(notes, "bound to this channel")
		case channelUnbind:
			set.Channels = slices.DeleteFunc(set.Channels, func(id string) bool {
				return id == i.ChannelID
			})
			// Said out loud, because an empty allowlist means ANYWHERE and unbinding the last
			// channel therefore does the opposite of what "unbind" sounds like. An operator who
			// tightened the list one channel at a time should not discover that by watching a
			// puzzle appear somewhere else.
			if len(set.Channels) == 0 {
				notes = append(notes, "unbound this channel, and that was the last one, so "+
					"games can now run anywhere")
			} else {
				notes = append(notes, "unbound this channel")
			}
		case channelAnywhere:
			set.Channels = nil
			notes = append(notes, "games can run anywhere")
		}
		// Checked against the two known modes rather than trusted, even though Discord only
		// offers those two choices: an interaction payload is user input at a trust boundary, and
		// an unrecognized mode is neither activity nor interval, so puzzles would simply stop
		// starting with nothing to say why.
		switch Mode(mode) {
		case ModeActivity, ModeInterval:
			set.Mode = Mode(mode)
			notes = append(notes, "mode is "+mode)
		case "":
		default:
			notes = append(notes, "ignored an unknown mode "+strconv.Quote(mode))
		}
		if minutes > 0 {
			set.Interval = clampInterval(time.Duration(minutes) * time.Minute)
			notes = append(notes, "interval is "+set.Interval.String())
		}
	})

	log.Printf("[WORDGAME] %s changed the settings via /%s: %s",
		who.UserID, configCommandName, set)
	// The change AND the resulting state, because a diff alone leaves an operator guessing at
	// what the other two dials are, and interval mode with an interval nobody has checked is the
	// combination that surprises people.
	s.guard.Respond(i, strings.Join(notes, ", ")+".\nword games: "+set.String(), true)
}

// clampInterval keeps a requested period inside the bounds Discord's own option only asks
// politely about. An interaction payload is user input at a trust boundary like any other.
func clampInterval(d time.Duration) time.Duration {
	return min(max(d, minInterval), maxInterval)
}

// configArgs reads the four optional options.
//
// All four are optional and any combination is legal, because they are independent dials rather
// than alternatives: "bind here and switch to interval every 20 minutes" is one intention and
// should be one command.
func configArgs(i *discordgo.Interaction) (channel, mode string, minutes int, reset bool) {
	if i == nil {
		return "", "", 0, false
	}
	for _, opt := range i.ApplicationCommandData().Options {
		if opt == nil {
			continue
		}
		switch opt.Name {
		case optChannel:
			channel = opt.StringValue()
		case optMode:
			mode = opt.StringValue()
		case optInterval:
			minutes = int(opt.IntValue())
		case optReset:
			reset = opt.BoolValue()
		}
	}
	return channel, mode, minutes, reset
}

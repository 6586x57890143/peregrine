// Package chat is the reactor: one message in, a list of named steps, and a short-circuit
// contract.
//
// This replaces a single 480-line handleMessage that ran twelve sections in sequence with no
// way for any of them to say "this message was for me, stop". That absence is finding 9: the
// !leaderboard branch had no return, so after answering the command the handler carried
// straight on into the aggro check, the reply generator, name extraction, LEARNING THE
// COMMAND INTO THE CORPUS, the voice handler and the image reposter. The bot answered a
// command, then replied to it as if it were conversation, then taught itself the string
// "!leaderboard".
//
// The fix is not a return statement. A return statement fixes it once, in one branch, and the
// next command someone adds has the same bug available to it. What closes it is making the
// contract explicit: every step reports whether it CONSUMED the message, and the runner stops
// when one does. A new command that returns handled gets the behaviour automatically, and one
// that forgets is visible as a missing return value rather than as an absent statement in the
// middle of four hundred lines.
//
// # Consumed means "this was addressed to me", not "I did something"
//
// The aggro step reacts and the reply step posts, and neither consumes, because a message
// that earns a reply is still conversation and must still be learned from. Only a command
// consumes. TestOnlyCommandsConsume parses this package and fails if any other step returns
// true, because a step that starts consuming silently stops learning for whatever it matches
// and no behavioural test would notice.
//
// # The features are interfaces this package declares
//
// aggro, images and games are their own packages, and chat names only the methods it calls.
// That is this module's dominant seam pattern and it is what keeps the step table testable
// without five real plugins behind it. Registration happens in cmd/bot.
package chat

import (
	"context"
	"log"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/generate"
	"github.com/6586x57890143/peregrine/internal/learn"
	"github.com/6586x57890143/peregrine/internal/names"
	"github.com/6586x57890143/peregrine/internal/plugins/games"
	"github.com/6586x57890143/peregrine/internal/plugins/images"
	"github.com/6586x57890143/peregrine/internal/plugins/tuning"
	"github.com/6586x57890143/peregrine/internal/safety"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// Guard is the send chokepoint.
type Guard interface {
	SendReply(channelID, content string, ref *discordgo.MessageReference) (*discordgo.Message, bool)
}

// Activity records where people are talking and who is around.
type Activity interface {
	Note(guildID, channelID, authorID string)
}

// Aggro reacts to the current target's messages.
type Aggro interface {
	Handle(guildID, channelID, messageID, authorID string)
}

// Images remembers URLs and occasionally reposts one.
//
// The interface names images.Attachment rather than a local shape, which costs this package
// an import of that plugin and buys the alternative: Go requires exact type identity on an
// interface method's parameters, so a local type would have forced an adapter whose only job
// was copying two string fields.
type Images interface {
	Capture(channelID, messageID, authorID, content string, attachments []images.Attachment)
	MaybeRepost(channelID string, addressed bool)
	Forget(guildID string, messageIDs ...string)
}

// Games handles guesses and the bang commands.
type Games interface {
	Guess(guildID, channelID, messageID, content, authorID, displayName string) bool
	Command(cmd, arg, guildID, channelID string, who games.Requester) bool
}

// Voice queues a voice attachment for transcription and reports whether it took it.
type Voice interface {
	Offer(channelID, messageID, authorID string, attachments []discordgo.MessageAttachment) bool
}

// Recorder is the tuning export.
//
// It names tuning.Generation rather than a local shape for the same reason Images names
// images.Attachment: Go requires exact type identity on an interface method's parameters,
// so a local type would force an adapter whose only job is copying fields.
//
// Every method must be safe to call on a nil-free no-op implementation, because the export
// is off by default and this package should not learn what "off" means.
type Recorder interface {
	// Record takes one generation attempt, including the ones that produced nothing.
	Record(tuning.Generation)

	// NoteReply says a human replied to one of the bot's messages, which is the strongest
	// engagement signal available. This package already computes REPLY_TO_BOT to decide
	// whether to answer, so it costs nothing new here.
	NoteReply(botMessageID string)

	// NoteActivity says a message arrived in a channel, as the denominator for the above.
	NoteActivity(channelID string)

	// Count tallies a named event.
	Count(event string)
}

// Speaker produces a sentence, and says why when it does not.
//
// The Outcome is not decoration. An empty string used to mean "corpus empty" or "every
// re-seed dead-ended" or "the author gate refused everything" with no way to tell, so a bot
// that had learned 27 messages and stayed quiet reported nothing an operator could act on.
type Speaker interface {
	Sentence(req generate.Request) (string, generate.Outcome, error)
}

// Options are the dials this package reads.
type Options struct {
	// SelfMention matches the bot being talked ABOUT rather than to. Compiled by config.
	SelfMention *regexp.Regexp

	// RoastChance is the probability of the roast persona on a direct interaction. An
	// overheard self-mention always roasts, which is not a chance at all.
	RoastChance float64

	// EnableImages and EnableVoice gate the two optional steps.
	EnableImages bool
	EnableVoice  bool
}

// Service is the reactor.
type Service struct {
	session  *discordgo.Session
	corpora  storage.Corpora
	gate     *safety.Gate
	guard    Guard
	learner  *learn.Learner
	speaker  Speaker
	memories *generate.Memories
	emoji    generate.EmojiResolver
	activity Activity
	aggro    Aggro
	images   Images
	games    Games
	voice    Voice
	recorder Recorder
	opts     Options

	dispatcher *core.Dispatcher
	botID      string
}

// noRecorder is what a nil Recorder means: the export is off and every call is a no-op.
//
// A null object rather than a nil check at each of the five call sites, matching
// wordgame.noCounter. The reason is the one this repo keeps rediscovering: a rule applied at
// four of five call sites is not a rule, and the fifth is where the nil dereference comes
// from.
type noRecorder struct{}

func (noRecorder) Record(tuning.Generation) {}
func (noRecorder) NoteReply(string)         {}
func (noRecorder) NoteActivity(string)      {}
func (noRecorder) Count(string)             {}

// Deps is everything the reactor needs. A struct rather than fifteen parameters, because a
// fifteen-parameter constructor is a positional-argument bug waiting to happen.
type Deps struct {
	Session  *discordgo.Session
	Corpora  storage.Corpora
	Gate     *safety.Gate
	Guard    Guard
	Learner  *learn.Learner
	Speaker  Speaker
	Memories *generate.Memories
	Emoji    generate.EmojiResolver
	Activity Activity
	Aggro    Aggro
	Images   Images
	Games    Games
	Voice    Voice
	Recorder Recorder

	Options Options
}

// New builds the reactor.
func New(d Deps) *Service {
	recorder := d.Recorder
	if recorder == nil {
		recorder = noRecorder{}
	}
	return &Service{
		session: d.Session, corpora: d.Corpora, gate: d.Gate, guard: d.Guard,
		learner: d.Learner, speaker: d.Speaker, memories: d.Memories, emoji: d.Emoji,
		activity: d.Activity, aggro: d.Aggro, images: d.Images, games: d.Games,
		voice: d.Voice, recorder: recorder, opts: d.Options,
	}
}

func (s *Service) Name() string { return "chat" }

// Init registers the gateway handlers.
//
// Here rather than in Start, because discordgo begins dispatching inside Open and Open
// happens between Init and Start. Registering in Start would drop every message that arrived
// in that window.
func (s *Service) Init(deps core.Deps) error {
	s.dispatcher = deps.Dispatcher

	s.session.AddHandler(s.onMessage)

	// The deletion handlers, gated on the feature that reads their result. Reposting off
	// means nothing is cached, so there is nothing to revoke and no reason to take a write
	// transaction on every deletion the bot sees.
	if s.opts.EnableImages {
		s.session.AddHandler(s.onDelete)
		s.session.AddHandler(s.onBulkDelete)
	}
	return nil
}

// Start records the bot's own identity, which is only knowable once the gateway is READY.
func (s *Service) Start(context.Context) error {
	user, err := s.session.User("@me")
	if err != nil {
		return err
	}
	s.botID = user.ID
	s.learner.SetBotID(user.ID)
	log.Printf("[INFO] Bot ID: %s", user.ID)
	return nil
}

// Shutdown does nothing: the dispatcher owns the in-flight work and cmd/bot drains it.
func (s *Service) Shutdown(context.Context) error { return nil }

// BotID reports the bot's own user ID. Read by the features that need to know who we are.
func (s *Service) BotID() string { return s.botID }

// onMessage is the gateway handler. It does the cheapest possible rejection and then hands
// the work to the dispatcher.
//
// It used to call wg.Add(1) on the same WaitGroup the shutdown path waited on and then spawn
// a goroutine per message, which was two bugs and a cost: an Add racing a Wait at zero
// panics, wg.Wait returning did not mean handlers had finished with the database, and
// goroutines-per-message is unbounded on a channel that can burst (finding 4).
//
// A dropped message is logged rather than retried. discordgo dispatches every event on its
// own goroutine, so blocking here to wait for queue space would grow goroutines without
// bound and turn a slow corpus into unbounded memory use. Best-effort is the honest
// semantics for chat.
func (s *Service) onMessage(_ *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}
	if !s.dispatcher.Submit(func(context.Context) { s.handle(m) }) {
		log.Printf("[QUEUE] dropped message from %s: work queue full (%d dropped so far)",
			m.Author.Username, s.dispatcher.Dropped())
	}
}

// onDelete revokes anything the deleted message contributed to the repost cache.
//
// Through the dispatcher, like every other gateway event: a moderator clearing a spam raid
// produces a burst and each one of these opens a write transaction, so doing it on
// discordgo's own goroutine would put an unbounded number of them in line for bbolt's single
// writer.
//
// Unlike onMessage this does NOT skip bots, and the difference is deliberate. That skips them
// because a bot's message must not be learned; here the question is whether a cached URL's
// source still exists, and the answer does not depend on who posted it.
func (s *Service) onDelete(_ *discordgo.Session, m *discordgo.MessageDelete) {
	if m.ID == "" {
		return
	}
	// The guild rides along because the cache is per guild now: a deletion revokes an image
	// in the server it was posted in and nowhere else.
	guildID, id := m.GuildID, m.ID
	if !s.dispatcher.Submit(func(context.Context) { s.images.Forget(guildID, id) }) {
		log.Printf("[QUEUE] dropped a message deletion in %s: work queue full (%d dropped so far)",
			m.ChannelID, s.dispatcher.Dropped())
	}
}

// onBulkDelete is the same for Discord's bulk event, which is what a moderator purging a
// channel produces. Handled as one unit of work rather than one per ID, so a hundred-message
// purge is one write transaction instead of a hundred.
func (s *Service) onBulkDelete(_ *discordgo.Session, m *discordgo.MessageDeleteBulk) {
	if len(m.Messages) == 0 {
		return
	}
	ids := append([]string(nil), m.Messages...)
	guildID := m.GuildID
	if !s.dispatcher.Submit(func(context.Context) { s.images.Forget(guildID, ids...) }) {
		log.Printf("[QUEUE] dropped a bulk deletion of %d messages in %s: work queue full (%d dropped so far)",
			len(ids), m.ChannelID, s.dispatcher.Dropped())
	}
}

// reaction is everything a step needs about one message. One per message.
//
// It exists mostly to hold the two things that used to be recomputed: the flag set and the
// mentioned-user list. Recomputing was not free, and in the second case it was expensive in a
// way that mattered.
type reaction struct {
	m     *discordgo.MessageCreate
	start time.Time
	flags map[string]bool

	// mentioned caches the resolved names for this message.
	//
	// It used to be computed THREE times per message: once by self-learning, once by name
	// extraction and once by the learn step. Each call makes a GuildMember REST request per
	// mention and then opens a read transaction, so a message mentioning three people cost
	// nine REST calls where three would do.
	mentioned    []names.User
	mentionedSet bool

	// referenced is the message this one replies to, when there is one. Filled by
	// stepClassify and read by stepReply, so the fetch happens once.
	referenced *discordgo.Message
}

// step is one stage of handling. It reports whether it CONSUMED the message.
type step struct {
	name string
	fn   func(*Service, *reaction) bool
}

// steps run in order, and the order is behaviour rather than taste.
//
// Three placements are load-bearing:
//
//   - The learn gate is FIRST, so a message the bot will not learn from is also not replied
//     to and not reacted to. That is a convenience rather than the protection: the protection
//     is CheckLearn inside the learn path, which every caller reaches.
//   - The activity step is AFTER the gate, so a spam flood cannot advertise a channel as busy
//     and pull the bot toward exactly the place it should be ignoring.
//   - Commands come BEFORE the reply and learn steps, which is the whole point of the
//     short-circuit. Putting them after would answer the command and then also reply to it
//     and learn it, which is finding 9 with extra steps.
var steps = []step{
	{"learn-gate", (*Service).stepLearnGate},
	{"activity", (*Service).stepActivity},
	{"memory", (*Service).stepMemory},
	{"word-game", (*Service).stepWordGame},
	{"commands", (*Service).stepCommands},
	{"aggro", (*Service).stepAggro},
	{"classify", (*Service).stepClassify},
	{"reply", (*Service).stepReply},
	{"learn", (*Service).stepLearn},
	{"voice", (*Service).stepVoice},
	{"images", (*Service).stepImages},
}

// handle runs the reactor over one message.
func (s *Service) handle(m *discordgo.MessageCreate) {
	r := &reaction{m: m, start: time.Now(), flags: map[string]bool{}}
	log.Printf("[MSG] from %s: %q", m.Author.Username, m.Content)

	for _, st := range steps {
		if st.fn(s, r) {
			log.Printf("[OK] msg from %s consumed by %s in %s",
				m.Author.Username, st.name, time.Since(r.start))
			return
		}
	}
	log.Printf("[OK] handled msg from %s in %s | flags: %+v",
		m.Author.Username, time.Since(r.start), r.flags)
}

// mentions resolves the people in this message at most once.
func (s *Service) mentions(r *reaction) []names.User {
	if !r.mentionedSet {
		// A nil session is passed as a nil INTERFACE rather than as a typed nil pointer, which
		// is the difference between names.Resolve skipping the member lookup and dereferencing
		// nothing. Its guard reads `s != nil`, and an interface holding a nil *discordgo.Session
		// is not nil. Unreachable in production, where there is always a session, and reachable
		// in every test that hands this package one message from a guild.
		var session names.Session
		if s.session != nil {
			session = s.session
		}
		r.mentioned = names.OfMessage(session, s.corpora, r.m, r.m.GuildID)
		r.mentionedSet = true
	}
	return r.mentioned
}

// stepLearnGate drops a message the corpus must not see.
//
// Reject early, so the bot neither replies to nor reacts to a message it will not learn from
// either. This is a convenience, not the protection: the protection is CheckLearn inside the
// learn path. If this step were deleted the corpus would still be safe; the bot would just
// waste work replying to spam.
//
// There used to be a filterSlurs call next to this, and removing it was load-bearing rather
// than tidying. It replaced matches in place, so the message was learned anyway with its
// structure intact and a harmless token in the slur's grammatical position (SPEC.md section
// 4, A5). With CheckLearn in place, laundering here would be strictly worse than useless,
// because the gate would receive the already cleaned text, find nothing, and allow it.
func (s *Service) stepLearnGate(r *reaction) bool {
	if v := s.gate.CheckLearn(r.m.Content); !v.Allowed {
		log.Printf("[FILTER] Message from %s dropped, no processing will occur: %s",
			r.m.Author.Username, v.Reason)
		return true
	}
	return false
}

// stepActivity records that this channel and this author are alive.
//
// AFTER the learn gate, deliberately: this number decides where the bot speaks unprompted,
// whether a channel has earned a word game, and who gets aggro, so counting spam would pull
// it toward the place it should be ignoring.
func (s *Service) stepActivity(r *reaction) bool {
	s.activity.Note(r.m.GuildID, r.m.ChannelID, r.m.Author.ID)

	// The tuning export's denominator, recorded from the same place and after the same gate,
	// so "did anyone say anything after the bot spoke" counts only traffic the corpus was
	// willing to see. Counting spam here would make a raid look like engagement.
	s.recorder.NoteActivity(r.m.ChannelID)
	return false
}

// stepMemory records the message in this channel's conversation memory.
//
// Per channel, so context does not bleed across unrelated threads (finding G8). Never
// consumes: remembering is not answering.
//
// MENTIONS ARE SUBSTITUTED FIRST, which they were not. Without it the memory stores <@123>
// blobs where a name belongs, and those reach the recent seed tier and the recency term as
// opaque tokens that match nothing. stepReply and stepLearn both already do this; memory was
// the one path that did not, which is M14's gap one step over. s.mentions is memoized on the
// reaction and two later steps call it anyway, so this costs no extra REST call.
//
// The names are recorded alongside the words so that WHO the channel was talking about decays
// at the same rate as WHAT it said.
func (s *Service) stepMemory(r *reaction) bool {
	mentioned := s.mentions(r)

	about := make([]string, 0, len(mentioned))
	for _, u := range mentioned {
		about = append(about, u.Username)
	}

	s.memories.For(r.m.ChannelID).Add(names.Substitute(r.m.Content, mentioned), about)
	return false
}

// stepWordGame runs the scramble game: a win if one is live here, otherwise a chance to
// start one.
//
// Does NOT consume, which is a decision recorded in SPEC.md section 10 rather than an
// oversight. A guess is ordinary chat, so it still reaches the reply and learn steps.
func (s *Service) stepWordGame(r *reaction) bool {
	name := r.m.Author.Username
	if r.m.Member != nil && r.m.Member.Nick != "" {
		name = r.m.Member.Nick
	}
	s.games.Guess(r.m.GuildID, r.m.ChannelID, r.m.ID, r.m.Content, r.m.Author.ID, name)
	return false
}

// stepCommands handles the bang commands, and it is the reason the short-circuit contract
// exists.
//
// They CONSUME the message. Before this, !leaderboard answered and then fell through into
// everything below it, so the bot replied to its own command as if it were conversation and
// then learned the string into the corpus (finding 9).
//
// A command is consumed even when it fails or is refused. An unauthorized !wordgame is still
// a command rather than something to reply to.
func (s *Service) stepCommands(r *reaction) bool {
	cmd, arg := commandFor(r.m.Content)
	if cmd == "" {
		return false
	}
	// Counted by command, never by argument. A planted word is user text, and a usage tally
	// carrying it would put arbitrary content into the tuning archive by a side door.
	s.recorder.Count("command:" + cmd)
	return s.games.Command(cmd, arg, r.m.GuildID, r.m.ChannelID, s.requester(r))
}

// requester is who sent a command and what Discord says they may do in that channel.
//
// Read from the STATE CACHE, which costs no request: discordgo computes the permission set from
// the guild, the member's roles and the channel's overwrites, all of which the gateway has
// already delivered because M3 added IntentsGuilds. The obvious alternative, fetching the member
// and folding the roles by hand, is a REST call per command and a second implementation of
// Discord's own permission arithmetic.
//
// A miss FAILS CLOSED, leaving Permissions zero. That matches internal/channels' rule that "we
// could not tell" has to mean "do not", and it costs nothing real: the bootstrap admin ID is
// checked separately and does not depend on this, so a cold cache degrades to exactly the
// authorization this command had before M25 rather than to none.
func (s *Service) requester(r *reaction) games.Requester {
	who := games.Requester{UserID: r.m.Author.ID}
	if r.m.GuildID == "" {
		// A DM has no administrators. Nothing to resolve, and nothing this bot runs there.
		return who
	}
	if s.session == nil || s.session.State == nil {
		// No state cache is the same answer as a cache miss below: we could not tell, so the
		// permissions stay zero and the check fails closed. It was unreachable while every
		// test message was guildless, and M31 gave them guilds.
		return who
	}
	perms, err := s.session.State.UserChannelPermissions(r.m.Author.ID, r.m.ChannelID)
	if err != nil {
		return who
	}
	who.Permissions = perms
	return who
}

// commandFor recognizes a command and its one optional argument, or returns "".
//
// # It still matches the whole message, and that is what makes the argument safe
//
// A bare command matches the WHOLE trimmed message, not a prefix. That is deliberate: a prefix
// match would make it impossible to talk ABOUT a command, so "you should try !leaderboard
// sometime" would be swallowed and answered instead of being ordinary chat.
//
// The argument form preserves exactly that property by accepting only TWO tokens whose FIRST is
// the command. "you should try !wordgame sometime" is four tokens and does not match; "!wordgame
// banana pancakes" is three and does not either. The alternative, taking everything after the
// command, would have made a sentence containing "!wordgame" into an invocation with the rest of
// the sentence as its word.
//
// Only !wordgame takes one. !leaderboard renders the invoker's own rank and has nothing to name.
func commandFor(content string) (cmd, arg string) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(content)))
	switch len(fields) {
	case 1:
		switch fields[0] {
		case "!leaderboard", "!wordgame":
			return fields[0], ""
		}
	case 2:
		// Letters, or digits, and nothing mixed. Checked HERE as well as in the games service,
		// and the two checks are about different questions: this one decides what counts as an
		// invocation at all, so "!wordgame :)" stays chat rather than becoming a command that
		// answers with a refusal, while the service decides what can be a puzzle or a run
		// length.
		//
		// Digits are a gauntlet ("!wordgame 5") and letters are a planted word. They cannot
		// collide, because a token that is all digits is not a word and a word is not a number,
		// which is what lets the argument stay a single untyped token instead of growing a
		// subcommand.
		if fields[0] == "!wordgame" && (isWord(fields[1]) || isNumber(fields[1])) {
			return fields[0], fields[1]
		}
	}
	return "", ""
}

// isWord reports whether a token is letters and nothing else.
func isWord(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// isNumber reports whether a token is ASCII digits and nothing else.
//
// unicode.IsDigit would also accept Devanagari and full-width digits, which strconv.Atoi then
// refuses, so the two would disagree about what an invocation is: the command would be
// recognized, consumed, and then quietly do nothing. Narrower here is what keeps them agreeing.
func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// stepAggro hands the message to the aggro feature.
//
// Never consumes: a reaction is not an answer, and the target's message still gets learned
// and possibly replied to, which is the point of aggro.
func (s *Service) stepAggro(r *reaction) bool {
	s.aggro.Handle(r.m.GuildID, r.m.ChannelID, r.m.ID, r.m.Author.ID)
	return false
}

// stepClassify computes the flag set the later steps read.
//
// Its own step so the flags are computed after the commands have had their chance to consume
// the message, which saves a ChannelMessage REST call on every command: the REPLY_TO_BOT
// check fetches the referenced message.
func (s *Service) stepClassify(r *reaction) bool {
	m := r.m

	if strings.Contains(m.Content, "<@"+s.botID+">") || strings.Contains(m.Content, "<@!"+s.botID+">") {
		r.flags["MENTIONED"] = true
	}

	// THE REFERENCED MESSAGE, kept rather than discarded (SPEC.md section 8, finding 50).
	//
	// This used to make a REST call, read ref.Author.ID to set one boolean, and throw the
	// content away. The content is exactly the context a reply needs, and it was already being
	// paid for.
	//
	// The gateway payload usually carries it, so the REST call is now a FALLBACK and the
	// request count goes down. It cannot be deleted: discordgo documents three distinct nil
	// cases, and only one of them (Discord did not attempt the fetch) is worth retrying.
	//
	// A FORWARD is not a reply. MessageReference.Type distinguishes them and the old check
	// ignored it, so a forwarded message triggered a fetch that could never identify a bot
	// author. A forward also carries snapshots rather than a referenced message, so there is
	// nothing here to answer.
	if ref := s.referenced(m); ref != nil {
		r.referenced = ref
		if ref.Author != nil && ref.Author.ID == s.botID {
			r.flags["REPLY_TO_BOT"] = true

			// A human answering the bot, which is the strongest signal the export can get
			// that a reply landed. Free here: this branch already exists to decide whether
			// to reply, so nothing extra is fetched or computed.
			s.recorder.NoteReply(ref.ID)
		}
	}

	// Voice attachments. Discord voice messages are always .ogg and named
	// voice-message.ogg.
	for _, att := range m.Attachments {
		if strings.EqualFold(att.Filename, "voice-message.ogg") && strings.EqualFold(filepath.Ext(att.Filename), ".ogg") {
			r.flags["VOICE"] = true
			break
		}
	}

	r.flags["TEXT"] = strings.TrimSpace(m.Content) != ""
	if r.flags["TEXT"] && s.opts.SelfMention != nil && s.opts.SelfMention.MatchString(m.Content) {
		r.flags["SELF_MENTION_KEYWORD"] = true
	}
	return false
}

// referenced returns the message m replies to, preferring the gateway payload.
//
// Three things this has to get right, and the old inline version got one of them:
//
//   - A FORWARD IS NOT A REPLY. MessageReference.Type distinguishes them, and a forward
//     carries message snapshots rather than a referenced message, so there is nothing to
//     answer and the fetch below could never succeed at identifying an author.
//   - ReferencedMessage is usually already on the payload for an ordinary reply, so asking
//     Discord again is asking the network for something already in hand, which is finding
//     17's shape. Preferring it makes this path cost FEWER requests than it used to.
//   - A nil ReferencedMessage is ambiguous. discordgo documents three cases: not a reply,
//     a reply Discord chose not to fetch, and a reply whose target was deleted. Only the
//     middle one is worth a REST call, and the other two make it fail harmlessly, so the
//     fallback stays rather than being cleverly conditioned.
func (s *Service) referenced(m *discordgo.MessageCreate) *discordgo.Message {
	ref := m.MessageReference
	if ref == nil || ref.MessageID == "" || ref.ChannelID == "" {
		return nil
	}
	if ref.Type == discordgo.MessageReferenceTypeForward {
		return nil
	}
	if m.ReferencedMessage != nil {
		return m.ReferencedMessage
	}

	got, err := s.session.ChannelMessage(ref.ChannelID, ref.MessageID)
	if err != nil {
		return nil
	}
	return got
}

// stepReply generates and posts a reply when the bot was addressed.
//
// Never consumes. A message that earns a reply is still ordinary conversation and must still
// be learned from, which is the distinction between this and a command.
func (s *Service) stepReply(r *reaction) bool {
	if !r.flags["TEXT"] {
		return false
	}
	if !r.flags["MENTIONED"] && !r.flags["REPLY_TO_BOT"] && !r.flags["SELF_MENTION_KEYWORD"] {
		return false
	}

	m := r.m
	replyStart := time.Now()

	// Mentions become names before anything reads the prompt. s.mentions memoizes on the
	// reaction and stepLearn calls it for every text message anyway, so this costs nothing new.
	// Without it a mention reaches generation as one opaque <@123> token that Canonical cannot
	// resolve, so the most explicit way to name somebody was invisible to every name tier.
	prompt := names.Substitute(m.Content, s.mentions(r))
	roast := false
	switch {
	case r.flags["SELF_MENTION_KEYWORD"] && !r.flags["MENTIONED"] && !r.flags["REPLY_TO_BOT"]:
		// Overheard rather than addressed: the bot is being talked ABOUT, so it always roasts.
		//
		// The prompt used to be replaced with the fixed string "<START> peregrine" here, which
		// threw the message away on the path the bot is most often used through: "peregrine
		// what is up with lachy" lost "lachy" entirely, so no name tier and no topic tier ever
		// saw it. Keeping the content is still self-referential by construction, because this
		// branch only runs when the self-mention pattern matched, so the bot's own name is
		// already a prompt word and seeds through tiers 1 and 5.
		roast = true
		log.Printf("[INFO] Activating roast mode due to a self-mention keyword. Prompt: %q", prompt)
	default:
		if rand.Float64() < s.opts.RoastChance {
			roast = true
			log.Println("[INFO] Activating roast mode for a direct interaction.")
		}
	}

	// THE REFERENCED MESSAGE STEERS, IT DOES NOT SEED. That distinction is the whole rule:
	// the referenced message says what we are talking about, the prompt says what was said to
	// me. Generation puts Context into the topic and association terms and never into the
	// prompt set or the prompt seed tier, because starting a reply on a third party's phrasing
	// is answering the wrong message.
	//
	// It is not learned here either, and that is not an omission: it is already in the corpus
	// under its own message ID, and learning it again would count its n-grams twice.
	var (
		context      string
		contextNames []string
	)
	if ref := r.referenced; ref != nil && ref.Content != "" && ref.Author != nil && ref.Author.ID != s.botID {
		// The bot's own messages are excluded on purpose. Its output already re-enters the
		// corpus through selfLearn, and feeding it back as context as well is a loop that
		// makes the bot progressively more like itself.
		context = names.Substitute(ref.Content, names.Spellings(ref.Author, ref.Member))
		if ref.Author.Username != "" {
			contextNames = append(contextNames, ref.Author.Username)
		}
	}

	// The trace is allocated unconditionally, which costs one struct on a path that is
	// already opening a read transaction and walking a corpus. Making it conditional would
	// mean this package knowing whether the export is on, which is precisely the knowledge
	// the Recorder seam exists to keep out of here.
	var trace generate.Trace

	reply, outcome, err := s.speaker.Sentence(generate.Request{
		GuildID:      m.GuildID,
		Prompt:       prompt,
		Context:      context,
		ContextNames: contextNames,
		Roast:        roast,
		Memory:       s.memories.For(m.ChannelID),
		Emoji:        s.emoji,
		Trace:        &trace,
	})

	// record closes over everything the export wants, so each of the four ways out of this
	// function below records once and cannot forget a field. THE SILENT OUTCOMES MATTER MOST:
	// "the bot said nothing" is the case where the seed tier and the starved-step count are
	// the only evidence there is, and it is the one an operator on a young corpus has to
	// diagnose (finding 32 made it visible in the log; this makes it countable).
	record := func(sentID, text string, sent bool) {
		s.recorder.Record(tuning.Generation{
			ID:         sentID,
			Trigger:    "reply",
			Channel:    m.ChannelID,
			Prompt:     prompt,
			HasContext: context != "",
			Names:      contextNames,
			Roast:      roast,
			Reply:      text,
			Outcome:    outcome.String(),
			Sent:       sent,
			Took:       time.Since(replyStart),
			Trace:      &trace,
		})
	}

	if err != nil {
		log.Printf("[ERR] reply generation failed: %v", err)
		record("", "", false)
		return false
	}
	if reply == "" {
		// SAID OUT LOUD. This returned silently until finding 32, so the bot was addressed,
		// classified the message correctly, decided it had nothing to say, and left no trace
		// of the decision. The bot staying quiet is the design; the operator being unable to
		// tell why is the bug, and on a fresh deploy it is the first question they have.
		switch outcome {
		case generate.CorpusEmpty:
			log.Printf("[RESP] nothing to say to %s yet: the corpus is empty, so ingestion "+
				"has not populated anything", m.Author.Username)
		case generate.TooShort:
			log.Printf("[RESP] nothing to say to %s: generation stayed under the %d-word floor. "+
				"On a young corpus this is usually PEREGRINE_MIN_DISTINCT_AUTHORS refusing "+
				"continuations only one person has said", m.Author.Username, 2)
		case generate.Produced:
			// Unreachable: Produced with an empty string would be a bug in generate.
			log.Printf("[RESP] nothing to say to %s, and no reason was given", m.Author.Username)
		}
		record("", "", false)
		return false
	}

	// Through the guard, and this is the call site finding 8 was about.
	// ChannelMessageSendReply set no AllowedMentions, so Discord's default applied and the
	// author was pinged on every single interaction.
	sent, ok := s.guard.SendReply(m.ChannelID, reply,
		&discordgo.MessageReference{MessageID: m.ID, ChannelID: m.ChannelID})
	if !ok {
		// The guard has already logged whether this was a refusal or a failure, and which.
		//
		// NO TEXT IN THE EXPORT for this one, which is the point of routing it through the
		// same helper as the successes rather than skipping it. The guard refuses on the
		// emit gate among other things, and internal/safety deliberately never records the
		// offending content anywhere: a telemetry file is not an exception to that.
		log.Printf("[RESP] reply to %s was not sent", m.Author.Username)
		record("", "", false)
		return false
	}
	log.Printf("[RESP] replied to %s in %s: %q", m.Author.Username, time.Since(replyStart), reply)
	record(sent.ID, reply, true)

	s.selfLearn(r, sent.ID, reply)
	return false
}

// selfLearn feeds the bot's own reply back into the corpus, keyed by THE REPLY'S OWN MESSAGE
// ID.
//
// That key is finding 6, and it was a data-loss bug rather than an inefficiency. Both this
// and the learn step used the USER's message ID, and the learn path dedupes on that ID, so
// whichever transaction committed first marked the ID seen and the other became a silent
// no-op: either the user's message or the bot's reply was thrown away on every single
// interaction, and which one depended on a goroutine race.
func (s *Service) selfLearn(r *reaction, replyID, replyContent string) {
	// Resolved from the state cache rather than a REST call. This used to be s.User("@me")
	// on every reply, which asks Discord who we are when the ID is already known and the
	// username is already cached.
	botName := "peregrine"
	if s.session != nil && s.session.State != nil && s.session.State.User != nil && s.session.State.User.Username != "" {
		botName = s.session.State.User.Username
	}
	botAsMention := names.User{Name: botName, UserID: s.botID, Username: botName}

	// The mentioned users come from the cache on the reaction, so this does not repeat the
	// REST calls the learn step already made.
	//
	// The name resolution is hoisted OUT of the write transaction, and that is a fix rather
	// than a tidy-up: it used to run inside the Update, which meant a read transaction and a
	// series of REST calls nested inside a write transaction while bbolt's single writer lock
	// was held.
	//
	// The bot's own user ID reaching the learn path is deliberate and safe: it compares to
	// the recorded bot ID and passes an empty author to LearnNgram, so self-learning
	// contributes nothing to author diversity (SPEC.md section 4, A6).
	mentioned := s.mentions(r)
	channelID := r.m.ChannelID

	// THE BOT HEARS ITSELF NOW. Its replies were written to the corpus and never added to
	// conversation memory, so the one participant that speaks in every exchange was the only
	// one absent from the channel's record of what was being discussed. Added synchronously,
	// before the goroutine, because Memory is in-process and ordering matters: the reply
	// should be the freshest entry when the next message arrives.
	s.memories.For(channelID).Add(replyContent, nil)

	guildID := r.m.GuildID
	go func() {
		store, err := s.corpora.For(guildID)
		if err != nil {
			log.Printf("[WARN] self-learning skipped for reply %s in %s: %v", replyID, channelID, err)
			return
		}
		if err := store.Update(func(w *storage.Writer) error {
			return s.learner.Message(w, replyContent, replyID, botAsMention, mentioned)
		}); err != nil {
			log.Printf("[WARN] self-learning failed for reply %s in %s: %v", replyID, channelID, err)
		}
	}()
}

// stepLearn records the author's message and the names in it.
func (s *Service) stepLearn(r *reaction) bool {
	if !r.flags["TEXT"] {
		return false
	}
	m := r.m
	mentioned := s.mentions(r)

	// The corpus this guild's text belongs in, resolved ONCE for the whole step.
	//
	// A message with no guild is a DM, and DMs are not learned: there is deliberately no
	// shared corpus for them, because a fallback file is where every threading mistake would
	// quietly drain. Said out loud rather than dropped, because a bot that silently learns
	// nothing is the failure mode this repository keeps rediscovering.
	store, err := s.corpora.For(m.GuildID)
	if err != nil {
		log.Printf("[LEARN] not learning message %s in %s: %v", m.ID, m.ChannelID, err)
		return false
	}

	if len(mentioned) > 0 {
		_ = store.Update(func(w *storage.Writer) error {
			for _, user := range mentioned {
				if _, err := names.Record(w, user.Name, user.UserID, user.Username); err != nil {
					log.Printf("[WARN] Failed to learn name %q during extraction: %v", user.Name, err)
				}
			}
			return nil
		})
	}

	// The author, whose own message content gets associated with them. The append that used to
	// follow, putting them into the mentioned slice so associate would see them, is gone:
	// Learner.Message merges the author into that set itself now. This step did it and the
	// backfill did not, so every backfilled message learned no associations at all.
	author := names.Primary(m.Author, m.Member)

	if err := store.Update(func(w *storage.Writer) error {
		return s.learner.Message(w, m.Content, m.ID, author, mentioned)
	}); err != nil {
		log.Printf("[WARN] learning message %s failed: %v", m.ID, err)
	}
	return false
}

// stepVoice offers voice attachments to the transcription feature.
func (s *Service) stepVoice(r *reaction) bool {
	if !s.opts.EnableVoice || !r.flags["VOICE"] || s.voice == nil {
		return false
	}
	atts := make([]discordgo.MessageAttachment, 0, len(r.m.Attachments))
	for _, att := range r.m.Attachments {
		if att != nil {
			atts = append(atts, *att)
		}
	}
	s.voice.Offer(r.m.ChannelID, r.m.ID, r.m.Author.ID, atts)
	return false
}

// stepImages caches candidate URLs and occasionally reposts one.
func (s *Service) stepImages(r *reaction) bool {
	if !s.opts.EnableImages {
		return false
	}
	m := r.m

	atts := make([]images.Attachment, 0, len(m.Attachments))
	for _, att := range m.Attachments {
		if att != nil {
			atts = append(atts, images.Attachment{URL: att.URL, ContentType: att.ContentType})
		}
	}
	s.images.Capture(m.ChannelID, m.ID, m.Author.ID, m.Content, atts)

	addressed := r.flags["MENTIONED"] || r.flags["REPLY_TO_BOT"]
	s.images.MaybeRepost(m.ChannelID, addressed)
	return false
}

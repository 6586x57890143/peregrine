// Package discordguard is the single chokepoint every outbound Discord call passes
// through.
//
// It exists for one reason above all others: NOTHING PEREGRINE POSTS MAY PING.
//
// That matters more here than in most bots, and the reason is structural rather than
// cautious. Peregrine's output is Markov text assembled from arbitrary user messages, so
// every send is untrusted-input-shaped BY CONSTRUCTION. A user mention that got learned
// is now a token in the corpus like any other, and the generator will emit it again
// whenever the chain walks through it, which means it pings that person forever. Nobody
// chose that and nobody can predict when it fires.
//
// discordgo will not stop it. Every send helper there builds its request with a nil
// AllowedMentions, and the field carries `omitempty`, so it is dropped from the JSON
// entirely and Discord reads a missing field as "parse every mention in the content".
// Supplying a non-nil AllowedMentions whose Parse is an explicit empty slice sends
// "parse":[] , which is Discord's documented "allow nothing" (SPEC.md section 8,
// finding 8).
//
// The fix is one pointer and one slice, and the second half is easy to leave out: see
// allowedMentions for why the slice is set explicitly rather than left zero, which is
// something the test in this package established by trying it.
//
// # Why a chokepoint and not thirteen call sites
//
// This is the same argument as safety.CheckLearn living inside learnMessage. There were
// thirteen places that send something, and a rule applied at twelve of them is not a
// rule: the thirteenth is where the incident comes from, and the fourteenth has not been
// written yet. Suppression here means a future send is covered without its author having
// to know this package exists.
//
// The same goes for CheckEmit. Until M10 it sat at the single exit from generation,
// which covered the reply path and nothing else: the autonomous poster, the word-game
// announcements and the transcription results all reached Discord without passing it.
// Generation is not the only thing that produces text.
//
// # What this deliberately does NOT port from merlin
//
// Merlin's discordguard carries a per-guild pause, a dry-run mode, a write governor and
// an audit journal, because its dangerous operations are irreversible from Discord's
// side: a deleted archive channel, a member stripped of every role. Peregrine deletes
// nothing and edits no permissions. Its dangerous operation is SPEAKING, and the
// controls that fit that are one process-wide pause and one content gate, both of which
// already exist in internal/safety. Porting the rest would be structure with no failure
// mode behind it.
package discordguard

import (
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Session is the narrow slice of discordgo the guard calls.
//
// *discordgo.Session satisfies it structurally, which is the convention the rest of
// this repo uses: the consumer declares what it needs and the concrete type happens to
// fit. It is also what lets the tests here assert on the exact request that would have
// gone to Discord, which is the only way to check a field that only matters once it has
// been marshalled.
//
// Note that every send goes through ChannelMessageSendComplex, including the ones a
// caller expresses as plain content. That is not indirection for its own sake: the
// simple helpers in discordgo are precisely the ones that cannot express
// AllowedMentions, so routing through Complex is what makes suppression possible at all.
type Session interface {
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageEditComplex(m *discordgo.MessageEdit, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageDelete(channelID, messageID string, options ...discordgo.RequestOption) error
	MessageReactionAdd(channelID, messageID, emojiID string, options ...discordgo.RequestOption) error
	MessageReactionRemove(channelID, messageID, emojiID, userID string, options ...discordgo.RequestOption) error

	// UpdateStatusComplex is the presence line. It is a GATEWAY send rather than a REST
	// call, and it is here for the same reason everything else is: it is text the bot puts
	// in front of the whole server, so the one place that knows how to gate text should be
	// the one place that sets it.
	UpdateStatusComplex(usd discordgo.UpdateStatusData) error

	// The interaction half, added in M26 with the first slash command this bot has ever
	// registered. Responding is a SEND and is gated like one; registering is a write and is
	// here for the reason Delete is, so that every Discord call has one place that logs it.
	InteractionRespond(interaction *discordgo.Interaction, resp *discordgo.InteractionResponse, options ...discordgo.RequestOption) error
	ApplicationCommandBulkOverwrite(appID, guildID string, commands []*discordgo.ApplicationCommand, options ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error)
}

// EmitGate is the outbound half of internal/safety. *safety.Gate satisfies it.
//
// Declared as an interface rather than taking the concrete type so this package does not
// import safety, which keeps the dependency pointing one way and lets a test drive the
// guard with a gate that refuses everything.
type EmitGate interface {
	// CheckEmit reports whether text may be sent. The guard only needs the boolean;
	// the gate itself owns the logging, the counters and the operator alert, because
	// the reason a send was refused belongs to whoever decided to refuse it.
	CheckEmit(text string) bool
}

// Guard owns every outbound call.
//
// Safe for concurrent use: it holds no mutable state of its own, and the gate's pause
// flag is an atomic. One instance serves every goroutine.
type Guard struct {
	session Session
	gate    EmitGate
	log     *slog.Logger

	// ignoreChannels are channel IDs the bot must never post in, from
	// PEREGRINE_IGNORE_CHANNELS.
	//
	// Enforced here rather than in the reply logic because it has to hold for the
	// autonomous poster and the word games too. An operator setting this is saying
	// "not in there", not "not in reply to a message in there", and a check in the
	// reply path only would have been the weaker of the two readings.
	ignoreChannels map[string]struct{}
}

// New builds a Guard. A nil logger discards.
func New(s Session, gate EmitGate, log *slog.Logger, ignoreChannels []string) *Guard {
	ignore := make(map[string]struct{}, len(ignoreChannels))
	for _, id := range ignoreChannels {
		if id != "" {
			ignore[id] = struct{}{}
		}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Guard{session: s, gate: gate, log: log, ignoreChannels: ignore}
}

// allowedMentions is the suppression, in one place.
//
// A non-nil pointer to a zero value, and every part of that matters:
//
//   - Non-nil, because a nil pointer is dropped by omitempty and Discord treats the
//     absent field as "parse everything".
//
//   - Parse set to an EXPLICIT empty slice, which marshals as "parse":[] , the form
//     Discord's documentation gives for "allow nothing".
//
//     This is deliberately more explicit than it strictly has to be, and the reason is
//     worth recording because it looks redundant. discordgo leaves Parse without
//     omitempty and its comment says a zero-value struct therefore allows no mentions,
//     which is true of the FIELD being present but not of its value: a nil slice
//     marshals as "parse":null, not "parse":[]. That works only if Discord treats a
//     present allowed_mentions object with a null parse the same as an empty one, which
//     is a reasonable reading of the API and is not something the documentation says.
//     Setting the slice costs nothing and removes the dependency on that reading. The
//     test asserts the wire form, so if a future discordgo changes the tags it fails
//     here rather than in a channel.
//
//   - Roles and Users likewise empty rather than nil, for the same reason.
//
//   - RepliedUser false, which is the field a reply needs and a plain send does not.
//     discordgo's ChannelMessageSendReply sets no AllowedMentions at all, so Discord's
//     default applied and the author of the replied-to message was pinged on EVERY
//     interaction. In a bot that answers whenever it hears its own name, that is a
//     notification per conversation, forever.
func allowedMentions() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{
		Parse:       []discordgo.AllowedMentionType{},
		Roles:       []string{},
		Users:       []string{},
		RepliedUser: false,
	}
}

// Send posts content to a channel with every mention suppressed and the emit gate
// applied. It reports whether the message was sent.
//
// A refusal is not an error and the bot stays SILENT: no fallback string, no apology
// message. Silence is always safe, whereas a fallback is a new output that has to be
// reasoned about, and in a bot that already replies selectively an unexplained silence
// is indistinguishable from it choosing not to answer.
func (g *Guard) Send(channelID, content string) (*discordgo.Message, bool) {
	return g.SendReply(channelID, content, nil)
}

// SendReply posts content as a reply to ref, or as a plain message when ref is nil.
//
// One method for both, because the reply case is the one that pings and splitting them
// is how a future caller ends up on a path that forgot RepliedUser.
func (g *Guard) SendReply(channelID, content string, ref *discordgo.MessageReference) (*discordgo.Message, bool) {
	if !g.permit(channelID, content, "send") {
		return nil, false
	}

	msg, err := g.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content:         content,
		Reference:       ref,
		AllowedMentions: allowedMentions(),
	})
	if err != nil {
		// Logged, never discarded. Every one of these calls used to throw its error
		// away, so a send Discord refused (missing permission, rate limit, channel
		// deleted mid-flight) was indistinguishable from one that worked: the bot
		// appeared to ignore people at random with nothing in the log to say why.
		g.log.Error("discord send failed", "channel", channelID, "err", err)
		return nil, false
	}
	return msg, true
}

// SendEmbed posts an embed, gated and suppressed exactly like a plain send.
//
// # The gate reads every text field, not the description
//
// An embed is still the bot speaking, and its text is still assembled from things people
// typed: the leaderboard interpolates Discord nicknames, which are user-controlled and can
// contain anything. So CheckEmit runs over the concatenation of every field that renders,
// because a rule applied to the description and not to a field value is the same shape as a
// rule applied at thirteen of fourteen send sites.
//
// # Embeds are documented not to ping, and that is not why mentions are suppressed here
//
// AllowedMentions is set exactly as it is for a plain message. Discord's documentation says
// mentions inside an embed do not notify, and that may well hold forever, but it is precisely
// the kind of reading this package's allowedMentions comment refuses to depend on: the same
// argument was available for discordgo's "a zero value allows no mentions", which is true of
// the field being present and not of its value. Setting it costs one struct.
func (g *Guard) SendEmbed(channelID string, embed *discordgo.MessageEmbed) (*discordgo.Message, bool) {
	if embed == nil {
		return nil, false
	}
	if !g.permit(channelID, embedText(embed), "send-embed") {
		return nil, false
	}

	msg, err := g.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{embed},
		AllowedMentions: allowedMentions(),
	})
	if err != nil {
		g.log.Error("discord embed send failed", "channel", channelID, "err", err)
		return nil, false
	}
	return msg, true
}

// embedText is everything in an embed that renders as words, for the gate.
//
// Built by walking the struct rather than by naming the two fields anybody remembers. A
// blocklisted word in a field value is exactly as much of an incident as one in the
// description, and the difference between them is which part of the struct somebody happened
// to use.
func embedText(e *discordgo.MessageEmbed) string {
	var b strings.Builder
	write := func(s string) {
		if s == "" {
			return
		}
		b.WriteString(s)
		b.WriteByte('\n')
	}

	write(e.Title)
	write(e.Description)
	if e.Author != nil {
		write(e.Author.Name)
	}
	if e.Footer != nil {
		write(e.Footer.Text)
	}
	for _, f := range e.Fields {
		if f == nil {
			continue
		}
		write(f.Name)
		write(f.Value)
	}
	return b.String()
}

// Presence sets the bot's status line.
//
// Routed through the guard so that the structural test has something to point at and so the
// call is logged, which is the same argument Delete makes: every Discord call has one place
// that knows about it.
//
// # It is content-gated and NOT pause-gated, and both halves are deliberate
//
// Gated on content, because the caller may put a word from the corpus in here. A status line
// derived from user text is public output with no reply chain around it and no human context,
// which makes it the same category of risk as the autonomous poster rather than a lower one.
// A caller passing only its own numbers pays a cheap check it cannot fail.
//
// NOT gated on the pause switch, and this is the asymmetry Unreact already makes. Presence
// carries no channel content and cannot ping anybody, so suppressing it during an incident
// buys nothing and costs the operator their only sign the process is still alive: a bot with
// a frozen status line and no messages looks like a bot that has died, at exactly the moment
// somebody needs to know which of the two they have. The caller that wants a corpus word in
// there is the one that has to care, and it does: CheckEmit still refuses it.
func (g *Guard) Presence(text string) bool {
	if text != "" && !g.gate.CheckEmit(text) {
		// The gate has already logged the category and the rule and deliberately not the
		// text. The caller falls back to something it composed itself.
		return false
	}

	if err := g.session.UpdateStatusComplex(discordgo.UpdateStatusData{
		Status: "online",
		Activities: []*discordgo.Activity{{
			Name: text,
			// Watching, because everything peregrine can honestly say about itself is an
			// observation: how much it has read and how many people it knows. "Playing" would
			// be a claim about a game that does not exist.
			Type: discordgo.ActivityTypeWatching,
		}},
	}); err != nil {
		// Info rather than Error, matching Delete and the reactions: a presence update that
		// fails is cosmetic, the gateway retries on the next tick, and alarming about it
		// trains an operator to stop reading the log.
		g.log.Info("discord presence update failed", "err", err)
		return false
	}
	return true
}

// Edit replaces the content of a message the bot already sent.
//
// Gated and suppressed like a send, and for the same reason rather than for symmetry:
// an edit can introduce a mention that the original did not have, so an unsuppressed
// edit is a send with extra steps. This is the path the transcription worker takes to
// fill in its placeholder, which means the text arriving here is a Whisper transcript
// of arbitrary audio.
func (g *Guard) Edit(channelID, messageID, content string) bool {
	if !g.permit(channelID, content, "edit") {
		return false
	}

	if _, err := g.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:         channelID,
		ID:              messageID,
		Content:         &content,
		AllowedMentions: allowedMentions(),
	}); err != nil {
		g.log.Error("discord edit failed", "channel", channelID, "message", messageID, "err", err)
		return false
	}
	return true
}

// Delete removes a message.
//
// NOT gated on content, because there is no content: deleting says nothing and cannot
// ping. It is still routed through here so that every Discord call has one place that
// logs it, and so an ignored channel stays untouched.
//
// It logs at a lower urgency than the others on purpose. Failing to delete is routinely
// benign: somebody removed the message first, or the bot has no Manage Messages in that
// channel, and neither is worth alarming an operator about.
func (g *Guard) Delete(channelID, messageID string) bool {
	if g.ignored(channelID, "delete") {
		return false
	}
	if err := g.session.ChannelMessageDelete(channelID, messageID); err != nil {
		g.log.Info("discord delete failed, often benign", "channel", channelID, "message", messageID, "err", err)
		return false
	}
	return true
}

// React adds a reaction.
//
// Gated on the pause switch but not on content, because an emoji the operator
// configured is not untrusted text. It is here so that PEREGRINE_PAUSE_ALL_WRITES means
// what it says: during an incident the bot should stop reacting too, since a reaction is
// still the bot visibly participating.
func (g *Guard) React(channelID, messageID, emoji string) bool {
	if g.ignored(channelID, "react") {
		return false
	}
	if !g.gate.CheckEmit("") {
		// An empty string cannot trip a content rule, so the only thing this can
		// refuse is the pause switch, which is exactly what is wanted.
		g.log.Info("reaction suppressed", "channel", channelID)
		return false
	}
	if err := g.session.MessageReactionAdd(channelID, messageID, emoji); err != nil {
		g.log.Info("discord reaction failed", "channel", channelID, "message", messageID, "err", err)
		return false
	}
	return true
}

// Unreact removes one of the bot's own reactions.
//
// NOT gated on the pause switch, and that asymmetry with React is deliberate. Removing a
// reaction is the bot withdrawing rather than participating, so an operator who has hit
// the emergency stop wants it to succeed: refusing it would leave the bot's mark on
// somebody's message with no way to take it back until the pause is lifted.
//
// It is still routed through here so the ignore list and the logging apply, and so the
// structural test covering the reaction methods has something to point at. Discovered by
// dropping the call during the M10b reactor split and noticing the test did NOT catch it,
// because MessageReactionRemove was missing from the forbidden list; both halves of that
// gap are fixed.
func (g *Guard) Unreact(channelID, messageID, emoji, userID string) bool {
	if g.ignored(channelID, "unreact") {
		return false
	}
	if err := g.session.MessageReactionRemove(channelID, messageID, emoji, userID); err != nil {
		g.log.Info("discord reaction removal failed", "channel", channelID, "message", messageID, "err", err)
		return false
	}
	return true
}

// Respond answers a slash command, gated and suppressed exactly like a channel send.
//
// # An ephemeral reply is still the bot speaking
//
// "Only the person who asked can see it" narrows who is harmed; it does not change whether the
// bot said it. So this runs the same CheckEmit and the same pause switch as everything else, and
// sets AllowedMentions explicitly rather than trusting that a private message cannot ping. The
// text arriving here is assembled from things people typed, exactly like a reply is.
//
// The ignore list applies too, on the interaction's channel. An operator who said "not in there"
// meant it about the whole bot, and a slash command is a way into a channel like any other.
//
// ephemeral is the caller's choice rather than this package's, because the two answers a command
// gives want different audiences: a refusal belongs to the person refused, and a puzzle belongs
// to the channel.
func (g *Guard) Respond(i *discordgo.Interaction, content string, ephemeral bool) bool {
	if i == nil {
		return false
	}
	if !g.permit(i.ChannelID, content, "respond") {
		return false
	}

	data := &discordgo.InteractionResponseData{
		Content:         content,
		AllowedMentions: allowedMentions(),
	}
	if ephemeral {
		data.Flags = discordgo.MessageFlagsEphemeral
	}

	if err := g.session.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: data,
	}); err != nil {
		// An Error, matching Send rather than Delete: somebody typed a command and got
		// nothing, and Discord gives an interaction three seconds before it shows the user a
		// failure of its own. A silent one here is a bot that looks broken to the one person
		// who was definitely paying attention.
		g.log.Error("discord interaction response failed", "channel", i.ChannelID, "err", err)
		return false
	}
	return true
}

// RespondEmbed is Respond with an embed, gated over every text field like SendEmbed.
func (g *Guard) RespondEmbed(i *discordgo.Interaction, embed *discordgo.MessageEmbed, ephemeral bool) bool {
	if i == nil || embed == nil {
		return false
	}
	if !g.permit(i.ChannelID, embedText(embed), "respond-embed") {
		return false
	}

	data := &discordgo.InteractionResponseData{
		Embeds:          []*discordgo.MessageEmbed{embed},
		AllowedMentions: allowedMentions(),
	}
	if ephemeral {
		data.Flags = discordgo.MessageFlagsEphemeral
	}

	if err := g.session.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: data,
	}); err != nil {
		g.log.Error("discord interaction embed response failed", "channel", i.ChannelID, "err", err)
		return false
	}
	return true
}

// RegisterCommands replaces the bot's application commands with exactly this set.
//
// # Not content-gated, and not pause-gated
//
// A command definition is the bot's own text, written in this repository and never assembled
// from anything a user typed, so there is nothing for CheckEmit to judge; it is the same
// category as the emoji an operator configured for React. And PAUSE_ALL_WRITES is a mute, not a
// deregistration: refusing this during an incident would leave the commands registered anyway,
// since they persist on Discord's side, while making the next clean startup fail to correct
// them. The pause stops the bot SAYING things, and Respond above is where that bites.
//
// It is routed through the guard regardless, for the reason Delete is: one place knows about
// every Discord call and logs it, and a command registered from somewhere else is a command
// nobody can find the source of.
//
// A bulk overwrite rather than one create per command, because it is idempotent and it DELETES
// what is no longer listed. Creating individually leaves a renamed or dropped command visible in
// every client forever, which an operator can only fix by hand.
func (g *Guard) RegisterCommands(appID string, commands []*discordgo.ApplicationCommand) bool {
	if appID == "" {
		// Empty means the bot's identity was not resolved, and registering against an empty
		// application ID is a REST call that cannot succeed. Named rather than attempted,
		// because the failure otherwise reads as a Discord problem.
		g.log.Error("refusing to register application commands with no application ID")
		return false
	}

	// Global rather than per guild. Guild commands appear instantly and global ones can take
	// up to an hour to propagate, but guild registration means enumerating guilds and
	// re-registering on every join, which is a second thing to keep in step for a bot whose
	// commands are the same everywhere.
	registered, err := g.session.ApplicationCommandBulkOverwrite(appID, "", commands)
	if err != nil {
		g.log.Error("discord command registration failed", "err", err)
		return false
	}
	g.log.Info("application commands registered", "count", len(registered))
	return true
}

// permit runs the two checks every text-bearing call shares.
func (g *Guard) permit(channelID, content, op string) bool {
	if g.ignored(channelID, op) {
		return false
	}
	if content == "" {
		// Discord refuses an empty message anyway, and reaching here with one means a
		// caller decided to stay silent and then called us regardless. Refusing keeps
		// that from becoming an API error in the log that looks like a real failure.
		return false
	}
	if !g.gate.CheckEmit(content) {
		// The gate has already logged the reason, the category and the rule, and it
		// deliberately does not log the content. Nothing to add here.
		return false
	}
	return true
}

func (g *Guard) ignored(channelID, op string) bool {
	if _, ok := g.ignoreChannels[channelID]; ok {
		g.log.Debug("channel is on the ignore list", "channel", channelID, "op", op)
		return true
	}
	return false
}

// Ignored reports whether a channel is on the ignore list, for callers that want to
// skip work rather than do it and have the result refused.
//
// Generating a reply costs a corpus walk, so the reply path checks this before
// generating. That is an optimization and not the enforcement: the enforcement is in
// permit, where a caller cannot forget it.
func (g *Guard) Ignored(channelID string) bool {
	_, ok := g.ignoreChannels[channelID]
	return ok
}

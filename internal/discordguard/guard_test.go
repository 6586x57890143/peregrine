package discordguard

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// fakeSession records what would have gone to Discord.
type fakeSession struct {
	sends       []*discordgo.MessageSend
	sendChans   []string
	edits       []*discordgo.MessageEdit
	deletes     [][2]string
	reactions   [][3]string
	unreactions [][4]string
	presences   []discordgo.UpdateStatusData

	sendErr     error
	editErr     error
	deleteErr   error
	reactErr    error
	unreactErr  error
	presenceErr error

	responses       []*discordgo.InteractionResponse
	respondErr      error
	registered      []*discordgo.ApplicationCommand
	registeredApp   string
	registeredGuild string
	registerErr     error
}

func (f *fakeSession) UpdateStatusComplex(usd discordgo.UpdateStatusData) error {
	f.presences = append(f.presences, usd)
	return f.presenceErr
}

func (f *fakeSession) InteractionRespond(_ *discordgo.Interaction, resp *discordgo.InteractionResponse, _ ...discordgo.RequestOption) error {
	f.responses = append(f.responses, resp)
	return f.respondErr
}

func (f *fakeSession) ApplicationCommandBulkOverwrite(appID, guildID string, commands []*discordgo.ApplicationCommand, _ ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	f.registeredApp = appID
	f.registeredGuild = guildID
	f.registered = commands
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	return commands, nil
}

func (f *fakeSession) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.sends = append(f.sends, data)
	f.sendChans = append(f.sendChans, channelID)
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return &discordgo.Message{ID: "sent", ChannelID: channelID}, nil
}

func (f *fakeSession) ChannelMessageEditComplex(m *discordgo.MessageEdit, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.edits = append(f.edits, m)
	if f.editErr != nil {
		return nil, f.editErr
	}
	return &discordgo.Message{ID: m.ID}, nil
}

func (f *fakeSession) ChannelMessageDelete(channelID, messageID string, _ ...discordgo.RequestOption) error {
	f.deletes = append(f.deletes, [2]string{channelID, messageID})
	return f.deleteErr
}

func (f *fakeSession) MessageReactionAdd(channelID, messageID, emoji string, _ ...discordgo.RequestOption) error {
	f.reactions = append(f.reactions, [3]string{channelID, messageID, emoji})
	return f.reactErr
}

func (f *fakeSession) MessageReactionRemove(channelID, messageID, emoji, userID string, _ ...discordgo.RequestOption) error {
	f.unreactions = append(f.unreactions, [4]string{channelID, messageID, emoji, userID})
	return f.unreactErr
}

// allowAll and blockAll are the two gate behaviours worth testing against.
type allowAll struct{}

func (allowAll) CheckEmit(string) bool { return true }

type blockAll struct{ calls int }

func (b *blockAll) CheckEmit(string) bool { b.calls++; return false }

// blockMatching refuses anything containing a substring, so a test can distinguish
// "the gate was consulted" from "the gate refused everything".
type blockMatching struct{ bad string }

func (b blockMatching) CheckEmit(text string) bool { return !strings.Contains(text, b.bad) }

func newGuard(s Session, gate EmitGate, ignore ...string) *Guard {
	return New(s, gate, nil, ignore)
}

// TestSuppressionSurvivesMarshalling is THE test in this package, and it asserts on the
// JSON rather than on the struct on purpose.
//
// The bug it pins is entirely a marshalling behaviour. discordgo's AllowedMentions field
// carries `omitempty`, so a nil pointer disappears from the request and Discord reads the
// absent field as "parse every mention in the content". Asserting only that the struct
// field is non-nil would pass with a value that still went out wrong.
//
// Both failure modes were confirmed by writing them and watching this test fail:
//
//	nil                              -> allowed_mentions absent entirely
//	&MessageAllowedMentions{}        -> "parse":null
//	explicit empty slices            -> "parse":[]
//
// The middle one is the interesting case. discordgo's own comment on Parse says a
// zero-value struct allows no mentions, and that is true of the field being PRESENT but
// not of its value being the documented empty array. Whether "parse":null suppresses
// depends on Discord treating a present object with a null parse like an empty one,
// which is a fair reading and is not written down. So the guard sets the slices and this
// test reads the wire form, which means a future discordgo tag change fails here rather
// than in a channel.
func TestSuppressionSurvivesMarshalling(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, allowAll{})

	// Content that would ping three different ways if unsuppressed. This is not a
	// contrived string: every one of these can end up in the corpus, because the corpus
	// is made of what users type.
	const content = "hey <@123456789> and <@&987654321> and @everyone"
	if _, ok := g.Send("chan", content); !ok {
		t.Fatal("Send reported failure")
	}
	if len(f.sends) != 1 {
		t.Fatalf("got %d sends, want 1", len(f.sends))
	}

	raw, err := json.Marshal(f.sends[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, `"allowed_mentions"`) {
		t.Fatalf("allowed_mentions was dropped from the request entirely, which Discord "+
			"reads as \"parse every mention\". This is finding 8 exactly. Body: %s", body)
	}
	if !strings.Contains(body, `"parse":[]`) {
		t.Errorf("want \"parse\":[] , Discord's documented \"allow nothing\". A nil Parse "+
			"marshals as \"parse\":null, which is not the same thing. Body: %s", body)
	}
	if strings.Contains(body, `"parse":null`) {
		t.Errorf("parse marshalled as null. Body: %s", body)
	}
}

// TestReplyDoesNotPingTheAuthor is the half that matters most in practice.
//
// discordgo's ChannelMessageSendReply sets no AllowedMentions at all, so Discord's
// default applied and the author of the replied-to message was pinged on EVERY
// interaction. The bot answers whenever it hears its own name, so that is a notification
// per conversation, forever, for anyone who talks to it.
func TestReplyDoesNotPingTheAuthor(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, allowAll{})

	ref := &discordgo.MessageReference{MessageID: "m1", ChannelID: "chan"}
	if _, ok := g.SendReply("chan", "a reply", ref); !ok {
		t.Fatal("SendReply reported failure")
	}

	sent := f.sends[0]
	if sent.Reference != ref {
		t.Error("the reference was dropped, so the message would not be a reply at all")
	}
	if sent.AllowedMentions == nil {
		t.Fatal("AllowedMentions is nil on a reply")
	}
	if sent.AllowedMentions.RepliedUser {
		t.Error("RepliedUser is true, so replying pings the author. That is a notification " +
			"per conversation for every person who ever talks to the bot")
	}

	raw, _ := json.Marshal(sent)
	if !strings.Contains(string(raw), `"replied_user":false`) {
		t.Errorf("replied_user must be explicitly false on the wire, got %s", raw)
	}
}

// TestEditIsSuppressedToo. An edit can introduce a mention the original did not have, so
// an unsuppressed edit is a send with extra steps. This is the path the transcription
// worker takes, where the text is Whisper's transcript of arbitrary audio.
func TestEditIsSuppressedToo(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, allowAll{})

	if !g.Edit("chan", "m1", "now mentioning <@123>") {
		t.Fatal("Edit reported failure")
	}
	if len(f.edits) != 1 {
		t.Fatalf("got %d edits, want 1", len(f.edits))
	}
	if f.edits[0].AllowedMentions == nil {
		t.Fatal("AllowedMentions is nil on an edit")
	}
	raw, _ := json.Marshal(f.edits[0])
	if !strings.Contains(string(raw), `"parse":[]`) {
		t.Errorf("an edit must suppress mentions as well as a send, got %s", raw)
	}
}

// TestEveryTextBearingCallConsultsTheGate is the A3 pin at this layer. Until M10,
// CheckEmit sat at the single exit from generation, which covered the reply path and
// nothing else: the autonomous poster, the word-game announcements and the transcription
// results all reached Discord without passing it. Generation is not the only thing that
// produces text.
func TestEveryTextBearingCallConsultsTheGate(t *testing.T) {
	cases := map[string]func(*Guard) bool{
		"Send": func(g *Guard) bool {
			_, ok := g.Send("chan", "blocked content here")
			return ok
		},
		"SendReply": func(g *Guard) bool {
			_, ok := g.SendReply("chan", "blocked content here", &discordgo.MessageReference{MessageID: "m"})
			return ok
		},
		"Edit":  func(g *Guard) bool { return g.Edit("chan", "m", "blocked content here") },
		"React": func(g *Guard) bool { return g.React("chan", "m", "\U0001F426") },
	}

	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeSession{}
			gate := &blockAll{}
			g := newGuard(f, gate)

			if call(g) {
				t.Error("the call succeeded while the gate refused everything")
			}
			if gate.calls == 0 {
				t.Error("the gate was never consulted, so this call site is outside the " +
					"chokepoint and A3 is not closed")
			}
			if len(f.sends)+len(f.edits)+len(f.reactions) != 0 {
				t.Error("something reached the session despite the refusal")
			}
		})
	}
}

// TestARefusalIsSilentNotAFallback. Silence is always safe; a fallback string is a new
// output that has to be reasoned about, and in a bot that already replies selectively an
// unexplained silence is indistinguishable from it choosing not to answer.
func TestARefusalIsSilentNotAFallback(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, blockMatching{bad: "exampleslur"})

	if _, ok := g.Send("chan", "this contains exampleslur somewhere"); ok {
		t.Error("a blocked send reported success")
	}
	if len(f.sends) != 0 {
		t.Fatalf("a blocked send still reached Discord: %+v", f.sends)
	}

	// And the gate is consulted rather than everything being refused.
	if _, ok := g.Send("chan", "this is perfectly ordinary"); !ok {
		t.Error("an allowed send was refused, so the guard is not consulting the gate but " +
			"blocking unconditionally")
	}
}

// TestDeleteIsNotContentGated. Deleting says nothing and cannot ping, so gating it on
// content would mean the bot cannot clean up after itself during an incident, which is
// exactly when it most needs to.
func TestDeleteIsNotContentGated(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, &blockAll{})

	if !g.Delete("chan", "m1") {
		t.Error("Delete was refused by the content gate; a delete has no content")
	}
	if len(f.deletes) != 1 {
		t.Fatalf("got %d deletes, want 1", len(f.deletes))
	}
}

// TestReactHonoursThePauseSwitch. A reaction is still the bot visibly participating, so
// PEREGRINE_PAUSE_ALL_WRITES has to stop it, even though an operator-configured emoji is
// not untrusted text.
func TestReactHonoursThePauseSwitch(t *testing.T) {
	f := &fakeSession{}
	if g := newGuard(f, &blockAll{}); g.React("chan", "m1", "\U0001F426") {
		t.Error("a reaction went out while writes were paused")
	}
	if len(f.reactions) != 0 {
		t.Fatalf("a reaction reached Discord: %+v", f.reactions)
	}

	f2 := &fakeSession{}
	if g := newGuard(f2, allowAll{}); !g.React("chan", "m1", "\U0001F426") {
		t.Error("a reaction was refused with the gate allowing")
	}
}

// TestIgnoredChannelsAreNeverPostedIn, and the enforcement is at the guard rather than in
// the reply logic, because an operator setting this means "not in there" and not "not in
// reply to a message in there". The autonomous poster and the word games have to respect
// it too.
func TestIgnoredChannelsAreNeverPostedIn(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, allowAll{}, "quiet", "")

	if _, ok := g.Send("quiet", "hello"); ok {
		t.Error("sent to an ignored channel")
	}
	if !g.Ignored("quiet") {
		t.Error("Ignored disagrees with the enforcement")
	}
	if g.Edit("quiet", "m", "hello") {
		t.Error("edited in an ignored channel")
	}
	if g.Delete("quiet", "m") {
		t.Error("deleted in an ignored channel")
	}
	if g.React("quiet", "m", "\U0001F426") {
		t.Error("reacted in an ignored channel")
	}
	if len(f.sends)+len(f.edits)+len(f.deletes)+len(f.reactions) != 0 {
		t.Error("something reached Discord for an ignored channel")
	}

	// An empty entry must not become an ignored channel, or a trailing comma in the
	// variable would silence the channel whose ID is the empty string. Nothing has that
	// ID, but the map lookup would still match a caller passing "" by mistake, which is
	// how a misconfigured value becomes a silent bot.
	if g.Ignored("") {
		t.Error("the empty string is on the ignore list")
	}
	if _, ok := g.Send("other", "hello"); !ok {
		t.Error("a channel not on the list was refused")
	}
}

// TestSendFailuresAreLoggedNotDiscarded. Every one of these calls used to throw its error
// away, so a send Discord refused was indistinguishable from one that worked: the bot
// appeared to ignore people at random with nothing in the log to say why.
func TestSendFailuresAreReported(t *testing.T) {
	want := errors.New("missing permissions")

	f := &fakeSession{sendErr: want}
	g := newGuard(f, allowAll{})
	if _, ok := g.Send("chan", "hello"); ok {
		t.Error("a failed send reported success")
	}

	f2 := &fakeSession{editErr: want}
	if g := newGuard(f2, allowAll{}); g.Edit("chan", "m", "hello") {
		t.Error("a failed edit reported success")
	}

	f3 := &fakeSession{deleteErr: want}
	if g := newGuard(f3, allowAll{}); g.Delete("chan", "m") {
		t.Error("a failed delete reported success")
	}

	f4 := &fakeSession{reactErr: want}
	if g := newGuard(f4, allowAll{}); g.React("chan", "m", "x") {
		t.Error("a failed reaction reported success")
	}
}

// TestEmptyContentIsRefusedBeforeDiscordSeesIt. Discord rejects an empty message anyway,
// so this only changes where the failure appears: a refusal here rather than an API error
// in the log that looks like a real problem. Reaching this means a caller decided to stay
// silent and then called us regardless, which the generation path can do when the emit
// gate refuses upstream.
func TestEmptyContentIsRefusedBeforeDiscordSeesIt(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, allowAll{})

	if _, ok := g.Send("chan", ""); ok {
		t.Error("an empty send reported success")
	}
	if len(f.sends) != 0 {
		t.Error("an empty message reached Discord")
	}
}

// TestNilLoggerIsUsable, because a caller building a guard in a test should not have to
// construct a logger to avoid a nil dereference on the first failure path.
func TestNilLoggerIsUsable(t *testing.T) {
	f := &fakeSession{sendErr: errors.New("boom")}
	g := New(f, allowAll{}, nil, nil)
	if _, ok := g.Send("chan", "hello"); ok {
		t.Error("expected failure")
	}
}

// TestUnreactIsNotPauseGated, which is the one asymmetry with React worth pinning.
//
// Removing a reaction is the bot WITHDRAWING rather than participating. An operator who
// has hit the emergency stop wants that to succeed: refusing it would leave the bot's mark
// on somebody's message with no way to take it back until the pause is lifted.
func TestUnreactIsNotPauseGated(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, &blockAll{})

	if !g.Unreact("chan", "m1", "\U0001F426", "bot") {
		t.Error("Unreact was refused while writes were paused; taking a reaction back is " +
			"the opposite of participating and must still work")
	}
	if len(f.unreactions) != 1 {
		t.Fatalf("got %d reaction removals, want 1", len(f.unreactions))
	}
	if got := f.unreactions[0]; got != [4]string{"chan", "m1", "\U0001F426", "bot"} {
		t.Errorf("Unreact passed %v", got)
	}
}

func TestUnreactRespectsTheIgnoreListAndReportsFailure(t *testing.T) {
	f := &fakeSession{}
	if g := newGuard(f, allowAll{}, "quiet"); g.Unreact("quiet", "m", "x", "bot") {
		t.Error("removed a reaction in an ignored channel")
	}

	f2 := &fakeSession{unreactErr: errors.New("gone")}
	if g := newGuard(f2, allowAll{}); g.Unreact("chan", "m", "x", "bot") {
		t.Error("a failed removal reported success")
	}
}

// An embed is still the bot speaking, and mentions in it are suppressed exactly as they are
// in a plain message. Asserted on the MARSHALLED form for the same reason the plain-send test
// is: "parse":null and "parse":[] are different bytes, and Discord treating them alike is a
// reading of the API rather than something the documentation says.
func TestAnEmbedSendCarriesTheSameSuppressionOnTheWire(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, allowAll{})

	if _, ok := g.SendEmbed("c1", &discordgo.MessageEmbed{
		Title:       "Weekly Leaderboard",
		Description: "one line about <@123>",
	}); !ok {
		t.Fatal("SendEmbed reported failure")
	}
	if len(f.sends) != 1 {
		t.Fatalf("got %d sends, want 1", len(f.sends))
	}

	encoded, err := json.Marshal(f.sends[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(encoded)
	if !strings.Contains(body, `"allowed_mentions"`) {
		t.Fatalf("allowed_mentions was dropped from the embed request entirely, which Discord "+
			"reads as \"parse every mention\": %s", body)
	}
	// Only parse is checked, and that is not an oversight. Roles and Users carry omitempty
	// in discordgo, so an explicit empty slice marshals away to nothing either way; the
	// guard sets them anyway as insurance against a future tag change, but there is no wire
	// form to assert. Parse does not carry omitempty, which is exactly why it is the field
	// where null and [] are distinguishable and therefore the field that matters.
	if !strings.Contains(body, `"parse":[]`) {
		t.Errorf("want \"parse\":[] on an embed send, Discord's documented \"allow nothing\": %s", body)
	}
	if strings.Contains(body, `"parse":null`) {
		t.Errorf("parse marshalled as null on an embed send: %s", body)
	}
}

// The gate has to see EVERY text field, not the two anybody remembers. A blocklisted word in
// a field value is exactly as much of an incident as one in the description, and which part
// of the struct it landed in is an accident of how the caller built it.
func TestTheGateReadsEveryTextFieldOfAnEmbed(t *testing.T) {
	// One embed per field, each carrying the bad word in a different place.
	cases := map[string]*discordgo.MessageEmbed{
		"title":       {Title: "NOPE"},
		"description": {Description: "NOPE"},
		"author":      {Title: "ok", Author: &discordgo.MessageEmbedAuthor{Name: "NOPE"}},
		"footer":      {Title: "ok", Footer: &discordgo.MessageEmbedFooter{Text: "NOPE"}},
		"field name":  {Title: "ok", Fields: []*discordgo.MessageEmbedField{{Name: "NOPE", Value: "v"}}},
		"field value": {Title: "ok", Fields: []*discordgo.MessageEmbedField{{Name: "n", Value: "NOPE"}}},
	}

	for where, embed := range cases {
		f := &fakeSession{}
		g := newGuard(f, blockMatching{bad: "NOPE"})

		if _, ok := g.SendEmbed("c1", embed); ok {
			t.Errorf("an embed with the blocked word in its %s was sent anyway", where)
		}
		if len(f.sends) != 0 {
			t.Errorf("a refused embed (%s) still reached the session", where)
		}
	}
}

// A nil embed is refused rather than dereferenced. Discord would reject an empty send anyway,
// and a caller reaching here with nothing has already decided to stay silent.
func TestANilEmbedIsRefused(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, allowAll{})

	if _, ok := g.SendEmbed("c1", nil); ok {
		t.Error("a nil embed was sent")
	}
	if len(f.sends) != 0 {
		t.Error("a nil embed reached the session")
	}
}

// The ignore list covers embeds too. An operator saying "not in there" means it about every
// kind of message, which is the whole reason the list lives in the guard rather than in the
// reply path.
func TestAnIgnoredChannelRefusesAnEmbed(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, allowAll{}, "c-quiet")

	if _, ok := g.SendEmbed("c-quiet", &discordgo.MessageEmbed{Title: "hello"}); ok {
		t.Error("an embed was sent into an ignored channel")
	}
	if len(f.sends) != 0 {
		t.Error("an ignored channel still reached the session")
	}
}

// The presence line is content-gated, because a caller may put a word from the corpus in it.
func TestPresenceIsContentGated(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, blockMatching{bad: "NOPE"})

	if g.Presence("currently thinking about: NOPE") {
		t.Error("a presence line carrying blocked text was set")
	}
	if len(f.presences) != 0 {
		t.Error("a refused presence still reached the session")
	}

	if !g.Presence("watching 1,204,553 n-grams") {
		t.Fatal("an ordinary presence line was refused")
	}
	if len(f.presences) != 1 {
		t.Fatalf("got %d presence updates, want 1", len(f.presences))
	}
	if len(f.presences[0].Activities) != 1 ||
		f.presences[0].Activities[0].Name != "watching 1,204,553 n-grams" {
		t.Errorf("presence data is wrong: %+v", f.presences[0])
	}
}

// NOT pause-gated, and that asymmetry with React is the point. Presence carries no channel
// content and cannot ping, so freezing it during an incident buys nothing and costs the
// operator their only sign the process is alive: a bot with a stale status line and no
// messages looks exactly like a bot that has died. Same reasoning as Unreact.
//
// A gate that refuses everything is what a paused Gate looks like from here, so the empty
// string is the case that distinguishes "paused" from "the content was bad".
func TestAnEmptyPresenceSkipsTheGateEntirely(t *testing.T) {
	blocked := &blockAll{}
	f := &fakeSession{}
	g := newGuard(f, blocked)

	if !g.Presence("") {
		t.Error("an empty presence line was refused; there is no content to refuse")
	}
	if blocked.calls != 0 {
		t.Errorf("the gate was consulted %d time(s) for a presence line with no text", blocked.calls)
	}
	if len(f.presences) != 1 {
		t.Errorf("got %d presence updates, want 1", len(f.presences))
	}
}

// A failed presence update is reported as a failure rather than swallowed, so a caller that
// wants to fall back to something simpler can.
func TestAFailedPresenceUpdateReportsFailure(t *testing.T) {
	f := &fakeSession{presenceErr: errors.New("gateway is reconnecting")}
	g := newGuard(f, allowAll{})

	if g.Presence("watching 12 n-grams") {
		t.Error("Presence reported success after the session returned an error")
	}
}

// ---------------------------------------------------------------- interactions, M26

// TestAnEphemeralResponseIsStillGated.
//
// The whole reason InteractionRespond was added to the forbidden list. An interaction response
// puts text in front of somebody and can carry a mention, so it is a send; "only the person who
// asked can see it" narrows who is harmed, not whether the bot said it.
//
// A slash command is also the surface an operator reaches for during an incident, which is
// exactly when PAUSE_ALL_WRITES is on, so a response that ignored the pause would be a hole
// opening at the worst moment.
func TestAnEphemeralResponseIsStillGated(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, &blockAll{})

	i := &discordgo.Interaction{ChannelID: "c1"}
	if g.Respond(i, "anything at all", true) {
		t.Error("an ephemeral response was sent past a refusing gate")
	}
	if len(f.responses) != 0 {
		t.Errorf("the refused response reached Discord anyway: %v", f.responses)
	}
}

// TestAResponseSuppressesMentions, set explicitly rather than trusted to be impossible.
//
// Whether an ephemeral message can ping is the same kind of question as whether an embed can,
// and allowedMentions' own comment refuses to lean on that sort of reading: setting the field
// costs one struct and depends on nothing.
func TestAResponseSuppressesMentions(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, allowAll{})

	if !g.Respond(&discordgo.Interaction{ChannelID: "c1"}, "hello", true) {
		t.Fatal("Respond refused an ordinary message")
	}
	if len(f.responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(f.responses))
	}

	data := f.responses[0].Data
	if data.AllowedMentions == nil {
		t.Fatal("the response carries no AllowedMentions, so Discord parses every mention in it")
	}
	am := data.AllowedMentions
	if am.Parse == nil || len(am.Parse) != 0 {
		t.Errorf("Parse = %v, want an explicitly empty slice", am.Parse)
	}
	if am.Roles == nil || am.Users == nil {
		t.Error("Roles or Users is nil, which marshals as null rather than as an empty array")
	}
	if am.RepliedUser {
		t.Error("RepliedUser is true")
	}
	if data.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Error("the ephemeral flag was not set, so a private answer went to the whole channel")
	}
}

// TestAPublicResponseIsNotEphemeral, because the caller chooses the audience: a refusal belongs
// to the person refused and a puzzle belongs to the channel.
func TestAPublicResponseIsNotEphemeral(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, allowAll{})

	if !g.Respond(&discordgo.Interaction{ChannelID: "c1"}, "hello", false) {
		t.Fatal("Respond refused an ordinary message")
	}
	if f.responses[0].Data.Flags&discordgo.MessageFlagsEphemeral != 0 {
		t.Error("a response asked to be public was sent ephemeral")
	}
}

// TestAResponseInAnIgnoredChannelIsRefused. An operator who said "not in there" meant it about
// the whole bot, and a slash command is a way into a channel like any other.
func TestAResponseInAnIgnoredChannelIsRefused(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, allowAll{}, "c1")

	if g.Respond(&discordgo.Interaction{ChannelID: "c1"}, "hello", true) {
		t.Error("a response was sent into an ignored channel")
	}
	if g.Respond(&discordgo.Interaction{ChannelID: "c1"}, "hello", false) {
		t.Error("a public response was sent into an ignored channel")
	}
	if len(f.responses) != 0 {
		t.Errorf("responses reached Discord from an ignored channel: %v", f.responses)
	}
}

// TestARespondEmbedIsGatedOverEveryField, matching SendEmbed. A blocklisted word in a field
// value is exactly as much of an incident as one in the description, and which of them it landed
// in is an accident of how the caller built the embed.
func TestARespondEmbedIsGatedOverEveryField(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, blockMatching{bad: "badword"})

	embed := &discordgo.MessageEmbed{
		Title:  "fine",
		Fields: []*discordgo.MessageEmbedField{{Name: "fine", Value: "badword"}},
	}
	if g.RespondEmbed(&discordgo.Interaction{ChannelID: "c1"}, embed, true) {
		t.Error("an embed with a blocked word in a FIELD was responded with")
	}
	if len(f.responses) != 0 {
		t.Errorf("it reached Discord anyway: %v", f.responses)
	}
}

// TestANilInteractionIsRefusedRatherThanPanicking. The handler builds these from a gateway
// payload, and a malformed one must not take the process down: a panic here is one feature's bad
// day becoming everybody's.
func TestANilInteractionIsRefusedRatherThanPanicking(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, allowAll{})

	if g.Respond(nil, "hello", true) {
		t.Error("a nil interaction was answered")
	}
	if g.RespondEmbed(nil, &discordgo.MessageEmbed{Title: "x"}, true) {
		t.Error("a nil interaction was answered with an embed")
	}
	if g.RespondEmbed(&discordgo.Interaction{ChannelID: "c1"}, nil, true) {
		t.Error("a nil embed was sent")
	}
}

// TestRegisteringCommandsIsABulkOverwrite.
//
// Idempotent, and it DELETES what is no longer listed. Creating commands one at a time leaves a
// renamed or dropped command visible in every client forever, which an operator can only fix by
// hand and will not think to.
func TestRegisteringCommandsIsABulkOverwrite(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, allowAll{})

	cmds := []*discordgo.ApplicationCommand{{Name: "wordgame", Description: "start a puzzle"}}
	if !g.RegisterCommands("app1", cmds) {
		t.Fatal("registration was refused")
	}
	if f.registeredApp != "app1" {
		t.Errorf("registered against app %q, want app1", f.registeredApp)
	}
	if f.registeredGuild != "" {
		t.Errorf("registered against guild %q, want global registration: per-guild means "+
			"enumerating guilds and re-registering on every join", f.registeredGuild)
	}
	if len(f.registered) != 1 {
		t.Errorf("registered %d commands, want 1", len(f.registered))
	}
}

// TestRegisteringWithNoApplicationIDIsNamedRatherThanAttempted. An empty ID means the bot's
// identity was not resolved, and a REST call that cannot succeed produces a Discord error that
// reads like Discord's fault.
func TestRegisteringWithNoApplicationIDIsNamedRatherThanAttempted(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, allowAll{})

	if g.RegisterCommands("", []*discordgo.ApplicationCommand{{Name: "wordgame"}}) {
		t.Error("registration was attempted with no application ID")
	}
	if f.registered != nil {
		t.Error("it called Discord anyway")
	}
}

// TestRegisteringIsNotPauseGated, which is deliberate and worth stating because everything else
// that reaches Discord here is.
//
// A command definition is the bot's own text from this repository, never assembled from anything
// a user typed, so there is nothing for the content gate to judge. And the pause is a MUTE, not a
// deregistration: commands persist on Discord's side, so refusing this during an incident would
// leave them registered anyway while making the next clean startup fail to correct them. What
// the pause has to stop is the bot SAYING things, which is Respond.
func TestRegisteringIsNotPauseGated(t *testing.T) {
	f := &fakeSession{}
	g := newGuard(f, &blockAll{})

	if !g.RegisterCommands("app1", []*discordgo.ApplicationCommand{{Name: "wordgame"}}) {
		t.Error("registration was refused by the emit gate; a command definition is not " +
			"user text, and a paused bot still needs its commands to be correct")
	}
}

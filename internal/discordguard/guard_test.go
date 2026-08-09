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

	sendErr    error
	editErr    error
	deleteErr  error
	reactErr   error
	unreactErr error
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

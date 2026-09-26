package games

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/names"
	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/wheel"
)

// The wheel, M35: the Discord half of internal/wheel.
//
// It lives in games rather than in a service of its own for one reason that decides it: the
// weekly board is games' in-memory Leaderboard, and saveBoard writes that copy over the blob
// on every win and at shutdown. Anything else writing gold into the same blob would be
// overwritten by the next save, so the only safe writer is the owner. It also keeps one
// interaction handler, where a second would see every interaction and need the same prefix
// discipline twice.
//
// # How the card stays current
//
// A press is answered with the repaint itself: Act, then UpdateEmbed on the message the
// button is on, which is one REST call inside Discord's three seconds. Everything that
// happens without a press, a turn timing out or the lobby closing, is the sweep's: Tick, then
// Stale, then EditEmbed. The engine's Version and Painted are what connect the two, so a
// press whose update was refused is simply stale and the next sweep paints it.
//
// # Where gold is paid
//
// persistResult, from every path that can receive a Result (a press, a modal, the sweep).
// The engine returns a Result exactly once, from the locked section that deletes the match,
// so there is no second payout for this code to guard against.

// Button and modal ids: wof:<action>:<turn>. The turn token is the engine's, so a press
// built against a turn that has moved on is refused rather than applied to the next one. A
// modal reuses its button's id; the interaction type says which is which.
const (
	wheelPrefix = "wof"

	actJoin      = "join"
	actLeave     = "leave"
	actStart     = "start"
	actSpin      = "spin"
	actConsonant = "cons"
	actVowel     = "vowel"
	actSolve     = "solve"
	actPick      = "pick"
	actNext      = "next"

	// wheelInputID is the one text input every wheel modal has.
	wheelInputID = "t"
)

var wheelActions = map[string]wheel.ActionKind{
	actJoin: wheel.Join, actLeave: wheel.Leave, actStart: wheel.Start, actSpin: wheel.Spin,
	actConsonant: wheel.Consonant, actVowel: wheel.Vowel, actSolve: wheel.Solve, actPick: wheel.Pick,
	actNext: wheel.Next,
}

const (
	wheelCommandName  = "wheel" // a subcommand of /game as of M37
	walletCommandName = "wallet"

	// walletKey is the lifetime wallet: a JSON map of user ID to gold, beside the weekly
	// board in the same bucket. It is never cached; each payout reads and writes it inside
	// the one transaction that also saves the board.
	walletKey = "wallet"

	// wheelSweepTick is a constant rather than configuration. Turns are tens of seconds long,
	// the word-game sweep's five would make a timeout visibly late, and with no match live a
	// tick is one map lookup.
	wheelSweepTick = time.Second

	// defaultRepostAfter is how many messages of conversation since the card went up move it
	// to the bottom of the channel. Below that the card is still on screen and an edit is
	// what people see; above it, an edit to a card a screen up is invisible, which is the
	// reasoning that turned word-game hints into reposts (M25).
	defaultRepostAfter = 6

	// repostCooldown is the least time between two posts of one card, whatever the channel is
	// doing. A flooded channel would otherwise have the card deleted and reposted every
	// second, which is noise and a rate-limit problem rather than visibility.
	repostCooldown = 20 * time.Second

	// maxPaintFailures is how many consecutive failed repaints abandon a match. More than
	// one, because a transient failure must not throw away a match with gold on it; not many,
	// because a deleted card fails forever and would be retried every second until the time
	// limit.
	maxPaintFailures = 3
)

func wheelID(action string, turn int) string {
	return wheelPrefix + ":" + action + ":" + strconv.Itoa(turn)
}

func parseWheelID(id string) (action string, turn int, ok bool) {
	parts := strings.Split(id, ":")
	if len(parts) != 3 || parts[0] != wheelPrefix {
		return "", 0, false
	}
	if _, known := wheelActions[parts[1]]; !known {
		return "", 0, false
	}
	turn, err := strconv.Atoi(parts[2])
	if err != nil {
		return "", 0, false
	}
	return parts[1], turn, true
}

// wheelOn reports whether the wheel can be played at all. A nil or empty manager is how a
// failed puzzle load turns the feature off rather than the bot.
func (s *Service) wheelOn() bool { return s.opts.Wheel && s.wheels.Available() }

// wheelRefusals is every engine error with what the person who pressed is told. One table,
// so a new error without a sentence here is caught by a test rather than answered with
// "something went wrong".
var wheelRefusals = []struct {
	err error
	msg string
}{
	{wheel.ErrNoMatch, "that game is over"},
	{wheel.ErrWrongPhase, "not possible right now"},
	{wheel.ErrLobbyFull, "the lobby is full"},
	{wheel.ErrAlreadyJoined, "you're already in"},
	{wheel.ErrNotJoined, "you're not in this game"},
	{wheel.ErrNotHost, "only the host can start early. it starts on its own when sign-ups close"},
	{wheel.ErrTooFewPlayers, "not enough players yet"},
	{wheel.ErrNotYourTurn, "not your turn"},
	{wheel.ErrStaleTurn, "too slow, that turn is over"},
	{wheel.ErrSpinFirst, "spin first"},
	{wheel.ErrMustCallConsonant, "call a consonant for your spin first"},
	{wheel.ErrBadLetter, "that's not a letter of the right kind"},
	{wheel.ErrNoConsonantsLeft, "no consonants left: buy a vowel or solve"},
	{wheel.ErrNoVowelsLeft, "no vowels left"},
	{wheel.ErrCannotAffordVowel, fmt.Sprintf("a vowel costs %d from this round's gold", wheel.VowelCost)},
	{wheel.ErrEmptySolve, "that answer was empty"},
	{wheel.ErrBadBonusPick, "pick three consonants and one vowel, none of R S T L N E"},
	{wheel.ErrMatchInProgress, "a game is already running here"},
	{wheel.ErrTooManyMatches, "too many wheels are spinning right now, try again soon"},
	{wheel.ErrNoPuzzles, "the wheel is off right now"},
}

func wheelRefusal(err error) (string, bool) {
	for _, r := range wheelRefusals {
		if errors.Is(err, r.err) {
			return r.msg, true
		}
	}
	return "something went wrong", false
}

func (s *Service) refuseWheel(i *discordgo.Interaction, err error) {
	msg, known := wheelRefusal(err)
	if !known {
		log.Printf("[WHEEL] unexpected error in %s: %v", i.ChannelID, err)
	}
	s.guard.Respond(i, msg, true)
}

// interactionPlayer is who pressed, and the name the card shows them under. Stored at join
// time by the engine, so rendering never makes a REST call for a name.
func interactionPlayer(i *discordgo.Interaction) (userID, name string) {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID, names.Primary(i.Member.User, i.Member).Name
	}
	if i.User != nil {
		return i.User.ID, names.Primary(i.User, nil).Name
	}
	return "", ""
}

// modalText reads a modal submit's custom_id and its one text input, or "" when the payload is
// not one. Comma-ok all the way down, for componentID's reason: the interaction type and its
// data are two fields off the wire, and a type assertion that trusts them panics the goroutine
// reading a malformed payload. Both pointer and value components are accepted, because
// discordgo decodes pointers and a hand-built payload may not.
func modalText(i *discordgo.Interaction) (customID, text string) {
	if i == nil {
		return "", ""
	}
	data, ok := i.Data.(discordgo.ModalSubmitInteractionData)
	if !ok {
		return "", ""
	}
	for _, c := range data.Components {
		var row []discordgo.MessageComponent
		switch r := c.(type) {
		case *discordgo.ActionsRow:
			row = r.Components
		case discordgo.ActionsRow:
			row = r.Components
		}
		for _, cc := range row {
			switch in := cc.(type) {
			case *discordgo.TextInput:
				if in.CustomID == wheelInputID {
					return data.CustomID, in.Value
				}
			case discordgo.TextInput:
				if in.CustomID == wheelInputID {
					return data.CustomID, in.Value
				}
			}
		}
	}
	return data.CustomID, ""
}

// handleWheel is /wheel: open a lobby in this channel.
//
// The card is a normal channel post and the acknowledgement is private, /wordgame's split: the
// thing everybody plays goes to the channel, and "it's up" goes to the person who asked.
// Anyone may open one, within the channels games may run in. A lobby is opt-in, one per
// channel, and closes on its own, which is not the shape of anything that needs an admin.
func (s *Service) handleWheel(i *discordgo.Interaction) {
	if !s.wheelOn() {
		s.guard.Respond(i, "the wheel is off right now", true)
		return
	}
	if i.GuildID == "" {
		s.guard.Respond(i, "the wheel needs a server channel", true)
		return
	}
	if !s.wheelAllowed(i.GuildID, i.ChannelID) {
		s.guard.Respond(i, "the wheel is restricted to another channel", true)
		return
	}

	userID, name := interactionPlayer(i)
	u, err := s.wheels.Open(i.GuildID, i.ChannelID, userID, name)
	if err != nil {
		msg, known := wheelRefusal(err)
		if !known {
			log.Printf("[WHEEL] /%s %s failed to open a lobby: %v", commandName, wheelCommandName, err)
		}
		// A jump link rather than a bare refusal, because the useful answer to "a game is
		// already running" is where.
		if v, ok := s.wheels.Snapshot(i.ChannelID); ok && errors.Is(err, wheel.ErrMatchInProgress) && v.MessageID != "" {
			msg += ": https://discord.com/channels/" + i.GuildID + "/" + i.ChannelID + "/" + v.MessageID
		}
		s.guard.Respond(i, msg, true)
		return
	}

	if !s.postWheel(u.View) {
		s.wheels.Abandon(i.ChannelID)
		s.guard.Respond(i, "could not post there: the channel is on the ignore list, or writes are paused", true)
		return
	}
	s.guard.Respond(i, "lobby's up", true)
	log.Printf("[WHEEL] Opened a lobby in channel %s.", i.ChannelID)
}

// postWheel sends a card for a view and records it as the match's message.
func (s *Service) postWheel(v wheel.View) bool {
	embed, comps := wheelCard(v, s.opts.WheelAssetURL)
	msg, ok := s.guard.SendEmbed(v.ChannelID, embed, comps...)
	if !ok || msg == nil {
		return false
	}
	s.wheels.Posted(v.ChannelID, msg.ID, v.Version)
	s.mu.Lock()
	s.wheelPostedAt[v.ChannelID] = time.Now()
	s.mu.Unlock()
	return true
}

// handleWheelButton is a press on a card.
//
// The four actions that need typing open a form, and the two checks every one of them would
// fail on, whose turn and which turn, are made BEFORE the form opens, so nobody types an
// answer that could never be applied. Everything else about the move is the engine's to judge
// on submit.
func (s *Service) handleWheelButton(i *discordgo.Interaction) {
	action, turn, ok := parseWheelID(componentID(i))
	if !ok {
		return
	}
	if i.GuildID == "" {
		s.guard.Respond(i, "the wheel needs a server channel", true)
		return
	}

	v, live := s.wheels.Snapshot(i.ChannelID)
	if !live {
		// A card this process no longer holds: the match ended, or the bot restarted with it
		// live. Replacing it heals the dead buttons on first touch, so a restart needs no
		// shutdown edit.
		embed, comps := endedCard()
		s.guard.UpdateEmbed(i, embed, comps...)
		return
	}

	userID, name := interactionPlayer(i)
	kind := wheelActions[action]
	if title, label, maxLen, typed := wheelModal(kind); typed {
		switch {
		case v.Current != userID:
			s.refuseWheel(i, wheel.ErrNotYourTurn)
		case turn != v.Turn:
			s.refuseWheel(i, wheel.ErrStaleTurn)
		default:
			s.guard.RespondModal(i, wheelID(action, turn), title, discordgo.TextInput{
				CustomID:  wheelInputID,
				Label:     label,
				Style:     discordgo.TextInputShort,
				Required:  true,
				MinLength: 1,
				MaxLength: maxLen,
			})
		}
		return
	}
	s.applyWheel(i, wheel.Action{Kind: kind, UserID: userID, Name: name, Turn: turn}, v)
}

// wheelModal is the form for an action that needs typing. The lengths are a courtesy so the
// client refuses nonsense early; the engine is what validates.
func wheelModal(kind wheel.ActionKind) (title, label string, maxLen int, typed bool) {
	switch kind {
	case wheel.Consonant:
		return "call a consonant", "consonant", 1, true
	case wheel.Vowel:
		return fmt.Sprintf("buy a vowel (%d)", wheel.VowelCost), "vowel", 1, true
	case wheel.Solve:
		return "solve the puzzle", "answer", 60, true
	case wheel.Pick:
		return "pick your letters", "3 consonants and a vowel, not RSTLNE", 12, true
	}
	return "", "", 0, false
}

// handleWheelModal is a submitted form. A form can only have been opened from a card, so its
// answer repaints that card like a press does.
func (s *Service) handleWheelModal(i *discordgo.Interaction) {
	id, text := modalText(i)
	action, turn, ok := parseWheelID(id)
	if !ok {
		return
	}
	kind := wheelActions[action]
	if _, _, _, typed := wheelModal(kind); !typed {
		return
	}
	userID, name := interactionPlayer(i)
	v, live := s.wheels.Snapshot(i.ChannelID)
	if !live {
		s.refuseWheel(i, wheel.ErrNoMatch)
		return
	}
	s.applyWheel(i, wheel.Action{Kind: kind, UserID: userID, Name: name, Turn: turn, Text: text}, v)
}

// applyWheel acts and answers the press with the repaint.
//
// A press can land on a card that has since been reposted: the old message is deleted, or about
// to be, while somebody's thumb was on it. The action still counts, because the turn token
// already says whether it may; what cannot happen is repainting a message that is gone. So that
// press is acknowledged without a change and the match is left stale, and the sweep repaints
// the live card within a second.
func (s *Service) applyWheel(i *discordgo.Interaction, a wheel.Action, before wheel.View) {
	u, err := s.wheels.Act(i.ChannelID, a)
	if err != nil {
		s.refuseWheel(i, err)
		return
	}
	embed, comps := wheelCard(u.View, s.opts.WheelAssetURL)
	switch {
	case i.Message != nil && before.MessageID != "" && i.Message.ID != before.MessageID:
		s.guard.Acknowledge(i)
		if u.Result != nil {
			// The match is gone from the manager, so the sweep cannot see it to repaint.
			s.guard.EditEmbed(i.ChannelID, before.MessageID, embed, comps...)
		}
	case s.guard.UpdateEmbed(i, embed, comps...) && u.Result == nil:
		// Painted only on success. A refused update leaves the match stale, and the sweep
		// repaints it within a second, which is the whole recovery path.
		s.wheels.Painted(i.ChannelID, u.View.Version)
	}
	s.afterWheel(u)
}

// afterWheel is what an update means beyond its repaint: a payout.
func (s *Service) afterWheel(u wheel.Update) {
	if u.Result != nil {
		s.finishWheel(u)
	}
}

// repostChatty moves every card the channel has talked past back to the bottom.
//
// Any time rather than only at a round boundary, because a card a screen up is a game nobody
// is playing. A press already in flight on the old card still counts (see applyWheel). The new
// card goes up BEFORE the old one comes down, the M25 order, so a refused send leaves the old
// card in place rather than none.
func (s *Service) repostChatty() {
	if s.counter == nil {
		return
	}
	after := s.opts.WheelRepostAfter
	if after <= 0 {
		after = defaultRepostAfter
	}
	s.mu.Lock()
	posted := make(map[string]time.Time, len(s.wheelPostedAt))
	for ch, at := range s.wheelPostedAt {
		posted[ch] = at
	}
	s.mu.Unlock()

	for ch, at := range posted {
		since := time.Since(at)
		if since < repostCooldown || s.counter.Count(ch, since) < after {
			continue
		}
		v, ok := s.wheels.Snapshot(ch)
		if !ok || v.MessageID == "" {
			continue
		}
		if s.postWheel(v) {
			s.guard.Delete(ch, v.MessageID)
		}
	}
}

// finishWheel pays out, crowns the winner and forgets the card.
func (s *Service) finishWheel(u wheel.Update) {
	ch := u.View.ChannelID
	s.mu.Lock()
	delete(s.wheelPostedAt, ch)
	delete(s.wheelPaintFails, ch)
	s.mu.Unlock()

	s.persistResult(u.View.GuildID, u.Result)
	log.Printf("[WHEEL] Match in channel %s ended: winner %q, %d paid.", ch, u.Result.Winner, len(u.Result.Awards))

	// A winner is announced as a line of its own as well as on the card, because the card may
	// be a screen up by now and "who won" is the one thing everybody in the channel wants.
	for _, a := range u.Result.Awards {
		if a.UserID == u.Result.Winner {
			s.guard.Send(ch, fmt.Sprintf("🎡 **%s** wins the wheel with %s gold", a.Name, commas(a.Gold)))
			break
		}
	}
}

// persistResult pays a finished match: gold onto the weekly board and into the lifetime
// wallet, in ONE transaction, so the two cannot disagree about a payout.
//
// The wallet is read and written inside the same Update that saves the board, because Writer
// embeds Reader. It is never held in memory, which is why it cannot be overwritten by a stale
// copy the way the board would be if anything but this service wrote it.
func (s *Service) persistResult(guildID string, r *wheel.Result) {
	if r == nil || len(r.Awards) == 0 {
		return
	}
	board := s.board(guildID)
	store, err := s.corpora.For(guildID)
	if board == nil || err != nil {
		log.Printf("[WHEEL] Could not pay out a match in guild %s: %v", guildID, err)
		return
	}
	for _, a := range r.Awards {
		board.AddGold(a.UserID, a.Name, a.Gold)
	}
	encoded, err := json.Marshal(board)
	if err != nil {
		log.Printf("[WHEEL] Could not encode the board for guild %s: %v", guildID, err)
		return
	}
	// ponytail: the whole wallet map is rewritten per match; per-user keys if a guild ever
	// reaches ten thousand earners.
	err = store.Update(func(w *storage.Writer) error {
		if err := w.PutBlob(storage.BlobLeaderboard, "current", encoded); err != nil {
			return err
		}
		wallet, err := readWallet(&w.Reader)
		if err != nil {
			return err
		}
		for _, a := range r.Awards {
			wallet[a.UserID] += a.Gold
		}
		out, err := json.Marshal(wallet)
		if err != nil {
			return err
		}
		return w.PutBlob(storage.BlobLeaderboard, walletKey, out)
	})
	if err != nil {
		// The in-memory board still has the gold and saves at shutdown; the wallet does not,
		// which is why this is an Error.
		log.Printf("[ERROR] [WHEEL] Could not save a payout in guild %s: %v", guildID, err)
	}
}

func readWallet(r *storage.Reader) (map[string]int, error) {
	wallet := map[string]int{}
	v, err := r.GetBlob(storage.BlobLeaderboard, walletKey)
	if err != nil || v == nil {
		return wallet, err
	}
	if err := json.Unmarshal(v, &wallet); err != nil {
		return nil, fmt.Errorf("decode wallet: %w", err)
	}
	return wallet, nil
}

// handleWallet is /wallet: lifetime gold in this server, and this week's.
func (s *Service) handleWallet(i *discordgo.Interaction) {
	if i.GuildID == "" {
		s.guard.Respond(i, "wallets are per server, ask in one", true)
		return
	}
	userID, _ := interactionPlayer(i)
	store, err := s.corpora.For(i.GuildID)
	if err != nil {
		log.Printf("[WHEEL] /%s: %v", walletCommandName, err)
		s.guard.Respond(i, "could not open the wallet", true)
		return
	}
	var wallet map[string]int
	if err := store.View(func(r *storage.Reader) error {
		var err error
		wallet, err = readWallet(r)
		return err
	}); err != nil {
		log.Printf("[WHEEL] /%s: %v", walletCommandName, err)
		s.guard.Respond(i, "could not open the wallet", true)
		return
	}
	week := 0
	if b := s.board(i.GuildID); b != nil {
		week = b.Golds()[userID]
	}
	s.guard.Respond(i, fmt.Sprintf("💰 %s gold in this server%s%s this week",
		commas(wallet[userID]), sep, commas(week)), true)
}

// wheelSweep is everything that happens without a press.
//
// Tick first, so a timeout or a closing lobby is applied, then Stale, which repaints every
// card not showing its match's current version: the timeouts just applied, and any press
// whose own repaint was refused. Only a finished match is painted here directly, because it
// is gone from the manager and Stale can no longer see it.
func (s *Service) wheelSweep() {
	for _, u := range s.wheels.Tick() {
		if u.Result != nil {
			if u.View.MessageID != "" {
				embed, comps := wheelCard(u.View, s.opts.WheelAssetURL)
				s.guard.EditEmbed(u.View.ChannelID, u.View.MessageID, embed, comps...)
			}
			s.finishWheel(u)
			continue
		}
		s.afterWheel(u)
	}
	s.repostChatty()

	for _, v := range s.wheels.Stale() {
		embed, comps := wheelCard(v, s.opts.WheelAssetURL)
		if s.guard.EditEmbed(v.ChannelID, v.MessageID, embed, comps...) {
			s.wheels.Painted(v.ChannelID, v.Version)
			s.mu.Lock()
			delete(s.wheelPaintFails, v.ChannelID)
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		s.wheelPaintFails[v.ChannelID]++
		fails := s.wheelPaintFails[v.ChannelID]
		s.mu.Unlock()
		if fails >= maxPaintFailures {
			// The card is gone or unreachable, so nobody can play this match, and retrying
			// every second until the time limit would only fill the log.
			log.Printf("[WHEEL] Abandoned the match in channel %s after %d failed repaints.", v.ChannelID, fails)
			s.wheels.Abandon(v.ChannelID)
			s.mu.Lock()
			delete(s.wheelPostedAt, v.ChannelID)
			delete(s.wheelPaintFails, v.ChannelID)
			s.mu.Unlock()
		}
	}
}

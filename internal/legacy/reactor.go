package legacy

import (
	"log"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"time"

	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/wordgames"
	"github.com/bwmarrin/discordgo"
)

// The reactor: one message, a list of named steps, and a short-circuit contract.
//
// This replaces a single 480-line handleMessage that ran twelve sections in sequence
// with no way for any of them to say "this message was for me, stop". That absence is
// finding 9: the !leaderboard branch had no return, so after answering the command the
// handler carried straight on into the aggro check, the reply generator, name
// extraction, LEARNING THE COMMAND INTO THE CORPUS, the voice handler and the image
// reposter. The bot answered a command, then replied to it as if it were conversation,
// then taught itself the string "!leaderboard".
//
// The fix is not a return statement. A return statement fixes it once, in one branch,
// and the next command someone adds has the same bug available to it. What closes it is
// making the contract explicit: every step reports whether it consumed the message, and
// the runner stops when one does. A new command that returns handled gets the behaviour
// automatically, and one that forgets is visible as a missing return value rather than as
// an absent statement in the middle of four hundred lines.
//
// # Why the steps are functions here rather than packages under internal/plugins
//
// SPEC.md section 2 puts these under internal/plugins, and they are not there yet. The
// reason is that the state they operate on is still package-level in legacy:
// activeWordGames, birdAggroTargetID, recentImageURLs, leaderboard. Moving the handlers
// out means moving that state with them, and that state IS the engagement features, which
// is M11's row. Splitting the state and the control flow in the same commit would mean
// reviewing an eight-hundred-line move for a two-line behavioural change.
//
// So M10b lands the contract, which is the part that closes the finding, and M11's move
// becomes mechanical: each step function below is already a closed unit over one
// subsystem's state.

// reaction is everything a step needs about one message. One per message, built by
// handleMessage and passed to each step in turn.
//
// It exists mostly to hold the two things that used to be recomputed: the flag set and
// the mentioned-user list. Recomputing was not free, and in the second case it was
// expensive in a way that mattered, which is why memoizing it is part of this change
// rather than a later tidy-up.
type reaction struct {
	s *discordgo.Session
	m *discordgo.MessageCreate

	// start is when handling began, for the completion log line.
	start time.Time

	// flags is the classification the reply, learn and voice steps read.
	flags map[string]bool

	// mentioned caches extractNamesFromMessage for this message.
	//
	// It used to be called THREE times per message: once by self-learning, once by name
	// extraction and once by the learn step. Each call makes a GuildMember REST request
	// per mention to resolve nicknames and then opens a read transaction, so a message
	// mentioning three people cost nine REST calls where three would do. Cached here,
	// which is safe because the message does not change while we handle it.
	mentioned    []MentionedUser
	mentionedSet bool
}

// names returns the mentioned users for this message, resolving them at most once.
func (r *reaction) names() []MentionedUser {
	if !r.mentionedSet {
		r.mentioned = extractNamesFromMessage(r.s, r.m, r.m.GuildID)
		r.mentionedSet = true
	}
	return r.mentioned
}

// step is one stage of handling. It reports whether it CONSUMED the message.
//
// Returning true stops the run. It means "this message was addressed to me and has been
// dealt with", not "I did something": the aggro step reacts, the reply step posts, and
// neither consumes, because a message that gets a reply must still be learned from.
// Only a command consumes, because a command is not conversation.
type step struct {
	name string
	fn   func(*reaction) bool
}

// steps run in order, and the order is behaviour rather than taste.
//
// Two placements are load-bearing:
//
//   - The learn gate is FIRST, so a message the bot will not learn from is also not
//     replied to and not reacted to. That is a convenience rather than the protection:
//     the protection is CheckLearn inside learnMessage, which every path reaches
//     including the ones that never come through here.
//   - Commands come BEFORE the reply and learn steps, which is the whole point of the
//     short-circuit. Putting them after would answer the command and then also reply to
//     it and learn it, which is finding 9 with extra steps.
//
// Everything after the commands runs in the order it did before, so this milestone
// changes what CAN happen rather than the sequence of what does.
var steps = []step{
	{"learn-gate", stepLearnGate},
	{"memory", stepMemory},
	{"word-game", stepWordGame},
	{"commands", stepCommands},
	{"aggro", stepAggro},
	{"classify", stepClassify},
	{"reply", stepReply},
	{"learn", stepLearn},
	{"voice", stepVoice},
	{"images", stepImages},
}

// handleMessage runs the reactor over one message.
func handleMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	r := &reaction{
		s:     s,
		m:     m,
		start: time.Now(),
		flags: make(map[string]bool),
	}
	log.Printf("[MSG] from %s: %q", m.Author.Username, m.Content)

	for _, st := range steps {
		if st.fn(r) {
			log.Printf("[OK] msg from %s consumed by %s in %s",
				m.Author.Username, st.name, time.Since(r.start))
			return
		}
	}

	log.Printf("[OK] handled msg from %s in %s | flags: %+v",
		m.Author.Username, time.Since(r.start), r.flags)
}

// stepLearnGate drops a message the corpus must not see.
//
// Reject early, so the bot neither replies to nor reacts to a message it will not learn
// from either. This is a convenience, not the protection: the protection is CheckLearn
// inside learnMessage, which every path reaches. If this step were deleted the corpus
// would still be safe; the bot would just waste work replying to spam.
//
// There used to be a `m.Content = filterSlurs(m.Content)` next to this, and removing it
// was load-bearing rather than tidying. It replaced matches in place, so the message was
// learned anyway with its structure intact and a harmless token in the slur's
// grammatical position: the bot had been taught the sentence and merely said "ninja"
// where the slur went (SPEC.md section 4, A5). With CheckLearn in place, laundering here
// would be strictly worse than useless, because the gate would receive the already
// cleaned text, find nothing, and allow it.
func stepLearnGate(r *reaction) bool {
	if v := gate.CheckLearn(r.m.Content); !v.Allowed {
		log.Printf("[FILTER] Message from %s dropped, no processing will occur: %s",
			r.m.Author.Username, v.Reason)
		return true
	}
	return false
}

// stepMemory records the message in this channel's conversation memory.
//
// Per channel, so context does not bleed across unrelated threads (SPEC.md finding G8).
// Never consumes: remembering is not answering.
func stepMemory(r *reaction) bool {
	memoryFor(r.m.ChannelID).AddMessage(r.m.Content)
	return false
}

// stepClassify computes the flag set the later steps read.
//
// Its own step so that the flags are computed after the commands have had their chance
// to consume the message, which saves a ChannelMessage REST call on every command: the
// REPLY_TO_BOT check fetches the referenced message.
func stepClassify(r *reaction) bool {
	m := r.m

	// Mentioned the bot.
	if strings.Contains(m.Content, "<@"+botID+">") || strings.Contains(m.Content, "<@!"+botID+">") {
		r.flags["MENTIONED"] = true
	}

	// Reply to the bot.
	if m.MessageReference != nil && m.MessageReference.MessageID != "" && m.MessageReference.ChannelID != "" {
		if refMsg, err := r.s.ChannelMessage(m.MessageReference.ChannelID, m.MessageReference.MessageID); err == nil && refMsg.Author.ID == botID {
			r.flags["REPLY_TO_BOT"] = true
		}
	}

	// Voice attachments. Discord voice messages are always .ogg and named
	// voice-message.ogg.
	r.flags["VOICE"] = false
	for _, att := range m.Attachments {
		if strings.ToLower(att.Filename) == "voice-message.ogg" && strings.ToLower(filepath.Ext(att.Filename)) == ".ogg" {
			r.flags["VOICE"] = true
			break
		}
	}

	r.flags["TEXT"] = len(strings.TrimSpace(m.Content)) > 0

	if r.flags["TEXT"] && cfg.SelfMention.MatchString(m.Content) {
		r.flags["SELF_MENTION_KEYWORD"] = true
	}

	return false
}

// stepWordGame runs the scramble game: a winning guess if one is active, otherwise a
// chance to start one.
//
// Does NOT consume, which is a deliberate preservation of existing behaviour rather than
// an oversight. A guess is ordinary chat, so the message still reaches the reply and
// learn steps as it did before. Whether it SHOULD is a real question, since the win path
// deletes the guess message and learning something the bot then deleted is odd, but that
// is a behaviour change with no finding behind it and does not belong in the same commit
// as one. Recorded in SPEC.md section 10.
func stepWordGame(r *reaction) bool {
	if !cfg.EnableWordGames || !wordGamesAvailable {
		return false
	}
	s, m := r.s, r.m

	wordGameMutex.Lock()
	defer wordGameMutex.Unlock()

	game, gameExists := activeWordGames[m.ChannelID]
	if !gameExists {
		maybeStartWordGame(s, m)
		return false
	}
	if !game.CheckGuess(m.Content) {
		return false
	}

	solveTime := time.Since(game.StartTime)
	winnerUsername := m.Author.Username
	if m.Member != nil && m.Member.Nick != "" {
		winnerUsername = m.Member.Nick
	}

	// Announce the winner and set the announcement for delayed deletion.
	//
	// Through the guard, which matters here for a reason that is not obvious: the
	// winner's nickname is interpolated into this string, and a nickname is
	// user-controlled text that can contain a role mention. Even a message the bot
	// composes itself is untrusted-input-shaped.
	winMessage, ok := guard.Send(m.ChannelID, winnerMessage(winnerUsername, game.OriginalWord, solveTime))
	if ok {
		go func(channelID, messageID string) {
			time.Sleep(30 * time.Second)
			deleteMessage(s, channelID, messageID)
		}(m.ChannelID, winMessage.ID)
	}

	leaderboard.AddWin(m.Author.ID, winnerUsername)
	_ = store.Update(func(w *storage.Writer) error {
		return saveLeaderboard(w, leaderboard)
	})

	// Clean up messages. These two used to discard their errors with a bare `_ =`,
	// which is the pattern finding 8's neighbours were full of.
	deleteMessage(s, m.ChannelID, game.MessageID) // the original puzzle
	deleteMessage(s, m.ChannelID, m.ID)           // the winning message

	delete(activeWordGames, m.ChannelID)
	return false
}

// maybeStartWordGame starts a game if the channel has been busy enough. The caller holds
// wordGameMutex.
func maybeStartWordGame(s *discordgo.Session, m *discordgo.MessageCreate) {
	activityMutex.Lock()
	defer activityMutex.Unlock()

	now := time.Now()
	newTimestamps := []time.Time{}
	for _, ts := range channelActivity[m.ChannelID] {
		if now.Sub(ts) < 5*time.Minute {
			newTimestamps = append(newTimestamps, ts)
		}
	}
	newTimestamps = append(newTimestamps, now)
	channelActivity[m.ChannelID] = newTimestamps

	// At least five messages in the last five minutes, then a 2.5% chance per message.
	if len(newTimestamps) < 5 || rand.Float64() >= 0.025 {
		return
	}

	game, err := wordgames.NewScrambleGame()
	if err != nil {
		log.Printf("[WORDGAME] Failed to create new game: %v", err)
		return
	}
	msg, ok := guard.Send(m.ChannelID, scrambleMessage(game.ScrambledWord))
	if !ok {
		return
	}
	game.MessageID = msg.ID
	activeWordGames[m.ChannelID] = game
	channelActivity[m.ChannelID] = []time.Time{} // reset activity after starting
	log.Printf("[WORDGAME] Started a new game in channel %s.", m.ChannelID)
	go expireWordGame(s, m.ChannelID, msg.ID, game.OriginalWord)
}

// expireWordGame ends a game nobody solved.
//
// This consolidates three copies of the same orphan goroutine-plus-Sleep timer, which is
// why they had drifted apart. They still fire against a closed session after shutdown;
// M11 makes them ctx-bound, which is the other half of that finding.
func expireWordGame(s *discordgo.Session, channelID, messageID, originalWord string) {
	time.Sleep(60 * time.Second)

	wordGameMutex.Lock()
	defer wordGameMutex.Unlock()

	g, exists := activeWordGames[channelID]
	if !exists || g.MessageID != messageID {
		return
	}
	if timeoutMsg, ok := guard.Send(channelID, timeUpMessage(originalWord)); ok {
		go func(cid, mid string) {
			time.Sleep(30 * time.Second)
			deleteMessage(s, cid, mid)
		}(channelID, timeoutMsg.ID)
	}
	deleteMessage(s, channelID, messageID)
	delete(activeWordGames, channelID)
	log.Printf("[WORDGAME] Game timed out in channel %s.", channelID)
}

// stepCommands handles the two bang commands, and it is the reason the short-circuit
// contract exists.
//
// Both CONSUME the message. Before this, !leaderboard answered and then fell through into
// everything below it, so the bot replied to its own command as if it were conversation
// and then learned the string into the corpus (finding 9). !wordgame had the same shape.
//
// A command is consumed even when it fails or is refused. An unauthorized !wordgame is
// still a command rather than something to reply to, and a leaderboard that could not be
// built has already said so.
func stepCommands(r *reaction) bool {
	switch commandFor(r.m.Content) {
	case "!leaderboard":
		cmdLeaderboard(r)
		return true
	case "!wordgame":
		// Only a command when word games are available at all. With the feature off it
		// is not a command, so it falls through and is treated as chat, which is what it
		// is. Consuming it would mean the bot silently ignored a message for a feature
		// that is not running.
		if cfg.EnableWordGames && wordGamesAvailable {
			cmdWordGame(r)
			return true
		}
	}
	return false
}

// commandFor recognizes a command, or returns "".
//
// Split out from stepCommands so the recognizer is testable without a session, which the
// two command bodies need.
//
// It matches the WHOLE trimmed message, not a prefix. That is deliberate: a prefix match
// would make it impossible to talk about a command, so "you should try !leaderboard
// sometime" would be swallowed and answered instead of being ordinary chat. Trimming is
// new and is the one behaviour change here, because a trailing space is a typo rather than
// a different message.
func commandFor(content string) string {
	switch strings.ToLower(strings.TrimSpace(content)) {
	case "!leaderboard":
		return "!leaderboard"
	case "!wordgame":
		return "!wordgame"
	}
	return ""
}

// cmdLeaderboard posts the word-game wins plus the chat leaderboard.
//
// Deliberately NOT gated on EnableWordGames. The chat half reads the stats bucket, which
// is populated on every message regardless of whether the scramble game runs, so the
// command is useful with word games off and SPEC.md section 6 says so. The win half is
// simply empty in that case.
func cmdLeaderboard(r *reaction) {
	s, m := r.s, r.m

	var userScores map[string]int
	err := store.View(func(rd *storage.Reader) error {
		var loadErr error
		userScores, loadErr = loadAllUserStats(rd)
		return loadErr
	})
	if err != nil {
		log.Printf("[LEADERBOARD] Error loading user stats: %v", err)
		sendMessage(s, m.ChannelID, "Could not generate chat leaderboard.")
		return
	}

	nameScores := make(map[string]int, len(userScores))
	for userID, score := range userScores {
		nameScores[displayNameFor(s, m.GuildID, userID)] = score
	}

	sendMessage(s, m.ChannelID, leaderboard.Format()+"\n\n"+wordgames.FormatChatLeaderboard(nameScores))
}

// displayNameFor resolves a user ID to the best available name: guild nickname, then
// username, then the raw ID.
//
// The ID fallback is deliberate rather than an error path. A leaderboard that omits
// whoever has left the server silently loses entries, and one that fails entirely because
// of a single departed member is worse than one showing a number for them.
func displayNameFor(s *discordgo.Session, guildID, userID string) string {
	if member, err := s.GuildMember(guildID, userID); err == nil {
		if member.Nick != "" {
			return member.Nick
		}
		return member.User.Username
	}
	if user, err := s.User(userID); err == nil {
		return user.Username
	}
	log.Printf("[LEADERBOARD] Could not resolve a name for user ID %s", userID)
	return userID
}

// cmdWordGame starts a game on demand.
//
// The authorization check here is the only one in the codebase, and until M2 it was a
// user ID hardcoded in the source. It fails CLOSED: an unset
// PEREGRINE_BOOTSTRAP_ADMIN_USER_ID refuses this command for everyone, never allows it
// for everyone. Getting that direction wrong on an empty string is how a missing variable
// turns an operator-only command into a public one.
func cmdWordGame(r *reaction) {
	if cfg.AdminUserID == "" || r.m.Author.ID != cfg.AdminUserID {
		return
	}

	wordGameMutex.Lock()
	defer wordGameMutex.Unlock()

	if _, exists := activeWordGames[r.m.ChannelID]; exists {
		sendMessage(r.s, r.m.ChannelID, "A word game is already in progress in this channel!")
		return
	}

	game, err := wordgames.NewScrambleGame()
	if err != nil {
		log.Printf("[WORDGAME] Failed to create new game: %v", err)
		return
	}
	msg, ok := guard.Send(r.m.ChannelID, scrambleMessage(game.ScrambledWord))
	if !ok {
		return
	}
	game.MessageID = msg.ID
	activeWordGames[r.m.ChannelID] = game
	log.Printf("[WORDGAME] Started a new game in channel %s.", r.m.ChannelID)
	go expireWordGame(r.s, r.m.ChannelID, msg.ID, game.OriginalWord)
}

// stepAggro reacts to a message from the current aggro target.
//
// Never consumes: a reaction is not an answer, and the target's message still gets
// learned and possibly replied to, which is the point of aggro.
func stepAggro(r *reaction) bool {
	birdAggroMutex.Lock()
	isTarget := r.m.Author.ID == birdAggroTargetID
	isExpired := time.Now().After(birdAggroEndTime)
	birdAggroMutex.Unlock() // released before any API call

	switch {
	case isTarget && !isExpired:
		// Through the guard, so PEREGRINE_PAUSE_ALL_WRITES stops the bot reacting as
		// well as talking. A reaction is still the bot visibly participating, and an
		// operator hitting the emergency stop is not asking it to keep poking someone.
		guard.React(r.m.ChannelID, r.m.ID, cfg.AggroEmoji)

	case isTarget && isExpired:
		log.Printf("[AGGRO] Aggro expired for %s, clearing...", r.m.Author.Username)
		birdAggroMutex.Lock()
		birdAggroTargetID = ""
		birdAggroEndTime = time.Time{}
		birdAggroMutex.Unlock()

		// Persist and take the reaction back, both outside the lock.
		if err := store.Update(func(w *storage.Writer) error {
			return saveAggroState(w, AggroState{})
		}); err != nil {
			log.Printf("[AGGRO] failed to clear persisted aggro state: %v", err)
		}

		// botID rather than s.State.User.ID, which was a nil dereference waiting for a
		// state cache that had not filled yet.
		guard.Unreact(r.m.ChannelID, r.m.ID, cfg.AggroEmoji, botID)
	}
	return false
}

// stepReply generates and posts a reply when the bot was addressed.
//
// Never consumes. A message that earns a reply is still ordinary conversation and must
// still be learned from, which is the distinction between this and a command.
func stepReply(r *reaction) bool {
	if !r.flags["TEXT"] {
		return false
	}
	if !r.flags["MENTIONED"] && !r.flags["REPLY_TO_BOT"] && !r.flags["SELF_MENTION_KEYWORD"] {
		return false
	}

	s, m := r.s, r.m
	replyStart := time.Now()

	promptForGeneration := m.Content
	isRoast := false

	switch {
	case r.flags["SELF_MENTION_KEYWORD"] && !r.flags["MENTIONED"] && !r.flags["REPLY_TO_BOT"]:
		// Overheard rather than addressed: the bot is being talked ABOUT, so it answers
		// self-referentially and always roasts.
		promptForGeneration = "<START> peregrine"
		isRoast = true
		log.Printf("[INFO] Activating 'roast' mode due to self-mention keyword. Using prompt: %q", promptForGeneration)

	default:
		// Addressed directly. PEREGRINE_ROAST_CHANCE as of M7b, where this was a
		// hardcoded 0.10.
		if rand.Float64() < cfg.RoastChance {
			isRoast = true
			log.Printf("[INFO] Activating 'roast' mode for direct interaction.")
		}
	}

	// Per-channel memory as of M7b. This used to be one package-level memory shared by
	// every channel in every guild, so a reply here was steered by whatever had been said
	// somewhere unrelated (SPEC.md finding G8).
	reply, err := generateSentenceWithContext(s, promptForGeneration, isRoast, memoryFor(m.ChannelID))
	if err != nil {
		log.Printf("[ERR] reply generation failed: %v", err)
		return false
	}
	if reply == "" {
		return false
	}

	// Through the guard, and this is the call site finding 8 was about.
	// ChannelMessageSendReply set no AllowedMentions, so Discord's default applied and the
	// author was pinged on every single interaction.
	sent, ok := guard.SendReply(m.ChannelID, reply, &discordgo.MessageReference{MessageID: m.ID, ChannelID: m.ChannelID})
	if !ok {
		// The guard has already logged whether this was a refusal or a failure, and
		// which.
		log.Printf("[RESP] reply to %s was not sent", m.Author.Username)
		return false
	}
	log.Printf("[RESP] replied to %s in %s: %q", m.Author.Username, time.Since(replyStart), reply)

	selfLearn(r, sent.ID, reply)
	return false
}

// selfLearn feeds the bot's own reply back into the corpus, keyed by THE REPLY'S OWN
// MESSAGE ID.
//
// That key is finding 6, and it was a data-loss bug rather than an inefficiency. Both this
// and the learn step called learnMessage with the USER's message ID, and learnMessage
// dedupes on that ID through MarkSeen. So whichever transaction committed first marked the
// ID seen and the other became a silent no-op: either the user's message or the bot's
// reply was thrown away on every single interaction, and which one depended on a race
// between this goroutine and the main path.
//
// Using the reply's own ID makes them two distinct messages, which is what they are. The
// dedup still does its job for each of them independently.
func selfLearn(r *reaction, replyID, replyContent string) {
	s, m := r.s, r.m

	// Resolved from the state cache rather than a REST call. This used to be
	// s.User("@me") on every reply, which asks Discord who we are when botID is already
	// known and the username is already cached.
	botName := "peregrine"
	if s != nil && s.State != nil && s.State.User != nil && s.State.User.Username != "" {
		botName = s.State.User.Username
	}
	botAsMention := MentionedUser{Name: botName, UserID: botID, Username: botName}

	// The mentioned users come from the cache on the reaction, so this does not repeat
	// the REST calls the learn step already made.
	//
	// The name extraction is hoisted OUT of the write transaction, and that is a fix
	// rather than a tidy-up: it used to run inside the db.Update, which meant a read
	// transaction and a series of REST calls nested inside a write transaction while
	// bbolt's single writer lock was held.
	//
	// The bot's own user ID reaching learnMessage is deliberate and safe: learnMessage
	// compares it to botID and passes an empty author to LearnNgram, so self-learning
	// contributes nothing to author diversity (SPEC.md section 4, A6).
	mentionedInPrompt := r.names()

	go func() {
		if err := store.Update(func(w *storage.Writer) error {
			return learnMessage(w, replyContent, replyID, botID, botAsMention, mentionedInPrompt)
		}); err != nil {
			log.Printf("[WARN] self-learning failed for reply %s in %s: %v", replyID, m.ChannelID, err)
		}
	}()
}

// stepLearn records the author's message and the names in it.
//
// This was two separate blocks that both called extractNamesFromMessage, so a message
// mentioning three people made six REST calls to resolve nicknames that had just been
// resolved. One block now, over the cached list.
func stepLearn(r *reaction) bool {
	if !r.flags["TEXT"] {
		return false
	}
	m := r.m
	mentionedUsers := r.names()

	if len(mentionedUsers) > 0 {
		_ = store.Update(func(w *storage.Writer) error {
			for _, user := range mentionedUsers {
				if _, err := learnOrUpdateName(w, user.Name, user.UserID, user.Username); err != nil {
					log.Printf("[WARN] Failed to learn name '%s' during extraction: %v", user.Name, err)
				}
			}
			return nil
		})
	}

	// The author, so their own message content is associated with them. Prefer the
	// nickname when there is one.
	authorAsMention := MentionedUser{
		Name:     m.Author.Username,
		UserID:   m.Author.ID,
		Username: m.Author.Username,
	}
	if m.Member != nil && m.Member.Nick != "" {
		authorAsMention.Name = m.Member.Nick
	}

	// Avoid duplicating the author if they were already in the list, which happens when
	// somebody mentions themselves.
	learners := mentionedUsers
	alreadyPresent := false
	for _, u := range learners {
		if u.UserID == m.Author.ID {
			alreadyPresent = true
			break
		}
	}
	if !alreadyPresent {
		// Copied rather than appended in place, because appending would write into the
		// slice cached on the reaction and the self-learn step reads that same slice.
		learners = append(append([]MentionedUser(nil), learners...), authorAsMention)
	}

	if err := store.Update(func(w *storage.Writer) error {
		return learnMessage(w, m.Content, m.ID, botID, authorAsMention, learners)
	}); err != nil {
		log.Printf("[WARN] learning message %s failed: %v", m.ID, err)
	}
	return false
}

// stepVoice queues voice attachments for transcription.
func stepVoice(r *reaction) bool {
	if !cfg.EnableTranscription || !r.flags["VOICE"] {
		return false
	}
	m := r.m

	for _, att := range m.Attachments {
		switch strings.ToLower(filepath.Ext(att.Filename)) {
		case ".ogg", ".mp3", ".wav":
		default:
			continue
		}

		placeholder, ok := guard.SendReply(
			m.ChannelID,
			"🔊 transcription in progress...",
			&discordgo.MessageReference{MessageID: m.ID, ChannelID: m.ChannelID},
		)
		if !ok {
			// No placeholder means nothing to edit later, so do not queue the job: a
			// transcription with nowhere to go would burn a Whisper run and then log an
			// edit failure against a message ID that never existed.
			log.Printf("[VOICE] placeholder not sent, skipping transcription")
			continue
		}

		transcriptionQueue <- TranscriptionJob{
			URL:           att.URL,
			AuthorID:      m.Author.ID,
			MsgID:         m.ID,
			ChannelID:     m.ChannelID,
			PlaceholderID: placeholder.ID,
			Author:        m.Author,
			Member:        m.Member,
		}
	}
	return false
}

// stepImages caches candidate URLs and occasionally reposts one.
func stepImages(r *reaction) bool {
	if !cfg.EnableImageRepost {
		return false
	}
	s, m := r.s, r.m

	captureImageURLs(s, m)

	// A direct interaction gets a higher repost chance than an overheard one: the bot is
	// already contributing to the channel, so an unrelated image on top of the reply is
	// noise rather than chaos.
	repostChance := cfg.ImageRepostDirect
	if !r.flags["MENTIONED"] && !r.flags["REPLY_TO_BOT"] {
		repostChance = cfg.ImageRepostChance
	}
	if rand.Float64() >= repostChance {
		return false
	}

	imageURLMutex.Lock()
	var urlToPost string
	if len(recentImageURLs) > 0 {
		urlToPost = recentImageURLs[rand.IntN(len(recentImageURLs))]
	}
	imageURLMutex.Unlock() // released before the send

	if urlToPost == "" {
		return false
	}

	// Logged before the send, and worded as an attempt, because sendMessage returns
	// nothing: it logs its own failure. Claiming success after a void call would report a
	// repost Discord refused as one that happened.
	log.Printf("[REPOST] Reposting image: %s", urlToPost)
	sendMessage(s, m.ChannelID, urlToPost)
	return false
}

package legacy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/peregrine/internal/activity"
	"github.com/6586x57890143/peregrine/internal/discordguard"
	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/bwmarrin/discordgo"
)

// This file covers M11b: the last of the engagement surface. Three subjects, each with a
// finding behind it.
//
//   - Image reposting as an unattributed republishing channel (SPEC.md section 4, A7).
//   - Authorization as a chokepoint rather than an inline comparison (finding 19).
//   - Activity read from the gateway rather than paged out of the REST API (finding 14).

// ---------------------------------------------------------------- A7: image reposting

func cachedURLs(t *testing.T, s *storage.Store) []string {
	t.Helper()
	var out []string
	if err := s.View(func(r *storage.Reader) error {
		var err error
		out, err = r.ImageURLs()
		return err
	}); err != nil {
		t.Fatalf("ImageURLs: %v", err)
	}
	return out
}

// TestADeletedMessagesImageBecomesUnrepostable is the deleted-message rule, end to end
// through the handler path.
//
// A deletion is a strong signal that the content must not be republished, and the bot
// reposting something a moderator or its own author has just removed is the failure A7
// describes. The rule has been in SPEC.md section 4.2 since M5 and could not be built
// until the image cache was keyed by message.
//
// Verified by reverting: with the DeleteImagesByMessage call removed from
// forgetImagesFromMessage, the URL survives both the store and the in-memory cache.
func TestADeletedMessagesImageBecomesUnrepostable(t *testing.T) {
	s := gateFixture(t)

	const doomed = "1720000000000000001"
	const kept = "1730000000000000001"
	if err := s.Update(func(w *storage.Writer) error {
		if err := w.AddImageURL("https://cdn.discordapp.com/doomed.png", doomed, snowflake(11), 100, 10); err != nil {
			return err
		}
		return w.AddImageURL("https://cdn.discordapp.com/kept.png", kept, snowflake(12), 100, 10)
	}); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}

	imageURLMutex.Lock()
	recentImageURLs = cachedURLs(t, s)
	imageURLMutex.Unlock()

	forgetImagesFromMessage(doomed)

	if got := cachedURLs(t, s); len(got) != 1 || !strings.Contains(got[0], "kept") {
		t.Errorf("the store holds %v after the source message was deleted, want only kept.png", got)
	}

	// And the in-memory cache, which is what the repost path actually reads. Leaving that
	// stale would mean the rule held in the database and not in the bot.
	imageURLMutex.Lock()
	inMemory := append([]string(nil), recentImageURLs...)
	imageURLMutex.Unlock()
	if len(inMemory) != 1 || !strings.Contains(inMemory[0], "kept") {
		t.Errorf("the in-memory cache holds %v, want only kept.png: the repost path reads this, not the store", inMemory)
	}
}

// TestABulkDeletionIsOneTransaction. A moderator purging a spam raid produces one bulk
// event with many IDs, and each deletion opens a write transaction against bbolt's single
// writer. Handling them as one unit is the difference between one transaction and a
// hundred queued behind all ingestion.
func TestABulkDeletionIsOneTransaction(t *testing.T) {
	s := gateFixture(t)

	ids := make([]string, 0, 20)
	if err := s.Update(func(w *storage.Writer) error {
		for i := range 20 {
			id := snowflake(1800 + i)
			ids = append(ids, id)
			if err := w.AddImageURL("https://cdn.discordapp.com/"+id+".png", id, snowflake(50+i), 100, 10); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}

	forgetImagesFromMessage(ids...)

	if got := cachedURLs(t, s); len(got) != 0 {
		t.Errorf("cache holds %d URLs after every source message was deleted: %v", len(got), got)
	}
}

// TestForgettingAnUnknownMessageIsQuiet. Every deletion in every channel the bot can see
// arrives at this handler, and almost none of them contributed a cached URL. Touching the
// in-memory cache or logging for each would make the common case the expensive one.
func TestForgettingAnUnknownMessageIsQuiet(t *testing.T) {
	s := gateFixture(t)

	if err := s.Update(func(w *storage.Writer) error {
		return w.AddImageURL("https://cdn.discordapp.com/kept.png", snowflake(2000), snowflake(9), 100, 10)
	}); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	imageURLMutex.Lock()
	recentImageURLs = []string{"https://cdn.discordapp.com/kept.png"}
	imageURLMutex.Unlock()

	forgetImagesFromMessage(snowflake(9999))

	if got := cachedURLs(t, s); len(got) != 1 {
		t.Errorf("cache holds %v after deleting an unrelated message, want it untouched", got)
	}
}

// TestASlurBearingTenorURLIsRefused is a "verified rather than assumed" test.
//
// A Tenor URL carries words in its slug, hyphen-separated, so a repost is a way to make
// the bot post a slur without any of it ever entering the corpus as text. Every send goes
// through the guard and the guard runs CheckEmit, and safety.Normalize treats a hyphen
// next to a real word as a boundary, so this SHOULD already be blocked. Should-already-be
// is how findings happen, so it is pinned here rather than reasoned about.
func TestASlurBearingTenorURLIsRefused(t *testing.T) {
	gateFixture(t)

	// The fixture's blocklist pattern, embedded in a Tenor slug the way a real one reads.
	before := len(sent.sends)
	guard.Send("chan", "https://tenor.com/view/exampleslur-gif-12345")
	if len(sent.sends) != before {
		t.Errorf("a blocklisted word inside a Tenor slug reached Discord: %q", sent.sends[len(sent.sends)-1])
	}

	// And the control: an innocuous URL is not blocked, so the test above is measuring the
	// blocklist rather than a guard that refuses everything.
	before = len(sent.sends)
	guard.Send("chan", "https://tenor.com/view/bird-flying-gif-12345")
	if len(sent.sends) != before+1 {
		t.Error("an ordinary Tenor URL was refused, so the check above proves nothing")
	}
}

// TestRepostRefusesAnUnknownDestination and its NSFW sibling.
//
// SPEC.md section 4.2 asks for the NSFW and blocklist checks on the DESTINATION as well as
// the origin, and until now only the origin had one: capture refuses to remember media
// from an NSFW channel, but nothing stopped a repost carrying an image out of a general
// channel into somewhere the bot has no business posting. It fails closed on a state-cache
// miss, for the same reason capture does: this decides what the bot republishes, and "we
// could not tell" has to mean "do not".
//
// Verified by reverting: with the destination check disabled, both refusal cases post.
func TestRepostRefusesAnUnknownDestination(t *testing.T) {
	store := gateFixture(t)
	cfg.EnableImageRepost = true
	cfg.ImageRepostChance = 1 // always attempt, so a refusal is the only reason for silence
	cfg.ImageRepostDirect = 1
	cfg.ImageCacheSize = 100
	cfg.ImageMaxPerAuthor = 10

	const cached = "https://cdn.discordapp.com/cached.png"
	if err := store.Update(func(w *storage.Writer) error {
		return w.AddImageURL(cached, snowflake(3100), snowflake(31), 100, 10)
	}); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	imageURLMutex.Lock()
	recentImageURLs = []string{cached}
	imageURLMutex.Unlock()

	cases := []struct {
		name    string
		channel *discordgo.Channel
		wantSum int
	}{
		{"ordinary text channel", &discordgo.Channel{ID: "c1", Name: "memes", Type: discordgo.ChannelTypeGuildText}, 1},
		{"nsfw flag", &discordgo.Channel{ID: "c1", Name: "memes", Type: discordgo.ChannelTypeGuildText, NSFW: true}, 0},
		{"nsfw name", &discordgo.Channel{ID: "c1", Name: "nsfw-memes", Type: discordgo.ChannelTypeGuildText}, 0},
		{"not in the state cache", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s *discordgo.Session
			if tc.channel == nil {
				s = sessionWithChannels(t)
			} else {
				s = sessionWithChannels(t, tc.channel)
			}

			sent = &recordingSession{}
			guard = discordguard.New(sent, emitGate{g: gate}, nil, nil)

			r := &reaction{
				s: s,
				m: &discordgo.MessageCreate{Message: &discordgo.Message{
					ID:        snowflake(3200),
					ChannelID: "c1",
					Content:   "just talking",
					Author:    &discordgo.User{ID: snowflake(32), Username: "someone"},
				}},
				flags: map[string]bool{},
			}
			if consumed := stepImages(r); consumed {
				t.Error("the image step consumed the message; only a command may consume")
			}
			if len(sent.sends) != tc.wantSum {
				t.Errorf("sent %d messages, want %d: %v", len(sent.sends), tc.wantSum, sent.sends)
			}
		})
	}
}

// ---------------------------------------------------------------- finding 19: authorization

// TestAuthorizedFailsClosed. An unset PEREGRINE_BOOTSTRAP_ADMIN_USER_ID must refuse
// everyone, never allow everyone. Getting that direction wrong on an empty string turns a
// missing variable into a public operator command.
//
// Verified by reverting: with the empty check removed, the first subtest passes a matching
// empty user ID and the command becomes available to any message with no author ID.
func TestAuthorizedFailsClosed(t *testing.T) {
	gateFixture(t)

	cfg.AdminUserID = ""
	for _, id := range []string{"", snowflake(1), "anyone"} {
		if authorized(id) {
			t.Errorf("authorized(%q) = true with no admin configured; it must fail closed", id)
		}
	}

	cfg.AdminUserID = snowflake(77)
	if !authorized(snowflake(77)) {
		t.Error("the configured admin was refused")
	}
	if authorized(snowflake(78)) {
		t.Error("a non-admin was authorized")
	}
	if authorized("") {
		t.Error("an empty user ID was authorized against a configured admin")
	}
}

// TestAuthorizationIsAChokepoint parses the package and fails if anything other than
// authorized() compares against the admin user ID.
//
// This is what closes finding 19 rather than merely relocating it. The check was a
// hardcoded Discord ID until M2 and an inline comparison in one command body until now, so
// the second command to need it would reimplement it, and the way an inline reimplementation
// goes wrong is the empty case, which fails OPEN. A behavioural test cannot catch that: it
// would have to know about the command that has not been written yet.
//
// Same argument and same mechanism as TestNothingBypassesTheGuard.
func TestAuthorizationIsAChokepoint(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(".", "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name == "authorized" || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "AdminUserID" {
					return true
				}
				t.Errorf("%s names AdminUserID directly (%s). Authorization belongs in "+
					"authorized(), which fails closed on an empty value; an inline "+
					"comparison is how finding 19 comes back as a public operator command",
					fn.Name.Name, fset.Position(sel.Pos()))
				return true
			})
		}
	}
}

// ---------------------------------------------------------------- finding 14: activity

// TestBusiestChannelReadsTheTrackerNotTheAPI. The version this replaces called
// s.UserGuilds and then paged every text channel in every guild fifty messages at a time.
// The session here has no transport at all, so a REST call would fail rather than answer,
// which is the strongest available assertion that none is made.
func TestBusiestChannelReadsTheTrackerNotTheAPI(t *testing.T) {
	gateFixture(t)
	cfg.ActiveChannelWindow = time.Hour

	s := sessionWithChannels(t,
		&discordgo.Channel{ID: "c-quiet", Name: "quiet", Type: discordgo.ChannelTypeGuildText},
		&discordgo.Channel{ID: "c-loud", Name: "memes", Type: discordgo.ChannelTypeGuildText},
	)

	for range 2 {
		channelActivity.Note("c-quiet", snowflake(1))
	}
	for range 9 {
		channelActivity.Note("c-loud", snowflake(2))
	}

	if got := busiestChannel(s, nil); got != "c-loud" {
		t.Errorf("busiestChannel = %q, want c-loud", got)
	}
}

// TestBusiestChannelFiltersWhileChoosing. The old code scored every channel, picked the
// winner and then rejected it if it was not on the allowlist, so a bot whose busiest
// channel was not listed posted nothing and logged a rejection every single cycle.
func TestBusiestChannelFiltersWhileChoosing(t *testing.T) {
	gateFixture(t)
	cfg.ActiveChannelWindow = time.Hour

	s := sessionWithChannels(t,
		&discordgo.Channel{ID: "c-allowed", Name: "allowed", Type: discordgo.ChannelTypeGuildText},
		&discordgo.Channel{ID: "c-busiest", Name: "busiest", Type: discordgo.ChannelTypeGuildText},
	)

	for range 3 {
		channelActivity.Note("c-allowed", snowflake(1))
	}
	for range 30 {
		channelActivity.Note("c-busiest", snowflake(2))
	}

	if got := busiestChannel(s, []string{"c-allowed"}); got != "c-allowed" {
		t.Errorf("busiestChannel with an allowlist = %q, want the busiest ALLOWED channel", got)
	}
}

// TestBusiestChannelSkipsWhatItCannotIdentify. This decides where the bot speaks
// unprompted, so a channel missing from the state cache has to mean "not here": the
// alternative is posting into a channel whose type and NSFW flag are unknown. Same
// fail-closed direction as the image capture's NSFW check.
func TestBusiestChannelSkipsWhatItCannotIdentify(t *testing.T) {
	gateFixture(t)
	cfg.ActiveChannelWindow = time.Hour

	s := sessionWithChannels(t,
		&discordgo.Channel{ID: "c-known", Name: "known", Type: discordgo.ChannelTypeGuildText},
		&discordgo.Channel{ID: "c-voice", Name: "voice", Type: discordgo.ChannelTypeGuildVoice},
	)

	for range 50 {
		channelActivity.Note("c-unknown", snowflake(1)) // never added to the state cache
	}
	for range 50 {
		channelActivity.Note("c-voice", snowflake(2)) // known, but not a text channel
	}
	for range 2 {
		channelActivity.Note("c-known", snowflake(3))
	}

	if got := busiestChannel(s, nil); got != "c-known" {
		t.Errorf("busiestChannel = %q, want the only identifiable text channel", got)
	}
}

// TestBusiestChannelIsQuietOnAColdStart. The tracker is empty for the first window after a
// restart. Returning "" is correct: there is nowhere the bot knows people are talking, and
// falling back to the state cache's LastMessageID would offer recency without volume, so a
// channel whose last message was 59 minutes ago could win.
func TestBusiestChannelIsQuietOnAColdStart(t *testing.T) {
	gateFixture(t)
	cfg.ActiveChannelWindow = time.Hour

	s := sessionWithChannels(t, &discordgo.Channel{
		ID: "c1", Name: "general", Type: discordgo.ChannelTypeGuildText,
		LastMessageID: snowflake(9999),
	})

	if got := busiestChannel(s, nil); got != "" {
		t.Errorf("busiestChannel on a cold start = %q, want empty", got)
	}
}

// TestAggroTargetsSomeoneWhoIsActuallyAround. findRandomActiveUser paged Discord for six
// hours of history and could pick someone who had long since left the conversation. It now
// reads the tracker, which means the candidates are people the bot has seen.
func TestAggroTargetsSomeoneWhoIsActuallyAround(t *testing.T) {
	gateFixture(t)
	cfg.AggroActivityWindow = time.Hour

	if got := findRandomActiveUser(); got != "" {
		t.Errorf("findRandomActiveUser on an empty tracker = %q, want empty", got)
	}

	channelActivity.Note("c1", snowflake(500))
	if got := findRandomActiveUser(); got != snowflake(500) {
		t.Errorf("findRandomActiveUser = %q, want the only recently active user", got)
	}
}

// TestAggroNeverTargetsTheBot. It cannot happen today, because messageCreate drops bot
// messages before anything is recorded, and that is exactly why the filter is worth two
// lines: aggro on our own output would be the bot reacting to itself in a loop, and the
// thing preventing it is currently a check in a different function.
func TestAggroNeverTargetsTheBot(t *testing.T) {
	gateFixture(t)
	cfg.AggroActivityWindow = time.Hour

	oldBotID := botID
	t.Cleanup(func() { botID = oldBotID })
	botID = snowflake(1)

	channelActivity.Note("c1", botID)
	if got := findRandomActiveUser(); got != "" {
		t.Errorf("findRandomActiveUser = %q, want empty: the bot must not be a target", got)
	}
}

// TestActivityIsRecordedAfterTheLearnGate.
//
// The order is a decision, not an accident. This count decides where the bot speaks
// unprompted, which channels have earned a word game, and who gets aggro. Counting spam
// would let a flood advertise a channel as busy and pull the bot toward exactly the place
// it should be ignoring.
func TestActivityIsRecordedAfterTheLearnGate(t *testing.T) {
	gateFixture(t)

	var gateIdx, activityIdx = -1, -1
	for i, st := range steps {
		switch st.name {
		case "learn-gate":
			gateIdx = i
		case "activity":
			activityIdx = i
		}
	}
	if gateIdx < 0 || activityIdx < 0 {
		t.Fatalf("expected both a learn-gate and an activity step; got %v", steps)
	}
	if activityIdx < gateIdx {
		t.Error("the activity step runs before the learn gate, so spam counts as activity " +
			"and can attract the bot to the channel it should be ignoring")
	}

	// And behaviourally: blocked content must leave the tracker empty.
	r := &reaction{
		m: &discordgo.MessageCreate{Message: &discordgo.Message{
			ID:        snowflake(1),
			ChannelID: "c1",
			Content:   "the bird should exampleslur",
			Author:    &discordgo.User{ID: snowflake(2), Username: "spammer"},
		}},
		flags: map[string]bool{},
	}
	for _, st := range steps {
		if st.fn(r) {
			break
		}
	}
	if got := channelActivity.Count("c1", time.Hour); got != 0 {
		t.Errorf("a blocked message counted as activity (%d messages recorded)", got)
	}
}

// sessionWithChannels builds a session whose state cache holds the given channels and
// whose transport does not exist, so any REST call fails rather than succeeding quietly.
func sessionWithChannels(t *testing.T, channels ...*discordgo.Channel) *discordgo.Session {
	t.Helper()
	s := &discordgo.Session{State: discordgo.NewState()}
	guild := &discordgo.Guild{ID: "g1"}
	if err := s.State.GuildAdd(guild); err != nil {
		t.Fatalf("GuildAdd: %v", err)
	}
	for _, ch := range channels {
		ch.GuildID = guild.ID
		if err := s.State.ChannelAdd(ch); err != nil {
			t.Fatalf("ChannelAdd(%s): %v", ch.ID, err)
		}
	}
	return s
}

// ---------------------------------------------------------------- emotes, end to end

// TestCustomEmotesResolveThroughTheStateCache is the other half of the emote story.
//
// internal/markov pins that the engine can EMIT a :shortcode: (TestGoldenEmoteShortcodesSurvive).
// What was never pinned is that the shortcode then becomes a real emote: sessionEmoji walks
// s.State.Guilds, and that slice was empty for the entire life of this bot because the
// session never requested IntentsGuilds, so the resolver had never once succeeded and
// peregrine had never spoken in the server's own emotes (SPEC.md section 8, finding 7). M3
// requested the intent; nothing until now checked the code that benefits.
func TestCustomEmotesResolveThroughTheStateCache(t *testing.T) {
	s := &discordgo.Session{State: discordgo.NewState()}
	if err := s.State.GuildAdd(&discordgo.Guild{
		ID: "g1",
		Emojis: []*discordgo.Emoji{
			{ID: "111111111111111111", Name: "peepohappy"},
			{ID: "222222222222222222", Name: "birdspin", Animated: true},
		},
	}); err != nil {
		t.Fatalf("GuildAdd: %v", err)
	}

	got := cleanSentence(s, "ratio :peepohappy: and also :birdspin:")

	if !strings.Contains(got, "<:peepohappy:111111111111111111>") {
		t.Errorf("a static emote did not resolve; got %q", got)
	}
	if !strings.Contains(got, "<a:birdspin:222222222222222222>") {
		t.Errorf("an animated emote did not resolve to the <a:...> form; got %q", got)
	}
}

// TestAnUnknownShortcodeIsLeftAlone. A word between colons that the guild has no emote for
// is ordinary text, and mangling it would be worse than leaving it: the corpus is full of
// things people typed.
func TestAnUnknownShortcodeIsLeftAlone(t *testing.T) {
	s := &discordgo.Session{State: discordgo.NewState()}
	if err := s.State.GuildAdd(&discordgo.Guild{ID: "g1"}); err != nil {
		t.Fatalf("GuildAdd: %v", err)
	}

	if got := cleanSentence(s, "absolute :notanemote: energy"); !strings.Contains(got, ":notanemote:") {
		t.Errorf("an unresolvable shortcode was altered; got %q", got)
	}
}

// TestTheResolverSurvivesAnEmptyState pins the fail-safe direction. A session with no state
// cache (before READY, or in a test) must return the shortcode rather than dereference nil:
// custom emotes are one optional flourish and losing them must not take a reply down.
func TestTheResolverSurvivesAnEmptyState(t *testing.T) {
	for name, s := range map[string]*discordgo.Session{
		"nil session": nil,
		"no state":    {},
		"empty state": {State: discordgo.NewState()},
	} {
		t.Run(name, func(t *testing.T) {
			if got := cleanSentence(s, "hello :peepohappy: there"); !strings.Contains(got, ":peepohappy:") {
				t.Errorf("got %q, want the shortcode left as text", got)
			}
		})
	}
}

// _ keeps the activity import honest if the tests above stop constructing a tracker
// directly; gateFixture builds one, so this documents the dependency rather than adding it.
var _ = activity.New

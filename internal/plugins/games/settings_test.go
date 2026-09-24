package games

import (
	"github.com/6586x57890143/peregrine/internal/storage"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// /wordgame-config, M30. The tests here are about the three things that make a stored setting
// different from an environment variable: it can be changed from Discord, it survives a restart,
// and the command that changes it has to work in a channel the setting itself excludes.

// configInteraction builds a /wordgame-config invocation from an administrator in a channel.
func configInteraction(channelID string, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.Interaction {
	return &discordgo.Interaction{
		Type:      discordgo.InteractionApplicationCommand,
		ChannelID: channelID,
		GuildID:   testGuild,
		Member: &discordgo.Member{
			User:        &discordgo.User{ID: snowflake(1)},
			Permissions: discordgo.PermissionAdministrator,
		},
		Data: discordgo.ApplicationCommandInteractionData{
			Name:    configCommandName,
			Options: opts,
		},
	}
}

func strOpt(name, v string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{
		Name: name, Type: discordgo.ApplicationCommandOptionString, Value: v,
	}
}

// A JSON number arrives as a float64, which is what IntValue converts, so building it as one is
// the difference between testing the parse and testing a value the gateway would never produce.
func intOpt(name string, n int) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{
		Name: name, Type: discordgo.ApplicationCommandOptionInteger, Value: float64(n),
	}
}

func boolOpt(name string, v bool) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{
		Name: name, Type: discordgo.ApplicationCommandOptionBoolean, Value: v,
	}
}

// TestBindingAChannelRestrictsGames is the feature: an operator types the command where they want
// puzzles and the activity trigger stops firing everywhere else, with no .env edit and no deploy.
func TestBindingAChannelRestrictsGames(t *testing.T) {
	s, _, _, _ := fixture(t, enabled())

	s.handleConfig(configInteraction("c1", strOpt(optChannel, channelBind)))

	if !s.allowed(testGuild, "c1") {
		t.Error("the channel the command was run in is not allowed, so binding did nothing")
	}
	if s.allowed(testGuild, "c2") {
		t.Error("another channel is still allowed, so the allowlist is not restricting anything")
	}
}

// TestTheConfigCommandWorksInADisallowedChannel is the trap this command could most easily have
// walked into. Every other path refuses a channel that is not on the allowlist; if this one did
// too, binding the wrong channel would leave the feature unrecoverable from Discord, because the
// only command that can fix it would be refused everywhere it could be typed.
func TestTheConfigCommandWorksInADisallowedChannel(t *testing.T) {
	opts := enabled()
	opts.AllowChannels = []string{"c1"}
	s, guard, _, _ := fixture(t, opts)

	s.handleConfig(configInteraction("c2", strOpt(optChannel, channelBind)))

	if !s.allowed(testGuild, "c2") {
		t.Fatalf("the command was refused in a channel that was not on the allowlist, which is "+
			"the one place it has to work: %v", guard.responded())
	}
	if !s.allowed(testGuild, "c1") {
		t.Error("binding replaced the allowlist instead of adding to it")
	}
}

// TestUnbindingTheLastChannelSaysGamesCanGoAnywhere. An empty allowlist means anywhere, so
// unbinding the last channel does the opposite of what the word sounds like. An operator who
// tightened the list one channel at a time should learn that from the reply rather than by
// watching a puzzle appear somewhere else.
func TestUnbindingTheLastChannelSaysGamesCanGoAnywhere(t *testing.T) {
	opts := enabled()
	opts.AllowChannels = []string{"c1"}
	s, guard, _, _ := fixture(t, opts)

	s.handleConfig(configInteraction("c1", strOpt(optChannel, channelUnbind)))

	replies := guard.responded()
	if len(replies) != 1 {
		t.Fatalf("responses = %v, want exactly one", replies)
	}
	if !replies[0].ephemeral {
		t.Error("a settings answer was public")
	}
	if !strings.Contains(replies[0].content, "anywhere") {
		t.Errorf("the reply did not say games can now run anywhere: %q", replies[0].content)
	}
}

// TestSettingsSurviveARestart. The whole point of a blob rather than an environment variable: the
// second Init must read what the command wrote, not what the process was started with.
func TestSettingsSurviveARestart(t *testing.T) {
	opts := enabled()
	s, guard, manager, tracker := fixture(t, opts)

	s.handleConfig(configInteraction("c1",
		strOpt(optChannel, channelBind),
		strOpt(optMode, string(ModeInterval)),
		intOpt(optInterval, 20)))

	// A second Service over the SAME corpora, which is what a restart is from this package's
	// point of view.
	restarted := New(s.corpora, guard, manager, nil, tracker, s.resolver, nil, opts)

	got := restarted.snapshot(testGuild)
	if got.Mode != ModeInterval || got.Interval != 20*time.Minute {
		t.Errorf("settings after a restart = %+v, want interval mode every 20m", got)
	}
	if !restarted.allowed(testGuild, "c1") || restarted.allowed(testGuild, "c2") {
		t.Errorf("the allowlist did not survive: %+v", got.Channels)
	}
}

// TestAnOutOfRangeIntervalIsClamped. Discord's own MinValue is a courtesy that stops a client
// sending an absurd number; an interaction payload is still user input at a trust boundary, and a
// stored interval of one minute is the two-minute default that PEREGRINE_WORDGAME_INTERVAL's
// minimum exists to rule out.
func TestAnOutOfRangeIntervalIsClamped(t *testing.T) {
	s, _, _, _ := fixture(t, enabled())

	s.handleConfig(configInteraction("c1", intOpt(optInterval, 1)))

	if got := s.snapshot(testGuild).Interval; got != minInterval {
		t.Errorf("interval = %s, want it clamped to %s", got, minInterval)
	}
}

// TestIntervalModeWaitsOutItsPeriod. The interval poster rides the sweep rather than owning a
// core.Loop, because a Loop's period is fixed when it starts and this one is editable. What that
// buys has to be paid for by the elapsed check actually working: without it, every sweep tick
// would start a puzzle, which at the default tick is one every five seconds.
func TestIntervalModeWaitsOutItsPeriod(t *testing.T) {
	s, guard, _, tracker := fixture(t, enabled())
	tracker.Note(testGuild, "c1", snowflake(2))

	s.handleConfig(configInteraction("c1",
		strOpt(optMode, string(ModeInterval)), intOpt(optInterval, 20)))

	// Nothing yet: the clock starts when the mode does, so the first puzzle is one period away.
	setInterval(t, s, time.Now())
	s.maybeInterval()
	if got := guard.puzzles(); len(got) != 0 {
		t.Fatalf("interval mode posted before its period elapsed: %v", got)
	}

	setInterval(t, s, time.Now().Add(-21*time.Minute))
	s.maybeInterval()
	onePuzzle(t, guard)
}

// TestActivityModeDoesNotPostOnTheInterval, which is the switch working in the other direction.
// The sweep calls maybeInterval on every tick regardless of mode, so the mode check is the only
// thing standing between an activity-mode server and a puzzle every sweep.
func TestActivityModeDoesNotPostOnTheInterval(t *testing.T) {
	s, guard, _, tracker := fixture(t, enabled())
	tracker.Note(testGuild, "c1", snowflake(2))

	setInterval(t, s, time.Now().Add(-24*time.Hour))
	s.maybeInterval()

	if got := guard.puzzles(); len(got) != 0 {
		t.Errorf("activity mode posted on the interval: %v", got)
	}
}

// TestTheConfigCommandWithNoOptionsReports. A settings command whose only mode is "change
// something" leaves an operator with no way to ask what the bot is currently doing, since the
// startup log line answers that once and then scrolls away.
func TestTheConfigCommandWithNoOptionsReports(t *testing.T) {
	opts := enabled()
	opts.Mode = ModeInterval
	s, guard, _, _ := fixture(t, opts)

	s.handleConfig(configInteraction("c1"))

	replies := guard.responded()
	if len(replies) != 1 {
		t.Fatalf("responses = %v, want exactly one", replies)
	}
	if !strings.Contains(replies[0].content, "interval mode") {
		t.Errorf("the report did not name the mode: %q", replies[0].content)
	}
}

// TestResetRestoresTheEnvironmentValues. The stored blob shadows three environment variables from
// the first change onwards, which is CLAUDE.md's "knob wired to nothing" trap pointing the other
// way: an operator editing .env would watch nothing happen. Reset is the way back.
func TestResetRestoresTheEnvironmentValues(t *testing.T) {
	opts := enabled()
	opts.AllowChannels = []string{"c1"}
	s, _, _, _ := fixture(t, opts)

	s.handleConfig(configInteraction("c2", strOpt(optChannel, channelBind)))
	s.handleConfig(configInteraction("c2", boolOpt(optReset, true)))

	if s.allowed(testGuild, "c2") {
		t.Error("reset did not undo the binding")
	}
	if !s.allowed(testGuild, "c1") {
		t.Error("reset did not restore the environment's allowlist")
	}
}

// TestAnUnknownModeIsRefusedRatherThanStored. Discord offers two choices, and a payload is still
// input: a mode that is neither would stop puzzles starting at all, with nothing in the settings
// to say why.
func TestAnUnknownModeIsRefusedRatherThanStored(t *testing.T) {
	s, _, _, _ := fixture(t, enabled())

	s.handleConfig(configInteraction("c1", strOpt(optMode, "whenever")))

	if got := s.snapshot(testGuild).Mode; got != ModeActivity {
		t.Errorf("mode = %q, want the configured %q left alone", got, ModeActivity)
	}
}

// setInterval winds one guild's interval clock, which lives on its per-guild state now.
func setInterval(t *testing.T, s *Service, at time.Time) {
	t.Helper()

	st, err := s.state(testGuild)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	s.mu.Lock()
	st.lastInterval = at
	s.mu.Unlock()
}

// TestABindInOneGuildLeavesOthersAlone, M31b, and it is the bug a live multi-guild bot hit.
//
// Settings are per guild, but a guild that has never run /wordgame-config is seeded from
// PEREGRINE_WORDGAME_CHANNELS, which is ONE flat list of channel IDs written when the bot was in
// one server. A straight membership test refuses every channel in every other server, so binding
// word games to a channel in one guild silently turned them off in all the others.
//
// The rule that replaces it: a list is a statement about the guilds it NAMES. It restricts those
// and says nothing about the rest, which is the reading an empty list has always had.
func TestABindInOneGuildLeavesOthersAlone(t *testing.T) {
	opts := enabled()
	opts.AllowChannels = []string{"c1"} // a channel in testGuild, and only there
	s, _, _, _ := fixture(t, opts)

	if !s.allowed(testGuild, "c1") {
		t.Error("the bound channel is not allowed in its own guild")
	}
	if s.allowed(testGuild, "c2") {
		t.Error("an unbound channel in the SAME guild is allowed, so the bind restricts nothing")
	}
	if !s.allowed(otherGuild, "other") {
		t.Error("a channel in another guild was refused because of a bind in this one, which is " +
			"the bug: one server's bind disabled word games everywhere else")
	}
}

// TestAGuildsOwnBindStillRestrictsThatGuild, which is the other half: the per-guild reading must
// not become "every list is advisory". A guild that binds a channel of its own is restricted to
// it, and that is exactly what the operator asked for.
func TestAGuildsOwnBindStillRestrictsThatGuild(t *testing.T) {
	s, _, _, _ := fixture(t, enabled())

	s.handleConfig(configInteraction("c1", strOpt(optChannel, channelBind)))

	if !s.allowed(testGuild, "c1") {
		t.Error("the bound channel is not allowed")
	}
	if s.allowed(testGuild, "c2") {
		t.Error("binding one channel did not restrict the others in the same guild")
	}
	if !s.allowed(otherGuild, "other") {
		t.Error("binding a channel in this guild restricted another guild")
	}
}

// TestEachGameHasItsOwnChannels is M36: binding the wheel says nothing about the scramble and
// the reverse, and both lists take more than one channel.
func TestEachGameHasItsOwnChannels(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())

	s.handleConfig(configInteraction("w1", strOpt(optChannel, channelBind), strOpt(optGame, gameWheel)))
	s.handleConfig(configInteraction("w2", strOpt(optChannel, channelBind), strOpt(optGame, gameWheel)))
	s.handleConfig(configInteraction("c1", strOpt(optChannel, channelBind)))

	if !s.wheelAllowed(testGuild, "w1") || !s.wheelAllowed(testGuild, "w2") {
		t.Errorf("the wheel is refused in a channel it was bound to: %v", guard.responded())
	}
	if s.wheelAllowed(testGuild, "c1") {
		t.Error("binding the scramble also allowed the wheel there")
	}
	if !s.allowed(testGuild, "c1") || s.allowed(testGuild, "w1") {
		t.Error("binding the wheel changed where the scramble may run")
	}

	s.handleConfig(configInteraction("w1", strOpt(optChannel, channelAnywhere), strOpt(optGame, gameWheel)))
	if !s.wheelAllowed(testGuild, "c9") || s.allowed(testGuild, "c9") {
		t.Error("allow anywhere for the wheel did not stay on the wheel")
	}

	// And an explicit "anywhere" survives a restart rather than being read as a pre-M36 blob
	// and inheriting the scramble's list.
	restarted := New(s.corpora, guard, s.manager, nil, s.counter, s.resolver, nil, enabled())
	if !restarted.wheelAllowed(testGuild, "c9") {
		t.Errorf("the wheel's anywhere did not survive a restart: %+v", restarted.snapshot(testGuild))
	}
}

// TestAPreWheelBlobKeepsTheWheelWhereItWas. Until M36 the wheel ran where the scramble did, so
// a stored blob with no wheel list must not loosen the wheel to anywhere on upgrade.
func TestAPreWheelBlobKeepsTheWheelWhereItWas(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())
	store, err := s.corpora.For(testGuild)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(w *storage.Writer) error {
		return w.PutBlob(storage.BlobConfig, settingsKey,
			[]byte(`{"channels":["c1"],"mode":"activity","interval":600000000000}`))
	}); err != nil {
		t.Fatal(err)
	}

	restarted := New(s.corpora, guard, s.manager, nil, s.counter, s.resolver, nil, enabled())
	if !restarted.wheelAllowed(testGuild, "c1") || restarted.wheelAllowed(testGuild, "c2") {
		t.Errorf("an old blob's wheel list = %v, want the scramble's [c1]",
			restarted.snapshot(testGuild).WheelChannels)
	}
}

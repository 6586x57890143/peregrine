package games

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// M26: the first application command this bot has ever registered.
//
// The tests worth having here are about the two things a slash command changes rather than about
// discordgo's plumbing: WHO the answer goes to, and whether there is one at all.

// interaction builds an application-command interaction from a guild member with the given
// permissions.
func interaction(userID string, perms int64, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.Interaction {
	return &discordgo.Interaction{
		Type:      discordgo.InteractionApplicationCommand,
		ChannelID: "c1",
		GuildID:   "g1",
		Member: &discordgo.Member{
			User:        &discordgo.User{ID: userID},
			Permissions: perms,
		},
		Data: discordgo.ApplicationCommandInteractionData{
			Name:    commandName,
			Options: opts,
		},
	}
}

func wordOpt(v string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{
		Name: optWord, Type: discordgo.ApplicationCommandOptionString, Value: v,
	}
}

func countOpt(n int) *discordgo.ApplicationCommandInteractionDataOption {
	// A JSON number arrives as a float64, which is what IntValue converts. Building it as one
	// here rather than as an int is the difference between testing the parse and testing a
	// value the gateway would never produce.
	return &discordgo.ApplicationCommandInteractionDataOption{
		Name: optCount, Type: discordgo.ApplicationCommandOptionInteger, Value: float64(n),
	}
}

// TestARefusedSlashCommandAnswersPrivately.
//
// This is the entire reason for the slash command, and it resolves a dilemma M21b could only
// pick a side of. Answering a non-admin in the CHANNEL advertises that the command exists and
// that they are not allowed to use it, so the bang command refuses in silence; the cost is that
// a legitimate operator whose command did nothing has to read the log. The case that actually
// bit is theirs: with PEREGRINE_BOOTSTRAP_ADMIN_USER_ID unset, Authorized fails closed and
// refuses the person who deployed the bot.
//
// An ephemeral reply says no to the person who asked and to nobody else, which is the answer
// that dilemma never had.
func TestARefusedSlashCommandAnswersPrivately(t *testing.T) {
	s, guard, _, _ := fixtureGauntlet(t)

	s.handleInteraction(interaction(snowflake(999), 0))

	replies := guard.responded()
	if len(replies) != 1 {
		t.Fatalf("responses = %v, want exactly one: an interaction that is never answered "+
			"shows the caller a red \"did not respond\" after three seconds", replies)
	}
	if !replies[0].ephemeral {
		t.Error("the refusal was public, which tells the whole channel that the command exists " +
			"and that this person may not use it")
	}
	if posts := guard.posts(); len(posts) != 0 {
		t.Errorf("a refused command still posted to the channel: %v", posts)
	}
}

// TestAnAdministratorCanStartAPuzzleWithNoBootstrapAdmin, which is M25's widening reaching the
// surface that most needed it: permissions arrive already computed in the payload, so this path
// makes no REST call and consults no cache.
func TestAnAdministratorCanStartAPuzzleWithNoBootstrapAdmin(t *testing.T) {
	opts := enabled()
	opts.AdminUserID = ""
	s, guard, _, _ := fixtureDictOpts(t, opts)

	s.handleInteraction(interaction(snowflake(500), discordgo.PermissionAdministrator))

	onePuzzle(t, guard)
	replies := guard.responded()
	if len(replies) != 1 || !replies[0].ephemeral {
		t.Errorf("responses = %v, want one ephemeral acknowledgement", replies)
	}
}

// TestThePuzzleIsPublicAndTheAcknowledgementIsNot.
//
// The split is the point of doing this as a slash command at all: the thing everybody is meant
// to see goes to the channel, and the thing only the operator needs goes to the operator.
func TestThePuzzleIsPublicAndTheAcknowledgementIsNot(t *testing.T) {
	s, guard, _, _ := fixtureGauntlet(t)

	s.handleInteraction(interaction(snowflake(1), 0)) // the bootstrap admin

	puzzle := onePuzzle(t, guard)

	replies := guard.responded()
	if len(replies) != 1 {
		t.Fatalf("responses = %v, want one", replies)
	}
	if !replies[0].ephemeral {
		t.Error("the acknowledgement was public, so the command announces itself twice")
	}
	if strings.Contains(replies[0].content, puzzle) {
		t.Error("the puzzle was sent as the ephemeral answer, so only the caller can play it")
	}
}

// TestEveryExitAnswersTheCaller.
//
// The slash-command shape of finding 32, inverted. The bot staying quiet in a CHANNEL is a
// design decision; an interaction that is never responded to is Discord showing the caller a
// failure, and the caller is the one person who was definitely paying attention.
func TestEveryExitAnswersTheCaller(t *testing.T) {
	t.Run("word games off", func(t *testing.T) {
		opts := enabled()
		opts.Enabled = false
		s, guard, _, _ := fixtureDictOpts(t, opts)

		s.handleInteraction(interaction(snowflake(1), 0))
		if got := guard.responded(); len(got) != 1 {
			t.Errorf("responses = %v, want one saying the feature is off", got)
		}
	})

	t.Run("a game is already running", func(t *testing.T) {
		s, guard, _, _ := fixtureGauntlet(t)
		s.handleInteraction(interaction(snowflake(1), 0))
		s.handleInteraction(interaction(snowflake(1), 0))

		replies := guard.responded()
		if len(replies) != 2 {
			t.Fatalf("responses = %v, want one per invocation", replies)
		}
		if !strings.Contains(strings.ToLower(replies[1].content), "already running") {
			t.Errorf("the second answer does not say why nothing happened: %q", replies[1].content)
		}
	})

	t.Run("an unusable planted word", func(t *testing.T) {
		s, guard, _, _ := fixtureGauntlet(t)
		s.handleInteraction(interaction(snowflake(1), 0, wordOpt("aa")))

		replies := guard.responded()
		if len(replies) != 1 {
			t.Fatalf("responses = %v, want one", replies)
		}
		// Names the rules rather than saying no, matching the bang command: the operator typed
		// a word and the interesting information is which rule it broke.
		if !strings.Contains(replies[0].content, "letters") {
			t.Errorf("the refusal does not say what would have been accepted: %q", replies[0].content)
		}
	})

	t.Run("the guard refuses the puzzle", func(t *testing.T) {
		s, guard, manager, _ := fixtureGauntlet(t)
		guard.refuse = true
		s.handleInteraction(interaction(snowflake(1), 0))
		guard.refuse = false

		// Nothing was sent, including the answer, because the guard refused that too. What
		// matters is that the game did not survive as an invisible puzzle blocking the channel.
		if manager.Active() != 0 {
			t.Error("a puzzle whose announcement was refused is still live, so the channel is " +
				"blocked until it times out against something nobody ever saw")
		}
	})
}

// TestACountRunsAGauntletAndAWordPlantsOne. The two options carry the same meanings the bang
// command's single argument does, so the slash form is not a different shape of one feature.
func TestACountRunsAGauntletAndAWordPlantsOne(t *testing.T) {
	t.Run("count", func(t *testing.T) {
		s, guard, _, _ := fixtureGauntlet(t)
		s.handleInteraction(interaction(snowflake(1), 0, countOpt(3)))

		posts := guard.posts()
		if len(posts) != 1 || !strings.Contains(posts[0], "round 1/3") {
			t.Fatalf("posts = %v, want round one of a run of three", posts)
		}
	})

	t.Run("word", func(t *testing.T) {
		s, guard, _, _ := fixtureGauntlet(t)
		// theWord is the fixture's only dictionary entry, so planting it is indistinguishable
		// from drawing it. "banana" is not in that dictionary, which is what makes this test
		// prove the plant went through.
		s.handleInteraction(interaction(snowflake(1), 0, wordOpt("banana")))

		posts := guard.posts()
		if len(posts) != 1 {
			t.Fatalf("posts = %v, want the puzzle", posts)
		}
		if strings.Contains(posts[0], "banana") {
			t.Error("the puzzle printed its own answer")
		}
		if !strings.Contains(posts[0], "6 letters") {
			t.Errorf("the card is not the planted six-letter word:\n%s", posts[0])
		}
	})

	// Both at once has an obvious reading, and refusing it would be an error message for a
	// request nobody would have to think about.
	t.Run("both prefers the count", func(t *testing.T) {
		s, guard, _, _ := fixtureGauntlet(t)
		s.handleInteraction(interaction(snowflake(1), 0, wordOpt("banana"), countOpt(2)))

		posts := guard.posts()
		if len(posts) != 1 || !strings.Contains(posts[0], "round 1/2") {
			t.Fatalf("posts = %v, want a run rather than a planted word", posts)
		}
	})
}

// TestInteractionRequesterFailsClosed. A DM has no Member and therefore no permissions, which is
// right rather than merely safe: there is no channel for a puzzle to be posted in.
func TestInteractionRequesterFailsClosed(t *testing.T) {
	if got := interactionRequester(nil); got.Permissions != 0 || got.UserID != "" {
		t.Errorf("a nil interaction produced %+v, want the zero value", got)
	}

	dm := &discordgo.Interaction{ChannelID: "d1", User: &discordgo.User{ID: snowflake(7)}}
	got := interactionRequester(dm)
	if got.UserID != snowflake(7) {
		t.Errorf("UserID = %q, want the DM's user", got.UserID)
	}
	if got.Permissions != 0 {
		t.Errorf("a DM carried permissions %d; there are no administrators in a DM", got.Permissions)
	}
}

// TestOnlyOurOwnCommandIsHandled. Anything else on the gateway is another application's event or
// a shape this build does not know, and answering it would be responding to an interaction the
// bot has no token for.
func TestOnlyOurOwnCommandIsHandled(t *testing.T) {
	s, guard, _, _ := fixtureGauntlet(t)

	other := interaction(snowflake(1), 0)
	other.Data = discordgo.ApplicationCommandInteractionData{Name: "somebodyelses"}
	s.onInteraction(nil, &discordgo.InteractionCreate{Interaction: other})

	component := interaction(snowflake(1), 0)
	component.Type = discordgo.InteractionMessageComponent
	s.onInteraction(nil, &discordgo.InteractionCreate{Interaction: component})

	s.onInteraction(nil, &discordgo.InteractionCreate{})
	s.onInteraction(nil, nil)

	if got := guard.responded(); len(got) != 0 {
		t.Errorf("responded to an interaction that was not ours: %v", got)
	}
	if got := guard.posts(); len(got) != 0 {
		t.Errorf("posted for an interaction that was not ours: %v", got)
	}
}

// TestTheRegisteredCommandMatchesWhatTheHandlerReads.
//
// A definition and a handler that disagree about an option name produce a command that runs and
// silently ignores its argument, which is the worst outcome available here: it looks like it
// worked. Nothing else checks that these two agree.
func TestTheRegisteredCommandMatchesWhatTheHandlerReads(t *testing.T) {
	// Every command onInteraction dispatches, against every option its handler reads. Keyed by
	// name rather than by index so adding a command is a row here and not a rewrite.
	want := map[string][]string{
		commandName:       {optWord, optCount},
		configCommandName: {optChannel, optMode, optInterval, optReset},
		boardCommandName:  {optScope},
	}

	defs := definitions(true)
	if len(defs) != len(want) {
		t.Fatalf("definitions() returned %d commands, want %d", len(defs), len(want))
	}
	for _, def := range defs {
		options, ok := want[def.Name]
		if !ok {
			t.Errorf("registered %q, which onInteraction does not dispatch, so it appears in "+
				"every client and answers nothing", def.Name)
			continue
		}
		names := map[string]bool{}
		for _, o := range def.Options {
			names[o.Name] = true
		}
		for _, opt := range options {
			if !names[opt] {
				t.Errorf("/%s reads option %q, which is not registered, so it can never "+
					"arrive and the command silently ignores it", def.Name, opt)
			}
		}
		if def.Description == "" {
			t.Errorf("/%s has no description; Discord refuses a command without one", def.Name)
		}
		delete(want, def.Name)
	}
	for name := range want {
		t.Errorf("onInteraction dispatches /%s, which is not registered, so nobody can run it",
			name)
	}

	// With word games off, the two commands that ARE the feature go away and the leaderboard
	// stays. !leaderboard has never been gated on the flag, because its chat half reads the
	// stats bucket, which is populated on every message.
	off := definitions(false)
	if len(off) != 1 || off[0].Name != boardCommandName {
		t.Errorf("with word games off the registered set is %v, want just /%s",
			off, boardCommandName)
	}
}

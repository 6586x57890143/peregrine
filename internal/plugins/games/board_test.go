package games

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// M32: the paginated local and global board.
//
// The assertions here are about the two things paging changes: WHICH rows a reader sees, and
// WHOSE board they are. Everything else about the leaderboard is already pinned in games_test.

// boardInteraction builds a /leaderboard invocation.
func boardInteraction(userID, guildID string, sc scope) *discordgo.Interaction {
	i := &discordgo.Interaction{
		Type:      discordgo.InteractionApplicationCommand,
		ChannelID: "c1",
		GuildID:   guildID,
		Member:    &discordgo.Member{User: &discordgo.User{ID: userID}},
		Data: discordgo.ApplicationCommandInteractionData{
			Name: boardCommandName,
			Options: []*discordgo.ApplicationCommandInteractionDataOption{{
				Name: optScope, Type: discordgo.ApplicationCommandOptionString, Value: string(sc),
			}},
		},
	}
	return i
}

// press builds a button press on a board.
func press(userID, guildID, customID string) *discordgo.Interaction {
	return &discordgo.Interaction{
		Type:      discordgo.InteractionMessageComponent,
		ChannelID: "c1",
		GuildID:   guildID,
		Member:    &discordgo.Member{User: &discordgo.User{ID: userID}},
		Data:      discordgo.MessageComponentInteractionData{CustomID: customID},
	}
}

// fillBoard gives n players descending scores in one guild.
func fillBoard(t *testing.T, s *Service, guildID string, n int) {
	t.Helper()
	board := s.board(guildID)
	if board == nil {
		t.Fatalf("no board for guild %s", guildID)
	}
	for i := range n {
		id := snowflake(4000 + i)
		for range n - i {
			board.AddWin(id, "player", time.Second, 1)
		}
	}
}

// TestALocalBoardCountsOnlyThisServer, which is the whole reason local and global are separate
// answers: before M31 there was one blended board and the question could not be asked.
func TestALocalBoardCountsOnlyThisServer(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())
	s.members = &countingMembers{seen: map[string]int{}}

	here := snowflake(700)
	elsewhere := snowflake(800)
	s.board(testGuild).AddWin(here, "here", time.Second, 5)
	s.board(otherGuild).AddWin(elsewhere, "elsewhere", time.Second, 5)

	s.handleLeaderboard(boardInteraction(here, testGuild, scopeLocal))

	embeds := guard.posted()
	if len(embeds) != 1 {
		t.Fatalf("got %d boards, want 1", len(embeds))
	}
	value := embeds[0].Fields[0].Value
	if !strings.Contains(value, "user-"+here) {
		t.Errorf("this server's own player is missing from its board:\n%s", value)
	}
	if strings.Contains(value, "user-"+elsewhere) {
		t.Errorf("a player from ANOTHER server is on this server's board:\n%s", value)
	}
}

// TestAGlobalBoardSumsEveryServer, and says how many it summed.
//
// What crosses the guild boundary is a user ID and an integer, never a word anybody typed,
// which is why this does not undo M31.
func TestAGlobalBoardSumsEveryServer(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())
	s.members = &countingMembers{seen: map[string]int{}}

	here := snowflake(700)
	elsewhere := snowflake(800)
	s.board(testGuild).AddWin(here, "here", time.Second, 5)
	s.board(otherGuild).AddWin(elsewhere, "elsewhere", time.Second, 9)

	s.handleLeaderboard(boardInteraction(here, testGuild, scopeGlobal))

	embeds := guard.posted()
	if len(embeds) != 1 {
		t.Fatalf("got %d boards, want 1", len(embeds))
	}
	value := embeds[0].Fields[0].Value
	for _, id := range []string{here, elsewhere} {
		if !strings.Contains(value, "user-"+id) {
			t.Errorf("a player from one of the two servers is missing:\n%s", value)
		}
	}
	// The count is in the title, because "everybody" with no size attached is a claim a reader
	// cannot check.
	if !strings.Contains(embeds[0].Title, "every server") {
		t.Errorf("the global board does not say it is global: %q", embeds[0].Title)
	}
}

// TestTheBoardIsPublicAndItsFailuresAreNot.
//
// The opposite split from every other interaction here, and deliberate: a leaderboard is the one
// thing this package produces that the whole channel wants to see. A DM has no board at all, and
// that answer is private because there is nobody else to tell.
func TestTheBoardIsPublicAndItsFailuresAreNot(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())
	s.members = &countingMembers{seen: map[string]int{}}
	s.board(testGuild).AddWin(snowflake(700), "here", time.Second, 5)

	s.handleLeaderboard(boardInteraction(snowflake(700), testGuild, scopeLocal))
	replies := guard.responded()
	if len(replies) != 1 {
		t.Fatalf("responses = %v, want one: an interaction that never answers shows the caller "+
			"a red failure after three seconds", replies)
	}
	if replies[0].ephemeral {
		t.Error("the board was ephemeral, so only the person who asked can see the scores")
	}

	dm := boardInteraction(snowflake(700), "", scopeLocal)
	s.handleLeaderboard(dm)
	replies = guard.responded()
	if len(replies) != 2 || !replies[1].ephemeral {
		t.Errorf("a DM was answered with %v, want one ephemeral refusal", replies)
	}
}

// TestAPressPagesTheBoardInPlace.
//
// Two things at once, and both are the point of a component response: the reader gets page two,
// and they get it by REPLACEMENT rather than as a second board posted under the first.
func TestAPressPagesTheBoardInPlace(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())
	s.members = &countingMembers{seen: map[string]int{}}
	fillBoard(t, s, testGuild, 25)

	s.handleBoardButton(press(snowflake(700), testGuild, buttonID(scopeLocal, 2)))

	if guard.updates != 1 {
		t.Fatalf("component responses = %d, want 1: a press must replace the board rather "+
			"than post another one", guard.updates)
	}
	embeds := guard.posted()
	if len(embeds) != 1 {
		t.Fatalf("got %d boards, want 1", len(embeds))
	}
	value := embeds[0].Fields[0].Value
	if !strings.Contains(value, "`11`") {
		t.Errorf("page two does not start at rank 11:\n%s", value)
	}
	if strings.Contains(value, "🥇") {
		t.Errorf("page two still carries the first place medal, so it is page one:\n%s", value)
	}
	if !strings.Contains(embeds[0].Description, "page 2/3") {
		t.Errorf("the card does not say which page it is: %q", embeds[0].Description)
	}
}

// TestAPressRendersForWhoeverPressed, not for whoever ran the command.
//
// The board is public, so anybody can page it, and the eleventh slot is the one row that is
// about the person looking. Restricting the buttons to the original caller would be more code
// and a worse answer.
func TestAPressRendersForWhoeverPressed(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())
	s.members = &countingMembers{seen: map[string]int{}}
	fillBoard(t, s, testGuild, 25)

	// snowflake(4020) is 21st, so they are off page one and their own row rides under the
	// divider. Nobody ran a command here at all: the press is the whole interaction.
	presser := snowflake(4020)
	s.handleBoardButton(press(presser, testGuild, buttonID(scopeLocal, 1)))

	embeds := guard.posted()
	if len(embeds) != 1 {
		t.Fatalf("got %d boards, want 1", len(embeds))
	}
	if !strings.Contains(embeds[0].Fields[0].Value, "user-"+presser) {
		t.Errorf("the eleventh slot is not the presser's row:\n%s", embeds[0].Fields[0].Value)
	}
}

// TestButtonsAppearOnlyWhenThereIsSomewhereToGo.
//
// Two dead buttons under a board that already fits on one page are furniture. On a board that
// does not fit, prev is disabled at the start rather than absent, so the row does not change
// width as somebody pages through it.
func TestButtonsAppearOnlyWhenThereIsSomewhereToGo(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())
	s.members = &countingMembers{seen: map[string]int{}}

	s.board(testGuild).AddWin(snowflake(700), "here", time.Second, 5)
	s.postLeaderboard(testGuild, "c1", snowflake(700))
	if row := guard.lastComponents(); len(row) != 0 {
		t.Errorf("a one-page board carried buttons: %v", row)
	}

	fillBoard(t, s, testGuild, 25)
	s.postLeaderboard(testGuild, "c1", snowflake(700))

	row := guard.lastComponents()
	if len(row) != 1 {
		t.Fatalf("a three-page board carried %d component rows, want 1", len(row))
	}
	actions, ok := row[0].(discordgo.ActionsRow)
	if !ok || len(actions.Components) != 2 {
		t.Fatalf("the row is not two buttons: %#v", row[0])
	}
	prev, _ := actions.Components[0].(discordgo.Button)
	next, _ := actions.Components[1].(discordgo.Button)
	if !prev.Disabled {
		t.Error("prev is live on page one, so a press would ask for page zero")
	}
	if next.Disabled {
		t.Error("next is dead on page one of three, so the board cannot be paged at all")
	}
	if next.CustomID != buttonID(scopeLocal, 2) {
		t.Errorf("next points at %q, want page two of the local board", next.CustomID)
	}
}

// TestAPressCarriesItsOwnStateAndNothingElse.
//
// The scope and the page ride in the custom_id, so there is no map to leak and a restart still
// answers a press. The GUILD deliberately does not: a component interaction already carries the
// guild it was pressed in, and a second copy that could disagree with it would be a way to
// render one server's board into another, which is exactly the leak M31 exists to prevent.
func TestAPressCarriesItsOwnStateAndNothingElse(t *testing.T) {
	sc, page, ok := parseButtonID(buttonID(scopeGlobal, 4))
	if !ok || sc != scopeGlobal || page != 4 {
		t.Errorf("round trip gave (%q, %d, %v), want (global, 4, true)", sc, page, ok)
	}
	if strings.Contains(buttonID(scopeGlobal, 4), testGuild) {
		t.Error("the custom_id carries a guild, which can disagree with the guild the press " +
			"came from")
	}

	// Somebody else's component, and a malformed one. Both are ignored rather than answered:
	// this bot has no token for another application's interaction.
	for _, id := range []string{"", "other:thing:1", "lb:local:notanumber", "lb:local"} {
		if _, _, ok := parseButtonID(id); ok {
			t.Errorf("custom_id %q was accepted as ours", id)
		}
	}
}

// TestAPressOnAnotherApplicationsComponentIsIgnored, end to end through the dispatcher's entry
// point, because the panic this guards against was in the payload decode rather than in the
// parse: discordgo's MessageComponentData type-ASSERTS, so an interaction whose type and data
// disagree took the goroutine down.
func TestAPressOnAnotherApplicationsComponentIsIgnored(t *testing.T) {
	s, guard, _, _ := fixture(t, enabled())

	mismatched := press(snowflake(700), testGuild, "lb:local:2")
	mismatched.Data = discordgo.ApplicationCommandInteractionData{Name: commandName}
	s.onInteraction(nil, &discordgo.InteractionCreate{Interaction: mismatched})

	s.onInteraction(nil, &discordgo.InteractionCreate{
		Interaction: press(snowflake(700), testGuild, "somebodyelse:1"),
	})

	if got := guard.responded(); len(got) != 0 {
		t.Errorf("answered a component that was not ours: %v", got)
	}
	if guard.updates != 0 {
		t.Errorf("updated a message for a component that was not ours")
	}
}

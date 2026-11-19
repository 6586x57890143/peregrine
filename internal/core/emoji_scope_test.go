package core

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

// A :shortcode: resolves against the guild being answered. Across every guild, a member
// who owns another server the bot is in could have :name: come out as their emote here.
func TestEmojiResolvesInItsOwnGuildOnly(t *testing.T) {
	s := &discordgo.Session{State: discordgo.NewState()}
	s.State.Guilds = []*discordgo.Guild{
		{ID: "home", Emojis: []*discordgo.Emoji{{ID: "1", Name: "ours"}}},
		{ID: "theirs", Emojis: []*discordgo.Emoji{{ID: "2", Name: "pic"}}},
	}
	home := SessionEmoji(s).(sessionEmoji).InGuild("home")
	if _, ok := home.ResolveEmoji("pic"); ok {
		t.Error("an emote from another guild resolved")
	}
	if got, ok := home.ResolveEmoji("ours"); !ok || got != "<:ours:1>" {
		t.Errorf("own emote = %q, %v", got, ok)
	}
}

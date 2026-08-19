package names

import "github.com/bwmarrin/discordgo"

// Display resolves a user ID to the best available name: guild nickname, then global name,
// then username, then the raw ID.
//
// Three sources, cheapest first, and the order is the point.
//
//  1. The gateway state cache, which costs NOTHING. discordgo already holds members it has
//     seen, and a leaderboard is asking about people who have just been talking, so this
//     answers most of it.
//  2. members, which in production is NewCachedSession: bounded, with a TTL, and shared with
//     the ingest and mention paths. discordgo's GuildMember is an unconditional REST GET with
//     no state-cache check, which is why that wrapper exists at all (M18).
//  3. A plain User lookup for somebody who has left the guild, which is the case a GLOBAL
//     leaderboard makes ordinary rather than rare: it ranks people the viewer's guild has
//     never had as members.
//
// The ID fallback is deliberate rather than an error path. A leaderboard that omits whoever has
// left the server silently loses entries, and one that fails entirely because of a single
// departed member is worse than one showing a number for them.
//
// It lives here rather than in a plugin because two of them now ask the question: chat's
// !leaderboard and games' /leaderboard. Two copies of one question, differing only in what each
// throws away, is the shape Spellings above exists to have collapsed once already.
func Display(s *discordgo.Session, members Session, guildID, userID string) string {
	if s != nil && s.State != nil && guildID != "" {
		if member, err := s.State.Member(guildID, userID); err == nil && member != nil {
			if name := Primary(member.User, member).Name; name != "" {
				return name
			}
		}
	}
	if members != nil && guildID != "" {
		if member, err := members.GuildMember(guildID, userID); err == nil && member != nil {
			if name := Primary(member.User, member).Name; name != "" {
				return name
			}
		}
	}
	if s != nil {
		if user, err := s.User(userID); err == nil && user != nil {
			if name := Primary(user, nil).Name; name != "" {
				return name
			}
		}
	}
	return userID
}

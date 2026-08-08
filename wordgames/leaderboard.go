package wordgames

import (
	"fmt"
	"sort"
	"strings"
)

// truncateRunes shortens s to at most max runes, appending "..." when it had to
// cut. It counts runes rather than bytes because both leaderboards render
// Discord nicknames into a fixed-width column, and byte slicing splits
// multi-byte runes: an emoji or accented character landing on the boundary
// produced invalid UTF-8 and a visible replacement character.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 3 {
		return string(r[:max])
	}
	return string(r[:max-3]) + "..."
}

// FormatChatLeaderboard formats the chat leaderboard into a string.
func FormatChatLeaderboard(scores map[string]int) string {
	if len(scores) == 0 {
		return "The chat leaderboard is empty."
	}

	type chatLeaderboardEntry struct {
		Username string
		Messages int
	}

	entries := make([]chatLeaderboardEntry, 0, len(scores))
	for username, count := range scores {
		entries = append(entries, chatLeaderboardEntry{Username: username, Messages: count})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Messages > entries[j].Messages
	})

	var builder strings.Builder
	builder.WriteString("💬 **Top 10 Chatters** 💬\n")
	builder.WriteString("```\n")
	builder.WriteString("Rank | Player          | Messages\n")
	builder.WriteString("-----+-----------------+----------\n")

	crowns := []string{"🥇", "🥈", "🥉"}

	for i, entry := range entries {
		var rankStr string
		if i < len(crowns) {
			rankStr = crowns[i] + "  "
		} else {
			rankStr = fmt.Sprintf("%-4s", fmt.Sprintf("%d.", i+1))
		}

		// Truncate by runes, not bytes. len() and s[:14] both count bytes, so
		// a nickname containing an emoji or any non-ASCII character was being
		// cut mid-rune and rendered as a replacement character in a
		// user-visible leaderboard. Discord nicknames are frequently neither
		// ASCII nor short.
		username := truncateRunes(entry.Username, 15)

		fmt.Fprintf(&builder, "%-4s | %-15s | %8d\n", rankStr, username, entry.Messages)
		if i >= 9 {
			break
		}
	}
	builder.WriteString("```")
	return builder.String()
}

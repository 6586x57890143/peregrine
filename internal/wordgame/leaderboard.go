package wordgame

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Leaderboard is the weekly win tally.
//
// Every field is unexported and the mutex is not part of the wire format, which is the fix
// for a real data race rather than a tidy-up. The old version exported all of them
// INCLUDING the mutex, on a struct that gets JSON-marshalled to be persisted, and the
// marshalling ran outside the lock while AddWin held it. Concurrent map read and write is a
// fatal runtime error in Go: no recover, no deferred database close, process gone.
//
// Persistence goes through MarshalJSON and UnmarshalJSON, so a caller cannot marshal it
// unlocked even by accident. That is the same reasoning as the storage Reader having no
// method that opens a transaction: make the mistake unexpressible rather than documented.
type Leaderboard struct {
	mu        sync.Mutex
	weekStart time.Time
	scores    map[string]Entry
}

// Entry is one player's tally.
type Entry struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Wins     int    `json:"wins"`
}

// wireLeaderboard is the persisted shape.
//
// Separate from Leaderboard so the mutex cannot be serialized and so the field names on
// disk are stable regardless of what the in-memory struct is called. lastReset is gone
// from it: the old format carried both a week start and a last-reset timestamp that had to
// agree, and the reset logic compared against the second while the display computed from
// the first. One field cannot disagree with itself.
type wireLeaderboard struct {
	WeekStart time.Time        `json:"week_start"`
	Scores    map[string]Entry `json:"scores"`

	// LastReset is read but never written, so a leaderboard persisted by the old code
	// still loads. It is deliberately not carried forward.
	LastReset time.Time `json:"last_reset,omitempty"`

	// WeekStartDate is the old field name for WeekStart, read for the same reason.
	WeekStartDate time.Time `json:"week_start_date,omitempty"`
}

// NewLeaderboard returns an empty leaderboard for the week containing now.
func NewLeaderboard(now time.Time) *Leaderboard {
	return &Leaderboard{
		weekStart: StartOfWeekUTC(now),
		scores:    map[string]Entry{},
	}
}

// StartOfWeekUTC is the Monday 00:00 UTC that the week containing t began at.
//
// Exported because the chat leaderboard in the corpus filters user stats by the same
// boundary, and the two disagreeing would mean a player's messages counted for a week the
// leaderboard was not showing.
func StartOfWeekUTC(t time.Time) time.Time {
	t = t.UTC()
	daysSinceMonday := (int(t.Weekday()) - int(time.Monday) + 7) % 7
	y, m, d := t.Date()
	return time.Date(y, m, d-daysSinceMonday, 0, 0, 0, 0, time.UTC)
}

// AddWin records a win.
func (l *Leaderboard) AddWin(userID, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	e := l.scores[userID]
	e.UserID = userID
	e.Username = username // refreshed, because people rename themselves
	e.Wins++
	l.scores[userID] = e
}

// MaybeReset clears the board if now falls in a later week than the one it holds, and
// reports whether it did.
//
// THIS IS WHERE THE NTP QUERY WENT (SPEC.md section 8, finding 17). The old check asked
// pool.ntp.org what day it was, hourly, and reset only when the answer was Monday between
// 00:00 and 00:59 UTC. Three things were wrong with that, and the third is the one that
// actually bit:
//
//   - It queried the network for something time.Now() answers. A bot whose clock is wrong
//     by an hour has a much larger problem than a leaderboard.
//   - A failed query inside that one-hour window skipped the reset for a whole WEEK, and
//     logged it as an error nobody would connect to a stale leaderboard six days later.
//   - The reset had to be observed within a specific hour, so it depended on the tick
//     landing there. A restart moved the tick's phase, and downtime across Monday morning
//     meant the reset never happened at all.
//
// Comparing week boundaries has none of those properties. It is idempotent, it needs no
// network, and it CATCHES UP: a bot that was off all Monday resets on its first tick back,
// because the week it holds is still the old one.
func (l *Leaderboard) MaybeReset(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	current := StartOfWeekUTC(now)
	if !l.weekStart.Before(current) {
		return false
	}
	l.weekStart = current
	l.scores = map[string]Entry{}
	return true
}

// WeekStart reports the Monday the current tally began.
func (l *Leaderboard) WeekStart() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.weekStart
}

// Entries returns the tally sorted by wins, highest first.
//
// Ties break on username so the order is stable: an unstable sort over a map made the
// displayed ranking shuffle between two identical calls, which reads as the bot being
// wrong about who is winning.
func (l *Leaderboard) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]Entry, 0, len(l.scores))
	for _, e := range l.scores {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Wins != out[j].Wins {
			return out[i].Wins > out[j].Wins
		}
		return out[i].Username < out[j].Username
	})
	return out
}

// MarshalJSON serializes under the lock.
func (l *Leaderboard) MarshalJSON() ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	scores := make(map[string]Entry, len(l.scores))
	for k, v := range l.scores {
		scores[k] = v
	}
	return json.Marshal(wireLeaderboard{WeekStart: l.weekStart, Scores: scores})
}

// UnmarshalJSON restores a persisted leaderboard, accepting the pre-M11 field names.
//
// The old format is read rather than rejected because the leaderboard is not derivable
// from anything else: unlike the corpus, which can be relearned from Discord history, a
// discarded week of wins is gone. That asymmetry is why storage refuses an old corpus
// outright and this does not.
func (l *Leaderboard) UnmarshalJSON(b []byte) error {
	var w wireLeaderboard
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.scores = w.Scores
	if l.scores == nil {
		l.scores = map[string]Entry{}
	}

	switch {
	case !w.WeekStart.IsZero():
		l.weekStart = StartOfWeekUTC(w.WeekStart)
	case !w.WeekStartDate.IsZero():
		l.weekStart = StartOfWeekUTC(w.WeekStartDate)
	default:
		// A leaderboard with scores and no date is from a format we do not recognise.
		// Treating it as the current week keeps the scores and simply delays the next
		// reset by up to seven days, which beats discarding a tally nobody can rebuild.
		l.weekStart = StartOfWeekUTC(time.Now())
	}
	return nil
}

// Format renders the tally for Discord.
//
// The next-reset timestamp is derived from the same week boundary the reset uses, so the
// board cannot promise a reset at a different moment from the one that happens. Those were
// two separate calculations before, one from the host clock and one from NTP.
func (l *Leaderboard) Format(now time.Time) string {
	entries := l.Entries()
	nextReset := StartOfWeekUTC(now).AddDate(0, 0, 7)

	var b strings.Builder
	b.WriteString("🏆 **Weekly Word Scramble Leaderboard** 🏆\n")
	fmt.Fprintf(&b, "Resets <t:%d:R>\n", nextReset.Unix())

	if len(entries) == 0 {
		b.WriteString("The leaderboard is empty. Be the first to win a game!")
		return b.String()
	}

	b.WriteString("```\n")
	b.WriteString("Rank | Player          | Wins\n")
	b.WriteString("-----+-----------------+------\n")

	crowns := []string{"🥇", "🥈", "🥉"}
	for i, e := range entries {
		if i >= 10 {
			break
		}
		rank := fmt.Sprintf("%-3s", fmt.Sprintf("%d.", i+1))
		if i < len(crowns) {
			// Emoji are roughly two columns wide, so one trailing space fills the
			// three-column rank field rather than the two %-3s would add.
			rank = crowns[i] + " "
		}
		fmt.Fprintf(&b, "%-3s | %-15s | %4d\n", rank, TruncateRunes(e.Username, 15), e.Wins)
	}
	b.WriteString("```")
	return b.String()
}

// FormatChatLeaderboard renders the message-count board, which is a different tally: it
// counts messages sent rather than games won, and is populated whether or not the scramble
// game runs.
func FormatChatLeaderboard(scores map[string]int) string {
	type row struct {
		name string
		n    int
	}
	rows := make([]row, 0, len(scores))
	for name, n := range scores {
		rows = append(rows, row{name: name, n: n})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].name < rows[j].name
	})

	var b strings.Builder
	b.WriteString("💬 **Weekly Chat Leaderboard** 💬\n")
	if len(rows) == 0 {
		b.WriteString("Nobody has said anything yet this week.")
		return b.String()
	}

	b.WriteString("```\n")
	b.WriteString("Rank | Player          | Msgs\n")
	b.WriteString("-----+-----------------+------\n")
	for i, r := range rows {
		if i >= 10 {
			break
		}
		fmt.Fprintf(&b, "%-3s | %-15s | %4d\n",
			fmt.Sprintf("%d.", i+1), TruncateRunes(r.name, 15), r.n)
	}
	b.WriteString("```")
	return b.String()
}

// TruncateRunes shortens s to at most max RUNES, appending three dots when it had to cut.
//
// Runes rather than bytes because both leaderboards render Discord nicknames into a
// fixed-width column, and byte slicing splits multi-byte runes: an emoji or accented
// character landing on the boundary produced invalid UTF-8 and a visible replacement glyph
// (fixed in M0).
//
// Three ASCII dots, NOT the single ellipsis character, which CI's prose check rejects
// repo-wide. That is worth a comment rather than a silent choice, because the character
// looks identical in most editors and this function is the obvious place to reach for it.
func TruncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

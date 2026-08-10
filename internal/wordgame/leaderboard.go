package wordgame

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/6586x57890143/peregrine/internal/corpus"
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
// AN ALIAS FOR corpus.StartOfWeekUTC, not a second implementation, and the difference is the
// whole point of it being here at all.
//
// This package carried its own copy until M21a: same answer, different arithmetic, in a
// package that imported nothing. corpus's own doc comment says that function "exists here
// precisely so that two consumers cannot hold two different answers" and records that it had
// three implementations before M11c, so this was a fourth that the collapse missed. The two
// happened to agree, which is exactly why nothing failed and nothing found it.
//
// It matters now rather than in principle. The leaderboard command renders the word-game
// tally and the chat tally as two halves of ONE embed, and the second filters user stats by
// corpus.StartOfWeekUTC while this decides when the first resets. Two boards under one
// heading, promising one reset time, computed from two functions is finding 28's shape with
// a user-visible failure attached: a player's messages counting for a week the board is not
// showing.
//
// Kept as an alias rather than deleted so callers of this package do not have to import
// corpus to ask a question this package answers.
func StartOfWeekUTC(t time.Time) time.Time { return corpus.StartOfWeekUTC(t) }

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

// NextReset is when the current week ends.
//
// Derived from the same boundary MaybeReset compares against, so the board cannot promise a
// reset at a different moment from the one that happens. Those were two separate calculations
// before, one from the host clock and one from NTP (finding 17).
func (l *Leaderboard) NextReset(now time.Time) time.Time {
	return StartOfWeekUTC(now).AddDate(0, 0, 7)
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

// Scores returns each player's win count by user ID.
//
// By ID rather than by name, and that is the whole shape of the M21a fix. The command used to
// build a map keyed by RESOLVED DISPLAY NAME, which cost one REST call per player before
// anything was sorted and threw the IDs away in the process. Two people with the same nickname
// merged into one row, and the viewer's own position became unaskable, because the only thing
// that identifies a viewer is their ID.
func (l *Leaderboard) Scores() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make(map[string]int, len(l.scores))
	for id, e := range l.scores {
		out[id] = e.Wins
	}
	return out
}

// Row is one line of a rendered board.
//
// Name is deliberately empty when Rank produces it: ranking is done on scores alone and the
// caller fills in names for the handful of rows that will actually be shown.
type Row struct {
	Rank   int
	UserID string
	Name   string
	Score  int
}

// Board is a ranked tally plus where the viewer stands in it.
type Board struct {
	// Top is the leading rows, at most the requested count.
	Top []Row

	// You is the viewer's own row when they have a score but are NOT in Top. Nil when they
	// are already shown above or have no score at all.
	//
	// This is the eleventh slot: ranks 1 to 10 as usual, and then the viewer's real position
	// under a divider, so somebody sitting at 18th can see it without scrolling a board that
	// does not go that far.
	You *Row

	// Unranked is true when the viewer has no score this week. Reported rather than omitted,
	// because a missing row is indistinguishable from a bug.
	Unranked bool

	// Players is how many people have any score at all, which is what makes a rank legible:
	// 18th of 20 and 18th of 300 are different news.
	Players int
}

// Rank turns raw scores into a Board.
//
// # Competition ranking, computed from scores alone
//
// A rank is one plus the number of people strictly ahead, so equal scores share a rank and the
// answer does not depend on any tie-break. That matters more than it looks: it means the
// viewer's rank can be computed WITHOUT resolving a single name, which is the entire reason
// this command stopped costing one Discord request per weekly talker.
//
// Ties are ordered by user ID for display. That is arbitrary but stable, and stability is the
// requirement: an unstable order made the ranking shuffle between two identical invocations,
// which reads as the bot being wrong about who is winning. Ordering ties by NAME would be
// prettier and would put the names back on the critical path, which is the cost this whole
// change exists to remove.
func Rank(scores map[string]int, viewerID string, top int) Board {
	if top <= 0 {
		top = 10
	}

	rows := make([]Row, 0, len(scores))
	for id, score := range scores {
		if score <= 0 {
			continue
		}
		rows = append(rows, Row{UserID: id, Score: score})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		return rows[i].UserID < rows[j].UserID
	})

	// One pass assigning competition ranks: the rank only moves when the score does.
	rank := 0
	for i := range rows {
		if i == 0 || rows[i].Score != rows[i-1].Score {
			rank = i + 1
		}
		rows[i].Rank = rank
	}

	board := Board{Players: len(rows)}
	if len(rows) > top {
		board.Top = append(board.Top, rows[:top]...)
	} else {
		board.Top = append(board.Top, rows...)
	}

	if viewerID == "" {
		return board
	}
	for i := range rows {
		if rows[i].UserID != viewerID {
			continue
		}
		if i >= len(board.Top) {
			you := rows[i]
			board.You = &you
		}
		return board
	}
	board.Unranked = true
	return board
}

// NamesNeeded is every user ID that will be rendered, which is at most eleven per board.
//
// The point of the whole rewrite: the caller resolves names for exactly this list rather than
// for every person who spoke this week. On a server with two hundred weekly talkers that is
// eleven Discord lookups instead of two hundred, and the two hundred were sequential and
// rate-limited.
func (b Board) NamesNeeded() []string {
	out := make([]string, 0, len(b.Top)+1)
	for _, r := range b.Top {
		out = append(out, r.UserID)
	}
	if b.You != nil {
		out = append(out, b.You.UserID)
	}
	return out
}

// WithNames returns the board with Name filled in from a resolver.
func (b Board) WithNames(resolve func(userID string) string) Board {
	out := b
	out.Top = make([]Row, len(b.Top))
	for i, r := range b.Top {
		r.Name = resolve(r.UserID)
		out.Top[i] = r
	}
	if b.You != nil {
		you := *b.You
		you.Name = resolve(you.UserID)
		out.You = &you
	}
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

// The two text renderers that used to live here are GONE as of M21a: Format built a fenced
// code block with a fixed-width Rank/Player/Wins table, and FormatChatLeaderboard did the same
// for message counts.
//
// They are deleted rather than kept alongside the embed, because a second renderer nobody
// calls is the shape this repo keeps finding dead (findings 34 and 37 in the seed ladder, the
// duplicate index in finding 28). Rendering lives in internal/plugins/games now, which is the
// package that may import discordgo; what this package owns is the RANKING, which needs no
// Discord at all and is what Rank and Board above are.
//
// The layout was also actively worse on the device most people read it on. A fenced code block
// does not wrap on a phone, so the table scrolled off the side, and the column alignment it
// spent effort on was invisible to whoever was scrolling.

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

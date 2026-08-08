package wordgames

import (
	"bufio"
	"embed"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// The dictionary is embedded rather than read from a path at runtime. It used
// to be loaded from the relative path "wordgames/dictionary.txt", which meant
// the bot only started when its working directory happened to be the repo
// root, and it called log.Fatalf on failure, so a missing 64 KB word list took
// down the whole bot including every feature unrelated to word games. In a
// container it is worse than fragile: the distroless image ships only the
// binary, so that path can never exist and the bot could not start at all.
// Embedding removes the failure mode instead of handling it.
//
//go:embed dictionary.txt
var embeddedDictionary embed.FS


// ScrambleGame holds the state of a single word scramble puzzle.
type ScrambleGame struct {
	OriginalWord  string
	ScrambledWord string
	StartTime     time.Time
	MessageID     string // The ID of the message the bot sent
}

// LeaderboardEntry tracks a user's wins for the week.
type LeaderboardEntry struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Wins     int    `json:"wins"`
}

// Leaderboard holds the weekly scores.
type Leaderboard struct {
	WeekStartDate time.Time                   `json:"week_start_date"`
	LastReset     time.Time                   `json:"last_reset"`
	Scores        map[string]LeaderboardEntry `json:"scores"`
	Mutex         sync.Mutex                  `json:"-"`
}

var wordList []string
var gameRand *rand.Rand

func init() {
	gameRand = rand.New(rand.NewSource(time.Now().UnixNano()))
}

// LoadDictionary loads the word list. With an empty path it reads the embedded
// dictionary, which is the normal case and cannot fail at runtime. A non-empty
// path overrides it with an operator-supplied list, so a custom word list stays
// possible without a rebuild.
func LoadDictionary(path string) error {
	var (
		file io.ReadCloser
		err  error
	)
	if path == "" {
		file, err = embeddedDictionary.Open("dictionary.txt")
	} else {
		file, err = os.Open(path)
	}
	if err != nil {
		return fmt.Errorf("failed to open dictionary: %w", err)
	}
	defer func() { _ = file.Close() }()

	wordList = nil
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		word := strings.TrimSpace(scanner.Text())
		if len(word) > 4 { // Only use words of a reasonable length
			wordList = append(wordList, word)
		}
	}
	if len(wordList) == 0 {
		return fmt.Errorf("dictionary is empty or could not be read")
	}
	return scanner.Err()
}

// scramble shuffles the letters of a word.
func scramble(word string) string {
	runes := []rune(word)
	gameRand.Shuffle(len(runes), func(i, j int) {
		runes[i], runes[j] = runes[j], runes[i]
	})
	// Ensure the scrambled word is not the same as the original
	if string(runes) == word && len(word) > 1 {
		return scramble(word) // Recurse if we get the same word
	}
	return string(runes)
}

// NewScrambleGame creates a new Word Scramble puzzle.
func NewScrambleGame() (*ScrambleGame, error) {
	if len(wordList) == 0 {
		return nil, fmt.Errorf("word dictionary is not loaded")
	}
	word := wordList[gameRand.Intn(len(wordList))]
	return &ScrambleGame{
		OriginalWord:  word,
		ScrambledWord: scramble(word),
		StartTime:     time.Now(),
	}, nil
}

// CheckGuess compares a user's guess against the original word.
func (g *ScrambleGame) CheckGuess(guess string) bool {
	return strings.EqualFold(guess, g.OriginalWord)
}

// --- Leaderboard Methods ---

// NewLeaderboard initializes a new leaderboard for the current week.
func NewLeaderboard() *Leaderboard {
	return &Leaderboard{
		WeekStartDate: time.Now().UTC(),
		LastReset:     time.Now().UTC(),
		Scores:        make(map[string]LeaderboardEntry),
	}
}

// AddWin records a win for a user.
func (l *Leaderboard) AddWin(userID, username string) {
	l.Mutex.Lock()
	defer l.Mutex.Unlock()

	entry, ok := l.Scores[userID]
	if !ok {
		entry = LeaderboardEntry{
			UserID:   userID,
			Username: username,
		}
	}
	entry.Username = username // Update username in case it changed
	entry.Wins++
	l.Scores[userID] = entry
}

// Format returns a string representation of the leaderboard.
func (l *Leaderboard) Format() string {
	l.Mutex.Lock()
	defer l.Mutex.Unlock()

	var builder strings.Builder
	builder.WriteString("🏆 **Weekly Word Scramble Leaderboard** 🏆\n")

	nextReset := nextMondayMidnightUTC()
	fmt.Fprintf(&builder, "Resets <t:%d:R>\n", nextReset.Unix())

	if len(l.Scores) == 0 {
		builder.WriteString("The leaderboard is empty. Be the first to win a game!")
		return builder.String()
	}

	// Convert map to slice for sorting
	entries := make([]LeaderboardEntry, 0, len(l.Scores))
	for _, entry := range l.Scores {
		entries = append(entries, entry)
	}

	// Sort by wins (descending)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Wins > entries[j].Wins
	})

	builder.WriteString("```\n")
	builder.WriteString("Rank | Player          | Wins\n")
	builder.WriteString("-----+-----------------+------\n")

	crowns := []string{"🥇", "🥈", "🥉"}

	for i, entry := range entries {
		var rankStr string
		// Emojis are wide, so they get special padding to align correctly.
		if i < len(crowns) {
			rankStr = crowns[i] + " " // Emoji is ~2 chars wide, add 1 space to fill 3 chars
		} else {
			rankStr = fmt.Sprintf("%-3s", fmt.Sprintf("%d.", i+1))
		}

		username := truncateRunes(entry.Username, 15)

		// Right align wins, left-align others.
		fmt.Fprintf(&builder, "%-3s | %-15s | %4d\n", rankStr, username, entry.Wins)
		if i >= 9 { // Show top 10
			break
		}
	}
	builder.WriteString("```")
	return builder.String()
}

// nextMondayMidnightUTC calculates the timestamp for the upcoming Monday at 00:00 UTC.
func nextMondayMidnightUTC() time.Time {
	now := time.Now().UTC()
	daysUntilMonday := (7 - int(now.Weekday()) + int(time.Monday)) % 7
	if daysUntilMonday == 0 && now.Hour() >= 0 { // If it's Monday, get next Monday
		daysUntilMonday = 7
	}
	year, month, day := now.Date()
	nextMonday := time.Date(year, month, day+daysUntilMonday, 0, 0, 0, 0, time.UTC)
	return nextMonday
}

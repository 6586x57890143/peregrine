package wordgame

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// Game is one live puzzle.
//
// MessageID is the announcement the bot posted, so the caller can delete it when the game
// ends. The Manager stores it rather than the caller, because the caller used to keep it in
// a map alongside the game and the two could disagree about which announcement belonged to
// which puzzle.
type Game struct {
	Word      string
	Scrambled string
	ChannelID string
	MessageID string
	StartedAt time.Time
	ExpiresAt time.Time
}

// Options are the numbers. Every one of them was a literal in the middle of the handler.
type Options struct {
	// Timeout is how long a puzzle stays open.
	Timeout time.Duration

	// AnnounceTTL is how long a win or timeout announcement stays before being deleted.
	// Zero keeps it forever.
	AnnounceTTL time.Duration

	// ActivityWindow and ActivityThreshold decide when a channel is busy enough to
	// deserve a game: at least Threshold messages within Window.
	ActivityWindow    time.Duration
	ActivityThreshold int

	// TriggerChance is the per-message probability of starting a game once a channel is
	// busy enough.
	TriggerChance float64

	// MaxChannels bounds the activity map.
	//
	// The old one was keyed by channel ID and never pruned except when a game started in
	// that channel, so a bot in many guilds accumulated an entry per channel forever.
	// It is the same slow leak the conversation memory had before M7b, and the same kind
	// a test using one channel would never reveal.
	MaxChannels int
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = 60 * time.Second
	}
	if o.AnnounceTTL < 0 {
		o.AnnounceTTL = 0
	}
	if o.ActivityWindow <= 0 {
		o.ActivityWindow = 5 * time.Minute
	}
	if o.ActivityThreshold <= 0 {
		o.ActivityThreshold = 5
	}
	if o.TriggerChance < 0 || o.TriggerChance > 1 {
		o.TriggerChance = 0.025
	}
	if o.MaxChannels <= 0 {
		o.MaxChannels = 500
	}
	return o
}

// Manager owns every live game and the activity tracking that starts them.
//
// One mutex over everything, deliberately. The old code had two (wordGameMutex and
// activityMutex) taken in a fixed order at one call site and separately at another, which
// is a deadlock waiting for a third caller. The critical sections here are map operations,
// so a single lock costs nothing and removes the ordering question entirely.
//
// It performs no I/O and takes no session. Every method returns what the caller should say
// or delete, so the caller can route it through the send chokepoint and the tests need no
// Discord at all.
type Manager struct {
	mu       sync.Mutex
	games    map[string]*Game // channel ID -> live game
	activity map[string][]time.Time
	pending  []PendingDelete

	dict *Dictionary
	src  Source
	opts Options
	now  func() time.Time
}

// PendingDelete is a message to remove once its time is up.
//
// This replaces a goroutine per announcement. Every game used to spawn one that slept 30
// seconds and then deleted, and after shutdown they woke against a closed session: on a
// busy server the goroutine count was bounded only by how often people played. A slice
// swept by the same tick that expires games is one mechanism instead of N.
type PendingDelete struct {
	ChannelID string
	MessageID string
	Due       time.Time
}

// NewManager builds a Manager. A nil Source means DefaultSource.
func NewManager(dict *Dictionary, src Source, opts Options) *Manager {
	if src == nil {
		src = DefaultSource{}
	}
	return &Manager{
		games:    map[string]*Game{},
		activity: map[string][]time.Time{},
		dict:     dict,
		src:      src,
		opts:     opts.withDefaults(),
		now:      time.Now,
	}
}

// Available reports whether games can run at all, which is false when the dictionary
// failed to load.
//
// A load failure disables word games and nothing else. That is the general rule this bot
// runs on: peregrine is a bag of loosely related engagement behaviours and exactly one of
// them failing should disable that one, never the process.
func (m *Manager) Available() bool { return m.dict.Len() > 0 }

// Start begins a game in a channel and returns it, or nil if one is already running.
//
// The caller announces it and then calls Announced with the message ID. Splitting those is
// what lets the announcement go through the guard: the Manager cannot send, so it cannot
// bypass it.
func (m *Manager) Start(channelID string) (*Game, error) {
	if !m.Available() {
		return nil, ErrNoDictionary
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.games[channelID]; exists {
		return nil, ErrGameInProgress
	}

	word := m.dict.words[m.src.IntN(len(m.dict.words))]
	now := m.now()
	g := &Game{
		Word:      word,
		Scrambled: scramble(m.src, word),
		ChannelID: channelID,
		StartedAt: now,
		ExpiresAt: now.Add(m.opts.Timeout),
	}
	m.games[channelID] = g

	// Starting a game clears the channel's activity, so the trigger has to build up
	// again rather than firing on the next message.
	delete(m.activity, channelID)

	return g, nil
}

// Announced records the message ID of a game's announcement.
//
// Separate from Start because the ID does not exist until the send has happened, and the
// send can be refused: the guard turns down a paused bot or an ignored channel. A game
// whose announcement was refused keeps an empty MessageID, and Resolve simply has nothing
// to delete, which is the correct behaviour rather than a special case.
func (m *Manager) Announced(channelID, messageID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g, ok := m.games[channelID]; ok {
		g.MessageID = messageID
	}
}

// Abandon ends a game without a winner and without an announcement.
//
// For the case where the puzzle could not be announced: the guard refuses a paused bot or
// an ignored channel, and an unannounced game is invisible, so leaving it live would block
// the channel until it timed out against a puzzle nobody ever saw.
func (m *Manager) Abandon(channelID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.games, channelID)
}

// Guess reports whether a message solves the live game in its channel.
//
// Returns the resolved game so the caller can announce the win, and removes it, so two
// people guessing at the same instant cannot both win: the second call finds no game. That
// was possible before, because the check and the removal were separate statements under a
// lock that was released in between.
func (m *Manager) Guess(channelID, guess string) (*Game, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Case-insensitive on purpose: a player who types the right word in the wrong case
	// has solved the puzzle. Trimmed too, because a trailing space is a typo rather than
	// a wrong answer.
	g, ok := m.games[channelID]
	if !ok || !strings.EqualFold(strings.TrimSpace(guess), g.Word) {
		return nil, false
	}
	delete(m.games, channelID)
	return g, true
}

// Note records channel activity and reports whether a game should start.
//
// It returns a decision rather than starting one, so the caller can check its own
// preconditions (the feature flag, the guard's ignore list) before committing.
func (m *Manager) Note(channelID string) bool {
	if !m.Available() {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, playing := m.games[channelID]; playing {
		return false
	}

	now := m.now()
	cutoff := now.Add(-m.opts.ActivityWindow)

	kept := m.activity[channelID][:0]
	for _, ts := range m.activity[channelID] {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, now)
	m.activity[channelID] = kept

	if len(m.activity) > m.opts.MaxChannels {
		m.evictQuietestChannel(channelID)
	}

	if len(kept) < m.opts.ActivityThreshold {
		return false
	}
	return m.src.IntN(1_000_000) < int(m.opts.TriggerChance*1_000_000)
}

// evictQuietestChannel drops the activity entry with the oldest last message, never the
// one being written. Caller holds the lock.
func (m *Manager) evictQuietestChannel(protect string) {
	var oldestID string
	var oldest time.Time
	for id, stamps := range m.activity {
		if id == protect || len(stamps) == 0 {
			continue
		}
		last := stamps[len(stamps)-1]
		if oldestID == "" || last.Before(oldest) {
			oldestID, oldest = id, last
		}
	}
	if oldestID != "" {
		delete(m.activity, oldestID)
	}
}

// Expired removes and returns every game whose time is up.
//
// Called from a ticker rather than from a timer per game, which is the whole point: one
// loop, panic-isolated by core.RunLoop, instead of a goroutine whose lifetime nobody owns.
// The resolution is the tick interval, so a game may live slightly past its timeout, and
// that is a trade worth making: a puzzle ending a few seconds late is invisible, whereas a
// goroutine per game surviving shutdown is not.
func (m *Manager) Expired() []*Game {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	var out []*Game
	for id, g := range m.games {
		if now.After(g.ExpiresAt) {
			out = append(out, g)
			delete(m.games, id)
		}
	}
	// Sorted so the caller's announcements, and the tests, are deterministic rather than
	// following Go's randomized map iteration.
	sort.Slice(out, func(i, j int) bool { return out[i].ChannelID < out[j].ChannelID })
	return out
}

// DeleteLater schedules a message for removal.
func (m *Manager) DeleteLater(channelID, messageID string) {
	if messageID == "" || m.opts.AnnounceTTL <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = append(m.pending, PendingDelete{
		ChannelID: channelID,
		MessageID: messageID,
		Due:       m.now().Add(m.opts.AnnounceTTL),
	})
}

// DueDeletions removes and returns every message whose deletion time has arrived.
func (m *Manager) DueDeletions() []PendingDelete {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	var due []PendingDelete
	kept := m.pending[:0]
	for _, p := range m.pending {
		if now.After(p.Due) {
			due = append(due, p)
			continue
		}
		kept = append(kept, p)
	}
	m.pending = kept
	return due
}

// Active reports how many games are live, for the status line.
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.games)
}

// Errors a caller has to tell apart. A game already running is an ordinary outcome that
// the admin command reports to the channel; a missing dictionary means the feature is off
// and there is nothing to say.
var (
	ErrNoDictionary   = errors.New("wordgame: no dictionary loaded, word games are unavailable")
	ErrGameInProgress = errors.New("wordgame: a game is already running in this channel")
)

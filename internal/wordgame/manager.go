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
	//
	// The Manager no longer counts those messages itself. It asks a Counter, which in
	// production is internal/activity's tracker, because the Manager's own per-channel
	// activity map was a second copy of a mechanism the aggro and autonomous-post
	// features needed as well: three consumers, all fed from the same call site, and one
	// of them counting for itself. ActivityWindow is still owned here, since how busy is
	// busy enough for a WORD GAME is this feature's judgement rather than the tracker's.
	ActivityWindow    time.Duration
	ActivityThreshold int

	// TriggerChance is the per-message probability of starting a game once a channel is
	// busy enough.
	TriggerChance float64

	// MaxChannels bounds the cooldown map, which is the only per-channel map left here.
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
	mu      sync.Mutex
	games   map[string]*Game     // channel ID -> live game
	started map[string]time.Time // channel ID -> when a game last started there
	pending []PendingDelete

	dict    *Dictionary
	src     Source
	counter Counter
	opts    Options
	now     func() time.Time
}

// Counter is how the Manager finds out whether a channel is busy.
//
// Declared here rather than imported, so this package still depends on nothing and its
// tests still need no tracker: internal/activity's Tracker satisfies it structurally.
type Counter interface {
	Count(channelID string, window time.Duration) int
}

// noCounter reports every channel as silent, which disables the activity trigger. It is
// what a nil Counter means, and it fails in the quiet direction: no counter is a reason
// to start no games, never a reason to start one on every message.
type noCounter struct{}

func (noCounter) Count(string, time.Duration) int { return 0 }

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

// NewManager builds a Manager. A nil Source means DefaultSource, and a nil Counter
// disables the activity trigger.
func NewManager(dict *Dictionary, src Source, counter Counter, opts Options) *Manager {
	if src == nil {
		src = DefaultSource{}
	}
	if counter == nil {
		counter = noCounter{}
	}
	return &Manager{
		games:   map[string]*Game{},
		started: map[string]time.Time{},
		dict:    dict,
		src:     src,
		counter: counter,
		opts:    opts.withDefaults(),
		now:     time.Now,
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

	// Recorded so the activity trigger cannot fire again here immediately. This replaces
	// clearing the channel's activity counter, which is no longer the Manager's to
	// clear: the tracker is a shared observer and zeroing it would lie to the aggro and
	// autonomous-post features about how busy the channel is. A cooldown of one
	// ActivityWindow reproduces the old behaviour exactly, because clearing meant a full
	// window of fresh traffic had to accumulate before the threshold could be met again.
	m.started[channelID] = now
	if len(m.started) > m.opts.MaxChannels {
		m.evictOldestCooldown(channelID)
	}

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

// MaybeStart reports whether a channel has earned a game.
//
// It returns a decision rather than starting one, so the caller can check its own
// preconditions (the feature flag, the guard's ignore list) before committing. It was
// called Note when it also did the counting; the counting is internal/activity's now,
// and the name says what is left.
//
// Three gates, cheapest first: no game already running here, no game started here
// within the last ActivityWindow, and at least ActivityThreshold messages inside that
// window. Then the dice.
func (m *Manager) MaybeStart(channelID string) bool {
	if !m.Available() {
		return false
	}

	now := m.now()

	m.mu.Lock()
	if _, playing := m.games[channelID]; playing {
		m.mu.Unlock()
		return false
	}
	if last, ok := m.started[channelID]; ok && now.Sub(last) < m.opts.ActivityWindow {
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()

	// Asked outside the lock. The Counter is another package's mutex, and holding this
	// one across a call into it is how a lock-ordering deadlock gets built. Nothing here
	// depends on the count and the game map being consistent with each other: the worst
	// case is starting a game on a count that was true a microsecond ago.
	if m.counter.Count(channelID, m.opts.ActivityWindow) < m.opts.ActivityThreshold {
		return false
	}
	return m.src.IntN(1_000_000) < int(m.opts.TriggerChance*1_000_000)
}

// evictOldestCooldown drops the oldest cooldown entry, never the one being written.
// Caller holds the lock.
//
// The map is keyed by channel and so grows with every guild the bot joins, which is the
// same slow leak the conversation memory had before M7b and the kind a test using one
// channel would never reveal. Losing a cooldown early is harmless: the worst outcome is
// a second game in a channel that has not had one for a while.
func (m *Manager) evictOldestCooldown(protect string) {
	var oldestID string
	var oldest time.Time
	for id, at := range m.started {
		if id == protect {
			continue
		}
		if oldestID == "" || at.Before(oldest) {
			oldestID, oldest = id, at
		}
	}
	if oldestID != "" {
		delete(m.started, oldestID)
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

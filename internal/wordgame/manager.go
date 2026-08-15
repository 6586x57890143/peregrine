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

	// NextHintAt is when the next rung of the ladder is due, and HintLevel is how many rungs
	// have actually been DELIVERED. Zero NextHintAt means hints are off or the ladder is
	// finished.
	//
	// Fields on the game rather than more timers, for the same reason ExpiresAt is one: the
	// sweep that already runs collects these too, so a hint costs no goroutine and inherits
	// the panic isolation and context binding RunLoop gives that sweep for free.
	//
	// HintLevel counts DELIVERED rungs, not due ones, and the distinction is the player's
	// money: the caller charges fewer points the further down the ladder a solve lands, so a
	// rung the guard refused to post must not raise it. See HintDelivered.
	NextHintAt time.Time
	HintLevel  int

	// rungs is how many letters each rung reveals and reveal is the order positions are
	// uncovered in, both fixed when the game starts. Unexported because they are the ladder's
	// working state rather than anything a caller should render: Mask is the rendering.
	rungs  []int
	reveal []int

	// Round and Rounds place this puzzle inside a gauntlet, counting from one. Both zero for
	// an ordinary standalone game, which is what lets the announcement say "Round 2 of 5"
	// only when there is a run to be part of.
	Round  int
	Rounds int

	// Planted records that an operator chose this word with !wordgame <word> rather than it
	// being drawn from the dictionary. Reported so a log line can tell the two apart.
	Planted bool
}

// Mask renders the answer with the letters revealed so far, as "s _ _ o _ a _ _ _ _".
func (g *Game) Mask() string {
	return mask(g.Word, g.reveal, g.Revealed())
}

// Revealed is how many letters the rungs delivered so far have uncovered.
func (g *Game) Revealed() int {
	if g.HintLevel <= 0 || len(g.rungs) == 0 {
		return 0
	}
	if g.HintLevel > len(g.rungs) {
		return g.rungs[len(g.rungs)-1]
	}
	return g.rungs[g.HintLevel-1]
}

// Rungs is how many hints this word's ladder has, which is not the configured number: a short
// word gets a shorter ladder, because a rung that reveals nothing new should not cost a point.
func (g *Game) Rungs() int { return len(g.rungs) }

// Letters is the length of the answer.
func (g *Game) Letters() int { return len([]rune(g.Word)) }

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

	// HintAfter is how long into a puzzle the first rung is revealed. Zero disables hints.
	//
	// Validated below Timeout by config, because a hint that lands after the game has ended
	// is a knob wired to nothing, which is worse than no knob.
	HintAfter time.Duration

	// HintLevels is how many rungs the ladder has at most. A word too short to support that
	// many gets fewer, because ladder() drops a rung that would reveal nothing new.
	HintLevels int

	// GauntletMax bounds how many puzzles one request may queue, and GauntletGap is the pause
	// between a puzzle concluding and its successor appearing.
	//
	// The gap is not politeness. A puzzle appearing in the same instant the previous answer
	// lands reads as a malfunction rather than as a round starting, and it gives the people
	// who lost no moment to see that they did.
	GauntletMax int
	GauntletGap time.Duration

	// MaxChannels bounds every per-channel map here: the cooldowns and both halves of the
	// gauntlet queue.
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
	if o.HintAfter < 0 || o.HintAfter >= o.Timeout {
		// A hint at or past the deadline never fires. Zero is the honest way to express "no
		// hints", so an out-of-range value becomes that rather than being clamped to
		// something the operator did not ask for.
		o.HintAfter = 0
	}
	if o.HintLevels <= 0 {
		o.HintLevels = 3
	}
	if o.GauntletMax <= 0 {
		o.GauntletMax = 10
	}
	if o.GauntletGap < 0 {
		o.GauntletGap = 0
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

	// The gauntlet: how many puzzles a channel is still owed, and the earliest the next one
	// may appear. Both keyed by channel and so both bounded by MaxChannels, like started.
	// This repo has shipped an unbounded per-channel map twice (the conversation memory
	// before M7b and this package's own activity map in M11a), which is why the bound is not
	// a nicety.
	//
	// In memory and nowhere else. A restart drops every queue, which is correct: a gauntlet
	// is a five-minute event, and persisting it would mean a redeploy resurrects puzzles into
	// a channel that has moved on. A week of wins is not re-derivable and is persisted; this
	// is, by asking again.
	queued  map[string]int
	rounds  map[string]int // how many were asked for, so a puzzle can say "Round 2 of 5"
	readyAt map[string]time.Time

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
		queued:  map[string]int{},
		rounds:  map[string]int{},
		readyAt: map[string]time.Time{},
		dict:    dict,
		src:     src,
		counter: counter,
		opts:    opts.withDefaults(),
		now:     time.Now,
	}
}

// WordBounds reports the length limits a planted word has to satisfy.
//
// So the refusal can say what would have been accepted. A command that answers "no" without
// saying why is the shape this milestone exists to remove from the word game.
func (m *Manager) WordBounds() (min, max int) { return m.dict.Bounds() }

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
	return m.StartWord(channelID, "")
}

// StartWord begins a game on a word the caller chose, or draws one when word is empty.
//
// This is !wordgame <word>: the operator planting a joke word rather than taking what the
// dictionary offers. It is one function rather than two because everything after choosing the
// word is identical, and because a second copy of the "is a game already running here" check
// is a second place for it to be got wrong.
//
// A PLANTED WORD IS HELD TO THE DICTIONARY'S OWN RULES. That is not politeness about input:
// LoadDictionary excludes words with fewer than two distinct letters specifically because
// scramble used to recurse forever on them, and its own bound is documented as "a belt to
// this braces". A word arriving by a route that skipped the loader would leave the belt doing
// that work alone, which is the arrangement that produced the original stack overflow.
func (m *Manager) StartWord(channelID, word string) (*Game, error) {
	if !m.Available() {
		return nil, ErrNoDictionary
	}
	planted := word != ""
	if planted && !m.dict.Usable(word) {
		return nil, ErrUnusableWord
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.games[channelID]; exists {
		return nil, ErrGameInProgress
	}

	if !planted {
		word = m.dict.words[m.src.IntN(len(m.dict.words))]
	}
	now := m.now()
	g := &Game{
		Word:      word,
		Scrambled: scramble(m.src, word),
		ChannelID: channelID,
		StartedAt: now,
		ExpiresAt: now.Add(m.opts.Timeout),
		Planted:   planted,
	}
	if m.opts.HintAfter > 0 {
		// The ladder is fixed here, once, and so is the order positions are revealed in.
		// Both have to outlive the individual rung: a superset relationship between rungs is
		// only expressible if the order was decided before the first one.
		g.rungs = ladder(g.Letters(), m.opts.HintLevels)
		if len(g.rungs) > 0 {
			g.reveal = revealOrder(m.src, g.Letters())
			g.NextHintAt = now.Add(m.opts.HintAfter)
		}
	}

	// A puzzle taken out of a queued run is numbered, so the announcement can say which round
	// it is. Decremented here rather than at the caller because Start is the only place that
	// knows the game actually began: a caller that decremented first and then hit
	// ErrGameInProgress would silently eat a round.
	if owed := m.queued[channelID]; owed > 0 {
		g.Rounds = m.rounds[channelID]
		g.Round = g.Rounds - owed + 1
		if owed == 1 {
			delete(m.queued, channelID)
			delete(m.rounds, channelID)
		} else {
			m.queued[channelID] = owed - 1
		}
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

// Announced records the message ID of a game's announcement and returns the one it replaced.
//
// Separate from Start because the ID does not exist until the send has happened, and the
// send can be refused: the guard turns down a paused bot or an ignored channel. A game
// whose announcement was refused keeps an empty MessageID, and Resolve simply has nothing
// to delete, which is the correct behaviour rather than a special case.
//
// It RETURNS THE SUPERSEDED ID because a hint reposts rather than edits now, so a game's
// announcement changes identity mid-life. Handing the old ID back is what stops it leaking:
// without it, a win landing between the repost and this call would delete whichever ID the
// game happened to be holding and orphan the other. The caller deletes it, so the deletion
// still goes through the guard.
func (m *Manager) Announced(channelID, messageID string) (superseded string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.games[channelID]
	if !ok {
		return ""
	}
	superseded = g.MessageID
	g.MessageID = messageID
	if superseded == messageID {
		return ""
	}
	return superseded
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

	// A refused announcement cancels the rest of the run as well. The guard refuses for
	// reasons that do not clear on their own (the bot is paused, the channel is on the ignore
	// list), so leaving the queue in place would march the whole gauntlet through the same
	// refusal one puzzle at a time and log it every time.
	delete(m.queued, channelID)
	delete(m.rounds, channelID)
	delete(m.readyAt, channelID)
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
	m.concluded(channelID)
	return g, true
}

// concluded records that a channel's puzzle has ended, so any queued successor waits out the
// gap before appearing. Caller holds the lock.
//
// Called from both endings rather than from the sweep, because "the previous one has finished"
// is the entire condition a gauntlet advances on and a solve and a timeout are equally
// finished. Recording it at one of the two would make a run stall on whichever ending the
// channel happened to produce.
func (m *Manager) concluded(channelID string) {
	if m.queued[channelID] == 0 {
		return
	}
	m.readyAt[channelID] = m.now().Add(m.opts.GauntletGap)
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
	// A channel part-way through a gauntlet is not a channel that has earned a random puzzle.
	// The run owns the channel until it finishes, and interleaving would break the round
	// numbering as well as the pacing.
	if m.queued[channelID] > 0 {
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

// Queue asks for a run of n puzzles in a channel, the first of which starts immediately.
//
// It records the debt and nothing else: the first puzzle is started by the caller and every
// later one by DueStarts, so a gauntlet is not a schedule but a standing answer to "may another
// one start here yet". That is what makes it advance on the previous puzzle CONCLUDING rather
// than on a clock, which is the property being asked for: a slow round pushes the whole run
// back instead of stacking puzzles on top of each other.
//
// Refuses a channel that already owes puzzles, matching ErrGameInProgress: two overlapping runs
// in one channel have no sensible round numbering and no sensible end.
func (m *Manager) Queue(channelID string, n int) (int, error) {
	if !m.Available() {
		return 0, ErrNoDictionary
	}
	if n < 1 {
		return 0, ErrBadGauntlet
	}
	n = min(n, m.opts.GauntletMax)

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.queued[channelID] > 0 {
		return 0, ErrGauntletInProgress
	}
	m.queued[channelID] = n
	m.rounds[channelID] = n
	if len(m.queued) > m.opts.MaxChannels {
		m.evictOldestGauntlet(channelID)
	}
	return n, nil
}

// Gauntlet reports how many puzzles a channel is still owed, for a log line or a status.
func (m *Manager) Gauntlet(channelID string) (remaining, total int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.queued[channelID], m.rounds[channelID]
}

// DueStarts is every channel owed a puzzle that can have one now.
//
// Collected by the sweep that already expires games and delivers hints, so a gauntlet costs no
// goroutine and inherits RunLoop's panic isolation and context binding for free. That is the
// same reasoning that put HintAt on the Game rather than behind a timer, applied to the other
// end of a puzzle's life.
func (m *Manager) DueStarts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	var out []string
	for id, owed := range m.queued {
		if owed <= 0 {
			continue
		}
		if _, playing := m.games[id]; playing {
			continue
		}
		if at, ok := m.readyAt[id]; ok && now.Before(at) {
			continue
		}
		out = append(out, id)
	}
	// Sorted for the same reason Expired and DueHints are.
	sort.Strings(out)
	return out
}

// evictOldestGauntlet drops the run whose next puzzle is furthest in the past, never the one
// being written. Caller holds the lock.
//
// Bounded for the same reason started is, and losing one is the mild failure: a run that stops
// early is a channel that stops receiving puzzles, which is quieter than the alternative. There
// is no timestamp on a queue entry, so readyAt orders them, and an entry that has never
// concluded a round sorts oldest because its zero time is.
func (m *Manager) evictOldestGauntlet(protect string) {
	var oldestID string
	var oldest time.Time
	for id := range m.queued {
		if id == protect {
			continue
		}
		at := m.readyAt[id]
		if oldestID == "" || at.Before(oldest) {
			oldestID, oldest = id, at
		}
	}
	if oldestID != "" {
		delete(m.queued, oldestID)
		delete(m.rounds, oldestID)
		delete(m.readyAt, oldestID)
	}
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
			m.concluded(id)
		}
	}
	// Sorted so the caller's announcements, and the tests, are deterministic rather than
	// following Go's randomized map iteration.
	sort.Slice(out, func(i, j int) bool { return out[i].ChannelID < out[j].ChannelID })
	return out
}

// DueHints returns every game whose next rung has come due, WITHOUT advancing it.
//
// Swept by the same tick as Expired and DueDeletions, which is the whole design: one loop for
// every deadline this package has, rather than a timer per event. The resolution is therefore
// the tick, so a hint can land a few seconds late, which is invisible.
//
// # Why this no longer marks the game as hinted
//
// It used to set Hinted as it collected, which was fine while a hint was an edit that either
// happened or did not matter. It stopped being fine when a rung became a PRICE: the guard can
// refuse the repost (a paused bot, an ignored channel, a rate limit), and a game whose level
// advanced on a hint nobody ever saw would charge the winner points for help that was never on
// screen. So the advance is a separate acknowledgement the caller makes after the repost
// succeeded. See HintDelivered.
//
// A game with no announcement is skipped rather than returned, because the repost replaces an
// announcement and there is nothing to replace when the guard refused the original send.
//
// # It returns COPIES, already carrying the rung that is due
//
// Two reasons, and the first is the one that would have been a bug. The caller renders the card
// before it can know whether the send will succeed, so it has to be given the game as it WILL
// look, not as it currently does: handing back the live game at its current level renders a hint
// card with no hint on it. Pre-setting HintLevel on a copy says "this is the rung on offer" and
// leaves the commit to HintDelivered.
//
// The second is that the live pointers are still in the map. Every other reader of a *Game here
// gets one the Manager has already deleted, so it is the caller's; these are not, and Guess and
// Announced write to them under the lock while the sweep would have been reading them without
// it. The slices inside the copy are shared and never written after Start.
func (m *Manager) DueHints() []*Game {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	var out []*Game
	for _, g := range m.games {
		if g.NextHintAt.IsZero() || now.Before(g.NextHintAt) || g.MessageID == "" {
			continue
		}
		if g.HintLevel >= len(g.rungs) {
			continue
		}
		pending := *g
		pending.HintLevel = g.HintLevel + 1
		out = append(out, &pending)
	}
	// Sorted for the same reason Expired is: deterministic announcements and deterministic
	// tests, rather than Go's randomized map iteration.
	sort.Slice(out, func(i, j int) bool { return out[i].ChannelID < out[j].ChannelID })
	return out
}

// HintDelivered advances a game's ladder after its rung has actually reached the channel.
//
// The counterpart to DueHints not mutating. level is the rung the caller believes it just
// posted, and a mismatch is ignored rather than applied, so a repost that lost a race with a
// win or an expiry cannot advance a game that has moved on.
//
// The next rung is scheduled evenly across what remains of the puzzle, dividing by the real
// rung count rather than the configured one: a short word has a shorter ladder, and pacing it
// as though it had the full number would bunch its hints at the start and leave the rest of
// the puzzle silent.
func (m *Manager) HintDelivered(channelID string, level int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.games[channelID]
	if !ok || g.HintLevel != level-1 || level > len(g.rungs) {
		return
	}
	g.HintLevel = level

	if level >= len(g.rungs) {
		g.NextHintAt = time.Time{}
		return
	}
	span := m.opts.Timeout - m.opts.HintAfter
	g.NextHintAt = g.StartedAt.Add(m.opts.HintAfter + span*time.Duration(level)/time.Duration(len(g.rungs)))
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

	// ErrUnusableWord is a word an operator supplied that the dictionary would have rejected:
	// wrong length, a non-letter in it, or fewer than two distinct letters. The last is the
	// one that matters, because it is what stops scramble recursing.
	ErrUnusableWord = errors.New("wordgame: that word cannot be a puzzle")

	// ErrGauntletInProgress is a second run asked for in a channel already running one.
	ErrGauntletInProgress = errors.New("wordgame: a gauntlet is already running in this channel")

	// ErrBadGauntlet is a run of fewer than one puzzle. The upper end is clamped rather than
	// refused, because asking for more than the cap is a reasonable thing to want and the cap
	// is the operator's answer to it; asking for zero is not a request at all.
	ErrBadGauntlet = errors.New("wordgame: a gauntlet needs at least one puzzle")
)

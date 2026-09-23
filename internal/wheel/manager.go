package wheel

import (
	"errors"
	"slices"
	"sync"
	"time"
)

// Options are the dials a match runs under. Zero values take the defaults.
type Options struct {
	Lobby       time.Duration // how long sign-ups stay open
	TurnTimeout time.Duration // how long a player has to act before the turn passes
	MaxDuration time.Duration // hard ceiling on a match, from the lobby closing
	MinPlayers  int
	MaxPlayers  int
	Rounds      int // regular rounds before the bonus
	IdleStrikes int // consecutive timeouts before a player is removed
	MaxChannels int // concurrent matches; not configuration, like wordgame's
}

func (o Options) withDefaults() Options {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	defInt := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&o.Lobby, 60*time.Second)
	def(&o.TurnTimeout, 45*time.Second)
	def(&o.MaxDuration, 30*time.Minute)
	defInt(&o.MinPlayers, 1)
	defInt(&o.MaxPlayers, 6)
	defInt(&o.Rounds, 3)
	defInt(&o.IdleStrikes, 2)
	defInt(&o.MaxChannels, 100)
	return o
}

// View is a copy of a match's state, everything a renderer needs and nothing it could use
// to change the match. It is built under the lock and read outside it.
type View struct {
	GuildID, ChannelID, MessageID, HostID string

	Version uint64 // bumped by every applied change; drives repainting only
	Turn    int    // the token a button or modal must carry
	Phase   Phase
	Reason  Reason // meaningful once Phase is Done or Aborted
	Round   int
	Rounds  int

	Category string
	Board    string // the phrase with hidden letters as '_'; the whole phrase once over
	Called   []rune // sorted
	Players  []PlayerView
	Current  string    // the user whose turn it is; empty in the lobby and once over
	Deadline time.Time // lobby close or turn end

	Pending    *Wedge // a spun gold value waiting for its consonant
	LastWedge  int    // index into the wheel of the last spin, or -1
	CanSpin    bool
	CanVowel   bool
	BonusPrize int // zero until the match is over, so the card cannot give it away

	Last []Event // what the most recent change did, kept so a sweep repaint still says it
}

// PlayerView is one seat.
type PlayerView struct {
	UserID, Name string
	Round, Bank  int
	Strikes      int
	Left         bool
}

// Update is what one change produced. Result is non-nil exactly once per match.
type Update struct {
	View   View
	Events []Event
	Result *Result
}

// Result is what a finished match pays out. Awards are the positive match banks, gold
// descending then user ID. Winner is empty when nobody still playing banked anything.
type Result struct {
	Outcome     Phase
	Reason      Reason
	Winner      string
	Awards      []Award
	BonusPlayed bool
	BonusWon    bool
	BonusPrize  int
}

// Award is one player's payout.
type Award struct {
	UserID, Name string
	Gold         int
}

// Manager owns every live match. One mutex covers the map and every match in it, which is
// wordgame's choice for wordgame's reason: nothing here is slow, and two locks are where a
// lock-ordering deadlock gets built.
type Manager struct {
	mu      sync.Mutex
	puzzles *Puzzles
	src     Source
	opts    Options
	matches map[string]*match
	now     func() time.Time
}

// NewManager builds a manager. A nil puzzle list gives one that reports unavailable and
// refuses every match, which is how a failed load turns the feature off rather than the
// bot. A nil Source is the production one.
func NewManager(p *Puzzles, src Source, opts Options) *Manager {
	if src == nil {
		src = DefaultSource{}
	}
	return &Manager{
		puzzles: p,
		src:     src,
		opts:    opts.withDefaults(),
		matches: map[string]*match{},
		now:     time.Now,
	}
}

// Available reports whether matches can be opened at all.
func (m *Manager) Available() bool { return m != nil && m.puzzles.Len() > 0 }

// Open starts a lobby in a channel with the opener as host and first player.
//
// MaxChannels REFUSES a new match rather than evicting an old one, which is the opposite
// of wordgame's cooldown map: evicting a cooldown is harmless, evicting a live match
// deletes people's game and the gold they were playing for.
func (m *Manager) Open(guildID, channelID, userID, name string) (Update, error) {
	if !m.Available() {
		return Update{}, ErrNoPuzzles
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.matches[channelID]; ok {
		return Update{}, ErrMatchInProgress
	}
	if len(m.matches) >= m.opts.MaxChannels {
		return Update{}, ErrTooManyMatches
	}
	now := m.now()
	g := &match{
		guildID:   guildID,
		channelID: channelID,
		hostID:    userID,
		opts:      m.opts,
		src:       m.src,
		puzzles:   m.puzzles,
		players:   []*player{{id: userID, name: name}},
		deadline:  now.Add(m.opts.Lobby),
		used:      map[int]bool{},
		wedge:     -1,
	}
	g.emit(Event{Kind: Joined, UserID: userID, Name: name})
	m.matches[channelID] = g
	return m.commit(g), nil
}

// Posted records the message the match is rendered on, and the version that message
// shows. Until a match is posted, Stale does not report it: there is nothing to edit.
func (m *Manager) Posted(channelID, messageID string, version uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g, ok := m.matches[channelID]; ok {
		g.messageID = messageID
		g.painted = version
	}
}

// Abandon removes a match without a result. It is for a match nobody can see: the guard
// refused the lobby post, or a repaint failed because the message is gone.
func (m *Manager) Abandon(channelID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.matches, channelID)
}

// Act applies one player action. On error nothing changed, and the error says why in a
// way the caller can turn into a private reply.
func (m *Manager) Act(channelID string, a Action) (Update, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.matches[channelID]
	if !ok {
		return Update{}, ErrNoMatch
	}
	g.events = nil
	if err := g.act(a, m.now()); err != nil {
		g.events = nil
		return Update{}, err
	}
	return m.commit(g), nil
}

// Tick applies whatever the clock says is due: a lobby closing, a turn timing out, a
// match hitting its time limit. It returns an Update per match that changed, sorted by
// channel so a test sees the same order every run.
func (m *Manager) Tick() []Update {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var out []Update
	for _, ch := range m.channels() {
		g := m.matches[ch]
		g.events = nil
		if g.tick(now) {
			out = append(out, m.commit(g))
		}
	}
	return out
}

// Stale returns the posted matches whose message does not show their current version.
// It changes nothing: Painted is the acknowledgement, so a refused edit is retried.
func (m *Manager) Stale() []View {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []View
	for _, ch := range m.channels() {
		g := m.matches[ch]
		if g.messageID != "" && g.version != g.painted {
			out = append(out, g.view())
		}
	}
	return out
}

// Painted records that a version is on screen.
//
// The LAST acknowledgement wins, not the highest. Two presses a moment apart can have
// their edits land out of order, so the message may show an older version than one that
// was already acknowledged; recording the max would mark the newer one painted and nothing
// would ever fix the card. Recording the last one leaves the match stale, and the next
// sweep repaints it.
func (m *Manager) Painted(channelID string, version uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g, ok := m.matches[channelID]; ok {
		g.painted = version
	}
}

// Snapshot returns the current View of a channel's match.
func (m *Manager) Snapshot(channelID string) (View, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.matches[channelID]
	if !ok {
		return View{}, false
	}
	return g.view(), true
}

// Active is the number of live matches.
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.matches)
}

// commit finishes a successful change: the version moves, the events become the card's
// latest line, and a finished match is removed in the same locked section that builds its
// Result, which is what makes paying it twice impossible.
func (m *Manager) commit(g *match) Update {
	g.version++
	g.last = g.events
	g.events = nil
	u := Update{View: g.view(), Events: slices.Clone(g.last)}
	if g.over() {
		u.Result = g.result()
		delete(m.matches, g.channelID)
	}
	return u
}

func (m *Manager) channels() []string {
	chs := make([]string, 0, len(m.matches))
	for ch := range m.matches {
		chs = append(chs, ch)
	}
	slices.Sort(chs)
	return chs
}

// Errors. None of them is returned after a change was made.
var (
	ErrNoPuzzles         = errors.New("wheel: no puzzles loaded")
	ErrMatchInProgress   = errors.New("wheel: a match is already running in this channel")
	ErrTooManyMatches    = errors.New("wheel: too many matches running")
	ErrNoMatch           = errors.New("wheel: no match in this channel")
	ErrWrongPhase        = errors.New("wheel: not possible at this point in the match")
	ErrLobbyFull         = errors.New("wheel: the lobby is full")
	ErrAlreadyJoined     = errors.New("wheel: already joined")
	ErrNotJoined         = errors.New("wheel: not in this match")
	ErrNotHost           = errors.New("wheel: only the host can start")
	ErrTooFewPlayers     = errors.New("wheel: not enough players")
	ErrNotYourTurn       = errors.New("wheel: not your turn")
	ErrStaleTurn         = errors.New("wheel: that turn is over")
	ErrSpinFirst         = errors.New("wheel: spin before calling a consonant")
	ErrMustCallConsonant = errors.New("wheel: call a consonant for the spin first")
	ErrBadLetter         = errors.New("wheel: not a letter of that kind")
	ErrNoConsonantsLeft  = errors.New("wheel: no consonants left")
	ErrNoVowelsLeft      = errors.New("wheel: no vowels left")
	ErrCannotAffordVowel = errors.New("wheel: not enough gold this round for a vowel")
	ErrEmptySolve        = errors.New("wheel: empty solve")
	ErrBadBonusPick      = errors.New("wheel: pick three consonants and one vowel, not RSTLNE")
)

package wheel

import (
	"slices"
	"strings"
	"time"
)

// Phase is where a match is. Done and Aborted only ever appear in a final View, because
// the Manager removes the match in the same locked section that ends it.
type Phase int

const (
	Lobby Phase = iota
	Round
	BonusPick
	BonusSolve
	Done
	Aborted
)

// Reason says why a match ended.
type Reason int

const (
	Completed     Reason = iota
	TimeLimit            // MaxDuration ran out; no bonus is played
	NoPlayers            // everybody left or struck out
	TooFewPlayers        // the lobby closed below MinPlayers
)

// ActionKind is what a player asked to do.
type ActionKind int

const (
	Join ActionKind = iota
	Leave
	Start
	Spin
	Consonant
	Vowel
	Solve
	Pick // the bonus round's three consonants and a vowel
)

// Action is one request from a player. Turn is the token the button or modal was built
// against, and is checked only for the actions that belong to a turn: joining or leaving
// a lobby is not made stale by somebody else's move.
type Action struct {
	Kind   ActionKind
	UserID string
	Name   string
	Turn   int
	Text   string // the letter, the solve attempt or the bonus pick
}

// EventKind names something that happened, for the card's "what just happened" line.
type EventKind int

const (
	Joined EventKind = iota
	Left
	Started
	RoundStart
	Spun         // Wedge and Amount (its gold value) are set
	LetterHit    // Letter, Count and Amount (gold earned, or the vowel's cost as negative)
	LetterMiss   // Letter, and Amount for a vowel's cost
	LetterRepeat // Letter; the turn passes and nothing is earned
	Solved       // Text is the phrase, Amount the gold banked
	WrongSolve   // the guess itself is deliberately not carried
	TimedOut
	StruckOut
	BonusStart
	BonusPicked // Text is the four picked letters
	BonusWon    // Amount is the prize
	BonusLost   // Amount is the prize that was missed
	Ended       // Reason is set
)

// Event is one thing that happened. A wrong solve never carries what was typed: the card
// is repainted from events, and one blocklisted guess would otherwise make every repaint
// of the match fail the emit gate.
type Event struct {
	Kind   EventKind
	UserID string
	Name   string
	Letter rune
	Count  int
	Amount int
	Wedge  Wedge
	Text   string
	Reason Reason
}

type player struct {
	id, name string
	round    int // this round's bank, lost to a bankrupt or to somebody else's solve
	bank     int // banked across the match; what is paid out
	strikes  int // consecutive turn timeouts
	left     bool
}

// match is one game in one channel. Its methods take the time rather than reading a
// clock and take no lock: the Manager holds the lock and owns the clock.
type match struct {
	guildID, channelID, messageID string
	hostID                        string
	opts                          Options
	src                           Source
	puzzles                       *Puzzles

	phase     Phase
	reason    Reason
	round     int
	players   []*player // join order in the lobby, seat order once started
	cur       int
	turn      int
	version   uint64
	painted   uint64
	deadline  time.Time
	startedAt time.Time

	puzzle  Puzzle
	used    map[int]bool
	called  map[rune]bool
	pending *Wedge
	wedge   int // index of the last wedge landed on, or -1

	bonusPrize  int
	bonusPlayed bool
	bonusWon    bool
	last        []Event
	events      []Event // this call's events, moved to last when the call succeeds
}

func (m *match) emit(e Event) { m.events = append(m.events, e) }

func (m *match) find(userID string) int {
	for i, p := range m.players {
		if p.id == userID {
			return i
		}
	}
	return -1
}

func (m *match) active() int {
	n := 0
	for _, p := range m.players {
		if !p.left {
			n++
		}
	}
	return n
}

// act applies one action. On error nothing has changed.
func (m *match) act(a Action, now time.Time) error {
	switch a.Kind {
	case Join:
		return m.join(a)
	case Leave:
		return m.leave(a.UserID, now)
	case Start:
		if m.phase != Lobby {
			return ErrWrongPhase
		}
		if a.UserID != m.hostID {
			return ErrNotHost
		}
		if len(m.players) < m.opts.MinPlayers {
			return ErrTooFewPlayers
		}
		m.start(now)
		return nil
	}

	// Everything below belongs to a turn, so the order of these checks is what a player
	// is told: the wrong phase first, then whose turn it is, then whether the press is
	// stale, and only then whether the move itself is legal.
	switch a.Kind {
	case Spin, Consonant, Vowel:
		if m.phase != Round {
			return ErrWrongPhase
		}
	case Solve:
		// The bonus solve is its own phase but the same button.
		if m.phase != Round && m.phase != BonusSolve {
			return ErrWrongPhase
		}
	case Pick:
		if m.phase != BonusPick {
			return ErrWrongPhase
		}
	default:
		return ErrWrongPhase
	}
	if m.players[m.cur].id != a.UserID {
		return ErrNotYourTurn
	}
	if a.Turn != m.turn {
		return ErrStaleTurn
	}

	var err error
	switch a.Kind {
	case Spin:
		err = m.spin(now)
	case Consonant:
		err = m.consonant(a.Text, now)
	case Vowel:
		err = m.vowel(a.Text, now)
	case Solve:
		if m.phase == BonusSolve {
			err = m.bonusSolve(a.Text)
		} else {
			err = m.solve(a.Text, now)
		}
	case Pick:
		err = m.pick(a.Text, now)
	}
	return err
}

func (m *match) join(a Action) error {
	if m.phase != Lobby {
		return ErrWrongPhase
	}
	if m.find(a.UserID) >= 0 {
		return ErrAlreadyJoined
	}
	if len(m.players) >= m.opts.MaxPlayers {
		return ErrLobbyFull
	}
	m.players = append(m.players, &player{id: a.UserID, name: a.Name})
	m.emit(Event{Kind: Joined, UserID: a.UserID, Name: a.Name})
	return nil
}

func (m *match) leave(userID string, now time.Time) error {
	i := m.find(userID)
	if i < 0 || m.players[i].left {
		return ErrNotJoined
	}
	p := m.players[i]
	m.emit(Event{Kind: Left, UserID: p.id, Name: p.name})

	if m.phase == Lobby {
		m.players = slices.Delete(m.players, i, i+1)
		if len(m.players) == 0 {
			m.end(Aborted, NoPlayers)
			return nil
		}
		if p.id == m.hostID {
			m.hostID = m.players[0].id
		}
		return nil
	}

	p.left = true
	if m.active() == 0 {
		m.end(Aborted, NoPlayers)
		return nil
	}
	if i == m.cur {
		switch m.phase {
		case Round:
			m.pass(now)
		case BonusPick, BonusSolve:
			// The bonus is one player's; leaving it is forfeiting it.
			m.bonusPlayed = true
			m.emit(Event{Kind: BonusLost, UserID: p.id, Name: p.name, Amount: m.bonusPrize})
			m.end(Done, Completed)
		}
	}
	return nil
}

// start closes the lobby. Seats are shuffled once so the host does not always open, and
// the opening seat then rotates per round.
func (m *match) start(now time.Time) {
	m.src.Shuffle(len(m.players), func(i, j int) { m.players[i], m.players[j] = m.players[j], m.players[i] })
	m.phase = Round
	m.startedAt = now
	m.emit(Event{Kind: Started})
	m.nextRound(now)
}

func (m *match) nextRound(now time.Time) {
	if m.round >= m.opts.Rounds {
		m.startBonus(now)
		return
	}
	m.round++
	i := m.puzzles.draw(m.src, m.used, false)
	m.used[i] = true
	m.puzzle = m.puzzles.all[i]
	m.called = map[rune]bool{}
	m.pending = nil
	for _, p := range m.players {
		p.round = 0
	}
	opener := (m.round - 1) % len(m.players)
	m.cur = m.seatFrom(opener)
	if m.cur < 0 {
		m.end(Aborted, NoPlayers)
		return
	}
	m.turn++
	m.deadline = now.Add(m.opts.TurnTimeout)
	m.emit(Event{Kind: RoundStart, Text: m.puzzle.Category})
}

// seatFrom returns the first seat at or after i whose player has not left, or -1.
func (m *match) seatFrom(i int) int {
	for k := range m.players {
		j := (i + k) % len(m.players)
		if !m.players[j].left {
			return j
		}
	}
	return -1
}

// pass hands the turn to the next seat still playing, which is the same seat in a solo
// match. The token moves either way, and that is the case it exists for.
func (m *match) pass(now time.Time) {
	m.pending = nil
	next := m.seatFrom(m.cur + 1)
	if next < 0 {
		m.end(Aborted, NoPlayers)
		return
	}
	m.cur = next
	m.turn++
	m.deadline = now.Add(m.opts.TurnTimeout)
}

// keep continues the current player's turn: a fresh deadline and a clean record.
func (m *match) keep(now time.Time) {
	m.players[m.cur].strikes = 0
	m.deadline = now.Add(m.opts.TurnTimeout)
}

func (m *match) spin(now time.Time) error {
	if m.pending != nil {
		return ErrMustCallConsonant
	}
	if m.hidden(false) == 0 {
		return ErrNoConsonantsLeft
	}
	p := m.players[m.cur]
	i, w := spin(m.src)
	m.wedge = i
	m.emit(Event{Kind: Spun, UserID: p.id, Name: p.name, Wedge: w, Amount: w.Value})
	switch w.Kind {
	case Gold:
		m.pending = &w
		m.keep(now)
	case Bankrupt:
		p.round = 0
		p.strikes = 0
		m.pass(now)
	case LoseTurn:
		p.strikes = 0
		m.pass(now)
	}
	return nil
}

func (m *match) consonant(text string, now time.Time) error {
	if m.pending == nil {
		return ErrSpinFirst
	}
	l, ok := parseLetter(text)
	if !ok || isVowel(l) {
		return ErrBadLetter
	}
	p := m.players[m.cur]
	value := m.pending.Value
	m.pending = nil
	p.strikes = 0
	if m.called[l] {
		m.emit(Event{Kind: LetterRepeat, UserID: p.id, Name: p.name, Letter: l})
		m.pass(now)
		return nil
	}
	m.called[l] = true
	n := m.count(l)
	if n == 0 {
		m.emit(Event{Kind: LetterMiss, UserID: p.id, Name: p.name, Letter: l})
		m.pass(now)
		return nil
	}
	p.round += value * n
	m.emit(Event{Kind: LetterHit, UserID: p.id, Name: p.name, Letter: l, Count: n, Amount: value * n})
	m.afterHit(now)
	return nil
}

func (m *match) vowel(text string, now time.Time) error {
	if m.pending != nil {
		return ErrMustCallConsonant
	}
	l, ok := parseLetter(text)
	if !ok || !isVowel(l) {
		return ErrBadLetter
	}
	if m.hidden(true) == 0 {
		return ErrNoVowelsLeft
	}
	p := m.players[m.cur]
	if p.round < VowelCost {
		return ErrCannotAffordVowel
	}
	p.round -= VowelCost
	p.strikes = 0
	if m.called[l] {
		m.emit(Event{Kind: LetterRepeat, UserID: p.id, Name: p.name, Letter: l, Amount: -VowelCost})
		m.pass(now)
		return nil
	}
	m.called[l] = true
	n := m.count(l)
	if n == 0 {
		m.emit(Event{Kind: LetterMiss, UserID: p.id, Name: p.name, Letter: l, Amount: -VowelCost})
		m.pass(now)
		return nil
	}
	m.emit(Event{Kind: LetterHit, UserID: p.id, Name: p.name, Letter: l, Count: n, Amount: -VowelCost})
	m.afterHit(now)
	return nil
}

// afterHit keeps the turn, unless that hit revealed the last hidden letter, in which case
// the revealer has solved it. A board with nothing left to guess, waiting for somebody to
// type it out, would only produce timeouts.
func (m *match) afterHit(now time.Time) {
	if m.hidden(false)+m.hidden(true) == 0 {
		m.win(now)
		return
	}
	m.keep(now)
}

func (m *match) solve(text string, now time.Time) error {
	if m.pending != nil {
		return ErrMustCallConsonant
	}
	guess := normalizeSolve(text)
	if guess == "" {
		return ErrEmptySolve
	}
	p := m.players[m.cur]
	p.strikes = 0
	if guess != normalizeSolve(m.puzzle.Phrase) {
		m.emit(Event{Kind: WrongSolve, UserID: p.id, Name: p.name})
		m.pass(now)
		return nil
	}
	m.win(now)
	return nil
}

// win banks the current player's round and moves on. Everybody else's round bank is
// dropped, which is the rule that makes solving early worth anything.
func (m *match) win(now time.Time) {
	p := m.players[m.cur]
	p.bank += p.round
	m.emit(Event{Kind: Solved, UserID: p.id, Name: p.name, Text: m.puzzle.Phrase, Amount: p.round})
	for _, q := range m.players {
		q.round = 0
	}
	m.nextRound(now)
}

// startBonus hands the bonus round to the top match bank still playing, ties to the
// earlier seat. Nobody having banked anything means there is nothing to play for.
func (m *match) startBonus(now time.Time) {
	best := -1
	for i, p := range m.players {
		if !p.left && p.bank > 0 && (best < 0 || p.bank > m.players[best].bank) {
			best = i
		}
	}
	if best < 0 {
		m.end(Done, Completed)
		return
	}
	i := m.puzzles.draw(m.src, m.used, true)
	m.used[i] = true
	m.puzzle = m.puzzles.all[i]
	m.called = map[rune]bool{}
	for _, r := range bonusGiven {
		m.called[r] = true
	}
	// Drawn now rather than on the solve, so the seeded draw does not depend on whether
	// the answer was right.
	m.bonusPrize = bonusPrizes[m.src.IntN(len(bonusPrizes))]
	m.pending = nil
	m.phase = BonusPick
	m.cur = best
	m.turn++
	m.deadline = now.Add(m.opts.TurnTimeout)
	p := m.players[best]
	m.emit(Event{Kind: BonusStart, UserID: p.id, Name: p.name, Text: m.puzzle.Category})
}

// pick takes exactly three consonants outside RSTLN and one vowel other than E, in any
// order and with any separators.
func (m *match) pick(text string, now time.Time) error {
	var letters []rune
	for _, r := range strings.ToUpper(text) {
		if isLetter(r) {
			letters = append(letters, r)
		} else if r != ' ' && r != ',' && r != '-' && r != '/' {
			return ErrBadBonusPick
		}
	}
	if len(letters) != 4 {
		return ErrBadBonusPick
	}
	seen := map[rune]bool{}
	cons, vows := 0, 0
	for _, r := range letters {
		if seen[r] || strings.ContainsRune(bonusGiven, r) {
			return ErrBadBonusPick
		}
		seen[r] = true
		if isVowel(r) {
			vows++
		} else {
			cons++
		}
	}
	if cons != 3 || vows != 1 {
		return ErrBadBonusPick
	}
	for _, r := range letters {
		m.called[r] = true
	}
	p := m.players[m.cur]
	p.strikes = 0
	slices.Sort(letters)
	m.emit(Event{Kind: BonusPicked, UserID: p.id, Name: p.name, Text: string(letters)})
	m.phase = BonusSolve
	m.turn++
	m.deadline = now.Add(m.opts.TurnTimeout)
	return nil
}

func (m *match) bonusSolve(text string) error {
	guess := normalizeSolve(text)
	if guess == "" {
		return ErrEmptySolve
	}
	p := m.players[m.cur]
	m.bonusPlayed = true
	if guess == normalizeSolve(m.puzzle.Phrase) {
		m.bonusWon = true
		p.bank += m.bonusPrize
		m.emit(Event{Kind: BonusWon, UserID: p.id, Name: p.name, Amount: m.bonusPrize, Text: m.puzzle.Phrase})
	} else {
		m.emit(Event{Kind: BonusLost, UserID: p.id, Name: p.name, Amount: m.bonusPrize, Text: m.puzzle.Phrase})
	}
	m.end(Done, Completed)
	return nil
}

// tick applies whatever the clock says is due, and reports whether anything changed. At
// most one turn timeout is applied per call, so a sweep that ran late does not skip
// several players' turns at once.
func (m *match) tick(now time.Time) bool {
	switch m.phase {
	case Lobby:
		if now.Before(m.deadline) {
			return false
		}
		if len(m.players) >= m.opts.MinPlayers {
			m.start(now)
		} else {
			m.end(Aborted, TooFewPlayers)
		}
		return true
	case Round, BonusPick, BonusSolve:
		if now.Sub(m.startedAt) >= m.opts.MaxDuration {
			m.end(Done, TimeLimit)
			return true
		}
		if now.Before(m.deadline) {
			return false
		}
		p := m.players[m.cur]
		m.emit(Event{Kind: TimedOut, UserID: p.id, Name: p.name})
		if m.phase != Round {
			m.bonusPlayed = true
			m.emit(Event{Kind: BonusLost, UserID: p.id, Name: p.name, Amount: m.bonusPrize, Text: m.puzzle.Phrase})
			m.end(Done, Completed)
			return true
		}
		p.strikes++
		if p.strikes >= m.opts.IdleStrikes {
			p.left = true
			m.emit(Event{Kind: StruckOut, UserID: p.id, Name: p.name})
			if m.active() == 0 {
				m.end(Aborted, NoPlayers)
				return true
			}
		}
		m.pass(now)
		return true
	}
	return false
}

func (m *match) end(phase Phase, reason Reason) {
	m.phase = phase
	m.reason = reason
	m.pending = nil
	m.emit(Event{Kind: Ended, Reason: reason})
}

func (m *match) over() bool { return m.phase == Done || m.phase == Aborted }

// count is how many times a letter appears in the phrase.
func (m *match) count(l rune) int { return strings.Count(m.puzzle.Phrase, string(l)) }

// hidden counts the distinct vowels, or consonants, not yet revealed.
func (m *match) hidden(vowels bool) int {
	seen := map[rune]bool{}
	for _, r := range m.puzzle.Phrase {
		if isLetter(r) && isVowel(r) == vowels && !m.called[r] {
			seen[r] = true
		}
	}
	return len(seen)
}

func (m *match) board() string {
	var b strings.Builder
	for _, r := range m.puzzle.Phrase {
		if isLetter(r) && !m.called[r] {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// result is what the match paid. Awards are every positive match bank, including players
// who left: gold they banked was earned. The winner has to still be playing.
func (m *match) result() *Result {
	r := &Result{
		Outcome: m.phase, Reason: m.reason, BonusPrize: m.bonusPrize,
		BonusPlayed: m.bonusPlayed, BonusWon: m.bonusWon,
	}
	best := -1
	for i, p := range m.players {
		if p.bank > 0 {
			r.Awards = append(r.Awards, Award{UserID: p.id, Name: p.name, Gold: p.bank})
		}
		if !p.left && p.bank > 0 && (best < 0 || p.bank > m.players[best].bank) {
			best = i
		}
	}
	if best >= 0 {
		r.Winner = m.players[best].id
	}
	slices.SortStableFunc(r.Awards, func(a, b Award) int {
		if a.Gold != b.Gold {
			return b.Gold - a.Gold
		}
		return strings.Compare(a.UserID, b.UserID)
	})
	return r
}

func (m *match) view() View {
	v := View{
		GuildID:   m.guildID,
		ChannelID: m.channelID,
		MessageID: m.messageID,
		HostID:    m.hostID,
		Version:   m.version,
		Turn:      m.turn,
		Phase:     m.phase,
		Reason:    m.reason,
		Round:     m.round,
		Rounds:    m.opts.Rounds,
		Deadline:  m.deadline,
		LastWedge: m.wedge,
		Last:      slices.Clone(m.last),
	}
	for _, p := range m.players {
		v.Players = append(v.Players, PlayerView{
			UserID: p.id, Name: p.name, Round: p.round, Bank: p.bank, Strikes: p.strikes, Left: p.left,
		})
	}
	if m.phase == Lobby {
		return v
	}
	v.Category = m.puzzle.Category
	v.Board = m.board()
	for r := range m.called {
		v.Called = append(v.Called, r)
	}
	slices.Sort(v.Called)
	if !m.over() {
		v.Current = m.players[m.cur].id
	} else {
		v.BonusPrize = m.bonusPrize
		v.Board = m.puzzle.Phrase
	}
	if m.pending != nil {
		w := *m.pending
		v.Pending = &w
	}
	if m.phase == Round {
		v.CanSpin = m.pending == nil && m.hidden(false) > 0
		v.CanVowel = m.pending == nil && m.hidden(true) > 0 && m.players[m.cur].round >= VowelCost
	}
	return v
}

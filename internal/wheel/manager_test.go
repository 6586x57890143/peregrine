package wheel

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var eventNames = map[EventKind]string{
	Joined: "joined", Left: "left", Started: "started", RoundStart: "round", Spun: "spun",
	LetterHit: "hit", LetterMiss: "miss", LetterRepeat: "repeat", Solved: "solved",
	WrongSolve: "wrong", TimedOut: "timeout", StruckOut: "struck", BonusStart: "bonus",
	BonusPicked: "picked", BonusWon: "won", BonusLost: "lost", Ended: "ended",
}

func describe(e Event) string {
	s := eventNames[e.Kind]
	if e.UserID != "" {
		s = e.UserID + " " + s
	}
	switch e.Kind {
	case Spun:
		s += fmt.Sprintf(" %d/%d", e.Wedge.Kind, e.Amount)
	case LetterHit, LetterMiss, LetterRepeat:
		s += fmt.Sprintf(" %c x%d %+d", e.Letter, e.Count, e.Amount)
	case Solved, BonusWon, BonusLost:
		s += fmt.Sprintf(" %d", e.Amount)
	case BonusPicked, RoundStart:
		s += " " + e.Text
	}
	return s
}

// The scripted match is also the one to READ: run it with -v and the log is a whole
// match, move by move, the way the golden samples are read for generation.
func TestFullMatchScripted(t *testing.T) {
	opts := testOpts()
	opts.Rounds = 2
	f := newFixture(t, opts, "phrase|hello world", "phrase|big if true", "thing|good job")
	var log []string
	rec := func(u Update) {
		for _, e := range u.Events {
			log = append(log, describe(e))
		}
	}
	f.open("a", "b")
	rec(f.ok(Action{Kind: Start, UserID: "a"}))
	rec(f.spinTo(w600))
	rec(f.do(Consonant, "l")) // a: 1800
	rec(f.do(Vowel, "o"))     // a: 1550
	rec(f.spinTo(wBankrupt))  // a: 0; b's turn
	rec(f.spinTo(w600))
	rec(f.do(Consonant, "w"))   // b: 600
	rec(f.solve("hello world")) // b banks 600; round 2 opens on b (seat 1)
	rec(f.spinTo(w2500))
	rec(f.do(Consonant, "g"))   // b: 2500
	rec(f.solve("big if tree")) // wrong; a's turn
	rec(f.spinTo(w600))
	rec(f.do(Consonant, "b"))   // a: 600
	rec(f.solve("big if true")) // a banks 600 and ties b; the earlier seat takes the bonus
	rec(f.do(Pick, "gdbo"))
	u := f.solve("good job")
	rec(u)
	t.Log("\n" + strings.Join(log, "\n"))

	want := []string{
		"started", "round phrase",
		"a spun 0/600", "a hit L x3 +1800",
		"a hit O x2 -250",
		"a spun 1/0",
		"b spun 0/600", "b hit W x1 +600",
		"b solved 600", "round phrase",
		"b spun 0/2500", "b hit G x1 +2500",
		"b wrong",
		"a spun 0/600", "a hit B x1 +600",
		"a solved 600", "a bonus",
		"a picked BDGO",
		"a won 5000", "ended",
	}
	if !slices.Equal(log, want) {
		t.Fatalf("event log:\n%s\nwant:\n%s", strings.Join(log, "\n"), strings.Join(want, "\n"))
	}
	r := u.Result
	wantAwards := []Award{{UserID: "a", Name: "a", Gold: 5600}, {UserID: "b", Name: "b", Gold: 600}}
	if r.Winner != "a" || !r.BonusWon || r.BonusPrize != 5000 || !reflect.DeepEqual(r.Awards, wantAwards) {
		t.Fatalf("result %+v", r)
	}
}

// playRandom drives a whole match with a random player: stale tokens, strangers, bad
// letters, leaves and clock jumps included. It checks the invariants after every step and
// returns the Result.
func playRandom(t *testing.T, seed uint64, check bool) *Result {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 1))
	opts := testOpts()
	opts.Rounds = 3
	opts.MaxPlayers = 4
	p := puzzlesOf(t, "phrase|hello world", "phrase|big if true", "thing|good job", "place|the ball pit", "event|a boss fight")
	m := NewManager(p, rand.New(rand.NewPCG(seed, 2)), opts)
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	users := []string{"a", "b", "c", "d", "e"}[:1+rng.IntN(4)]
	if _, err := m.Open(testGuild, testChannel, users[0], users[0]); err != nil {
		t.Fatal(err)
	}
	for _, u := range users[1:] {
		if _, err := m.Act(testChannel, Action{Kind: Join, UserID: u, Name: u}); err != nil {
			t.Fatal(err)
		}
	}
	texts := []string{"", "l", "o", "e", "z", "a", "1", "gdbo", "cdmi", "hello world", "big if true", "good job", "the ball pit", "a boss fight", "nope"}
	kindsAll := []ActionKind{Join, Leave, Start, Spin, Spin, Consonant, Consonant, Consonant, Vowel, Solve, Pick, Next}

	var result *Result
	prev, _ := m.Snapshot(testChannel)
	for step := 0; step < 20000; step++ {
		var u Update
		var err error
		ticked := false
		if rng.IntN(6) == 0 {
			// Mostly small steps, sometimes a stall long enough to time a turn out.
			now = now.Add(time.Duration(rng.IntN(12)) * time.Second)
			if rng.IntN(8) == 0 {
				now = now.Add(opts.TurnTimeout)
			}
			us := m.Tick()
			ticked = true
			if len(us) > 1 {
				t.Fatalf("seed %d: %d updates for one match", seed, len(us))
			}
			if len(us) == 1 {
				u = us[0]
			}
		} else {
			cur, _ := m.Snapshot(testChannel)
			a := sensible(rng, m, cur)
			if rng.IntN(3) == 0 {
				// Chaos: any action, any player, any text.
				a = Action{Kind: kindsAll[rng.IntN(len(kindsAll))], Text: texts[rng.IntN(len(texts))], Turn: cur.Turn}
				a.UserID = users[rng.IntN(len(users))]
				if a.Kind == Leave && rng.IntN(8) != 0 {
					a.Kind = Spin // leaving is rare, or most matches would be a walkout
				}
			}
			if a.UserID == "" {
				a.UserID = users[rng.IntN(len(users))]
			}
			if rng.IntN(10) == 0 {
				a.Turn-- // a stale press
			}
			u, err = m.Act(testChannel, a)
			if err != nil {
				after, _ := m.Snapshot(testChannel)
				if check && !reflect.DeepEqual(prev, after) {
					t.Fatalf("seed %d step %d: %v changed the match", seed, step, err)
				}
				continue
			}
		}
		if ticked && u.Result == nil && len(u.Events) == 0 {
			v, _ := m.Snapshot(testChannel)
			if check && !reflect.DeepEqual(prev, v) {
				t.Fatalf("seed %d step %d: an idle tick changed the match", seed, step)
			}
			continue
		}
		v := u.View
		if check {
			checkInvariants(t, seed, step, m, prev, u)
		}
		prev = v
		if u.Result != nil {
			result = u.Result
			break
		}
	}
	if result == nil {
		t.Fatalf("seed %d: no result within the step bound", seed)
	}
	if _, ok := m.Snapshot(testChannel); ok || m.Active() != 0 {
		t.Fatalf("seed %d: a finished match is still live", seed)
	}
	if _, err := m.Act(testChannel, Action{Kind: Spin, UserID: "a"}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("seed %d: acting on a finished match: %v", seed, err)
	}
	return result
}

// sensible is what a player who knows the rules (and, a fifth of the time, the answer)
// would press, so random matches actually reach solves, bonuses and every ending.
func sensible(rng *rand.Rand, m *Manager, v View) Action {
	a := Action{UserID: v.Current, Turn: v.Turn}
	phrase := ""
	if g, ok := m.matches[testChannel]; ok {
		phrase = g.puzzle.Phrase
	}
	letter := func(from string) string { return string(from[rng.IntN(len(from))]) }
	switch v.Phase {
	case Lobby:
		a.Kind, a.UserID = Start, v.HostID
	case Intermission:
		a.Kind, a.UserID = Next, v.Players[rng.IntN(len(v.Players))].UserID
	case BonusPick:
		a.Kind, a.Text = Pick, []string{"gdbo", "cdmi", "hpwa", "fkyu"}[rng.IntN(4)]
	case BonusSolve:
		a.Kind, a.Text = Solve, phrase
		if rng.IntN(2) == 0 {
			a.Text = "wrong guess"
		}
	case Round:
		switch {
		case v.Pending != nil:
			a.Kind, a.Text = Consonant, letter("BCDFGHJKLMNPQRSTVWXYZ")
		case rng.IntN(5) == 0:
			a.Kind, a.Text = Solve, phrase
		case v.CanVowel && rng.IntN(3) == 0:
			a.Kind, a.Text = Vowel, letter("AEIOU")
		case v.CanSpin:
			a.Kind = Spin
		default:
			a.Kind, a.Text = Solve, phrase
		}
	}
	return a
}

func checkInvariants(t *testing.T, seed uint64, step int, m *Manager, prev View, u Update) {
	t.Helper()
	v := u.View
	fail := func(format string, args ...any) {
		t.Fatalf("seed %d step %d: "+format, append([]any{seed, step}, args...)...)
	}
	if v.Version != prev.Version+1 {
		fail("version %d -> %d", prev.Version, v.Version)
	}
	if v.Turn < prev.Turn {
		fail("turn went backwards %d -> %d", prev.Turn, v.Turn)
	}
	for _, p := range v.Players {
		if p.Round < 0 || p.Bank < 0 {
			fail("negative gold %+v", p)
		}
	}
	for _, e := range u.Events {
		if e.Kind == Spun && e.Wedge.Kind == Bankrupt && seat(v, e.UserID).Round != 0 {
			fail("bankrupt left %d in the round bank", seat(v, e.UserID).Round)
		}
		if e.Kind == WrongSolve && e.Text != "" {
			fail("a wrong solve carried its text")
		}
	}
	if v.Current != "" && seat(v, v.Current).Left {
		fail("the current player has left")
	}
	for i := 1; i < len(v.Called); i++ {
		if v.Called[i] <= v.Called[i-1] {
			fail("called not sorted and unique: %q", string(v.Called))
		}
	}
	if u.Result == nil && v.Phase != Lobby && v.Phase != Intermission {
		phrase := m.matches[testChannel].puzzle.Phrase
		called := map[rune]bool{}
		for _, r := range v.Called {
			called[r] = true
		}
		want := []rune(phrase)
		for i, r := range want {
			if isLetter(r) && !called[r] {
				want[i] = '_'
			}
		}
		if v.Board != string(want) {
			fail("board %q does not match phrase %q and called %q", v.Board, phrase, string(v.Called))
		}
	}
	if r := u.Result; r != nil && r.Winner != "" {
		w := seat(v, r.Winner)
		if w.Left {
			fail("the winner had left")
		}
		for _, p := range v.Players {
			if !p.Left && p.Bank > w.Bank {
				fail("winner %s has %d but %s has %d", r.Winner, w.Bank, p.UserID, p.Bank)
			}
		}
	}
}

func TestRandomPlayInvariants(t *testing.T) {
	for seed := range uint64(300) {
		playRandom(t, seed, true)
	}
}

func TestSeededMatchIsReproducible(t *testing.T) {
	for seed := range uint64(20) {
		a, b := playRandom(t, seed, false), playRandom(t, seed, false)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("seed %d: %+v vs %+v", seed, a, b)
		}
	}
}

// Nobody touches anything: the match must still end, by strikes rather than by the time
// limit, which is set far away so it cannot be what ends it.
func TestIdleMatchAlwaysTerminates(t *testing.T) {
	for n := 1; n <= 6; n++ {
		opts := testOpts()
		opts.MaxPlayers = 6
		opts.MaxDuration = 3 * time.Hour
		f := newFixture(t, opts)
		f.begin([]string{"a", "b", "c", "d", "e", "f"}[:n]...)
		var r *Result
		for tick := 1; tick <= n*opts.IdleStrikes; tick++ {
			f.advance(opts.TurnTimeout)
			for _, u := range f.m.Tick() {
				if u.Result != nil {
					r = u.Result
					if tick != n*opts.IdleStrikes {
						t.Fatalf("%d players ended after %d ticks", n, tick)
					}
				}
			}
		}
		if r == nil || r.Outcome != Aborted || r.Reason != NoPlayers {
			t.Fatalf("%d players: result %+v", n, r)
		}
	}
}

func TestErrorsDoNotMutate(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a", "b")
	f.earn("l")
	before := f.view()
	cases := []Action{
		{Kind: Join, UserID: "c"},
		{Kind: Start, UserID: "a"},
		{Kind: Spin, UserID: "b", Turn: before.Turn},
		{Kind: Spin, UserID: "a", Turn: before.Turn - 1},
		{Kind: Consonant, UserID: "a", Turn: before.Turn, Text: "t"},
		{Kind: Vowel, UserID: "a", Turn: before.Turn, Text: "x"},
		{Kind: Solve, UserID: "a", Turn: before.Turn, Text: "!!"},
		{Kind: Pick, UserID: "a", Turn: before.Turn, Text: "gdbo"},
		{Kind: Leave, UserID: "zed"},
	}
	for _, a := range cases {
		if _, err := f.m.Act(testChannel, a); err == nil {
			t.Fatalf("%+v succeeded", a)
		}
		if !reflect.DeepEqual(before, f.view()) {
			t.Fatalf("%+v changed the match", a)
		}
	}
}

func TestViewIsACopy(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	f.earn("l")
	v := f.view()
	v.Players[0].Bank = 99
	v.Called[0] = 'Q'
	v.Last[0].Amount = 99
	v.Pending = &Wedge{Value: 1}
	if w := f.view(); w.Players[0].Bank == 99 || w.Called[0] == 'Q' || w.Last[0].Amount == 99 {
		t.Fatal("a View shares memory with the match")
	}
}

func TestResultReturnedExactlyOnce(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	f.earn("l")
	u := f.solve("hello world")
	if u.Result != nil {
		t.Fatal("the result arrived before the bonus")
	}
	f.advance(testOpts().TurnTimeout)
	var results int
	for range 3 {
		for _, u := range f.m.Tick() {
			if u.Result != nil {
				results++
			}
		}
		f.advance(time.Hour)
	}
	if results != 1 {
		t.Fatalf("%d results", results)
	}
	if _, err := f.m.Act(testChannel, Action{Kind: Leave, UserID: "a"}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestStaleReturnsUnpaintedOnly(t *testing.T) {
	f := newFixture(t, testOpts())
	u, _ := f.m.Open(testGuild, testChannel, "a", "a")
	f.m.Posted(testChannel, "m1", u.View.Version)
	if len(f.m.Stale()) != 0 {
		t.Fatal("a freshly posted match is stale")
	}
	u = f.ok(Action{Kind: Join, UserID: "b"})
	st := f.m.Stale()
	if len(st) != 1 || st[0].Version != u.View.Version || st[0].MessageID != "m1" {
		t.Fatalf("stale %+v", st)
	}
	f.m.Painted(testChannel, u.View.Version)
	if len(f.m.Stale()) != 0 {
		t.Fatal("still stale after the paint")
	}
}

// Two edits land out of order: the message shows the OLDER one. Recording the max would
// mark the newer version painted and nothing would ever fix the card.
func TestPaintedLastAckWins(t *testing.T) {
	f := newFixture(t, testOpts())
	u, _ := f.m.Open(testGuild, testChannel, "a", "a")
	f.m.Posted(testChannel, "m1", u.View.Version)
	u1 := f.ok(Action{Kind: Join, UserID: "b"})
	u2 := f.ok(Action{Kind: Join, UserID: "c"})
	f.m.Painted(testChannel, u2.View.Version)
	f.m.Painted(testChannel, u1.View.Version)
	st := f.m.Stale()
	if len(st) != 1 || st[0].Version != u2.View.Version {
		t.Fatalf("stale %+v", st)
	}
}

func TestStaleSkipsUnpostedMatch(t *testing.T) {
	f := newFixture(t, testOpts())
	f.open("a", "b")
	if len(f.m.Stale()) != 0 {
		t.Fatal("an unposted match is stale: there is no message to edit")
	}
}

func TestAbandonRemovesSilently(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	f.m.Abandon(testChannel)
	if f.m.Active() != 0 {
		t.Fatal("still live")
	}
	f.advance(time.Hour)
	if us := f.m.Tick(); len(us) != 0 {
		t.Fatalf("an abandoned match produced %+v", us)
	}
}

func TestMaxChannelsRefusesNewMatch(t *testing.T) {
	opts := testOpts()
	opts.MaxChannels = 2
	f := newFixture(t, opts)
	for _, ch := range []string{"c1", "c2"} {
		if _, err := f.m.Open(testGuild, ch, "a", "a"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.m.Open(testGuild, "c3", "a", "a"); !errors.Is(err, ErrTooManyMatches) {
		t.Fatalf("err = %v", err)
	}
	if f.m.Active() != 2 {
		t.Fatal("an existing match was evicted")
	}
}

func TestTickSortedByChannel(t *testing.T) {
	f := newFixture(t, testOpts())
	for _, ch := range []string{"c9", "c1", "c5"} {
		if _, err := f.m.Open(testGuild, ch, "a", "a"); err != nil {
			t.Fatal(err)
		}
	}
	f.advance(testOpts().Lobby)
	var got []string
	for _, u := range f.m.Tick() {
		got = append(got, u.View.ChannelID)
	}
	if !slices.Equal(got, []string{"c1", "c5", "c9"}) {
		t.Fatalf("order %v", got)
	}
}

func TestTheGuildTravelsWithTheMatch(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	f.advance(testOpts().TurnTimeout)
	us := f.m.Tick()
	if us[0].View.GuildID != testGuild {
		t.Fatalf("guild %q; a Tick result has no other way to know whose gold it is", us[0].View.GuildID)
	}
}

// Meaningful under -race: many presses of one Spin, one token, exactly one lands.
func TestConcurrentSpinPressesActOnce(t *testing.T) {
	f := newFixture(t, testOpts())
	f.begin("a")
	f.m.src = DefaultSource{}
	v := f.view()
	var wg sync.WaitGroup
	var mu sync.Mutex
	okCount := 0
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.m.Act(testChannel, Action{Kind: Spin, UserID: "a", Turn: v.Turn}); err == nil {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if okCount != 1 {
		t.Fatalf("%d spins landed", okCount)
	}
}

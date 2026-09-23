package wheel

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// script is a Source whose spins are exactly what the test says. Only a draw sized like
// the wheel consumes a scripted spin (an empty queue lands on 600); every other draw
// (puzzles, the bonus prize) takes from `other`, or 0, which is the first candidate.
// Shuffle leaves the seats in join order unless reverse is set, and counts its calls.
type script struct {
	spins    []int
	other    []int
	reverse  bool
	shuffles int
}

func (s *script) IntN(n int) int {
	if n == len(wedges) {
		if len(s.spins) == 0 {
			return 1
		}
		v := s.spins[0]
		s.spins = s.spins[1:]
		return v % n
	}
	if len(s.other) == 0 {
		return 0
	}
	v := s.other[0]
	s.other = s.other[1:]
	return v % n
}

func (s *script) Shuffle(n int, swap func(i, j int)) {
	s.shuffles++
	if s.reverse {
		for i := 0; i < n/2; i++ {
			swap(i, n-1-i)
		}
	}
}

// Wedge indexes the tests spin to, named so a scripted match reads as a match.
const (
	w2500     = 0
	w600      = 1
	wBankrupt = 7
	wLoseTurn = 15
)

func puzzlesOf(t *testing.T, lines ...string) *Puzzles {
	t.Helper()
	p, err := parse(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// hello is the default puzzle: consonants H L W R D, vowels E O, L three times.
var hello = []string{"phrase|hello world", "thing|good job"}

type fixture struct {
	t   *testing.T
	m   *Manager
	src *script
	now time.Time
}

const (
	testGuild   = "g1"
	testChannel = "c1"
)

func testOpts() Options {
	return Options{
		Lobby:       60 * time.Second,
		TurnTimeout: 30 * time.Second,
		MaxDuration: 30 * time.Minute,
		MinPlayers:  1,
		MaxPlayers:  3,
		Rounds:      1,
		IdleStrikes: 2,
	}
}

func newFixture(t *testing.T, opts Options, lines ...string) *fixture {
	t.Helper()
	if len(lines) == 0 {
		lines = hello
	}
	f := &fixture{t: t, src: &script{}, now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
	f.m = NewManager(puzzlesOf(t, lines...), f.src, opts)
	f.m.now = func() time.Time { return f.now }
	return f
}

func (f *fixture) advance(d time.Duration) { f.now = f.now.Add(d) }

// open puts the first user in as host and joins the rest.
func (f *fixture) open(users ...string) {
	f.t.Helper()
	if _, err := f.m.Open(testGuild, testChannel, users[0], users[0]); err != nil {
		f.t.Fatal(err)
	}
	for _, u := range users[1:] {
		f.ok(Action{Kind: Join, UserID: u, Name: u})
	}
}

// begin opens and starts a match.
func (f *fixture) begin(users ...string) Update {
	f.t.Helper()
	f.open(users...)
	return f.ok(Action{Kind: Start, UserID: users[0]})
}

func (f *fixture) view() View {
	f.t.Helper()
	v, ok := f.m.Snapshot(testChannel)
	if !ok {
		f.t.Fatal("no match")
	}
	return v
}

// try performs a turn action as the current player with the current token.
func (f *fixture) try(kind ActionKind, text string) (Update, error) {
	v, _ := f.m.Snapshot(testChannel)
	return f.m.Act(testChannel, Action{Kind: kind, UserID: v.Current, Turn: v.Turn, Text: text})
}

func (f *fixture) do(kind ActionKind, text string) Update {
	f.t.Helper()
	u, err := f.try(kind, text)
	if err != nil {
		f.t.Fatalf("%v %q: %v", kind, text, err)
	}
	return u
}

func (f *fixture) ok(a Action) Update {
	f.t.Helper()
	u, err := f.m.Act(testChannel, a)
	if err != nil {
		f.t.Fatalf("%+v: %v", a, err)
	}
	return u
}

// spinTo scripts the next spin and performs it.
func (f *fixture) spinTo(wedge int) Update {
	f.t.Helper()
	f.src.spins = append(f.src.spins, wedge)
	return f.do(Spin, "")
}

// earn spins 600 and calls a consonant that hits.
func (f *fixture) earn(letter string) Update {
	f.t.Helper()
	f.spinTo(w600)
	return f.do(Consonant, letter)
}

func seat(v View, id string) PlayerView {
	for _, p := range v.Players {
		if p.UserID == id {
			return p
		}
	}
	return PlayerView{}
}

func kinds(evs []Event) []EventKind {
	out := make([]EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func has(evs []Event, k EventKind) bool {
	for _, e := range evs {
		if e.Kind == k {
			return true
		}
	}
	return false
}

// A typo in the shipped file would otherwise shrink the pool quietly in production, and
// nobody reads a startup warning about a list that still loads.
func TestEmbeddedPuzzlesAllValid(t *testing.T) {
	p, err := LoadPuzzles("")
	if err != nil {
		t.Fatal(err)
	}
	if p.Rejected() != 0 {
		raw, _ := embeddedPuzzles.ReadFile("puzzles.txt")
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if _, ok := parseLine(line); !ok {
				t.Errorf("rejected: %q", line)
			}
		}
	}
	if p.Len() < 100 {
		t.Errorf("only %d puzzles; the list is meant to hold at least 100", p.Len())
	}
	if len(p.bonus) == 0 {
		t.Error("no puzzle is bonus-eligible")
	}
}

func TestLoadPuzzlesSkipsBadLinesAndCounts(t *testing.T) {
	good := []string{
		"phrase|touch grass",
		"food & drink|  a   bowl of   ramen ",
		"thing|rock & roll 2.0!",
	}
	bad := []string{
		"no pipe here",
		"a|b|c",
		"|empty category",
		"a category much too long to fit|some words",
		"phrase|café au lait",
		"phrase|pizza \U0001F355",
		"phrase|snake_case",
		"phrase|" + strings.Repeat("ab ", 20),
		"phrase|aeiou eau",
		"phrase|ab",
		"phrase|supercalifragilistic",
	}
	lines := append([]string{"# a comment", ""}, good...)
	lines = append(lines, bad...)
	p := puzzlesOf(t, lines...)
	if p.Len() != len(good) || p.Rejected() != len(bad) {
		t.Fatalf("Len=%d Rejected=%d, want %d and %d", p.Len(), p.Rejected(), len(good), len(bad))
	}
}

func TestAWordOverThirteenLettersIsRejected(t *testing.T) {
	p := puzzlesOf(t, "phrase|abcdefghijklm", "phrase|abcdefghijklmn", "phrase|o'clock-ish-ly")
	if p.Len() != 2 || p.all[0].Phrase != "ABCDEFGHIJKLM" || p.Rejected() != 1 {
		t.Fatalf("Len=%d Rejected=%d %+v", p.Len(), p.Rejected(), p.all)
	}
}

func TestLoadPuzzlesNoUsableIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.txt")
	if err := os.WriteFile(path, []byte("bad line\nphrase|ab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPuzzles(path)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "2 rejected") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadPuzzlesMissingFileIsError(t *testing.T) {
	if _, err := LoadPuzzles(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Fatal("a missing file loaded")
	}
}

func TestLoadPuzzlesUppercasesAndCollapsesSpaces(t *testing.T) {
	p := puzzlesOf(t, "  Food  &  Drink |  cold   pizza ")
	if got := p.all[0]; got.Phrase != "COLD PIZZA" || got.Category != "Food & Drink" {
		t.Fatalf("got %+v", got)
	}
}

func TestNilPuzzlesIsUnavailable(t *testing.T) {
	m := NewManager(nil, nil, Options{})
	if m.Available() {
		t.Fatal("available with no puzzles")
	}
	if _, err := m.Open(testGuild, testChannel, "a", "a"); !errors.Is(err, ErrNoPuzzles) {
		t.Fatalf("err = %v", err)
	}
}

func TestPunctuationAndDigitsAutoRevealed(t *testing.T) {
	f := newFixture(t, testOpts(), "thing|rock & roll 2.0!")
	f.begin("a")
	if got := f.view().Board; got != "____ & ____ 2.0!" {
		t.Fatalf("board = %q", got)
	}
}

func TestNormalizeSolve(t *testing.T) {
	want := normalizeSolve("YOU CAN'T WIN THEM ALL")
	for _, in := range []string{
		"you can't win them all",
		"  you   cant win them all ",
		"You can’t win them all",
		"you can‘t win them all!",
		"you-can't-win-them-all",
	} {
		if got := normalizeSolve(in); got != want {
			t.Errorf("%q normalized to %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"you can't win them", "you can't win them all x", "you cant wim them all"} {
		if normalizeSolve(in) == want {
			t.Errorf("%q matched", in)
		}
	}
}

func TestDrawDoesNotRepeatWithinMatch(t *testing.T) {
	p := puzzlesOf(t, "a|touch grass", "b|skill issue", "c|big if true", "d|let him cook")
	used := map[int]bool{}
	for range 4 {
		i := p.draw(DefaultSource{}, used, false)
		if used[i] {
			t.Fatalf("puzzle %d drawn twice", i)
		}
		used[i] = true
	}
	// Exhausted: it repeats rather than hanging or panicking.
	_ = p.draw(DefaultSource{}, used, false)
}

func TestBonusEligibleNeedsThreeHiddenAfterRSTLNE(t *testing.T) {
	cases := map[string]bool{
		"GOOD JOB":    true,  // G D J B O
		"STREET":      false, // nothing hidden
		"TOUCH GRASS": true,
		"ABBA":        false, // A B only
	}
	for phrase, want := range cases {
		if got := bonusEligible(phrase); got != want {
			t.Errorf("%s: %v, want %v", phrase, got, want)
		}
	}
}

func TestWedgeTableShape(t *testing.T) {
	if len(wedges) != 24 {
		t.Fatalf("%d wedges", len(wedges))
	}
	counts := map[WedgeKind]int{}
	for _, w := range wedges {
		counts[w.Kind]++
		if w.Kind == Gold && (w.Value <= 0 || w.Value%50 != 0) {
			t.Errorf("gold wedge %d", w.Value)
		}
	}
	if counts[Bankrupt] != 2 || counts[LoseTurn] != 1 {
		t.Fatalf("counts %v", counts)
	}
	if wedges[w600].Value != 600 || wedges[w2500].Value != 2500 ||
		wedges[wBankrupt].Kind != Bankrupt || wedges[wLoseTurn].Kind != LoseTurn {
		t.Fatal("the named test indexes no longer point where their names say")
	}
	for _, v := range bonusPrizes {
		if v <= 0 {
			t.Fatalf("bonus prize %d", v)
		}
	}
}

func TestSpinUsesSource(t *testing.T) {
	src := &script{spins: []int{wBankrupt}}
	if i, w := spin(src); i != wBankrupt || w.Kind != Bankrupt {
		t.Fatalf("spin = %d %+v", i, w)
	}
}

// Package wheel is a Wheel of Fortune match engine: a sign-up lobby, rounds of spin,
// call and solve, and a bonus round for whoever banked the most gold.
//
// It knows nothing about Discord, for the reason internal/wordgame does not: the Manager
// decides what happened and hands back a View of the board, and the caller renders it and
// sends it through the guard. That is what makes a full match testable without a gateway
// connection, and what keeps every repaint behind the same emit gate as everything else
// the bot says.
//
// # Three rules the integration relies on
//
//   - A Result is returned exactly once, from the same locked section that removes the
//     match. Gold is paid from the Result, so paying twice is not a check somebody has to
//     remember; it is not expressible.
//   - An error changes nothing. Every refusal maps to a private reply to the person who
//     pressed, and a refusal that half-applied a move would leave the card disagreeing
//     with the game.
//   - The Turn token changes whenever the turn changes hands, the round changes or the
//     phase changes, and every turn action carries the token it was pressed against. A
//     button press is a message that can arrive late; without the token, a click aimed at
//     a timed-out turn lands on whoever holds the next one, including the same player in
//     a solo match.
package wheel

import "math/rand/v2"

// Source is the randomness a match draws on: spins, seat order, puzzles and the bonus
// prize. It matches internal/wordgame's Source rather than importing it, because two games
// sharing a two-method interface is not worth coupling one feature to another.
type Source interface {
	IntN(n int) int
	Shuffle(n int, swap func(i, j int))
}

// DefaultSource is the production randomness: a stateless adapter over math/rand/v2's
// goroutine-safe top-level functions, so there is no shared generator to race on.
type DefaultSource struct{}

func (DefaultSource) IntN(n int) int                     { return rand.IntN(n) }
func (DefaultSource) Shuffle(n int, swap func(i, j int)) { rand.Shuffle(n, swap) }

// VowelCost is what buying a vowel takes from the round bank, hit or miss.
const VowelCost = 250

// WedgeKind says what a spin landed on.
type WedgeKind int

const (
	Gold WedgeKind = iota
	Bankrupt
	LoseTurn
)

// Wedge is one slice of the wheel. Value is meaningful only for Gold.
type Wedge struct {
	Kind  WedgeKind
	Value int
}

// wedges is the wheel, and it is a constant rather than configuration for the reason
// markov's weights are: the values only mean anything relative to each other and to
// VowelCost, and an operator has no instrument to judge them. TestWedgeTableShape pins it.
var wedges = []Wedge{
	{Gold, 2500}, {Gold, 600}, {Gold, 700}, {Gold, 600}, {Gold, 650}, {Gold, 500},
	{Gold, 700}, {Bankrupt, 0}, {Gold, 600}, {Gold, 550}, {Gold, 500}, {Gold, 600},
	{Bankrupt, 0}, {Gold, 650}, {Gold, 700}, {LoseTurn, 0}, {Gold, 800}, {Gold, 500},
	{Gold, 650}, {Gold, 500}, {Gold, 900}, {Gold, 300}, {Gold, 400}, {Gold, 450},
}

// bonusPrizes is the envelope the bonus round draws from, uniformly.
var bonusPrizes = []int{5000, 7500, 10000, 12500, 15000, 25000}

// spin draws a wedge and returns its index, which the View carries so a renderer can show
// where the wheel stopped.
func spin(src Source) (int, Wedge) {
	i := src.IntN(len(wedges))
	return i, wedges[i]
}

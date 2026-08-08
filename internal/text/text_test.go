package text

import (
	"slices"
	"strings"
	"testing"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"plain words lowercased", "Hello World", []string{"hello", "world"}},
		{"punctuation is not a token", "hi, there! ok?", []string{"hi", "there", "ok"}},
		{"digits", "top 10 birds", []string{"top", "10", "birds"}},
		{
			// URL casing is preserved because path segments are case-sensitive:
			// lowercasing one produces a link that looks right and 404s.
			"url keeps its case",
			"look at https://Example.com/Path/To/Thing",
			[]string{"look", "at", "https://Example.com/Path/To/Thing"},
		},
		{"steam protocol", "join steam://connect/1.2.3.4", []string{"join", "steam://connect/1.2.3.4"}},
		{"user mention", "hey <@1234> there", []string{"hey", "<@1234>", "there"}},
		{"nickname mention", "hey <@!1234>", []string{"hey", "<@!1234>"}},
		{"role mention", "hey <@&1234>", []string{"hey", "<@&1234>"}},
		{"channel mention", "see <#987>", []string{"see", "<#987>"}},
		{"custom emote", "nice <:kek:123>", []string{"nice", "<:kek:123>"}},
		{"animated emote", "nice <a:kek:123>", []string{"nice", "<a:kek:123>"}},
		{"shortcode survives tokenizing", "so :kek: yes", []string{"so", ":kek:", "yes"}},
		{"unicode emoji is a token", "bird \U0001F426 here", []string{"bird", "\U0001F426", "here"}},
		{"straight apostrophe", "don't stop", []string{"don't", "stop"}},
		{
			// THE load-bearing case. Discord clients substitute a curly
			// apostrophe as you type, so without the literal right single quote
			// in tokenRegex's character class this splits into "don" and "t" and
			// the corpus fills with fragments.
			"curly apostrophe stays one token",
			"don’t stop",
			[]string{"don’t", "stop"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Tokenize(c.in)
			if !slices.Equal(got, c.want) {
				t.Errorf("Tokenize(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestTokenizeCurlyApostropheRegression is deliberately separate and named for
// what it protects. The character it depends on is the one exception to this
// repository's plain-punctuation rule, and CI's prose check is configured not to
// scan for it precisely so this can exist. If someone "cleans up" the tokenizer,
// this is what should fail.
func TestTokenizeCurlyApostropheRegression(t *testing.T) {
	for _, word := range []string{"don’t", "can’t", "y’all", "it’s"} {
		got := Tokenize(word)
		if len(got) != 1 {
			t.Errorf("Tokenize(%q) split into %d tokens (%q); the curly apostrophe must be inside tokenRegex's character class", word, len(got), got)
		}
	}
}

func TestLowerExceptURLs(t *testing.T) {
	if got := LowerExceptURLs("SHOUTING"); got != "shouting" {
		t.Errorf("got %q, want %q", got, "shouting")
	}
	url := "https://Example.com/CaseSensitive"
	if got := LowerExceptURLs(url); got != url {
		t.Errorf("got %q, want the URL unchanged", got)
	}
}

func TestShortcode(t *testing.T) {
	if name, ok := Shortcode(":kek:"); !ok || name != "kek" {
		t.Errorf("Shortcode(\":kek:\") = %q, %v; want \"kek\", true", name, ok)
	}
	for _, notShortcode := range []string{"kek", ":kek", "kek:", "<:kek:123>", "::", ":a b:"} {
		if _, ok := Shortcode(notShortcode); ok {
			t.Errorf("Shortcode(%q) reported a match", notShortcode)
		}
	}
}

func TestSimilarity(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want float64
	}{
		{"both empty", "", "", 0},
		{"one empty", "bird", "", 0},
		{"identical", "the bird is loud", "the bird is loud", 1},
		{"disjoint", "aaa bbb", "ccc ddd", 0},
		// Union {the,bird} plus {the,cat} = 3 distinct, intersection {the} = 1.
		{"half overlap", "the bird", "the cat", 1.0 / 3.0},
		// Case and repetition are folded out by tokenizing into sets, which is
		// what makes this usable as a parrot check.
		{"case insensitive", "The Bird", "the bird", 1},
		{"repetition ignored", "bird bird bird", "bird", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Similarity(c.a, c.b)
			if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("Similarity(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// fakeEmoji is the whole reason CleanSentence takes an interface instead of a
// *discordgo.Session: the emote path is now testable without a gateway.
type fakeEmoji map[string]string

func (f fakeEmoji) ResolveEmoji(name string) (string, bool) {
	v, ok := f[name]
	return v, ok
}

func TestCleanSentence(t *testing.T) {
	emoji := fakeEmoji{"kek": "<:kek:999>"}

	cases := []struct {
		name  string
		in    string
		want  string
		emoji EmojiResolver
	}{
		{"empty", "", "", emoji},
		{"whitespace only", "   ", "", emoji},
		{"strips sentence punctuation", "well, that happened!", "well that happened", emoji},
		{
			// The bot has never been able to do this. The resolver walked
			// s.State.Guilds, which was always empty without IntentsGuilds.
			"resolves a known shortcode to a real emote",
			"that is :kek: honestly",
			"that is <:kek:999> honestly",
			emoji,
		},
		{
			"unknown shortcode is left as text",
			"that is :nope: honestly",
			"that is :nope: honestly",
			emoji,
		},
		{"nil resolver is safe", "that is :kek: honestly", "that is :kek: honestly", nil},
		{"NoEmoji resolves nothing", "that is :kek: honestly", "that is :kek: honestly", NoEmoji{}},
		{
			// Punctuation stripping must not touch a URL: a trailing comma
			// removed from inside a query string is a broken link.
			"url survives intact",
			"see https://example.com/a,b?c=1 now",
			"see https://example.com/a,b?c=1 now",
			emoji,
		},
		{"mentions pass through", "hi <@123> and <#456>", "hi <@123> and <#456>", emoji},
		{"resolved emotes pass through", "yes <a:kek:123>", "yes <a:kek:123>", emoji},
		{
			// Stuttering is a Markov artifact and goes.
			"collapses immediate duplicates",
			"very very good",
			"very good",
			emoji,
		},
		{
			// Non-adjacent repetition is the memetic register this bot exists for
			// and must survive. "ratio ratio ratio" collapsing to "ratio" would
			// be a quality regression, not a fix.
			"keeps non-adjacent repetition",
			"ratio bozo ratio bozo ratio",
			"ratio bozo ratio bozo ratio",
			emoji,
		},
		{"x.com becomes fxtwitter", "look https://x.com/u/status/1", "look https://fxtwitter.com/u/status/1", emoji},
		{"twitter.com becomes fxtwitter", "look https://twitter.com/u/status/1", "look https://fxtwitter.com/u/status/1", emoji},
		{"other hosts untouched", "look https://example.com/x.com/", "look https://example.com/x.com/", emoji},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CleanSentence(c.in, c.emoji); got != c.want {
				t.Errorf("CleanSentence(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestCleanSentenceKeepsMemeticRepetition is separate because it is a quality
// property rather than a correctness one, and the temptation to "fix" it by
// deduplicating harder is real. SPEC.md section 5.3 explains why not.
func TestCleanSentenceKeepsMemeticRepetition(t *testing.T) {
	in := "ratio ratio ratio"
	got := CleanSentence(in, NoEmoji{})
	// Adjacent duplicates do collapse, so this becomes one "ratio". That is the
	// documented behavior; what must NOT happen is the interleaved case above
	// losing its repeats.
	if got != "ratio" {
		t.Errorf("CleanSentence(%q) = %q, want %q", in, got, "ratio")
	}

	interleaved := "cope seethe cope seethe cope"
	if got := CleanSentence(interleaved, NoEmoji{}); got != interleaved {
		t.Errorf("interleaved repetition must survive: CleanSentence(%q) = %q", interleaved, got)
	}
	if strings.Count(CleanSentence(interleaved, NoEmoji{}), "cope") != 3 {
		t.Error("the copypasta cadence was flattened, which is a quality regression")
	}
}

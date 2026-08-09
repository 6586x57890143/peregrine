package wordgame

import (
	"testing"
	"unicode/utf8"
)

// TestTruncateRunes pins the fix for a bug that was visible to every user of the
// leaderboards. Both formatters used to do `if len(username) > 15 {
// username = username[:14] + "..." }`, where len() counts bytes and [:14]
// slices bytes. Discord nicknames are routinely neither ASCII nor short, so any
// nickname with an emoji or an accented character landing on the boundary was
// cut mid-rune, and the leaderboard rendered a replacement character.
func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under the limit is untouched", "short", 15, "short"},
		{"exactly at the limit is untouched", "123456789012345", 15, "123456789012345"},
		{"ascii over the limit", "1234567890123456", 15, "123456789012..."},
		// The regression case: byte slicing at 14 lands in the middle of a
		// 4-byte emoji and produces invalid UTF-8.
		{"emoji at the boundary", "aaaaaaaaaaaaaa\U0001F926bbbb", 15, "aaaaaaaaaaaa..."},
		{"all emoji", "\U0001F600\U0001F601\U0001F602\U0001F603\U0001F604", 3, "\U0001F600\U0001F601\U0001F602"},
		{"accented over the limit", "ààààààààààààààààà", 15, "àààààààààààà..."},
		{"empty stays empty", "", 15, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateRunes(tt.in, tt.max)
			if got != tt.want {
				t.Errorf("TruncateRunes(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
			// The property that actually matters, independent of the exact
			// expected strings above: output is always valid UTF-8 and never
			// longer than the limit in runes.
			if !utf8.ValidString(got) {
				t.Errorf("TruncateRunes(%q, %d) produced invalid UTF-8: %q", tt.in, tt.max, got)
			}
			if n := utf8.RuneCountInString(got); n > tt.max {
				t.Errorf("TruncateRunes(%q, %d) returned %d runes, over the limit", tt.in, tt.max, n)
			}
		})
	}
}

// TestTruncateRunesNeverSplitsARune is the same invariant as a sweep rather than
// a table, across every cut position in a string of multi-byte runes. This is
// the shape of test that would have caught the original bug without anyone
// having to guess which nickname triggered it.
func TestTruncateRunesNeverSplitsARune(t *testing.T) {
	const s = "aé\U0001F600b\U0001F926cç\U0001F680dñ\U0001F308e"
	for max := 0; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateRunes(s, max)
		if !utf8.ValidString(got) {
			t.Errorf("max=%d produced invalid UTF-8: %q", max, got)
		}
		if n := utf8.RuneCountInString(got); n > max {
			t.Errorf("max=%d returned %d runes", max, n)
		}
	}
}

// TestLoadDictionaryUsesEmbedded covers the other M0 fix: the dictionary used to
// be read from the relative path "wordgames/dictionary.txt" and a failure was
// log.Fatalf, so the bot only started from the repo root and a missing 64 KB
// word list took down every unrelated feature with it. An empty path now means
// the embedded copy, which cannot go missing at runtime.
func TestLoadDictionaryUsesEmbedded(t *testing.T) {
	d, err := LoadDictionary("", DictionaryOptions{})
	if err != nil {
		t.Fatalf("LoadDictionary(\"\") on the embedded dictionary: %v", err)
	}
	if d.Len() == 0 {
		t.Fatal("embedded dictionary loaded but produced no words")
	}
	for _, w := range d.words {
		if n := utf8.RuneCountInString(w); n < 5 || n > 12 {
			t.Fatalf("word list contains %q with %d runes, outside the default bounds", w, n)
		}
	}
}

// TestLoadDictionaryMissingFileErrors asserts an explicit override that does not
// exist reports an error rather than silently leaving the previous list in
// place. The caller turns this into "word games disabled" plus a warning, never
// into process exit.
func TestLoadDictionaryMissingFileErrors(t *testing.T) {
	if _, err := LoadDictionary("testdata/does-not-exist.txt", DictionaryOptions{}); err == nil {
		t.Fatal("expected an error for a missing dictionary override, got nil")
	}
}

// Package text is the tokenizer and the string handling around it.
//
// It is a leaf: it imports nothing from this module and must stay that way, so
// the tokenizer that decides what the bot learns can be tested without a
// database, a Discord session, or a config. Everything here is a pure function
// except Interner, which is explicitly per-call state.
package text

import (
	"regexp"
	"strings"
)

// tokenRegex is the single decision about what counts as a token, and therefore
// about what the bot can ever learn or say.
//
// The literal right single quote inside the final character class is
// LOAD-BEARING and the one deliberate exception to this repository's plain
// punctuation rule, which CI otherwise enforces. Discord clients substitute a
// curly apostrophe as you type, so without it "don't" tokenizes as "don" plus
// "t" and the corpus fills with fragments. The prose check deliberately does not
// scan for this character. Do not "clean it up".
var tokenRegex = regexp.MustCompile(`(?:https?|steam):\/\/\S+|<@!?&?\d+>|<#\d+>|<a?:\w+:\d+>|:\w+:|[\p{L}\p{N}\p{So}'’]+`)

// The remaining patterns classify an already-extracted token. All four are
// package-level because they used to be compiled per call, and one of them
// (the punctuation stripper in CleanSentence) was compiled once per token per
// sentence, which is a regex compile on the hot path of every reply.
var (
	urlRegex        = regexp.MustCompile(`^(?:https?|steam):\/\/\S+$`)
	emoteRegex      = regexp.MustCompile(`^<a?:\w+:\d+>$`)
	shortcodeRegex  = regexp.MustCompile(`^:(\w+):$`)
	wordPunctuation = regexp.MustCompile(`[.,!?]`)
)

// Tokenize splits a message into tokens, lowercasing everything except URLs.
//
// URLs keep their case because path segments are case-sensitive: lowercasing one
// produces a link that looks right and 404s.
func Tokenize(msg string) []string {
	tokens := tokenRegex.FindAllString(msg, -1)
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		out = append(out, LowerExceptURLs(token))
	}
	return out
}

// LowerExceptURLs lowercases a single token unless it is a URL.
func LowerExceptURLs(token string) string {
	if urlRegex.MatchString(token) {
		return token
	}
	return strings.ToLower(token)
}

// IsURL reports whether a token is a URL, for callers that need the distinction
// without re-deriving the pattern.
func IsURL(token string) bool { return urlRegex.MatchString(token) }

// IsEmote reports whether a token is a fully resolved Discord custom emote, the
// <a:name:id> form rather than a bare :shortcode:.
func IsEmote(token string) bool { return emoteRegex.MatchString(token) }

// Shortcode returns the name inside a :shortcode: token, and whether the token
// was one.
func Shortcode(token string) (string, bool) {
	m := shortcodeRegex.FindStringSubmatch(token)
	if len(m) != 2 {
		return "", false
	}
	return m[1], true
}

// Similarity is a Jaccard index over token sets: the size of the intersection
// over the size of the union. Used to reject a generated sentence that is too
// close to the prompt, which reads as the bot parroting rather than replying.
func Similarity(a, b string) float64 {
	setA := tokenSet(a)
	setB := tokenSet(b)

	inter := 0
	for w := range setA {
		if _, ok := setB[w]; ok {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func tokenSet(s string) map[string]struct{} {
	tokens := Tokenize(s)
	set := make(map[string]struct{}, len(tokens))
	for _, w := range tokens {
		set[w] = struct{}{}
	}
	return set
}

// EmojiResolver turns a :shortcode: into a Discord emote reference such as
// <a:name:id>, or returns false if the guild has no such emote.
//
// An interface rather than a *discordgo.Session because it is the only thing
// CleanSentence needed the session for, and taking the session would drag
// discordgo into a leaf package and make the sentence cleaner untestable. The
// consumer declares the minimal interface; the concrete type satisfies it
// structurally, which is the seam pattern the rest of the restructure uses.
type EmojiResolver interface {
	ResolveEmoji(name string) (string, bool)
}

// NoEmoji resolves nothing. Used where no session is available and by tests that
// are not about emotes.
type NoEmoji struct{}

// ResolveEmoji always reports that the shortcode is unknown.
func (NoEmoji) ResolveEmoji(string) (string, bool) { return "", false }

// CleanSentence normalizes a generated sentence for posting: it resolves emote
// shortcodes, preserves URLs and mentions, strips sentence punctuation from
// ordinary words, and collapses immediate duplicates.
//
// The duplicate collapsing is deliberately only immediate (adjacent) and
// deliberately not aggressive. Memetic repetition is the register this bot exists
// to produce, so "ratio ratio ratio" has to survive; what has to go is the
// stuttering artifact of a Markov chain re-picking the same token twice in a row.
func CleanSentence(str string, emoji EmojiResolver) string {
	str = strings.TrimSpace(str)
	if str == "" {
		return str
	}
	if emoji == nil {
		emoji = NoEmoji{}
	}

	tokens := Tokenize(str)
	cleaned := make([]string, 0, len(tokens))
	var last string

	appendUnlessRepeat := func(token string) {
		if token != last {
			cleaned = append(cleaned, token)
			last = token
		}
	}

	for _, token := range tokens {
		// A :shortcode: the server actually has becomes a real emote reference.
		// Until M3 added the GUILDS intent this could never succeed, because the
		// guild state it reads was always empty.
		if name, ok := Shortcode(token); ok {
			resolved := LowerExceptURLs(token)
			if emote, found := emoji.ResolveEmoji(name); found {
				resolved = emote
			}
			appendUnlessRepeat(resolved)
			continue
		}

		// URLs, mentions and resolved emotes pass through untouched: stripping
		// punctuation from a URL breaks it, and mangling a mention leaves visible
		// markup in the output.
		if IsURL(token) || strings.HasPrefix(token, "<@") || strings.HasPrefix(token, "<#") || IsEmote(token) {
			appendUnlessRepeat(rewriteEmbed(token))
			continue
		}

		if word := wordPunctuation.ReplaceAllString(token, ""); word != "" {
			appendUnlessRepeat(word)
		}
	}

	return strings.Join(cleaned, " ")
}

// rewriteEmbed swaps x.com and twitter.com for fxtwitter.com, which is the only
// host rewrite here and exists because Discord stopped rendering previews for the
// originals. A link nobody can see is not a repost worth making.
func rewriteEmbed(token string) string {
	if !IsURL(token) {
		return token
	}
	token = strings.Replace(token, "://x.com/", "://fxtwitter.com/", 1)
	token = strings.Replace(token, "://twitter.com/", "://fxtwitter.com/", 1)
	return token
}

package text

// StopWords are the English function words excluded from associative learning and from
// prompt-topic extraction.
//
// One list, unexported, so it cannot be mutated by a consumer and cannot drift into two
// copies. It is PLAIN ENGLISH FUNCTION WORDS ONLY, and that limitation is load-bearing
// rather than an oversight: it is half the reason clustering collapsed (SPEC.md finding
// 29). In a meme server the highest-count associations of every name are "lol", "bird" and
// "ratio", none of which this list excludes, so any algorithm that treats "shares a
// high-count association" as similarity finds every pair similar. Extending the list with
// server-specific filler would change what the corpus learns, so do not do it casually.
var stopWords = map[string]struct{}{
	"a": {}, "about": {}, "above": {}, "after": {}, "again": {}, "against": {}, "all": {}, "am": {}, "an": {}, "and": {}, "any": {}, "are": {}, "as": {}, "at": {},
	"be": {}, "because": {}, "been": {}, "before": {}, "being": {}, "below": {}, "between": {}, "both": {}, "but": {}, "by": {},
	"can": {}, "could": {}, "did": {}, "do": {}, "does": {}, "doing": {}, "down": {}, "during": {},
	"each": {}, "few": {}, "for": {}, "from": {}, "further": {},
	"had": {}, "has": {}, "have": {}, "having": {}, "he": {}, "he'd": {}, "he'll": {}, "he's": {}, "her": {}, "here": {}, "here's": {}, "hers": {}, "herself": {}, "him": {}, "himself": {}, "his": {}, "how": {}, "how's": {},
	"i": {}, "i'd": {}, "i'll": {}, "i'm": {}, "i've": {}, "if": {}, "in": {}, "into": {}, "is": {}, "it": {}, "it's": {}, "its": {}, "itself": {},
	"let's": {}, "me": {}, "more": {}, "most": {}, "my": {}, "myself": {},
	"no": {}, "nor": {}, "not": {}, "of": {}, "off": {}, "on": {}, "once": {}, "only": {}, "or": {}, "other": {}, "ought": {}, "our": {}, "ours": {}, "ourselves": {}, "out": {}, "over": {}, "own": {},
	"same": {}, "she": {}, "she'd": {}, "she'll": {}, "she's": {}, "should": {}, "so": {}, "some": {}, "such": {},
	"than": {}, "that": {}, "that's": {}, "the": {}, "their": {}, "theirs": {}, "them": {}, "themselves": {}, "then": {}, "there": {}, "there's": {}, "these": {}, "they": {}, "they'd": {}, "they'll": {}, "they're": {}, "they've": {}, "this": {}, "those": {}, "through": {}, "to": {}, "too": {},
	"under": {}, "until": {}, "up": {}, "very": {},
	"was": {}, "we": {}, "we'd": {}, "we'll": {}, "we're": {}, "we've": {}, "were": {}, "what": {}, "what's": {}, "when": {}, "when's": {}, "where": {}, "where's": {}, "which": {}, "while": {}, "who": {}, "who's": {}, "whom": {}, "why": {}, "why's": {}, "with": {}, "would": {},
	"you": {}, "you'd": {}, "you'll": {}, "you're": {}, "you've": {}, "your": {}, "yours": {}, "yourself": {}, "yourselves": {},
}

// danglingTails are the function words a sentence cannot END on without reading as though
// it was cut off mid-thought.
//
// A SEPARATE LIST FROM stopWords, deliberately, because they answer different questions and
// merging them would be wrong in both directions. stopWords asks "does this token carry a
// topic", which is about the association indexes; this asks "would a reader expect another
// word after it", which is about where a sentence may stop. The two disagree on exactly the
// words that matter: "it" is a stop word and ends a sentence perfectly ("i am going to lose
// it"), while "about" is a stop word that cannot end one unless the phrase before it is
// already complete.
//
// The failure this exists to catch, found by reading golden samples rather than by reasoning:
// nearly a third of generated replies ended on a trailing preposition. The engine was right
// to allow it by its own rule, which is what made it invisible. "about" is followed by the
// end sentinel in the corpus, by two different authors, because two people ended a message
// with "what are you talking about". So generation ended "nurock is coping about" and the
// end-token exemption in TrimDangling protected it: the corpus attested that the TOKEN can
// end a message, and threw away the construction that made it true.
//
// Prepositions, conjunctions, determiners and auxiliaries only. Pronouns and most adverbs
// are absent on purpose, since they genuinely close sentences in this register.
var danglingTails = map[string]struct{}{
	// prepositions
	"about": {}, "above": {}, "across": {}, "after": {}, "against": {}, "along": {}, "among": {},
	"around": {}, "at": {}, "before": {}, "behind": {}, "below": {}, "beneath": {}, "beside": {},
	"between": {}, "beyond": {}, "by": {}, "despite": {}, "during": {}, "except": {}, "for": {},
	"from": {}, "in": {}, "inside": {}, "into": {}, "near": {}, "of": {}, "off": {}, "on": {},
	"onto": {}, "outside": {}, "over": {}, "past": {}, "per": {}, "than": {}, "through": {},
	"throughout": {}, "to": {}, "toward": {}, "towards": {}, "under": {}, "underneath": {},
	"until": {}, "upon": {}, "with": {}, "within": {}, "without": {},

	// conjunctions and subordinators
	"and": {}, "but": {}, "or": {}, "nor": {}, "because": {}, "although": {}, "though": {},
	"unless": {}, "whereas": {}, "whether": {}, "while": {}, "since": {},

	// determiners and possessives
	"a": {}, "an": {}, "the": {}, "this": {}, "that": {}, "these": {}, "those": {}, "my": {},
	"your": {}, "his": {}, "her": {}, "its": {}, "our": {}, "their": {}, "every": {}, "each": {},
	"another": {}, "such": {},

	// auxiliaries and copulas
	"am": {}, "is": {}, "are": {}, "was": {}, "were": {}, "be": {}, "been": {}, "being": {},
	"has": {}, "have": {}, "had": {}, "having": {}, "does": {}, "do": {}, "did": {}, "doing": {},
	"will": {}, "would": {}, "shall": {}, "should": {}, "can": {}, "could": {}, "may": {},
	"might": {}, "must": {}, "ought": {},
}

// IsDanglingTail reports whether ending a sentence on this lowercased token would leave it
// hanging. See danglingTails for why this is not IsStopWord.
func IsDanglingTail(lower string) bool {
	_, ok := danglingTails[lower]
	return ok
}

// determiners bind rightward to the noun they introduce, so nothing may be inserted after
// one. "the tbh bird" is the failure; see markov.safeInsertPositions.
var determiners = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "this": {}, "that": {}, "these": {}, "those": {},
	"my": {}, "your": {}, "his": {}, "her": {}, "its": {}, "our": {}, "their": {},
	"every": {}, "each": {}, "another": {}, "some": {}, "any": {}, "no": {},
}

// IsDeterminer reports whether a lowercased token introduces a noun phrase.
func IsDeterminer(lower string) bool {
	_, ok := determiners[lower]
	return ok
}

// conjunctions cannot begin a reply, because they promise a clause that was never there.
//
// The mirror of danglingTails and deliberately much smaller. A reply may open on a
// determiner ("the bird is loose"), a pronoun ("it knows what it did"), an interrogative
// ("what do you mean") or an auxiliary ("is anyone awake"), and all of those read fine. What
// does not read fine is opening on a word whose whole job is to join this clause to a
// previous one: seed selection produced "and lachy are you know what i am going to lose it"
// from the prompt window "and lachy are", which reads as the first half going missing.
//
// Prepositions are deliberately NOT here, even though they are function words. "at this hour
// is a mistake" opens perfectly well as a chat fragment, and excluding them would cost real
// output for no gain.
var conjunctions = map[string]struct{}{
	"and": {}, "but": {}, "or": {}, "nor": {}, "because": {}, "although": {}, "though": {},
	"unless": {}, "whereas": {}, "since": {}, "than": {}, "yet": {},
}

// CanOpenSentence reports whether a reply may begin on this lowercased token.
func CanOpenSentence(lower string) bool {
	_, ok := conjunctions[lower]
	return !ok
}

// pronouns are the tokens that can close a sentence, but only when something governs them.
//
// They are deliberately NOT in danglingTails, because "i am going to lose it" is a perfectly
// good line and trimming its last word would be wrong. Whether a trailing pronoun dangles
// depends on the word before it, which is what IsGovernedPronoun answers.
var pronouns = map[string]struct{}{
	"i": {}, "me": {}, "my": {}, "mine": {}, "myself": {},
	"you": {}, "your": {}, "yours": {}, "yourself": {}, "yourselves": {},
	"he": {}, "him": {}, "his": {}, "himself": {},
	"she": {}, "her": {}, "hers": {}, "herself": {},
	"it": {}, "its": {}, "itself": {},
	"we": {}, "us": {}, "our": {}, "ours": {}, "ourselves": {},
	"they": {}, "them": {}, "their": {}, "theirs": {}, "themselves": {},
	"this": {}, "that": {}, "these": {}, "those": {},
}

// IsGovernedPronoun reports whether a trailing pronoun is doing a job, given the word in
// front of it. Both arguments are already lowercased.
//
// A pronoun needs a governor, and a function word is not one. "lose it" is a verb and its
// object and closes a sentence fine; "are you" and "knows what it" are a function word
// followed by a pronoun that is waiting for a verb that never arrives, and both turned up in
// golden samples reading as though the message was cut off.
//
// Returns true when the token is not a pronoun at all, because then the question does not
// arise and the caller should leave it alone.
func IsGovernedPronoun(prev, tok string) bool {
	if _, ok := pronouns[tok]; !ok {
		return true
	}
	if prev == "" {
		return false
	}
	return !IsStopWord(prev)
}

// IsStopWord reports whether a lowercased token is an English function word.
//
// Callers pass an already-lowercased token, because every caller has one: the learn path
// and the generation path both lowercase before they get here, and lowercasing again per
// call would be work on the hot path for no change in answer.
func IsStopWord(lower string) bool {
	_, ok := stopWords[lower]
	return ok
}

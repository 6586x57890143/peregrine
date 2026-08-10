// Package generate turns a Discord prompt into a sentence.
//
// It is the glue between internal/markov, which owns the probability model and every
// heuristic, and the features that want something said. What lives here is the part that
// is genuinely not the engine's: reading the configured dials, resolving the names in a
// prompt, opening ONE read transaction for the whole attempt, walking the step loop, and
// cleaning the result for Discord.
//
// # One transaction, and no cached Generator
//
// A markov.Generator holds a Corpus, which in production is a *storage.Reader bound to one
// transaction. So a Generator must not outlive its transaction, and there is deliberately
// no package-level one: Sentence constructs a fresh Generator inside each store.View, which
// costs a struct allocation per reply. Caching it in a package variable would reintroduce
// exactly the class of bug the Reader type exists to make unwritable.
//
// That whole shape is finding 1's fix. Generation used to run inside a db.View and call
// helpers that each opened their own db.View: bbolt holds mmaplock.RLock for a read
// transaction's entire life and takes the write lock to grow the mmap, and Go's RWMutex
// queues new readers behind a waiting writer, so outer-read plus waiting-writer plus
// inner-read is a deadlock with no timeout and no recovery. The version that could nest
// does not compile now, because a Reader has no method that starts a transaction.
//
// # There is no emit gate here
//
// There used to be, at the single exit from this path, and it moved into
// internal/discordguard in M10a. That was a move rather than an addition: generation is not
// the only thing that produces text (the transcription results were the worst case), so the
// gate belongs at the send chokepoint every path traverses. Leaving a copy here would
// invite the belief that a producer gates itself, which is how the autonomous poster, the
// word-game announcements and the transcripts all reached Discord ungated in the first
// place.
package generate

import (
	"log"
	"math"
	"strings"

	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/markov"
	"github.com/6586x57890143/peregrine/internal/names"
	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/text"
)

// Options are the dials this path reads, all of them from the environment via
// internal/config.
//
// The thirteen logit weights are NOT here, deliberately: they are only meaningful relative
// to each other and an operator has no instrument to judge them, so they are constants in
// internal/markov tuned by reading golden samples. These are the dials SPEC.md section 5.3
// says belong to the operator.
type Options struct {
	MaxNGram           int
	MinWords           int
	MaxWords           int
	Temperature        float64
	TopK               int
	TopP               float64
	KNDiscount         float64
	KNRawMix           float64
	MinDistinctAuthors int
	PromptRelevance    float64
	RoastChance        float64
}

// Params maps the configured dials onto the engine's.
//
// Every field here is read by code that exists, which is the rule internal/config runs on:
// a knob wired to nothing is worse than no knob, because an operator tunes it during an
// incident, nothing happens, and the bot gets blamed for ignoring its configuration.
func (o Options) Params() markov.Params {
	return markov.Params{
		MaxNGram:           o.MaxNGram,
		Temperature:        o.Temperature,
		TopK:               o.TopK,
		TopP:               o.TopP,
		KNDiscount:         o.KNDiscount,
		KNRawMix:           o.KNRawMix,
		MinDistinctAuthors: o.MinDistinctAuthors,
		PromptRelevance:    o.PromptRelevance,
	}
}

// EmojiResolver turns a :shortcode: into a Discord emote reference. Satisfied structurally
// by the session adapter in the caller, which is what keeps discordgo out of here.
type EmojiResolver = text.EmojiResolver

// Generator produces sentences from a corpus.
type Generator struct {
	store *storage.Store
	opts  Options
}

// New returns a Generator. It holds the Store rather than a Reader, because it opens its
// own transaction per sentence and a Reader cannot be held across one.
func New(store *storage.Store, opts Options) *Generator {
	return &Generator{store: store, opts: opts}
}

// Outcome says WHY Sentence produced nothing, which the caller needs in order to log
// something an operator can act on.
//
// An empty string with a nil error used to cover three different situations, and the
// difference between them is the difference between "wait" and "your configuration is
// wrong". A bot that had learned 27 messages and stayed quiet reported nothing at all, and
// working out which of the three it was took reading the source (SPEC.md section 8, finding
// 32).
type Outcome int

const (
	// Produced means there is a sentence.
	Produced Outcome = iota

	// CorpusEmpty means nothing has been learned yet, so there was nothing to say. On a
	// fresh deploy this is the expected answer until ingestion has run.
	CorpusEmpty

	// TooShort means every re-seed dead-ended below the two-word floor.
	//
	// On a YOUNG corpus this usually means the author-diversity gate is doing its job:
	// PEREGRINE_MIN_DISTINCT_AUTHORS defaults to 2, so until several people have said
	// similar things almost no continuation is eligible and the walk dies immediately. It is
	// the single most likely reason a freshly deployed bot is mute, and it is not a fault.
	TooShort
)

// String names the outcome for a log field.
func (o Outcome) String() string {
	switch o {
	case Produced:
		return "produced"
	case CorpusEmpty:
		return "corpus-empty"
	case TooShort:
		return "too-short"
	}
	return "unknown"
}

// Sentence generates a reply to prompt, steered by mem, and returns "" with a reason when
// there is nothing worth saying.
//
// Returning empty is a normal outcome rather than a failure: an empty corpus, a young one
// where the author-diversity gate refuses everything, or a dead-ended seed all produce it.
// The caller stays silent, which is what this bot does anyway whenever it decides not to
// answer. What the caller must NOT do is stay silent in the log as well: the bot's silence
// is a feature and the operator's is a bug.
func (g *Generator) Sentence(prompt string, roast bool, mem *Memory, emoji EmojiResolver) (string, Outcome, error) {
	// No <START> sentinel here any more, and it was never a fallback in the first place. The
	// learn path only ever appends <end> and never prepends a start token, so nothing in the
	// corpus follows "<START>": tiers 1 and 5 could not match it, and seed selection already
	// fell through to FirstPrefix exactly as it does for an empty prompt. It was a token that
	// looked like it did something.
	promptWords := text.Tokenize(prompt)

	var recentWords []string
	if mem != nil {
		recentWords = mem.WeightedWords()
	}

	var (
		sentence        []string
		recognizedNames []string
		empty           bool
	)

	// ONE read transaction for the whole attempt, and genuinely one: everything inside
	// used to reach back for its own. Reader.CorpusEmpty replaces a Bucket.Stats() call
	// that walked every page in the largest bucket in the database on every reply purely to
	// answer "is there anything in here" (finding 11).
	err := g.store.View(func(r *storage.Reader) error {
		if r.CorpusEmpty() {
			// Recorded rather than inferred from the empty sentence below, because a
			// dead-ended walk on a populated corpus produces the same empty slice and the
			// two need different advice.
			empty = true
			return nil
		}

		// Up to three attempts, keeping the longest, and this is a RE-SEED rather than the
		// discard-and-retry M7b deleted. The distinction matters because they look alike:
		// the old mechanism threw away an end token and continued from the same prefix,
		// which fought the length decision, whereas this abandons the whole attempt and
		// draws a different seed.
		//
		// The failure mode it answers showed up in the golden samples rather than in
		// reasoning, which is the point of reading them. A seed drawn from a non-prompt
		// tier can dead-end on its very first step, because the length floor is a logit
		// penalty on the end token and a penalty does nothing when no candidate is eligible
		// at all. The result was one-word replies like "roof". A short reply lands; a
		// one-word non-sequitur reads as the bot malfunctioning.
		//
		// Attempts are cheap: they share this transaction, and the corpus reads they repeat
		// are the ones storage has cheap answers for.
		const attempts = 3
		for i := range attempts {
			words, found := g.attempt(r, promptWords, recentWords, prompt, roast)
			if len(words) > len(sentence) {
				sentence, recognizedNames = words, found
			}
			if len(sentence) >= g.opts.MinWords {
				break
			}
			if i == attempts-1 {
				log.Printf("[INFO] generation reached only %d words in %d attempts for prompt %q",
					len(sentence), attempts, prompt)
			}
		}
		return nil
	})
	if err != nil {
		return "", Produced, err
	}
	if empty {
		return "", CorpusEmpty, nil
	}

	// Below two words there is nothing worth posting. One word is not a punchy reply, it is
	// a reply that looks broken, and silence is indistinguishable from the bot choosing not
	// to answer, which it does all the time anyway.
	if len(sentence) < 2 {
		return "", TooShort, nil
	}

	final := strings.Join(sentence, " ")
	final = strings.ReplaceAll(final, markov.EndToken, "")
	final = text.CleanSentence(final, emoji)

	// The persona post-pass. One mechanism with the in-sampler lexicon bias, rather than
	// two independent coin flips (finding G6), and its insertion point comes from a
	// triangular draw concentrated mid-sentence, because a flat draw over the interior put
	// filler at the edges where an interjection reads as a typo.
	persona := markov.PersonaNeutral
	if roast {
		persona = markov.PersonaRoast
	}
	return markov.Style(nil, markov.DefaultWeights(), final, persona, len(recognizedNames) > 0), Produced, nil
}

// attempt drives one sentence out of the engine and returns it with the names it
// recognized in the prompt.
func (g *Generator) attempt(r *storage.Reader, promptWords, recentWords []string, prompt string, roast bool) ([]string, []string) {
	engine := markov.New(r, g.opts.Params(), nil)

	var recognized []string
	// BOTH SPELLINGS are collected, and that is the point rather than belt and braces. The
	// canonical form is what the name-topic index is keyed by, and the surface form is what
	// people typed and therefore what the n-gram index actually learned, so a seed tier and a
	// scoring bonus that only knew one of them would miss half the time.
	promptNames := map[string]struct{}{}
	var nameTokens []string
	for _, word := range promptWords {
		name, ok := names.Canonical(r, word)
		if !ok {
			continue
		}
		recognized = append(recognized, name)
		for _, spelling := range []string{name, word} {
			if _, dup := promptNames[spelling]; dup {
				continue
			}
			promptNames[spelling] = struct{}{}
			nameTokens = append(nameTokens, spelling)
		}
	}

	// Core topics: the prompt's non-stop words, weighted by the log of their global count
	// so significance does not grow too quickly.
	//
	// Keyed by the word itself rather than by a text.Interner id. The interner is gone from
	// this path, and that is a simplification the engine bought rather than a change in
	// behaviour: the ids only ever existed to key maps within one attempt, they were never
	// persisted, and the engine's Corpus interface speaks in strings. Nothing may persist
	// an interner id, and having none here is the strongest form of that.
	coreTopics := make(map[string]float64, len(promptWords))
	for _, word := range promptWords {
		if text.IsStopWord(word) {
			continue
		}
		coreTopics[word] = math.Log(float64(r.TopicCount(word)) + 1)
	}

	assoc := make(map[string]corpus.TopicAssoc)
	for _, nm := range recognized {
		key := text.LowerExceptURLs(nm)
		found, err := r.NameTopicsFor(key)
		if err != nil {
			log.Printf("[WARN] name-topic lookup for %s failed: %v", key, err)
			continue
		}
		for word, data := range found {
			existing := assoc[word]
			existing.Count += data.Count
			existing.PosSum += data.PosSum
			assoc[word] = existing
		}
	}

	// Seed selection is the engine's, and the two-hop tier inside it is what replaces the
	// concept-cluster tier that had never once fired (finding 29).
	seedIn := markov.SeedInput{
		PromptWords: promptWords,
		RecentWords: recentWords,
		Names:       recognized,
		NameTokens:  nameTokens,
	}
	seed := engine.Seed(seedIn)
	if seed == "" {
		// The corpus offered nothing. A prompt word beats a sentinel: at worst it echoes,
		// and at best it has continuations the seed tiers happened not to rank.
		if len(promptWords) == 0 {
			return nil, recognized
		}
		seed = promptWords[0]
	}

	words := text.Tokenize(seed)

	// ONE length model, replacing three mechanisms that competed: an end-token multiplier
	// below 40% progress, a discard-and-retry, and a 30 + rand(15) loop bound. The target
	// is sampled per sentence and skewed short, and the engine shifts the end-token logit
	// against it, so there is a single answer to how long this should be (finding G7).
	length := markov.NewLength(markov.DefaultSource{}, g.opts.MinWords, g.opts.MaxWords)

	step := &markov.Step{
		Prefix:       append([]string{}, words...),
		Sentence:     append([]string{}, words...),
		Prompt:       prompt,
		PromptSet:    wordSet(promptWords),
		RecentSet:    wordSet(recentWords),
		Used:         make(map[string]int, length.Max),
		Ngrams:       make(map[string]struct{}, length.Max*3),
		Length:       length,
		CoreTopics:   coreTopics,
		CurrentTopic: markov.SeedTopic(seed),
		NameAssoc:    assoc,
		PromptNames:  promptNames,
	}
	if roast {
		step.Persona = markov.PersonaRoast
	}
	for _, w := range words {
		step.Used[w]++
	}

	// chose records whether the sentence ended because the model picked the end sentinel, as
	// opposed to running out of chain or hitting the cap. TrimDangling takes it because the
	// two endings are different claims: somebody really did end a message on that word, which
	// is evidence, whereas a sentence that merely stopped is not. It trims a trailing
	// preposition either way, because that attestation was about a construction the composite
	// key layout does not record.
	chose := false

	for !length.Done(len(step.Sentence)) {
		if len(step.Prefix) == 0 {
			break
		}
		step.Position = float64(len(step.Sentence)) / float64(length.Max)

		next, err := engine.Next(step)
		if err != nil {
			log.Printf("[WARN] generation step failed: %v", err)
			break
		}

		if next == markov.EndToken {
			// Unconditional. The floor lives in the length model as a logit penalty, so
			// there is no discard-and-retry here: if the model chose to end despite that
			// penalty, the alternatives were worse, and overriding it would put the
			// decision back in two places.
			chose = true
			break
		}

		if next == "" {
			// A dead end. Either the prefix has no continuation, or the author-diversity
			// gate refused everything that did, which on a young corpus is the common case
			// and is the gate working.
			//
			// Jump decides for itself whether jumping is worth it here, and often it is not:
			// a jump appends a word with no n-gram relationship to the one before it, so the
			// join is guaranteed rough. Its bounds live in the engine because the golden
			// harness makes the same call and the two must not disagree.
			jump := engine.Jump(seedIn, step)
			if jump == "" {
				break
			}
			next = jump
		}

		step.Sentence = append(step.Sentence, next)
		lower := text.LowerExceptURLs(next)
		step.Used[lower]++
		step.Ngrams[lower] = struct{}{}
		if len(step.Sentence) >= 2 {
			step.Ngrams[text.LowerExceptURLs(strings.Join(step.Sentence[len(step.Sentence)-2:], " "))] = struct{}{}
		}
		if len(step.Sentence) >= 3 {
			step.Ngrams[text.LowerExceptURLs(strings.Join(step.Sentence[len(step.Sentence)-3:], " "))] = struct{}{}
		}

		step.Prefix = append(step.Prefix, next)
		if len(step.Prefix) > g.opts.MaxNGram-1 {
			step.Prefix = step.Prefix[1:]
		}
	}

	step.Sentence = markov.TrimDangling(step.Sentence, chose)
	return step.Sentence, recognized
}

// wordSet builds a normalized presence set.
//
// Once per sentence, where the old scorer rebuilt the prompt set inside the per-candidate
// loop: it tokenized the whole prompt and allocated a map once per candidate per step per
// generated word.
func wordSet(words []string) map[string]struct{} {
	out := make(map[string]struct{}, len(words))
	for _, w := range words {
		out[text.LowerExceptURLs(w)] = struct{}{}
	}
	return out
}

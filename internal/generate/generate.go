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

// Sentence generates a reply to prompt, steered by mem, and returns "" when there is
// nothing worth saying.
//
// Returning empty is a normal outcome rather than a failure: an empty corpus, a young one
// where the author-diversity gate refuses everything, or a dead-ended seed all produce it.
// The caller stays silent, which is what this bot does anyway whenever it decides not to
// answer.
func (g *Generator) Sentence(prompt string, roast bool, mem *Memory, emoji EmojiResolver) (string, error) {
	promptWords := text.Tokenize(prompt)
	if len(promptWords) == 0 {
		promptWords = []string{"<START>"}
	}

	var recentWords []string
	if mem != nil {
		recentWords = mem.WeightedWords()
	}

	var (
		sentence        []string
		recognizedNames []string
	)

	// ONE read transaction for the whole attempt, and genuinely one: everything inside
	// used to reach back for its own. Reader.CorpusEmpty replaces a Bucket.Stats() call
	// that walked every page in the largest bucket in the database on every reply purely to
	// answer "is there anything in here" (finding 11).
	err := g.store.View(func(r *storage.Reader) error {
		if r.CorpusEmpty() {
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
		return "", err
	}

	// Below two words there is nothing worth posting. One word is not a punchy reply, it is
	// a reply that looks broken, and silence is indistinguishable from the bot choosing not
	// to answer, which it does all the time anyway.
	if len(sentence) < 2 {
		return "", nil
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
	return markov.Style(nil, markov.DefaultWeights(), final, persona, len(recognizedNames) > 0), nil
}

// attempt drives one sentence out of the engine and returns it with the names it
// recognized in the prompt.
func (g *Generator) attempt(r *storage.Reader, promptWords, recentWords []string, prompt string, roast bool) ([]string, []string) {
	engine := markov.New(r, g.opts.Params(), nil)

	var recognized []string
	for _, word := range promptWords {
		if name, ok := names.Canonical(r, word); ok {
			recognized = append(recognized, name)
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
		CurrentTopic: seed,
		NameAssoc:    assoc,
	}
	if roast {
		step.Persona = markov.PersonaRoast
	}
	for _, w := range words {
		step.Used[w]++
	}

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
			break
		}

		if next == "" {
			// A dead end. Either the prefix has no continuation, or the author-diversity
			// gate refused everything that did, which on a young corpus is the common case
			// and is the gate working.
			jump := engine.Jump(seedIn, step.Sentence)
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

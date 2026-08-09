package markov

import (
	"strings"

	"github.com/6586x57890143/peregrine/internal/corpus"
)

// fakeCorpus is a Corpus made of Go maps, which is the property the whole seam exists
// to buy: the probability model and a thousand lines of heuristics are exercised here
// with no bbolt file, no transaction and no session.
//
// It deliberately MIRRORS storage's write semantics rather than inventing convenient
// ones, because a fake that disagrees with the real writer tests the fake. The three
// places that matters:
//
//   - Author diversity counts distinct (prefix, next, author) triples, so repeating a
//     phrase as one author must not move it. That is the anti-poisoning control and a
//     fake that counted occurrences would make the gate look like it works.
//   - The n-gram loop starts at order 2, never 1, so no empty prefix is ever written.
//   - Distinct predecessors count distinct contexts a token follows, not occurrences.
//
// seam_test.go closes the loop by running the same assertions against a real
// *storage.Reader, so a divergence between this and storage shows up as a test
// failure rather than as a green suite over a wrong model.
type fakeCorpus struct {
	counts   map[string]map[string]uint64          // prefix -> next -> count
	authors  map[string]map[string]map[string]bool // prefix -> next -> author set
	knSucc   map[string]uint64                     // prefix -> N1+(prefix .)
	knPre    map[string]map[string]bool            // token -> set of contexts
	preTotal uint64

	topics     map[string]uint64
	topicTotal uint64
	topicWords map[string]map[string]corpus.TopicAssoc
	nameTopics map[string]map[string]corpus.TopicAssoc
	names      map[string]bool
}

func newFake() *fakeCorpus {
	return &fakeCorpus{
		counts:     map[string]map[string]uint64{},
		authors:    map[string]map[string]map[string]bool{},
		knSucc:     map[string]uint64{},
		knPre:      map[string]map[string]bool{},
		topics:     map[string]uint64{},
		topicWords: map[string]map[string]corpus.TopicAssoc{},
		nameTopics: map[string]map[string]corpus.TopicAssoc{},
		names:      map[string]bool{},
	}
}

// learn ingests one message the way learnMessage does, at the given order.
func (f *fakeCorpus) learn(maxNGram int, authorID, text string) {
	words := strings.Fields(text)
	for _, w := range words {
		f.topics[w]++
		f.topicTotal++
	}
	for n := maxNGram; n >= 2; n-- {
		if len(words) < n {
			continue
		}
		for i := 0; i <= len(words)-n; i++ {
			f.learnNgram(strings.Join(words[i:i+n-1], " "), words[i+n-1], authorID)
		}
	}
}

func (f *fakeCorpus) learnNgram(prefix, next, authorID string) {
	if prefix == "" {
		panic("fake corpus asked to write an empty prefix, which storage refuses (finding 5)")
	}
	if f.counts[prefix] == nil {
		f.counts[prefix] = map[string]uint64{}
	}
	first := f.counts[prefix][next] == 0
	f.counts[prefix][next]++

	if authorID != "" {
		if f.authors[prefix] == nil {
			f.authors[prefix] = map[string]map[string]bool{}
		}
		if f.authors[prefix][next] == nil {
			f.authors[prefix][next] = map[string]bool{}
		}
		f.authors[prefix][next][authorID] = true
	}

	if first {
		f.knSucc[prefix]++
	}
	if f.knPre[next] == nil {
		f.knPre[next] = map[string]bool{}
	}
	if !f.knPre[next][prefix] {
		f.knPre[next][prefix] = true
		f.preTotal++
	}
}

// addTopicWord and addNameTopic mirror the association writers.
func (f *fakeCorpus) addTopicWord(word, assoc string, position float64) {
	if f.topicWords[word] == nil {
		f.topicWords[word] = map[string]corpus.TopicAssoc{}
	}
	a := f.topicWords[word][assoc]
	a.Count++
	a.PosSum += position
	f.topicWords[word][assoc] = a
}

func (f *fakeCorpus) addNameTopic(name, topic string, position float64) {
	if f.nameTopics[name] == nil {
		f.nameTopics[name] = map[string]corpus.TopicAssoc{}
	}
	a := f.nameTopics[name][topic]
	a.Count++
	a.PosSum += position
	f.nameTopics[name][topic] = a
}

// ----- Corpus implementation -----

func (f *fakeCorpus) Successors(prefix string) ([]corpus.Successor, error) {
	inner := f.counts[prefix]
	if len(inner) == 0 {
		return nil, nil
	}
	// Sorted by token, matching the cursor scan order a real Reader returns. Go's map
	// iteration is randomized, and returning it unsorted would make the fake produce
	// a different candidate order on every run, which would hide exactly the class of
	// bug M6b had to delete a heuristic over.
	tokens := make([]string, 0, len(inner))
	for t := range inner {
		tokens = append(tokens, t)
	}
	sortStrings(tokens)

	out := make([]corpus.Successor, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, corpus.Successor{Token: t, Count: inner[t], Authors: f.authorCount(prefix, t)})
	}
	return out, nil
}

func (f *fakeCorpus) Successor(prefix, next string) (corpus.Successor, bool, error) {
	c, ok := f.counts[prefix][next]
	if !ok {
		return corpus.Successor{}, false, nil
	}
	return corpus.Successor{Token: next, Count: c, Authors: f.authorCount(prefix, next)}, true, nil
}

func (f *fakeCorpus) authorCount(prefix, next string) uint32 {
	return uint32(len(f.authors[prefix][next]))
}

// HasSuccessors mirrors the real reader's contract, which is NOT a byte-prefix match:
// keys are <prefix> NUL <next>, so a query for "the" must not be satisfied by keys
// under "the cat". Here that falls out of the map being keyed by exact prefix, which
// makes this the one method where the fake is trivially right and storage's version
// needed a bounded cursor range to be.
func (f *fakeCorpus) HasSuccessors(prefix string) bool {
	if prefix == "" {
		return false
	}
	return len(f.counts[prefix]) > 0
}

// FirstPrefix returns the lowest prefix in sorted order, matching a cursor's First().
func (f *fakeCorpus) FirstPrefix() (string, bool) {
	if len(f.counts) == 0 {
		return "", false
	}
	keys := make([]string, 0, len(f.counts))
	for k := range f.counts {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys[0], true
}

func (f *fakeCorpus) PrefixTotal(prefix string) uint64 {
	var total uint64
	for _, c := range f.counts[prefix] {
		total += c
	}
	return total
}

func (f *fakeCorpus) KNStats(prefix, token string) (corpus.KNStats, error) {
	return corpus.KNStats{
		DistinctSuccessors:   f.knSucc[prefix],
		DistinctPredecessors: uint64(len(f.knPre[token])),
	}, nil
}

func (f *fakeCorpus) TotalDistinctPredecessors() uint64 { return f.preTotal }
func (f *fakeCorpus) TotalTopicCount() uint64           { return f.topicTotal }
func (f *fakeCorpus) TopicCount(word string) uint64     { return f.topics[word] }
func (f *fakeCorpus) IsName(key string) bool            { return f.names[key] }

func (f *fakeCorpus) TopicWordsFor(word string) (map[string]corpus.TopicAssoc, error) {
	return f.topicWords[word], nil
}

func (f *fakeCorpus) NameTopicsFor(name string) (map[string]corpus.TopicAssoc, error) {
	return f.nameTopics[name], nil
}

// sortStrings avoids importing sort into a file that is otherwise all data.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// testParams are the shipped defaults with the author gate off, which is what most
// tests want: the gate has its own tests and leaving it on would make every other
// test's fixture need two authors.
func testParams() Params {
	return Params{
		MaxNGram:           5,
		Temperature:        1.0,
		TopK:               40,
		TopP:               0.95,
		KNDiscount:         0.75,
		KNRawMix:           0.25,
		MinDistinctAuthors: 0,
		MinWords:           4,
		MaxWords:           18,
		CooccurrenceWindow: 5,
		PromptRelevance:    0.6,
	}
}

// newStep builds a Step with the maps populated, since a nil map read is fine but the
// caller in production always has them.
func newStep(prefix []string) *Step {
	return &Step{
		Prefix:     prefix,
		Sentence:   append([]string{}, prefix...),
		PromptSet:  map[string]struct{}{},
		RecentSet:  map[string]struct{}{},
		Used:       map[string]int{},
		Ngrams:     map[string]struct{}{},
		CoreTopics: map[string]float64{},
		NameAssoc:  map[string]corpus.TopicAssoc{},
		Length:     Length{Min: 4, Max: 18, Target: 8},
	}
}

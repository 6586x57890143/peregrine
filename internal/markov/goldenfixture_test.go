package markov

import (
	"strings"

	"github.com/6586x57890143/peregrine/internal/text"
)

// The golden fixture, rebuilt.
//
// The old one was twenty-six synthetic lines with one name and nine hand-listed
// associations. It could not answer any of the questions the harness exists to answer:
// whether a reply lands, whether it incorporates the people around it, and whether it
// RECOMBINES rather than recites. Tuning a weight against it would be finding 29's mistake,
// a constant validated against nothing.
//
// Two things about this one are deliberate and load-bearing.
//
// FIRST, THE ASSOCIATIONS ARE WRITTEN THE WAY learn.associate WRITES THEM rather than
// hand-listed. The old fixture listed nine pairs by hand, which meant the association tiers
// and every positional heuristic were being judged against numbers somebody typed rather
// than against what the learn path actually produces. A fixture that disagrees with the
// writer tests the fixture, which is the same rule fake_test.go already states about
// storage's write semantics.
//
// SECOND, THE TOPICS OVERLAP ON PURPOSE. "funny by combining varied concepts" needs two
// concepts that share a bridge word, and a corpus of cleanly separated subjects cannot
// produce it. So ranked and sleep share "queue", food and drama share "cold", and the bird
// runs through everything.

// fixtureMessage is one authored line: who said it, who it is about, and what it says.
//
// About is the mentioned people, NOT including the author. learnFixture merges the author
// in, exactly as Learner.Message does, because that merge is what finding 33 was.
type fixtureMessage struct {
	author string
	about  []string
	text   string
}

// learnFixture ingests one message the way learn.Learner.Message does: n-grams, then the
// name-to-word and windowed word-to-word association indexes.
//
// The window is 5, matching PEREGRINE_COOCCURRENCE_WINDOW's default, and stop words and the
// end sentinel are excluded from associations, matching learn.skipAssoc. Getting either
// wrong here would make every association tier in the harness read a corpus the bot cannot
// actually have.
func (f *fakeCorpus) learnFixture(maxNGram, window int, m fixtureMessage) {
	full := m.text + " " + EndToken
	f.learn(maxNGram, m.author, full)

	words := strings.Fields(full)

	names := make([]string, 0, len(m.about)+1)
	names = append(names, m.author)
	names = append(names, m.about...)

	skip := func(w string) bool { return w == EndToken || text.IsStopWord(w) }

	// name -> word, unwindowed over the whole message.
	for _, name := range names {
		for i, w := range words {
			if skip(w) {
				continue
			}
			f.addNameTopic(name, w, float64(i)/float64(len(words)))
		}
	}

	// word -> word, windowed, both directions, recording the ASSOCIATE's position.
	for i, w := range words {
		if skip(w) {
			continue
		}
		lo, hi := i-window, i+window
		if lo < 0 {
			lo = 0
		}
		if hi > len(words)-1 {
			hi = len(words) - 1
		}
		for j := lo; j <= hi; j++ {
			if i == j || skip(words[j]) {
				continue
			}
			f.addTopicWord(w, words[j], float64(j)/float64(len(words)))
		}
	}
}

// goldenCorpus is the fixture the harness generates from.
//
// Roughly two hundred lines across ten authors and five recurring subjects, with five named
// people who are talked about differently from each other. The repetition is not padding: a
// diversity threshold of 2 is on in production, so a phrase only one person ever said is
// refused at every step, and a fixture without genuinely shared phrasing generates nothing
// and prints blank lines.
func goldenCorpus() *fakeCorpus {
	f := newFake()

	for _, m := range fixtureMessages() {
		f.learnFixture(5, 5, m)
	}

	// The five people the server talks about. Recorded as names so IsName, Canonical and
	// every name tier can see them.
	for _, n := range []string{"greg", "lachy", "beezle", "alexiane", "nurock"} {
		f.names[n] = true
	}

	return f
}

// fixtureMessages is the authored corpus.
//
// Written by hand rather than generated, because the whole point is that it reads like the
// server the bot lives in. Five subjects that overlap: the bird (the bot itself), ranked,
// sleep, food, and drama. The bridges between them are deliberate, and they are where a
// recombined sentence has to come from.
//
// # How two authors come to agree, and why it is not by repeating each other
//
// The author-diversity gate admits a continuation only once two people have produced it, so
// a fixture has to contain genuine agreement or it generates nothing. The first version of
// this file produced that agreement the lazy way, by giving two authors near-identical
// sentences ("beezle the pizza is cold" and "beezle the pizza is cold again lmao"). That
// worked, and it made the corpus a trap: the only paths the gate admitted were whole
// messages, so the engine could not do anything BUT recite, and the recitation metric was
// measuring the fixture instead of the engine.
//
// Real chat does not agree that way. Two people almost never type the same five-word
// content sequence, but they constantly share short idioms ("at this hour", "is cooked",
// "someone please contain") embedded in otherwise different sentences. That is what makes a
// corpus recombinable at all, and it is what this fixture now does: SHARED IDIOMS INSIDE
// DIFFERENT SENTENCES, never shared sentences.
//
// The exception is deliberate and load-bearing: a meme really is typed verbatim by several
// people, so "bird moment" and "ratio ratio ratio" stay exact duplicates. Those are the
// register, and a fixture that made even the memes vary would be a different lie.
func fixtureMessages() []fixtureMessage {
	return []fixtureMessage{
		// ---- the bird, which is the bot and runs through everything ----
		// Note the shared IDIOMS rather than shared sentences: "is loose", "at this hour",
		// "someone please contain" and "knows what it did" each turn up in several
		// DIFFERENT messages by different people.
		{"alice", nil, "the bird is loose again"},
		{"bob", nil, "someone please contain the bird before it gets out"},
		{"carol", nil, "is the bird loose in the server again"},
		{"dave", nil, "the bird knows what it did honestly"},
		{"erin", nil, "i think the bird knows what it did"},
		{"frank", nil, "why is the bird awake at this hour"},
		{"grace", nil, "the bird is malding in the server again"},
		{"heidi", nil, "someone please contain this bird"},
		{"ivan", nil, "this is peak bird behaviour and nobody is stopping it"},
		{"judy", nil, "peak bird behaviour honestly"},
		{"alice", nil, "the bird ratioed him and left"},
		{"bob", nil, "the bird is cooked"},
		{"carol", nil, ":birdstare: what is happening in here"},
		{"dave", nil, ":birdstare: the server is doomed"},
		{"erin", nil, "the server is doomed and i am fine with it"},
		{"frank", nil, "the server is cooked at this point"},

		// True memes, typed verbatim by several people. These are the register, and they
		// are the one place identical lines are honest.
		{"bob", nil, "bird moment"},
		{"carol", nil, "bird moment"},
		{"dave", nil, "bird moment"},
		{"ivan", nil, "ratio ratio ratio"},
		{"judy", nil, "ratio ratio ratio"},
		{"alice", nil, "ratio ratio ratio"},
		{"bob", nil, "no cap fr fr"},
		{"carol", nil, "no cap fr fr"},

		// ---- greg: coping, and the queue ----
		{"alice", []string{"greg"}, "greg said the bird was fine which it is not"},
		{"bob", []string{"greg"}, "greg is coping about the queue again"},
		{"carol", []string{"greg"}, "honestly greg is coping hard"},
		{"dave", []string{"greg"}, "absolutely cringe behaviour from greg today"},
		{"erin", []string{"greg"}, "greg is cope incarnate"},
		{"frank", []string{"greg"}, "greg is cringe and he knows it"},
		{"grace", []string{"greg"}, "what a clown greg is"},
		{"heidi", []string{"greg"}, "greg has been in queue for an hour now"},
		{"ivan", []string{"greg"}, "greg is malding in queue again"},
		{"judy", []string{"greg"}, "why is greg awake at this hour"},
		{"alice", []string{"greg"}, "peak greg behaviour"},
		{"bob", []string{"greg"}, "someone please contain greg"},

		// ---- lachy: sleep, and the hour ----
		{"carol", []string{"lachy"}, "lachy is awake at this hour again"},
		{"dave", []string{"lachy"}, "lachy never sleeps and it shows"},
		{"erin", []string{"lachy"}, "i swear lachy never sleeps"},
		{"frank", []string{"lachy"}, "lachy said he was going to sleep an hour ago"},
		{"grace", []string{"lachy"}, "lachy said he was done and then queued again"},
		{"heidi", []string{"lachy"}, "lachy is in queue at this hour somehow"},
		{"ivan", []string{"lachy"}, "someone please contain lachy"},
		{"judy", []string{"lachy"}, "peak lachy behaviour"},
		{"alice", []string{"lachy"}, "lachy is cooked"},
		{"bob", []string{"lachy"}, "lachy is malding about ranked"},
		{"carol", []string{"lachy"}, "what is lachy even doing at this hour"},

		// ---- beezle: food, and being cold ----
		{"dave", []string{"beezle"}, "beezle ordered pizza again"},
		{"erin", []string{"beezle"}, "beezle ordered pizza at this hour"},
		{"frank", []string{"beezle"}, "the pizza is cold beezle"},
		{"grace", []string{"beezle"}, "beezle eats cold pizza and says nothing"},
		{"heidi", []string{"beezle"}, "why is the pizza cold again"},
		{"ivan", []string{"beezle"}, "peak beezle behaviour"},
		{"judy", []string{"beezle"}, "beezle is coping about the pizza"},
		{"alice", []string{"beezle"}, "beezle said the pizza was fine"},
		{"bob", []string{"beezle"}, "beezle is cooked"},
		{"carol", []string{"beezle"}, "someone please contain beezle"},
		{"dave", []string{"beezle"}, "beezle moment"},
		{"erin", []string{"beezle"}, "beezle moment"},

		// ---- alexiane: drama, and the ratio ----
		{"frank", []string{"alexiane"}, "alexiane ratioed him in front of everyone"},
		{"grace", []string{"alexiane"}, "alexiane is starting drama again"},
		{"heidi", []string{"alexiane"}, "is alexiane starting drama in the server"},
		{"ivan", []string{"alexiane"}, "alexiane said the drama was fine"},
		{"judy", []string{"alexiane"}, "peak alexiane behaviour"},
		{"alice", []string{"alexiane"}, "the drama is cold now honestly"},
		{"bob", []string{"alexiane"}, "the drama is cooked"},
		{"carol", []string{"alexiane"}, "alexiane moment"},
		{"dave", []string{"alexiane"}, "alexiane moment"},
		{"erin", []string{"alexiane"}, "someone please contain alexiane"},
		{"frank", []string{"alexiane"}, "why is alexiane awake at this hour"},

		// ---- nurock: ranked, and being hardstuck ----
		{"grace", []string{"nurock"}, "nurock is hardstuck again"},
		{"heidi", []string{"nurock"}, "nurock has been hardstuck for a month"},
		{"ivan", []string{"nurock"}, "nurock lost ranked again"},
		{"judy", []string{"nurock"}, "nurock lost ranked at this hour"},
		{"alice", []string{"nurock"}, "nurock is in queue for ranked"},
		{"bob", []string{"nurock"}, "nurock is coping about ranked"},
		{"carol", []string{"nurock"}, "peak nurock behaviour"},
		{"dave", []string{"nurock"}, "nurock is malding about ranked"},
		{"erin", []string{"nurock"}, "nurock moment"},
		{"frank", []string{"nurock"}, "nurock moment"},
		{"grace", []string{"nurock"}, "someone please contain nurock"},

		// ---- the bridges, which is where recombination comes from ----
		// queue bridges ranked and sleep. cold bridges pizza and drama. "at this hour",
		// "is cooked", "is a mistake" and "someone please contain" cross everybody.
		{"heidi", nil, "the queue is cooked"},
		{"ivan", nil, "queue at this hour is a mistake"},
		{"judy", nil, "ranked at this hour is a mistake honestly"},
		{"alice", nil, "pizza at this hour is a mistake"},
		{"bob", nil, "drama at this hour is a mistake"},
		{"carol", nil, "the pizza is cold and so is the drama"},
		{"dave", nil, "everything is cold now"},
		{"erin", nil, "everything is fine actually"},
		{"frank", nil, "nothing is fine actually"},
		{"grace", nil, "nothing is fine and everyone is coping"},
		{"heidi", nil, "everyone is coping about something"},
		{"ivan", nil, "everyone is malding at this hour"},
		{"judy", nil, "we are all hardstuck honestly"},
		{"alice", nil, "we are all coping in queue"},
		{"bob", nil, "is anyone awake at this hour"},

		// ---- conversational filler, shared idioms in different sentences ----
		{"carol", nil, "that is genuinely insane behaviour"},
		{"dave", nil, "genuinely insane behaviour from everyone today"},
		{"erin", nil, "i am going to lose it"},
		{"frank", nil, "i am going to lose it honestly"},
		{"grace", nil, "what do you even mean by that"},
		{"heidi", nil, "what do you mean exactly"},
		{"ivan", nil, "do you know what i mean"},
		{"judy", nil, "you know what i think about that"},
		{"alice", nil, "i do not know what you mean"},
		{"bob", nil, "bro what are you talking about"},
		{"carol", nil, "bro what are you on about"},
		{"dave", nil, "what are you on about honestly"},
		{"erin", nil, "hey guys did you know the bird is loose"},
		{"frank", nil, "hey guys did you know greg is coping"},

		// ---- cross-person lines, so the name indexes are not one topic each ----
		{"grace", []string{"greg", "lachy"}, "greg and lachy are both in queue again"},
		{"heidi", []string{"greg", "nurock"}, "greg and nurock are both hardstuck"},
		{"ivan", []string{"beezle", "lachy"}, "beezle and lachy are awake at this hour"},
		{"judy", []string{"alexiane", "greg"}, "alexiane ratioed greg honestly"},
		{"alice", []string{"alexiane", "greg"}, "alexiane ratioed greg in the server"},
		{"bob", []string{"nurock", "lachy"}, "nurock and lachy lost ranked again"},
		{"carol", []string{"beezle", "greg"}, "beezle and greg are coping about the pizza"},
		{"dave", []string{"lachy"}, "lachy is malding in queue at this hour"},
		{"erin", []string{"beezle"}, "beezle is hardstuck and eating cold pizza"},
		{"frank", []string{"alexiane"}, "alexiane is starting drama about ranked"},
		{"grace", []string{"nurock"}, "nurock is coping about the drama now"},
		{"heidi", []string{"greg"}, "the bird ratioed greg honestly"},
		{"ivan", []string{"lachy"}, "the bird ratioed lachy and left"},
		{"judy", []string{"greg"}, "greg said the bird was cooked"},
		{"alice", []string{"beezle"}, "beezle said the bird ate the pizza"},
		{"bob", []string{"nurock"}, "the bird is hardstuck honestly"},
		{"carol", []string{"alexiane"}, "the bird is starting drama again"},
	}
}

// Package learn is the only way anything enters the corpus.
//
// # Why this is a package and not a method somewhere convenient
//
// SPEC.md section 4, A1 was the highest-value finding in the review: learnMessage had four
// callers and exactly one of them filtered. The live message handler ran the filters; the
// historical backfill, self-learning and voice transcripts passed content straight
// through. Since the backfill re-read the trailing 24 hours every ten minutes, a message
// the live path blocked was learned anyway, unfiltered, minutes later, which defeated the
// live filter entirely.
//
// A check at one of four call sites is not a check. Making this a package with one entry
// point means a fifth caller is covered without anyone remembering to cover it, which is
// the difference between fixing a bug and making it unwritable. The AST test in this
// package fails if the CheckLearn call leaves Learner.Message's body, because that
// regression passes every behavioural test.
//
// # Reject, never launder
//
// safety.Verdict has no field for rewritten text, so laundering is unexpressible rather
// than merely discouraged. A rewritten message is still learned, with its structure intact
// and a harmless token sitting in the offending word's grammatical position: the bot has
// been taught the sentence. There used to be a filterSlurs call in the live handler doing
// exactly that, and deleting it was load-bearing twice over, because with the gate in
// place laundering BEFORE the gate would hand it pre-cleaned text and defeat it.
package learn

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/names"
	"github.com/6586x57890143/peregrine/internal/safety"
	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/text"
)

// EndToken marks the end of a thought in the corpus.
//
// It is the same string internal/markov emits and strips, and the duplication is
// deliberate: markov must not import this package (it imports nothing from this module by
// design) and this package must not import markov (learning does not depend on
// generation). One string in two places beats a dependency in either direction, and
// TestEndTokenMatchesTheEngine pins that they agree.
const EndToken = "<end>"

// Options are the dials the learn path reads. Every one of them comes from the
// environment via internal/config; none has a default here, because a zero MaxNGram
// learning nothing silently is exactly the failure this project keeps closing.
type Options struct {
	// MaxNGram is the longest context recorded. The ingestion loop runs down to 2 and
	// never to 1: at n == 1 the prefix is empty, and the empty prefix was one bbolt key
	// holding a JSON map of the entire vocabulary that nothing ever read (finding 5).
	MaxNGram int

	// MaxHistory bounds the dedup window.
	MaxHistory int

	// CooccurrenceWindow bounds the association loop to this many words on each side. 0
	// restores the old all-pairs behaviour, which was quadratic in message length inside
	// the single write transaction that serializes all ingestion (finding 12).
	CooccurrenceWindow int
}

// Learner writes to the corpus. One per process.
type Learner struct {
	gate  *safety.Gate
	opts  Options
	botID string

	// mentionOnce caches the bot-mention pattern, which used to be regexp.MustCompile'd
	// inside the learn path: once per message per caller, with the bot ID interpolated
	// into the pattern every time (finding 16). Per-Learner rather than package-level, so
	// a test can build a second one.
	mentionOnce sync.Once
	mention     *regexp.Regexp
}

// New returns a Learner. The gate must not be nil: an absent gate would mean an
// unfiltered corpus, and an empty ruleset is indistinguishable from a working one until
// the bot has already posted something the operator has to answer for.
func New(gate *safety.Gate, opts Options) *Learner {
	return &Learner{gate: gate, opts: opts}
}

// SetBotID records the bot's own user ID, which is only known after READY.
//
// It matters for two things: stripping the bot's own mention out of message text so the
// corpus does not fill with a user ID, and excluding the bot from author-diversity counts
// so self-learning cannot bootstrap a phrase into eligibility (SPEC.md section 4, A6).
func (l *Learner) SetBotID(id string) { l.botID = id }

// BotID reports what SetBotID was given.
func (l *Learner) BotID() string { return l.botID }

func (l *Learner) mentionPattern() *regexp.Regexp {
	l.mentionOnce.Do(func() {
		l.mention = regexp.MustCompile(fmt.Sprintf(`(?i)<@!?%s>|@peregrine`, regexp.QuoteMeta(l.botID)))
	})
	return l.mention
}

// Message ingests one message: the gate, the dedup window, the author's stats, the topic
// counts, the name associations, the co-occurrence window and the n-grams.
//
// Every path into the corpus goes through here. Do not add a second one.
func (l *Learner) Message(w *storage.Writer, msg, msgID string, author names.User, mentioned []names.User) error {
	msg = strings.TrimSpace(l.mentionPattern().ReplaceAllString(msg, ""))

	// Mentions of OTHER people become their names, immediately after the bot's own mention has
	// been removed. The bot's is stripped rather than substituted, so that stays as it was.
	//
	// The tokenizer keeps <@123> as one whole token, so without this the corpus stores an
	// opaque ID where a name belongs and the chain can never walk into or out of a person's
	// name. It goes BEFORE the gate on purpose: the gate has to see the text that will
	// actually be learned, and a substituted name is text the corpus will hold. The
	// consequence is that a member whose name matches a blocklist pattern costs the message,
	// which is the correct direction for a gate that fails closed.
	//
	// This is not the laundering the package comment forbids. That rule is about rewriting
	// flagged content so it passes; this is normalizing a display encoding before anything is
	// judged, and safety.Verdict still has no field for rewritten text.
	msg = names.Substitute(msg, mentioned)
	if msg == "" {
		return nil
	}

	// THE LEARNING GATE, inside this function and not at any of its callers. See the
	// package comment: that placement is the whole of A1's fix, and the AST test in
	// learn_test.go fails if it moves. Do not hoist it for performance; the normalizer is
	// cheap and the corpus is forever.
	//
	// The verdict is to DROP THE MESSAGE WHOLE, never to rewrite it.
	if v := l.gate.CheckLearn(msg); !v.Allowed {
		return nil
	}

	words := text.Tokenize(msg)
	if len(words) == 0 {
		return nil
	}
	words = append(words, EndToken)

	start := time.Now()

	// The dedup window, checked before anything is written, because the backfill re-reads
	// recent history and without this every pass would re-learn the same messages and
	// double-count their n-grams (finding 13).
	//
	// A message ID that is not a snowflake is an error rather than a miss: it means a
	// caller invented one, and silently learning it twice is worse than saying so.
	seen, err := w.Seen(msgID)
	if err != nil {
		return fmt.Errorf("dedup check for message %s: %w", msgID, err)
	}
	if seen {
		return nil
	}

	authorName, err := l.recordAuthor(w, author)
	if err != nil {
		return err
	}

	if err := w.IncMessagesLearned(); err != nil {
		return fmt.Errorf("bump learned counter: %w", err)
	}

	// Global topic counts, which is also where unigram frequency lives: one key per word,
	// where it always belonged. The old ingestion loop tried to keep it in the n-gram
	// bucket under an empty prefix, and that key held a map of the entire vocabulary
	// (finding 5).
	for _, word := range words {
		if err := w.IncTopic(text.LowerExceptURLs(word)); err != nil {
			return fmt.Errorf("increment topic: %w", err)
		}
	}

	canonicalNames, err := l.recordNames(w, mentioned)
	if err != nil {
		return err
	}

	// THE AUTHOR COUNTS AS A NAME THIS MESSAGE IS ABOUT, and adding them here rather than at
	// the callers is the whole point of this line.
	//
	// associate returns early when this set is empty, so a caller that passed no names taught
	// the corpus no associations AT ALL: not the name-to-word index every name-aware seed tier
	// reads, and not the word-to-word index tiers 3 and 6 and all of Jump read. The live chat
	// handler appended the author to its mentioned slice and the backfill did not, and the
	// backfill is where most of a corpus comes from, so both indexes stayed nearly empty on a
	// bot that looked like it was learning fine.
	//
	// That is A1's shape exactly: one entry point, two callers, one of them doing an extra
	// step. Every test in this package passed the author in, which is how the divergence
	// stayed invisible. Put it here and a third caller cannot get it wrong.
	if authorName != "" {
		canonicalNames[authorName] = struct{}{}
	}

	if err := l.associate(w, words, canonicalNames); err != nil {
		return err
	}

	total, err := l.ngrams(w, words, author)
	if err != nil {
		return err
	}

	// Recorded last, so a failure anywhere above rolls the whole message back rather than
	// marking it seen and then not learning it.
	if err := w.MarkSeen(msgID, time.Now(), l.opts.MaxHistory); err != nil {
		return fmt.Errorf("mark message %s seen: %w", msgID, err)
	}

	// The history size comes from a counter rather than Bucket.Stats(), which walked every
	// page in the bucket once per message to fill this one log field (finding 11).
	log.Printf("[LEARNED] msg=%q | words=%d | ngrams=%d | history=%d | names=%d | took=%s",
		msg, len(words), total, w.HistoryCount(), len(canonicalNames), time.Since(start))

	return nil
}

// recordAuthor updates the author's name record and their weekly message count, and returns
// the canonical form of their name.
//
// The canonical name is returned rather than discarded because Message needs it for the
// association set: an author is a name this message is about, and the caller is not the right
// place to decide that. An empty return means there was nothing to record, which is the same
// answer for a message with no author ID and for a name that would not write.
func (l *Learner) recordAuthor(w *storage.Writer, author names.User) (string, error) {
	if author.UserID == "" {
		return "", nil
	}
	canonical, err := names.Record(w, author.Name, author.UserID, author.Username)
	if err != nil {
		// Not fatal to the message. A name that would not write is a lost association,
		// whereas abandoning here would lose the n-grams too.
		log.Printf("[WARN] Failed to record author name %q: %v", author.Name, err)
		return "", nil
	}

	stat, _, err := w.UserStat(author.UserID)
	if err != nil {
		return "", fmt.Errorf("read stats for %s: %w", author.UserID, err)
	}
	now := time.Now().UTC()
	if stat.LastTimestamp.Before(corpus.StartOfWeekUTC(now)) {
		stat.Count = 1 // a new week for this user
	} else {
		stat.Count++
	}
	stat.LastTimestamp = now
	if err := w.PutUserStat(author.UserID, stat); err != nil {
		return "", fmt.Errorf("write stats for %s: %w", author.UserID, err)
	}
	return canonical, nil
}

// recordNames writes every mentioned person and returns the set of canonical names.
func (l *Learner) recordNames(w *storage.Writer, mentioned []names.User) (map[string]struct{}, error) {
	canonical := make(map[string]struct{}, len(mentioned))
	for _, user := range mentioned {
		name, err := names.Record(w, user.Name, user.UserID, user.Username)
		if err != nil {
			log.Printf("[WARN] Failed to learn name %q: %v", user.Name, err)
			continue
		}
		canonical[name] = struct{}{}
	}
	return canonical, nil
}

// associate writes the name-to-word and word-to-word indexes.
//
// Both are gated on a name being present, which is the original design: associations are
// built from user-provided context rather than from every message. Message now always puts
// the author in that set, so in practice the guard only catches a message with no author ID
// at all, such as a webhook. It stays because the early return is what made the missing
// author cost the whole message its associations, and a guard that describes its own
// precondition is worth more than one fewer branch.
//
// The word-to-word loop is WINDOWED, which closes the rest of finding 12. It was all-pairs
// and therefore quadratic in message length, running inside the single write transaction
// that serializes every other write in the process: a 200-word message produced nearly
// 40,000 read-add-write pairs and blocked all ingestion while it did. With a window of 5
// the same message produces about 2,000.
//
// The window is a genuine model change and in the right direction. "Co-occurs anywhere in
// the same message" is a weak claim that gets weaker as messages get longer, since a
// 200-word message links every word in it to every other. Proximity is what association
// actually means.
//
// Both directions of each pair are recorded, because the index is direction-sensitive: it
// stores the position of the ASSOCIATE, so (a, b) and (b, a) carry different position sums
// and the readers use that.
func (l *Learner) associate(w *storage.Writer, words []string, canonicalNames map[string]struct{}) error {
	if len(canonicalNames) == 0 {
		return nil
	}

	for name := range canonicalNames {
		for i, word := range words {
			lw := text.LowerExceptURLs(word)
			if l.skipAssoc(lw) {
				continue
			}
			if err := w.AddNameTopic(name, lw, float64(i)/float64(len(words))); err != nil {
				return fmt.Errorf("associate %q with %q: %w", name, lw, err)
			}
		}
	}

	window := l.opts.CooccurrenceWindow
	for i, wordA := range words {
		lwA := text.LowerExceptURLs(wordA)
		if l.skipAssoc(lwA) {
			continue
		}

		lo, hi := 0, len(words)-1
		if window > 0 {
			lo = max(i-window, 0)
			hi = min(i+window, len(words)-1)
		}

		for j := lo; j <= hi; j++ {
			if i == j {
				continue
			}
			lwB := text.LowerExceptURLs(words[j])
			if l.skipAssoc(lwB) {
				continue
			}
			if err := w.AddTopicWord(lwA, lwB, float64(j)/float64(len(words))); err != nil {
				return fmt.Errorf("associate %q with %q: %w", lwA, lwB, err)
			}
		}
	}
	return nil
}

// skipAssoc reports whether a token is excluded from associative learning: the end
// sentinel, and stop words, which would otherwise be the top association of everything.
//
// The stop-word list lives in internal/text rather than here, because the generation path
// excludes the same words when it extracts a prompt's topics, and two copies of that list
// would eventually disagree about what the corpus considers meaningful.
func (l *Learner) skipAssoc(lower string) bool {
	return lower == EndToken || text.IsStopWord(lower)
}

// ngrams writes the Markov contexts and returns how many.
//
// The loop starts at 2, not 1, and that single bound is finding 5. At n == 1 the prefix
// slice is empty, so the key was "" and every unigram in the corpus accumulated into ONE
// bbolt key whose value was a JSON map of the entire vocabulary, unmarshalled and
// re-marshalled once per word per message. Nothing ever read it, because every reader
// builds a prefix of at least one word. Writer.LearnNgram refuses an empty prefix as well,
// so the bug cannot come back through a different caller.
func (l *Learner) ngrams(w *storage.Writer, words []string, author names.User) (int, error) {
	// An empty author for the bot's own output, which is what keeps self-learning out of
	// the author-diversity counts: if the bot counted as an author, anything it said once
	// would bootstrap itself toward eligibility to be said again (A6).
	authorID := author.UserID
	if authorID != "" && authorID == l.botID {
		authorID = ""
	}

	total := 0
	for n := l.opts.MaxNGram; n >= 2; n-- {
		if len(words) < n {
			continue
		}
		for i := 0; i <= len(words)-n; i++ {
			prefix := text.LowerExceptURLs(strings.Join(words[i:i+n-1], " "))
			next := text.LowerExceptURLs(words[i+n-1])
			if err := w.LearnNgram(prefix, next, authorID); err != nil {
				return 0, fmt.Errorf("learn %q -> %q: %w", prefix, next, err)
			}
			total++
		}
	}
	return total, nil
}

// Associations writes ONLY the two co-occurrence indexes, for a message that was learned
// before the finding-33 fix landed.
//
// # Why a second entry point exists at all
//
// The package comment says every path into the corpus goes through Message and not to add a
// second one, so this needs a reason rather than a convenience. Finding 33 repaired the
// WRITER: the backfill passed no author, associate returned early on an empty name set, and
// every historical message therefore wrote neither index. Repairing the writer does not
// repair the data, and the data is not re-derivable: the corpus stores n-grams and counts and
// never message text, while associations need original word sequences with positions. So the
// only way back is to re-read Discord, and re-reading through Message would count every
// n-gram a second time (finding 13).
//
// The general lesson, recorded as finding 46: a fix to a writer is not a fix to the data, and
// whether the data is re-derivable is a property of the layout that has to be checked before
// the fix is called done.
//
// # What it deliberately does NOT write
//
// No n-grams, because they are already correct and writing them again is finding 13. No
// history entry, because the dedup window is capped at PEREGRINE_MAX_HISTORY and filling it
// with old message IDs would evict the live entries that stop real double-learning. No
// messages-learned counter and no topic counts, both of which are already correct because
// they are written OUTSIDE associate's guard. No weekly stats, which would double every
// user's count. And no names.Record, because that bumps Name.Count on every call: the
// historical pass already recorded these people, so this resolves canonical names by READING
// through names.Canonical instead.
//
// # The gate is called here, literally
//
// Not through a shared helper, and not by the caller. A gate at one of two entry points is
// not a gate, which is A1's whole lesson, and the AST test in this package requires every
// exported method taking a *storage.Writer to contain this call in its own body. A helper
// would be one hop that test has to follow, and the next refactor makes it two.
func (l *Learner) Associations(w *storage.Writer, msg string, author names.User, mentioned []names.User) error {
	msg = strings.TrimSpace(l.mentionPattern().ReplaceAllString(msg, ""))
	msg = names.Substitute(msg, mentioned)
	if msg == "" {
		return nil
	}

	if v := l.gate.CheckLearn(msg); !v.Allowed {
		return nil
	}

	words := text.Tokenize(msg)
	if len(words) == 0 {
		return nil
	}
	words = append(words, EndToken)

	// Canonical names by READING. See above: names.Record would inflate Name.Count for
	// people the historical pass already recorded.
	canonical := make(map[string]struct{}, len(mentioned)+1)
	for _, u := range mentioned {
		if name, ok := names.Canonical(&w.Reader, u.Username); ok {
			canonical[name] = struct{}{}
		}
	}
	if author.Username != "" {
		if name, ok := names.Canonical(&w.Reader, author.Username); ok {
			canonical[name] = struct{}{}
		}
	}

	return l.associate(w, words, canonical)
}

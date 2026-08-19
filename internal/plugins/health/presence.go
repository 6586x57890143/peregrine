package health

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"

	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/text"
)

// The presence line.
//
// # Why it lives in the health service
//
// It is fed from the corpus counts, and reportStatus already reads them on a ticker. Doing it
// anywhere else would mean a second Reader.Status, which walks every page in several buckets:
// that cost is the whole reason the status line is on a ticker rather than on the message path
// (finding 11). One page walk, two consumers.
//
// It also means there is no new loop, no new goroutine and no new interval to configure. The
// rotation is the status tick.
//
// # Round robin, not random
//
// The line changes every tick and cycles rather than being drawn fresh each time. A random
// draw repeats, and a status line that shows the same fact twice in a row reads as a bot that
// has stopped updating, which is the opposite of what a heartbeat in the member list is for.

// Presence sets the bot's status line. *discordguard.Guard satisfies it.
//
// Declared here rather than imported so this package keeps its own dependency list, matching
// Queue, Gate and Latency above. It returns a bool because a corpus-word line can be REFUSED
// by the emit gate, and the caller falls back to something it composed itself.
type Presence interface {
	Presence(text string) bool
}

// Topics supplies a word the corpus has been about, for the occasional line that quotes it.
//
// A separate interface from Presence because it is optional and because it is the only part of
// this feature that touches the corpus's CONTENT rather than its counts. A nil one means the
// counts-only lines, which is what an operator gets by setting the chance to zero.
type Topics interface {
	// TopTopicWord returns a frequent non-stop word, or "" when the corpus has none.
	TopTopicWord() string
}

// PresenceOptions are the dials.
type PresenceOptions struct {
	// Enabled turns the line off entirely, leaving whatever Discord shows by default.
	Enabled bool

	// CorpusWordChance is how often the line quotes a word from the corpus instead of
	// reporting a count. Zero disables that variant, which is the setting for an operator who
	// does not want the bot's status to be user-derived text at all.
	CorpusWordChance float64
}

// presenceLine builds the status text for this tick.
//
// Two kinds of line, and the difference between them is a safety question rather than a
// stylistic one:
//
//   - A COUNT is composed by the bot from its own numbers. It cannot ping, cannot carry a
//     slur, and passes the emit gate trivially.
//   - A CORPUS WORD is text somebody typed, on public display, with no reply chain or human
//     context around it. That is the autonomous poster's category of risk, so it goes through
//     the same gate every other emission does. The guard refuses it if the blocklist matches,
//     and this falls back to a count.
//
// The returned bool says whether the line is corpus-derived, so the caller can retry with a
// count when the gate refuses it rather than leaving the status frozen.
func (s *Service) presenceLine(st storage.Status) (line string, fromCorpus bool) {
	if s.topics != nil && s.presenceOpts.CorpusWordChance > 0 &&
		rand.Float64() < s.presenceOpts.CorpusWordChance {
		if word := s.topics.TopTopicWord(); word != "" {
			return "thoughts about " + word, true
		}
	}
	return s.countLine(st), false
}

// countLine picks the next fact in the rotation.
//
// A fact is skipped when its number is zero, because "knows 0 people" on a fresh deploy reads
// as broken rather than as new. If every fact is zero the bot says it is listening, which on
// an empty corpus is exactly true.
func (s *Service) countLine(st storage.Status) string {
	facts := []struct {
		n    uint64
		text string
	}{
		{uint64(max(st.Ngrams, 0)), "%s phrases it has picked up"},
		{st.Learned, "%s messages go by"},
		{uint64(max(st.Topics, 0)), "%s words it knows"},
		{uint64(max(st.Names, 0)), "%s people it has met"},
		{uint64(max(st.NameTopics, 0)), "%s things said about people"},
		{st.ImageCache, "%s images it remembers"},
	}

	// Round robin over the facts that have a number worth showing.
	for range len(facts) {
		f := facts[s.presenceAt%len(facts)]
		s.presenceAt++
		if f.n > 0 {
			return fmt.Sprintf(f.text, commas(f.n))
		}
	}
	return "an empty corpus, waiting"
}

// updatePresence is called from reportStatus with the counts it already has.
func (s *Service) updatePresence(st storage.Status) {
	if !s.presenceOpts.Enabled || s.presence == nil {
		return
	}

	line, fromCorpus := s.presenceLine(st)
	if s.presence.Presence(line) {
		return
	}
	if !fromCorpus {
		// A refused count line means the gate is refusing everything, which is a real
		// condition and not this feature's to solve. The guard has already logged it.
		return
	}
	// A corpus word the blocklist covers. Fall back to a count rather than leaving the status
	// frozen: a stale status line is how a running bot comes to look like a dead one.
	s.presence.Presence(s.countLine(st))
}

// commas groups a number for reading. A corpus reaches seven figures and an unbroken run of
// digits is the one thing in a status line that is genuinely hard to read at a glance.
func commas(n uint64) string {
	s := strconv.FormatUint(n, 10)
	if len(s) <= 3 {
		return s
	}

	var out strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		out.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if out.Len() > 0 {
			out.WriteByte(',')
		}
		out.WriteString(s[i : i+3])
	}
	return out.String()
}

// CorpusTopics returns a Topics backed by the corpus, mirroring SessionLatency above: the
// adapter lives here so cmd/bot does not have to know how a topic word gets chosen.
func CorpusTopics(corpora *storage.Set) Topics { return corpusTopics{corpora: corpora} }

type corpusTopics struct{ corpora *storage.Set }

// topicScanLimit bounds how much of the vocabulary one lookup walks.
//
// The bucket is one key per distinct word and reaches tens of thousands on a real corpus, so
// an unbounded walk would be finding 11's shape on a ticker. Three hundred sequential
// eight-byte reads is nothing, and starting from a random letter is what stops the answer
// always being a word beginning with "a".
const topicScanLimit = 300

// topicMinCount is the frequency floor. A word said twice is not what the server is about,
// and putting it in the status line would read as the bot quoting noise.
const topicMinCount = 5

// TopTopicWord picks a frequent word from a random slice of the vocabulary.
//
// Random slice rather than the global maximum, and that is a feature rather than an
// approximation: the single most frequent non-stop word in a corpus never changes, so a status
// line built from it would say the same thing forever. Sampling gives a line that moves while
// still only ever showing something the server genuinely says a lot.
func (c corpusTopics) TopTopicWord() string {
	if c.corpora == nil {
		return ""
	}

	// One guild's vocabulary, chosen at random, rather than a word merged across servers. The
	// presence line is one line for a bot that is in several servers, so it has to be a word
	// from somewhere rather than a word from everywhere: merging vocabularies would put one
	// server's inside joke in front of another, which is the thing M31 exists to stop.
	guilds := c.corpora.Guilds()
	if len(guilds) == 0 {
		return ""
	}
	store, err := c.corpora.For(guilds[rand.IntN(len(guilds))])
	if err != nil {
		return ""
	}

	// A random starting letter. rand's top-level functions are goroutine-safe and auto-seeded,
	// which is the rule this repo runs on: there is no shared *rand.Rand anywhere.
	from := string(rune('a' + rand.IntN(26)))

	var best string
	var bestCount uint64
	_ = store.View(func(r *storage.Reader) error {
		r.ScanTopics(from, topicScanLimit, func(word string, count uint64) bool {
			if count < topicMinCount || count <= bestCount {
				return true
			}
			// Filtered HERE rather than in storage, which must not learn what a word means.
			// Stop words are excluded for the obvious reason and short tokens because a
			// two-character word carries no sense on its own, even though the corpus counts
			// them deliberately (finding G10).
			if len([]rune(word)) < 4 || text.IsStopWord(word) {
				return true
			}
			best, bestCount = word, count
			return true
		})
		return nil
	})
	return best
}

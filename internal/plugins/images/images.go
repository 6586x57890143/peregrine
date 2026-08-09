// Package images is the repost feature: the bot remembers image and Tenor URLs people
// post, and occasionally posts one back.
//
// SPEC.md section 6 calls this the cheapest chaos in the bot, and section 4's A7 calls it
// an unattributed republishing channel. Both are true, which is why this package exists
// rather than the feature being deleted: it takes a URL somebody else posted and
// republishes it under the bot's own name, in a channel of the bot's choosing, so the
// operator wears whatever comes out.
//
// # The caps and the delete rule are the store's, not this package's
//
// storage.Writer.AddImageURL enforces the cache size and the per-author cap, and
// DeleteImagesByMessage revokes what a deleted message contributed. That placement is
// deliberate, for the same reason CheckLearn lives inside the learn path: a rule applied by
// the current caller is not a rule. What lives here is the decision to remember a URL at
// all and the decision to post one.
//
// Those mitigations were specified in M5 and could not be built until M11b, because the
// cache was keyed by the URL alone and nothing recorded where an entry came from.
package images

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/channels"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/text"
)

// Only these two sources are cached. Discord's own CDN and Tenor are the two things people
// post that are safe to hand back verbatim: they are already public, already hosted
// elsewhere, and carry no token in the URL that would leak with them.
var (
	discordCDN = regexp.MustCompile(`^https?://cdn\.discordapp\.com/\S+$`)
	tenor      = regexp.MustCompile(`^https?://tenor\.com/view/\S+$`)
)

// Guard is the send chokepoint, so a repost cannot skip mention suppression, the emit gate
// or the ignore list. The emit gate matters more here than it looks: a Tenor slug carries
// hyphen-separated words, so a URL can contain a slur that never entered the corpus as text.
type Guard interface {
	Send(channelID, content string) (*discordgo.Message, bool)
}

// Options are the dials, all from the environment.
type Options struct {
	// Chance is the repost probability for an ambient message, Direct for one that
	// addressed the bot.
	//
	// Direct is deliberately the LOWER of the two: when the bot is already answering a
	// mention it is contributing to the channel anyway, so an unrelated image on top of the
	// reply is noise rather than chaos.
	Chance float64
	Direct float64

	// CacheSize bounds the cache and MaxPerAuthor bounds one person's share of it. The
	// second must be below the first or it is not a cap, and config refuses that pairing.
	CacheSize    int
	MaxPerAuthor int
}

// Service is the feature.
type Service struct {
	store    *storage.Store
	guard    Guard
	channels channels.Resolver
	opts     Options

	mu     sync.Mutex
	recent []string
}

// New builds the service.
func New(store *storage.Store, guard Guard, channels channels.Resolver, opts Options) *Service {
	return &Service{store: store, guard: guard, channels: channels, opts: opts}
}

func (s *Service) Name() string { return "images" }

// Init loads the cache from the corpus.
func (s *Service) Init(core.Deps) error {
	var urls []string
	if err := s.store.View(func(r *storage.Reader) error {
		var err error
		urls, err = r.ImageURLs()
		return err
	}); err != nil {
		log.Printf("[ERR] Failed to load image URLs from the corpus: %v", err)
		return nil
	}
	s.set(urls)
	log.Printf("[INFO] Loaded %d image URLs from the corpus.", len(urls))
	return nil
}

// Start does nothing: this feature has no background work. The three sleeping goroutines
// per game that the word games needed are not a pattern to copy.
func (s *Service) Start(context.Context) error { return nil }

// Shutdown does nothing, for the same reason.
func (s *Service) Shutdown(context.Context) error { return nil }

// Capture remembers at most one URL from a message.
//
// One rather than all of them, which is the original behaviour and worth keeping: a message
// with twelve attachments would otherwise spend twelve of the author's cache slots on one
// post.
//
// It FAILS CLOSED on a state-cache miss. The check exists to keep the bot from reposting
// NSFW media into a channel of its choosing, and "we could not tell" has to mean "do not"
// for that to be worth anything. A miss is rare and transient, and the cost of being wrong
// in the safe direction is one image not being remembered.
func (s *Service) Capture(channelID, messageID, authorID, content string, attachments []Attachment) {
	ch, ok := s.channels.Channel(channelID)
	if !ok {
		log.Printf("[WARN] channel %s is not in the state cache, not caching this URL", channelID)
		return
	}
	if ch.NotSafeForWork() {
		log.Printf("[INFO] Skipping image cache for NSFW-flagged or named channel #%s", ch.Name)
		return
	}

	var candidates []string
	for _, att := range attachments {
		if strings.HasPrefix(att.ContentType, "image/") && discordCDN.MatchString(att.URL) {
			candidates = append(candidates, att.URL)
		}
	}
	for _, word := range text.Tokenize(content) {
		if discordCDN.MatchString(word) || tenor.MatchString(word) {
			candidates = append(candidates, word)
		}
	}
	if len(candidates) == 0 {
		return
	}
	chosen := candidates[rand.IntN(len(candidates))]

	// The URL is attributed to its message and its author, which is what lets the store cap
	// one author's share and lets a later deletion revoke it. Both caps and the trim are
	// the store's, so a second caller cannot skip them.
	//
	// NOT holding the mutex across this. It used to wrap the whole function including the
	// write transaction, so one goroutine's bbolt write (which serializes against every
	// other write in the process) also blocked every other capture from reading the slice.
	var urls []string
	if err := s.store.Update(func(w *storage.Writer) error {
		if err := w.AddImageURL(chosen, messageID, authorID, s.opts.CacheSize, s.opts.MaxPerAuthor); err != nil {
			return fmt.Errorf("save image URL: %w", err)
		}
		var err error
		urls, err = w.ImageURLs()
		return err
	}); err != nil {
		log.Printf("[WARN] DB operation for the image cache failed: %v", err)
		return
	}

	// Updated ONLY after the write succeeded, so the two cannot disagree about what is
	// repostable.
	s.set(urls)
	log.Printf("[IMG] Captured URL: %s, cache size: %d", chosen, len(urls))
}

// Attachment is the part of a Discord attachment this package reads.
type Attachment struct {
	URL         string
	ContentType string
}

// MaybeRepost posts a cached URL into the channel, on a roll of the dice.
//
// The DESTINATION gets the same NSFW check the origin gets, which SPEC.md section 4.2 asks
// for. Capture refuses to remember media from an NSFW channel; without this a repost could
// carry an image out of a general channel into one the bot has no business posting in.
func (s *Service) MaybeRepost(channelID string, addressed bool) {
	chance := s.opts.Chance
	if addressed {
		chance = s.opts.Direct
	}
	if rand.Float64() >= chance {
		return
	}

	ch, ok := s.channels.Channel(channelID)
	if !ok {
		log.Printf("[REPOST] channel %s is not in the state cache, not reposting there", channelID)
		return
	}
	if ch.NotSafeForWork() {
		return
	}

	s.mu.Lock()
	var url string
	if len(s.recent) > 0 {
		url = s.recent[rand.IntN(len(s.recent))]
	}
	s.mu.Unlock() // released before the send

	if url == "" {
		return
	}

	// Logged as an attempt rather than a success, because the guard may refuse it and says
	// so itself. Claiming success after a call that reports nothing would record a repost
	// Discord turned down as one that happened.
	log.Printf("[REPOST] Reposting image: %s", url)
	s.guard.Send(channelID, url)
}

// Forget drops every cached URL the given messages contributed.
//
// This is the deleted-message rule: a deletion is a strong signal that the content should
// not be republished, and the bot reposting something a moderator or its own author has
// just removed is the worst version of this feature.
//
// It is deliberately silent about IDs it does not hold, which is almost all of them: every
// message deletion in every channel the bot can see arrives here.
func (s *Service) Forget(messageIDs ...string) {
	removed := 0
	var urls []string
	if err := s.store.Update(func(w *storage.Writer) error {
		for _, id := range messageIDs {
			n, err := w.DeleteImagesByMessage(id)
			if err != nil {
				return err
			}
			removed += n
		}
		if removed == 0 {
			return nil
		}
		var err error
		urls, err = w.ImageURLs()
		return err
	}); err != nil {
		log.Printf("[WARN] could not revoke cached images for deleted messages: %v", err)
		return
	}
	if removed == 0 {
		return
	}

	s.set(urls)
	log.Printf("[IMG] Dropped %d cached URL(s) whose source message was deleted.", removed)
}

// Cached reports how many URLs are held, for the status line and for tests.
func (s *Service) Cached() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recent)
}

func (s *Service) set(urls []string) {
	s.mu.Lock()
	s.recent = urls
	s.mu.Unlock()
}

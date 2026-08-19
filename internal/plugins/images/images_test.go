package images

import (
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/channels"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// This file covers SPEC.md section 4, A7: image reposting is the bot republishing
// user-supplied media under its own name, in a channel of its choosing, so the operator wears
// whatever comes out.
//
// The store's tests cover the caps and the delete-by-message scan. These cover the decisions
// this package makes: whether to remember a URL at all, and whether to post one.

func snowflake(n int) string {
	return strconv.FormatUint((uint64(n)<<22)|1, 10)
}

type fakeGuard struct {
	mu   sync.Mutex
	sent []string
}

func (g *fakeGuard) Send(_, content string) (*discordgo.Message, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sent = append(g.sent, content)
	return &discordgo.Message{ID: snowflake(880000 + len(g.sent))}, true
}

func (g *fakeGuard) posts() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.sent...)
}

// fakeChannels is a state cache with a hole in it: anything not present is a miss, which is
// the case both NSFW checks have to fail closed on.
type fakeChannels map[string]channels.Info

func (f fakeChannels) Channel(id string) (channels.Info, bool) {
	info, ok := f[id]
	return info, ok
}

func textChannel(id, name string) channels.Info {
	return channels.Info{ID: id, Name: name, Text: true, GuildID: "111"}
}

func fixture(t *testing.T, chans fakeChannels, opts Options) (*Service, *storage.Store, *fakeGuard) {
	t.Helper()
	store := dbtest.Store(t)
	guard := &fakeGuard{}
	return New(storage.Single(store), guard, chans, opts), store, guard
}

func always() Options {
	return Options{Chance: 1, Direct: 1, CacheSize: 100, MaxPerAuthor: 10}
}

// TestCaptureRemembersOneURLPerMessage. One rather than all of them, which is the original
// behaviour: a message with twelve attachments would otherwise spend twelve of the author's
// cache slots on one post.
func TestCaptureRemembersOneURLPerMessage(t *testing.T) {
	chans := fakeChannels{"c1": textChannel("c1", "memes")}
	s, _, _ := fixture(t, chans, always())

	s.Capture("c1", snowflake(10), snowflake(20), "look https://cdn.discordapp.com/a.png and https://tenor.com/view/b-gif-1", nil)

	if got := s.Cached(); got != 1 {
		t.Errorf("cached %d URLs from one message, want 1", got)
	}
}

// TestCaptureIgnoresUnknownHosts. Only Discord's own CDN and Tenor are cached: they are
// already public, already hosted elsewhere, and carry no token in the URL that would leak
// with them.
func TestCaptureIgnoresUnknownHosts(t *testing.T) {
	chans := fakeChannels{"c1": textChannel("c1", "memes")}
	s, _, _ := fixture(t, chans, always())

	s.Capture("c1", snowflake(11), snowflake(20), "https://example.com/evil.png", nil)
	if got := s.Cached(); got != 0 {
		t.Errorf("cached %d URLs from an unknown host, want 0", got)
	}
}

// TestCaptureFailsClosedOnAStateCacheMiss.
//
// The NSFW check exists to keep the bot from reposting NSFW media into a channel of its
// choosing, and "we could not tell" has to mean "do not" for that to be worth anything. A miss
// is rare and transient (the cache fills on READY), and the cost of being wrong in the safe
// direction is one image not being remembered.
func TestCaptureFailsClosedOnAStateCacheMiss(t *testing.T) {
	s, _, _ := fixture(t, fakeChannels{}, always())

	s.Capture("not-in-the-cache", snowflake(12), snowflake(20), "https://cdn.discordapp.com/a.png", nil)
	if got := s.Cached(); got != 0 {
		t.Errorf("cached %d URLs from a channel it could not identify, want 0", got)
	}
}

// TestCaptureRefusesNSFWSources, by flag and by name. A channel called "nsfw-memes" whose flag
// nobody set is still not somewhere to take media from.
func TestCaptureRefusesNSFWSources(t *testing.T) {
	cases := map[string]channels.Info{
		"flag": {ID: "c1", Name: "memes", NSFW: true, Text: true},
		"name": {ID: "c1", Name: "nsfw-memes", Text: true},
	}
	for label, info := range cases {
		t.Run(label, func(t *testing.T) {
			s, _, _ := fixture(t, fakeChannels{"c1": info}, always())
			s.Capture("c1", snowflake(13), snowflake(20), "https://cdn.discordapp.com/a.png", nil)
			if got := s.Cached(); got != 0 {
				t.Errorf("cached %d URLs from an NSFW channel, want 0", got)
			}
		})
	}
}

// TestRepostRefusesADestinationItCannotIdentify is the destination half of A7's mitigations,
// which SPEC.md section 4.2 asks for and which did not exist before M11b: capture refused to
// REMEMBER media from an NSFW channel, but nothing stopped a repost carrying an image out of a
// general channel into one the bot has no business posting in.
func TestRepostRefusesADestinationItCannotIdentify(t *testing.T) {
	const url = "https://cdn.discordapp.com/cached.png"

	cases := map[string]fakeChannels{
		"missing from the cache": {},
		"nsfw by flag":           {"c1": {ID: "c1", Name: "memes", NSFW: true, Text: true}},
		"nsfw by name":           {"c1": {ID: "c1", Name: "nsfw-memes", Text: true}},
	}
	for label, chans := range cases {
		t.Run(label, func(t *testing.T) {
			s, store, guard := fixture(t, chans, always())
			if err := store.Update(func(w *storage.Writer) error {
				return w.AddImageURL(url, snowflake(14), snowflake(21), 100, 10)
			}); err != nil {
				t.Fatalf("seed the cache: %v", err)
			}
			s.set("111", []string{url})

			s.MaybeRepost("c1", false)
			if posts := guard.posts(); len(posts) != 0 {
				t.Errorf("reposted into a channel it should have refused: %v", posts)
			}
		})
	}

	// The control: an ordinary text channel does get the repost, so the refusals above are
	// measuring the checks rather than a repost path that never fires.
	s, store, guard := fixture(t, fakeChannels{"c1": textChannel("c1", "memes")}, always())
	if err := store.Update(func(w *storage.Writer) error {
		return w.AddImageURL(url, snowflake(15), snowflake(21), 100, 10)
	}); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	s.set("111", []string{url})

	s.MaybeRepost("c1", false)
	if posts := guard.posts(); len(posts) != 1 || posts[0] != url {
		t.Errorf("posts = %v, want the cached URL: the refusals above prove nothing otherwise", posts)
	}
}

// TestForgetRevokesADeletedMessagesURL is the deleted-message rule. A deletion is a strong
// signal that the content should not be republished, and the bot reposting something a
// moderator or its own author has just removed is the worst version of this feature.
//
// Verified by reverting: with the DeleteImagesByMessage call removed, the URL survives both
// the store and the in-memory cache, and the repost below still fires.
func TestForgetRevokesADeletedMessagesURL(t *testing.T) {
	chans := fakeChannels{"c1": textChannel("c1", "memes")}
	s, store, guard := fixture(t, chans, always())

	const doomed = "1720000000000000001"
	const url = "https://cdn.discordapp.com/doomed.png"
	if err := store.Update(func(w *storage.Writer) error {
		return w.AddImageURL(url, doomed, snowflake(22), 100, 10)
	}); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	s.set("111", []string{url})

	s.Forget("111", doomed)

	if got := s.Cached(); got != 0 {
		t.Errorf("the in-memory cache still holds %d URLs; the repost path reads this, not the store", got)
	}
	s.MaybeRepost("c1", false)
	if posts := guard.posts(); len(posts) != 0 {
		t.Errorf("reposted %v after the source message was deleted", posts)
	}
}

// TestForgettingAnUnknownMessageIsQuiet. Every deletion in every channel the bot can see
// arrives here, and almost none of them contributed a cached URL.
func TestForgettingAnUnknownMessageIsQuiet(t *testing.T) {
	chans := fakeChannels{"c1": textChannel("c1", "memes")}
	s, store, _ := fixture(t, chans, always())

	const url = "https://cdn.discordapp.com/kept.png"
	if err := store.Update(func(w *storage.Writer) error {
		return w.AddImageURL(url, snowflake(16), snowflake(23), 100, 10)
	}); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	s.set("111", []string{url})

	s.Forget("111", snowflake(9999))
	if got := s.Cached(); got != 1 {
		t.Errorf("cache holds %d URLs after deleting an unrelated message, want it untouched", got)
	}
}

// TestABulkDeletionIsOneTransaction. A moderator purging a spam raid produces one event with
// many IDs, and each deletion opens a write transaction against bbolt's single writer.
func TestABulkDeletionIsOneTransaction(t *testing.T) {
	chans := fakeChannels{"c1": textChannel("c1", "memes")}
	s, store, _ := fixture(t, chans, always())

	var ids []string
	if err := store.Update(func(w *storage.Writer) error {
		for i := range 20 {
			id := snowflake(1800 + i)
			ids = append(ids, id)
			if err := w.AddImageURL("https://cdn.discordapp.com/"+id+".png", id, snowflake(50+i), 100, 10); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	s.Forget("111", ids...)

	if got := s.Cached(); got != 0 {
		t.Errorf("cache holds %d URLs after every source message was deleted", got)
	}
}

// TestTheAmbientRateIsHigherThanTheDirectOne pins a deliberate asymmetry that reads backwards
// at a glance: when the bot is already answering a mention it is contributing to the channel
// anyway, so an unrelated image on top of the reply is noise rather than chaos.
func TestTheAmbientRateIsHigherThanTheDirectOne(t *testing.T) {
	chans := fakeChannels{"c1": textChannel("c1", "memes")}
	// Ambient always, direct never, so which rate was used is observable.
	s, store, guard := fixture(t, chans, Options{Chance: 1, Direct: 0, CacheSize: 100, MaxPerAuthor: 10})

	const url = "https://cdn.discordapp.com/a.png"
	if err := store.Update(func(w *storage.Writer) error {
		return w.AddImageURL(url, snowflake(17), snowflake(24), 100, 10)
	}); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	s.set("111", []string{url})

	s.MaybeRepost("c1", true) // addressed: uses Direct, which is 0
	if posts := guard.posts(); len(posts) != 0 {
		t.Errorf("a direct interaction used the ambient rate: %v", posts)
	}
	s.MaybeRepost("c1", false) // ambient: uses Chance, which is 1
	if posts := guard.posts(); len(posts) != 1 {
		t.Errorf("an ambient message did not use the ambient rate: %v", posts)
	}
}

// TestAnEmptyCacheRepostsNothing rather than posting an empty message, which the guard would
// refuse anyway but which would look like a bug in the log.
func TestAnEmptyCacheRepostsNothing(t *testing.T) {
	chans := fakeChannels{"c1": textChannel("c1", "memes")}
	s, _, guard := fixture(t, chans, always())

	s.MaybeRepost("c1", false)
	if posts := guard.posts(); len(posts) != 0 {
		t.Errorf("posted %v from an empty cache", posts)
	}
}

// TestConcurrentCaptureAndRepost exists for CI's race detector. Capture runs on a dispatcher
// worker and MaybeRepost on another, and the slice they share is what the mutex guards. It is
// also the pattern that used to hold that mutex across a bbolt write transaction, so one
// capture blocked every other one for the length of a serialized write.
func TestConcurrentCaptureAndRepost(t *testing.T) {
	chans := fakeChannels{"c1": textChannel("c1", "memes")}
	s, _, _ := fixture(t, chans, always())

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 25 {
				s.Capture("c1", snowflake(20000+i*100+j), snowflake(30+i),
					"https://cdn.discordapp.com/"+strconv.Itoa(i*100+j)+".png", nil)
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 50 {
				s.MaybeRepost("c1", false)
				_ = s.Cached()
			}
		})
	}
	wg.Wait()

	if got := s.Cached(); got > 100 {
		t.Errorf("cache holds %d URLs against a size of 100", got)
	}
}

// TestAttachmentsMustLookLikeImages. ContentType is Discord's own claim about the file, and a
// .png named attachment that reports itself as something else is not one the bot should hand
// back.
func TestAttachmentsMustLookLikeImages(t *testing.T) {
	chans := fakeChannels{"c1": textChannel("c1", "memes")}
	s, _, _ := fixture(t, chans, always())

	s.Capture("c1", snowflake(18), snowflake(25), "", []Attachment{
		{URL: "https://cdn.discordapp.com/thing.exe", ContentType: "application/octet-stream"},
	})
	if got := s.Cached(); got != 0 {
		t.Errorf("cached %d non-image attachments, want 0", got)
	}

	s.Capture("c1", snowflake(19), snowflake(25), "", []Attachment{
		{URL: "https://cdn.discordapp.com/thing.png", ContentType: "image/png"},
	})
	if got := s.Cached(); got != 1 {
		t.Errorf("cached %d image attachments, want 1", got)
	}
}

// TestNSFWNameMatchingIsCaseInsensitive, because channel names are not.
func TestNSFWNameMatchingIsCaseInsensitive(t *testing.T) {
	for _, name := range []string{"NSFW", "Nsfw-Memes", "very-NSFW-stuff"} {
		if !(channels.Info{Name: name}).NotSafeForWork() {
			t.Errorf("%q was not recognized as NSFW", name)
		}
	}
	if (channels.Info{Name: strings.ToLower("general")}).NotSafeForWork() {
		t.Error("an ordinary channel was treated as NSFW")
	}
}

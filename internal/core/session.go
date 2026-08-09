package core

import (
	"fmt"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// NewSession builds the single shared *discordgo.Session.
//
// IntentsGuilds is new in M3 and is not a performance tweak. Without it
// s.State.Guilds is always empty, and two things depended on it:
//
//   - Custom emote output. The :shortcode: resolver walks s.State.Guilds to turn
//     a learned shortcode into a real <a:name:id> reference, so it has never once
//     succeeded. In a meme-heavy server the server's own emotes are most of the
//     register, which makes this an engagement bug before it is anything else.
//   - The NSFW check. With no cached guild state the channel lookup falls back to
//     a REST call on every single message, which is likely the largest
//     rate-limit consumer in the bot.
//
// One intent fixes both (SPEC.md section 8, finding 7).
//
// MESSAGE CONTENT is privileged and is what the bot exists for: without it
// Discord refuses the connection outright and peregrine can read nothing. That
// is why callers must pair this with WatchReady rather than trusting Open to
// report the problem.
func NewSession(token string) (*discordgo.Session, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	s.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsMessageContent
	return s, nil
}

// readyTimeout bounds how long WatchReady's waiter blocks on Discord's READY.
// Generous relative to a healthy connect, which takes a second or two, because
// being wrong in the impatient direction is a crash loop on a slow network while
// being wrong in the patient direction is only a slower failure on a broken one.
const readyTimeout = 45 * time.Second

// WatchReady arms a one-shot watch for Discord's READY and returns a function
// that blocks until it arrives, reporting a diagnosable error if it never does.
//
// It is split from the waiting deliberately and must be called BEFORE Open.
// discordgo starts its listen goroutine inside Open, so READY can be dispatched
// the moment Open returns; registering the handler afterwards races against that,
// and losing the race means waiting the full timeout and then failing startup on
// a connection that was perfectly healthy.
//
// The reason to check at all: Open returns as soon as the identify payload has
// been sent, never that Discord accepted it. When the gateway rejects the
// identify (close code 4014, "disallowed intents", meaning the application was
// not granted a privileged intent it asked for) discordgo neither surfaces the
// close code nor gives up. It reconnects, is rejected again, and loops. Open
// having returned nil, startup carries on and logs that the bot is running.
//
// For peregrine that failure is close to invisible. The process looks healthy,
// holds no gateway connection, and receives no messages, so it learns nothing and
// replies to nobody, which is indistinguishable from a quiet channel. Under
// restart: unless-stopped in the compose file it is a silent crash loop. Waiting
// for READY is what separates "connected" from "has not failed loudly yet", so
// this error is meant to be fatal at startup: a bot that cannot connect has
// nothing useful to do, and a container that exits with an explanation is far
// easier to diagnose than one claiming success.
func WatchReady(s *discordgo.Session) func() error {
	return watchReady(s, readyTimeout)
}

// readyWatcher is the sliver of *discordgo.Session watchReady needs, so both the
// timeout and the dispatch path are testable with no gateway connection.
type readyWatcher interface {
	AddHandler(handler any) func()
}

func watchReady(s readyWatcher, timeout time.Duration) func() error {
	ready := make(chan struct{})
	var once sync.Once
	remove := s.AddHandler(func(*discordgo.Session, *discordgo.Ready) {
		// discordgo re-dispatches READY on every successful reconnect and closing
		// a closed channel panics. sync.Once rather than a bool because the
		// dispatch goroutine and the waiter below are different goroutines.
		once.Do(func() { close(ready) })
	})

	return func() error {
		defer remove()
		select {
		case <-ready:
			return nil
		case <-time.After(timeout):
			return fmt.Errorf("no READY from Discord within %s: the gateway is refusing this connection. "+
				"The usual cause is the MESSAGE CONTENT privileged intent not being enabled. Tick "+
				"\"Message Content Intent\" under Bot in the Discord Developer Portal. Peregrine cannot "+
				"run without it: reading messages is the entire point", timeout)
		}
	}
}

// SessionEmoji resolves a :shortcode: against the guilds the session can see.
//
// It lives here because core owns the session, and it is the seam that keeps discordgo out
// of internal/text: that package declares the minimal EmojiResolver interface it needs and
// this satisfies it structurally, so the sentence cleaner is testable with a two-line fake
// instead of a gateway connection.
//
// It walks s.State.Guilds, which was empty for the entire life of this bot because the
// session never requested IntentsGuilds, so the resolver had never once succeeded and
// peregrine had never spoken in the server's own emotes. NewSession above requests the
// intent; this is the code that finally benefits (SPEC.md section 8, finding 7). In a meme
// server the server's own emotes are most of the register, which makes this the largest
// single improvement to how the output READS in the whole restructure.
func SessionEmoji(s *discordgo.Session) EmojiResolver { return sessionEmoji{s: s} }

// EmojiResolver is the shape internal/text's sentence cleaner needs. Declared here rather
// than imported so core keeps its own dependency list, and satisfied structurally at the
// call site: an interface value with a compatible method set assigns across packages.
type EmojiResolver interface {
	ResolveEmoji(name string) (string, bool)
}

type sessionEmoji struct{ s *discordgo.Session }

// ResolveEmoji returns the <:name:id> form of a guild emote, or false.
//
// An unknown shortcode returns false rather than an empty string, and the caller leaves the
// text alone: a word between colons that no guild has an emote for is ordinary text, and
// mangling it would be worse than ignoring it, because the corpus is full of things people
// typed.
func (e sessionEmoji) ResolveEmoji(name string) (string, bool) {
	if e.s == nil || e.s.State == nil {
		return "", false
	}
	for _, guild := range e.s.State.Guilds {
		for _, emoji := range guild.Emojis {
			if emoji.Name == name {
				return emoji.MessageFormat(), true
			}
		}
	}
	return "", false
}

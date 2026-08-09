// Package names resolves the people a message is about, and records what the corpus
// knows about them.
//
// It exists as a package because three subsystems need it and each needed a different
// part: the learn path records a message's mentions, the reply path needs the same list
// without paying for it twice, and generation needs to know whether a prompt word is
// somebody's name. Keeping them together keeps one answer to "what is this person
// called", which matters because a Discord user has up to three: a username, a per-guild
// nickname, and whatever people type at them.
//
// # The two-phase split is about transaction duration
//
// Resolve makes REST calls and touches no corpus. FromContent reads the corpus and makes
// no network call. That was one pass once, which held a bbolt read transaction open
// across N HTTP round trips: a read transaction holds mmaplock.RLock for its entire life,
// so every writer waiting to grow the mmap was waiting on Discord's latency
// (SPEC.md section 8, finding 1).
//
// The split is enforced by the types rather than by a convention. FromContent takes the
// *storage.Reader it is already inside, and a Reader has no method that starts a
// transaction, so the version that could nest does not compile.
package names

import (
	"log"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/text"
)

// User is one person a message referred to, under one of their names.
//
// The same person appears more than once when they have a nickname, deliberately: the
// corpus learns both spellings, because people address each other by whichever they see.
// Name is the spelling that appeared; Username is the canonical one.
type User struct {
	Name     string
	UserID   string
	Username string
}

// mentionMarkup matches a user mention, in both the plain and the legacy nickname form.
//
// Deliberately not a role mention: <@&123> has an ampersand where a digit would be, so it does
// not match, and a role is not a person this package can name.
var mentionMarkup = regexp.MustCompile(`<@!?(\d+)>`)

// Spellings returns every name one person is addressed by, display form first.
//
// A Discord user has up to three names and people type whichever one they see: a per-guild
// nickname, a global display name, and the username underneath. Every entry carries the same
// UserID and Username, so Record canonicalizes them all to the username and writes the rest as
// aliases pointing at it.
//
// This exists because three separate sites hand-built this and each got a different answer,
// which is finding 28's shape: two or more copies of one question, differing only in what each
// throws away. All three threw away GlobalName, and since usernames became lowercase handles
// that is the name most people are actually addressed by, so typing somebody's display name
// recognized nobody.
//
// Order is display-first because Substitute and Primary want the spelling a human would have
// typed, not the handle underneath it.
func Spellings(u *discordgo.User, member *discordgo.Member) []User {
	if u == nil {
		return nil
	}

	out := make([]User, 0, 3)
	seen := make(map[string]struct{}, 3)
	add := func(name string) {
		if name == "" {
			return
		}
		lower := strings.ToLower(name)
		if _, dup := seen[lower]; dup {
			return
		}
		seen[lower] = struct{}{}
		out = append(out, User{Name: name, UserID: u.ID, Username: u.Username})
	}

	if member != nil {
		add(member.Nick)
	}
	add(u.GlobalName)
	add(u.Username)
	return out
}

// Primary returns just the display spelling, for a caller that wants one User rather than all
// of them.
//
// It cannot panic on a nil user, which is why callers use this instead of indexing Spellings.
// A nil Author has turned up in a test fixture here before, and production always having one is
// not a reason to write the version that would crash if it did not.
//
// KNOWN LIMITATION, stated because it is narrow rather than because it is acceptable in
// general: a caller that keeps only this loses the other spellings, so an author who has BOTH a
// guild nickname and a different global name gets the nickname recorded and the global name
// not. Record always writes the username as the canonical form, and the moment anybody
// @mentions that person Resolve records all three, so the gap closes on its own.
func Primary(u *discordgo.User, member *discordgo.Member) User {
	all := Spellings(u, member)
	if len(all) == 0 {
		return User{}
	}
	return all[0]
}

// Substitute replaces user-mention markup with the display spelling of the person it refers to.
//
// The tokenizer keeps <@123> as ONE whole token on purpose, so without this a mention is an
// opaque blob that Canonical cannot resolve: the most explicit way to name somebody was
// invisible to every name-aware part of generation, and the corpus filled with ID tokens where
// a name belonged.
//
// An ID that is not in users is left exactly as it was. That is today's behaviour for every
// mention, and stripping it would drop a word out of the middle of a sentence, which costs the
// structure around it for no gain.
//
// No network access: the caller already resolved these, so this is a map lookup per mention.
func Substitute(content string, users []User) string {
	if content == "" || len(users) == 0 {
		return content
	}

	// First spelling wins, which after Spellings is the display form. Later entries for the
	// same person are the same person under a handle nobody types.
	byID := make(map[string]string, len(users))
	for _, u := range users {
		if u.UserID == "" || u.Name == "" {
			continue
		}
		if _, have := byID[u.UserID]; !have {
			byID[u.UserID] = u.Name
		}
	}

	return mentionMarkup.ReplaceAllStringFunc(content, func(match string) string {
		id := mentionMarkup.FindStringSubmatch(match)[1]
		if name, ok := byID[id]; ok {
			return name
		}
		return match
	})
}

// Session is the part of discordgo the resolver needs. Declared here so a test needs one
// method rather than a gateway connection.
type Session interface {
	GuildMember(guildID, userID string, options ...discordgo.RequestOption) (*discordgo.Member, error)
}

// Resolve returns the people explicitly @mentioned in a message, with their per-guild
// nicknames, plus the set of user IDs it consumed.
//
// The ID set goes back to the caller so FromContent does not add anyone twice, which is
// what stops a message that both mentions and names somebody counting them as two people.
func Resolve(s Session, mentions []*discordgo.User, guildID string) ([]User, map[string]struct{}) {
	users := []User{}
	seen := make(map[string]struct{}, len(mentions))

	for _, u := range mentions {
		if u == nil {
			continue
		}
		if _, ok := seen[u.ID]; ok {
			continue
		}
		seen[u.ID] = struct{}{}

		// The server nickname, when there is one. This is the only REST call on this path, and
		// it is the only spelling the gateway payload does not already carry.
		var member *discordgo.Member
		if guildID != "" && s != nil {
			if got, err := s.GuildMember(guildID, u.ID); err == nil {
				member = got
			}
		}

		users = append(users, Spellings(u, member)...)
	}
	return users, seen
}

// FromContent returns known names appearing in the text that were not @mentioned, and
// adds them to seen as it goes.
//
// It takes the Reader rather than opening a transaction, so a caller that already holds
// one can use it. seen may be nil.
func FromContent(r *storage.Reader, content string, seen map[string]struct{}) []User {
	if seen == nil {
		seen = map[string]struct{}{}
	}

	var users []User
	for _, word := range text.Tokenize(content) {
		lw := text.LowerExceptURLs(word)
		data, ok, err := r.Name(lw)
		if err != nil || !ok {
			continue
		}

		// The word found may be an alias (a nickname); the record with the user ID on it
		// is the canonical one.
		canonical := lw
		if data.Canonical != "" {
			canonical = data.Canonical
		}
		canonicalData, ok, err := r.Name(canonical)
		if err != nil || !ok || canonicalData.DiscordUserID == "" {
			continue
		}
		if _, dup := seen[canonicalData.DiscordUserID]; dup {
			continue
		}
		seen[canonicalData.DiscordUserID] = struct{}{}
		users = append(users, User{
			Name:     canonical,
			UserID:   canonicalData.DiscordUserID,
			Username: canonical,
		})
	}
	return users
}

// Canonical reports the canonical form of a token if the corpus recognizes it as a name
// or an alias.
//
// It takes the Reader it is already inside, and that signature is half of finding 1's
// fix. It used to open its own db.View, and its only caller runs inside the read
// transaction that wraps generation, so every recognized-name lookup was a nested bbolt
// transaction: an outer read holds mmaplock.RLock for its whole life, a writer waiting to
// grow the mmap queues for the write lock, and Go's RWMutex then queues the inner read
// behind that writer. Unrecoverable, no timeout, and likelier the bigger the file gets.
func Canonical(r *storage.Reader, token string) (string, bool) {
	if token == "" {
		return "", false
	}
	lower := text.LowerExceptURLs(token)
	data, ok, err := r.Name(lower)
	if err != nil || !ok {
		return "", false
	}
	if data.Canonical != "" {
		return data.Canonical, true
	}
	return lower, true
}

// Record writes a person's canonical name and, when they used a different one, an alias
// pointing at it. It returns the canonical form.
func Record(w *storage.Writer, name, discordUserID, username string) (string, error) {
	canonical := text.LowerExceptURLs(username)
	nameKey := text.LowerExceptURLs(name)

	canonicalData, _, err := w.Name(canonical)
	if err != nil {
		return "", err
	}
	canonicalData.Count++
	canonicalData.DiscordUserID = discordUserID
	if err := w.PutName(canonical, canonicalData); err != nil {
		return "", err
	}

	if nameKey != canonical {
		aliasData, _, err := w.Name(nameKey)
		if err != nil {
			return "", err
		}
		aliasData.DiscordUserID = discordUserID
		aliasData.Canonical = canonical
		if err := w.PutName(nameKey, aliasData); err != nil {
			return "", err
		}
	}

	return canonical, nil
}

// OfMessage is Resolve plus FromContent, in that order, for a caller that holds no
// transaction and wants the whole list.
//
// The store parameter is what makes the phase order visible: this function opens the read
// transaction itself, after the REST calls have finished, which is the arrangement finding
// 1 was about. A caller that already holds a Reader must use FromContent directly.
//
// A corpus read failure degrades to the @mentions alone rather than failing the message.
// Losing the names somebody typed costs the bot some association data; losing the message
// costs it the message.
func OfMessage(s Session, store *storage.Store, m *discordgo.MessageCreate, guildID string) []User {
	users, seen := Resolve(s, m.Mentions, guildID)

	var fromContent []User
	if err := store.View(func(r *storage.Reader) error {
		fromContent = FromContent(r, m.Content, seen)
		return nil
	}); err != nil {
		log.Printf("[WARN] name lookup for message %s failed, using @mentions only: %v", m.ID, err)
		return users
	}
	return append(users, fromContent...)
}

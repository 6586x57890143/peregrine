package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// One corpus per guild, M31.
//
// # Why separate FILES rather than a guild dimension on every key
//
// The bot used to keep one corpus for every guild it is in, so a phrase learned in one server
// could be generated into another: a leak of one server's chat into somebody else's, and a
// register problem on top of it, since a corpus blended across servers reads like neither.
//
// A guild prefix on every key would have touched all ten key builders in codec.go, the six
// buckets whose keys have no separator at all today, and 35 Reader plus 20 Writer methods. It
// would also have broken the cheap answers: Bucket.Stats().KeyN cannot be filtered by prefix,
// and Reader.Status calls it six times on the health ticker.
//
// The deciding reason is not the diff though, it is what the two designs make POSSIBLE. A
// prefix is isolation by convention: one call site that forgets it crosses guilds silently, and
// no test can cover the call site nobody has written yet. A separate *Store per guild cannot
// name another guild's keys at all, which is the same argument Reader already makes about
// nested transactions: the version that could go wrong does not compile.
//
// The costs are real and accepted. N files means N flocks and N mmaps, which is why the set is
// bounded. It also means bbolt's single writer is now per guild, which is a straight win for
// ingestion: a backfill in one server no longer serializes against live traffic in another.
type Set struct {
	dir string
	max int

	// gen is the learn generation stamped into a corpus the first time it is opened.
	//
	// A parameter rather than a constant because storage is the bottom layer and must not
	// import learn. It moved here from cmd/bot, which stamped it once at startup: that ran
	// before the gateway was even open, so it could not know a guild, and a corpus created
	// later would have carried no boundary for a repair to measure against.
	gen int

	mu     sync.Mutex
	open   map[string]*Store
	closed bool
}

// ErrNoGuild is returned by For when there is no guild to resolve a corpus for.
//
// A message with no guild is a DM, and DMs are not learned. There is deliberately no fallback
// corpus for them: a shared "no guild" file is exactly the thing this package was split up to
// remove, and it would be the file that every mistake drains into.
//
// It is also a trap worth naming. internal/plugins/ingest and internal/plugins/repair build a
// discordgo.MessageCreate around a message fetched over REST, and a REST message carries an
// EMPTY GuildID: they pass the guild separately because of it. Anything that reaches for
// m.GuildID instead of the threaded value would key every backfilled message in the bot under
// the empty guild, and this error is what turns that into a loud failure instead of a corpus
// nobody can explain.
var ErrNoGuild = errors.New("no guild id, so there is no corpus to use")

// ErrTooManyCorpora is returned by For when opening another guild would exceed the cap.
var ErrTooManyCorpora = errors.New("too many guild corpora open")

// ErrSetClosed is returned by For after Close.
//
// It exists because the alternative is worse than an error: without it a goroutine still in
// flight at shutdown would REOPEN the corpus it was looking for, creating a file and taking a
// flock that nothing left alive will ever release. The bot spawns exactly that kind of late
// work, self-learning being the clearest case, and a test found it by failing to delete its own
// temporary directory.
var ErrSetClosed = errors.New("corpus set is closed")

// Corpora is what every consumer takes: a way to get the corpus for one guild.
//
// An interface rather than *Set so that a test can hand a service one store, and so that
// nothing above this package can enumerate or close corpora it did not ask for. Fan-out over
// guilds is deliberately NOT on it: a service that wants every guild takes a *Set.
type Corpora interface {
	For(guildID string) (*Store, error)
}

// Single is the one-store adapter: every guild resolves to the same corpus.
//
// It exists for tests, which mostly care about one guild's behaviour and should not have to
// invent a guild ID to exercise it. It is not used in production, and using it there would
// undo this entire milestone.
func Single(s *Store) Corpora { return single{s} }

type single struct{ store *Store }

func (s single) For(string) (*Store, error) { return s.store, nil }

// corpusSuffix is the file extension. The name of a corpus is its guild ID, so the directory
// listing is the guild list.
const corpusSuffix = ".db"

// OpenSet prepares a directory of per-guild corpora. It opens none of them.
//
// Lazy rather than eager because the guild list is not knowable here: this runs before the
// gateway is open, and a bot that opened every file it found would also open corpora for guilds
// it has since been removed from.
func OpenSet(dir string, max, generation int) (*Set, error) {
	if dir == "" {
		return nil, errors.New("no corpus directory")
	}
	if max <= 0 {
		return nil, fmt.Errorf("refusing a corpus cap of %d", max)
	}
	// 0700 rather than 0750: the corpus is the most sensitive thing this process owns, and the
	// container runs as a single non-root user with no group to share it with.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create corpus directory %s: %w", dir, err)
	}
	return &Set{dir: dir, max: max, gen: generation, open: map[string]*Store{}}, nil
}

// For returns the corpus for one guild, opening it on first use.
//
// The returned *Store is owned by the Set and must not be closed by the caller.
func (s *Set) For(guildID string) (*Store, error) {
	if err := validGuildID(guildID); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrSetClosed
	}
	if store, ok := s.open[guildID]; ok {
		return store, nil
	}
	if len(s.open) >= s.max {
		// Refused rather than evicted. Closing a corpus to make room would drop the flock on a
		// file another goroutine is mid-transaction on, and an LRU over open databases is a
		// cache with a fsync in its eviction path. If a bot is really in more guilds than this,
		// the cap is the thing to raise.
		return nil, fmt.Errorf("%w: %d already open and PEREGRINE_MAX_GUILD_CORPORA is %d, so "+
			"guild %s got none", ErrTooManyCorpora, len(s.open), s.max, guildID)
	}

	store, err := Open(filepath.Join(s.dir, guildID+corpusSuffix))
	if err != nil {
		return nil, err
	}
	if s.gen > 0 {
		if err := store.Update(func(w *Writer) error {
			return w.RecordLearnGeneration(s.gen, time.Now())
		}); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("recording learn generation for guild %s: %w", guildID, err)
		}
	}
	s.open[guildID] = store
	return store, nil
}

// Guilds is every guild this set has a corpus for, open or merely on disk, sorted.
//
// On disk as well as open, because the loops that fan out over it run on tickers and would
// otherwise skip a guild until something happened to touch it. A corpus file IS the record
// that the bot has learned from a guild.
func (s *Set) Guilds() []string {
	seen := map[string]struct{}{}

	s.mu.Lock()
	for id := range s.open {
		seen[id] = struct{}{}
	}
	s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			id := strings.TrimSuffix(e.Name(), corpusSuffix)
			if id == e.Name() {
				continue
			}
			// Anything that is not a guild ID is somebody else's file, and this directory is
			// mounted. Skipped rather than opened.
			if validGuildID(id) != nil {
				continue
			}
			seen[id] = struct{}{}
		}
	}

	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Close closes every open corpus and reports every failure rather than the first.
//
// Joined rather than short-circuited: each corpus holds its own flock, and stopping at the
// first error would leave the rest held by a process that is exiting anyway.
func (s *Set) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true

	var errs []error
	for id, store := range s.open {
		if err := store.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close corpus for guild %s: %w", id, err))
		}
		delete(s.open, id)
	}
	return errors.Join(errs...)
}

// validGuildID refuses anything that is not a Discord snowflake.
//
// This is a path component, so it is a trust boundary rather than a formality: a guild ID
// containing a separator or a parent reference would put a corpus somewhere the operator did
// not mount, and an empty one is the DM case ErrNoGuild describes. Snowflakes are decimal
// digits, so digits-only is both the correct rule and the strictest one available.
func validGuildID(guildID string) error {
	if guildID == "" {
		return ErrNoGuild
	}
	for _, r := range guildID {
		if r < '0' || r > '9' {
			return fmt.Errorf("%q is not a guild id", guildID)
		}
	}
	return nil
}

// Backup writes one guild's corpus to a path, consistently.
//
// It exists on the Set rather than the caller reaching For and then Backup because a snapshot
// is a whole-file operation and the backup service should not hold a *Store it might keep: the
// set owns every handle's lifetime, and one place closing what another opened is how a flock
// outlives the thing that took it.
func (s *Set) Backup(guildID, path string) error {
	store, err := s.For(guildID)
	if err != nil {
		return err
	}
	return store.Backup(path)
}

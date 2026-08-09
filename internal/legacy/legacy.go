// Package legacy holds peregrine's original single-file implementation while it
// is taken apart one subsystem at a time (SPEC.md section 9). It is a holding
// pen, not a design.
//
// Every later milestone moves one subsystem out of here into a real package, so
// this package only ever shrinks, and M13 deletes it. It exists at all because
// two main packages cannot share code: turning cmd/bot into the entrypoint
// required the 3,200 lines it was calling to live somewhere importable, and
// moving them verbatim was the only way to keep `go build ./...` green at every
// commit while ending at merlin's layout.
//
// Nothing new goes in here. The M1 move was deliberately verbatim so the diff
// read as a rename, which means every defect catalogued in SPEC.md section 8 is
// still present and still sits at the line it was found at. Fix them where the
// milestone table says to fix them, in the package that takes ownership.
//
// The one exception to "verbatim" is the entrypoint itself: main() became
// Run(ctx), its six log.Fatal calls became returned errors, and the signal wait
// became a ctx wait. That was not cosmetic. log.Fatal calls os.Exit, which skips
// every deferred function, so the old startup path could fail after opening the
// corpus and never close it, leaving the bbolt flock held until the process was
// reaped. There is now no os.Exit anywhere in this package.
package legacy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"math"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/6586x57890143/peregrine/internal/activity"
	"github.com/6586x57890143/peregrine/internal/config"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/discordguard"
	"github.com/6586x57890143/peregrine/internal/ingest"
	"github.com/6586x57890143/peregrine/internal/markov"
	"github.com/6586x57890143/peregrine/internal/safety"
	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/text"
	"github.com/6586x57890143/peregrine/internal/wordgame"
	"github.com/6586x57890143/peregrine/voicenotes"
	"github.com/bwmarrin/discordgo"
)

// The twelve bucket-name constants that used to live here are gone, and with them
// this package's ability to name a bucket at all. They are unexported in
// internal/storage now, which is what stops the clustering package owning
// storage-layer names as it did: three of these were aliases of constants exported
// by an algorithm package, pointing the dependency the wrong way round
// (SPEC.md section 2).

// cfg is the loaded configuration, set once by Run before anything reads it.
//
// A package-level variable rather than a parameter threaded through 40 functions,
// because those functions are being deleted milestone by milestone and rewriting
// each signature twice is churn for no reader's benefit. As each subsystem moves
// out of this package it takes its own config fields with it as struct fields on
// a real type. Nothing here writes to it after Run sets it.
//
// The Creativity constant that used to sit here is GONE as of M7a, along with the
// scoring it was an exponent of. It was applied as pow(score, 1/(Creativity+0.01)),
// so at its 0.75 default the exponent was 1.316, which sharpened the distribution:
// the knob's arithmetic inverted its own name and could never reach the half of its
// own range that would add chaos. M2 deliberately refused to promote it for that
// reason, and PEREGRINE_TEMPERATURE replaces it now that the scoring underneath is a
// normalized log-probability and the dial actually moves. There is deliberately no
// PEREGRINE_CREATIVITY. ContextWindow and CoherencyBalance are gone entirely: they
// were declared and never read.
var cfg *config.Config

// genParams turns the config into the engine's dials.
//
// There is deliberately no package-level *markov.Generator, and the reason is the
// seam rather than style: a Generator holds a markov.Corpus, which here is a
// *storage.Reader BOUND TO ONE TRANSACTION. A Generator that outlived its
// transaction would hold a Reader whose transaction had closed, which is the class of
// bug the Reader type exists to make unwritable. So a Generator is constructed inside
// each store.View, which costs one small struct allocation per reply and cannot be
// wrong.
func genParams() markov.Params {
	return markov.Params{
		MaxNGram:           cfg.MaxNGram,
		Temperature:        cfg.Temperature,
		TopK:               cfg.TopK,
		TopP:               cfg.TopP,
		KNDiscount:         cfg.KNDiscount,
		KNRawMix:           cfg.KNRawMix,
		MinDistinctAuthors: cfg.MinDistinctAuthors,
		PromptRelevance:    cfg.PromptRelevanceBoost,
	}
}

// Four local types are gone here, and they are worth naming because each was a
// duplicate of something internal/corpus now owns.
//
// WordPosData and TopicAssociationData were both JSON shapes for a co-occurrence
// record: a count plus a sum of relative positions. They are corpus.TopicAssoc,
// whose MeanPosition method replaces the `data.PosSum / float64(data.Count)`
// division that was open-coded at nine call sites, one of which could divide by
// zero. WordPosData additionally carried four fields (Position, TopicBias,
// Sentiment, and a Word that restated the map key) that no code ever read.
//
// NameData is corpus.Name and WeeklyStat is corpus.WeeklyStat.
//
// TopicClusterData and ConceptCluster were type aliases into the clustering
// package, which is the dependency inversion described above. See the note on
// findBestSeed for why the concept-cluster paths went with them.

// ActiveChannelInfo holds a channel and its recent message count.
type ActiveChannelInfo struct {
	Channel      *discordgo.Channel
	MessageCount int
}

// TranscriptionJob holds the necessary info for a voice transcription task.
type TranscriptionJob struct {
	URL           string
	AuthorID      string
	MsgID         string
	ChannelID     string
	PlaceholderID string
	Author        *discordgo.User
	Member        *discordgo.Member
}

// ----- GLOBALS -----
//
// All of these are assigned once, by Service.Init, and read everywhere. They are
// the shape the pre-restructure code had and they go away with the subsystems
// that use them.
//
// Two are gone as of M3 and are worth naming so they are not reintroduced.
//
// stopSignal was a package-level channel closed on shutdown, which a dozen
// functions selected on. Cancellation now arrives as a context.Context parameter,
// so there is one mechanism rather than two and a test can stop a loop without
// touching package state.
//
// wg was a package-level WaitGroup that startup Added to and shutdown Waited on,
// and that the per-message handler ALSO Added to. An Add racing a Wait at zero
// panics, so a message arriving during shutdown could take the process down on
// its way out (SPEC.md section 8, finding 4). Background loops now use the
// Service's own WaitGroup, which only RunLoop touches and only during Start, and
// per-message work goes through core.Dispatcher, whose Adds all happen before any
// event can arrive.
// store replaces what was `var db *bbolt.DB`, and the type is the whole of M6b.
//
// Nothing in this package can reach a *bbolt.DB any more, so nothing here can open
// a transaction: only store.View and store.Update can, and they hand back a Reader
// or Writer that has no method to open another. The generation path used to run
// inside a db.View and call helpers (isRecognizedName, getNextMap) that each opened
// their own db.View, which is an unrecoverable hang that gets likelier as the file
// grows (SPEC.md section 8, finding 1). Those helpers now take the Reader they are
// already inside, and the version that could nest no longer compiles.
var store *storage.Store
var dg *discordgo.Session
var dispatcher *core.Dispatcher

// gate is the safety gate. Assigned by Service.Init and never nil afterwards,
// because cmd/bot treats a failed blocklist load as fatal: an empty ruleset is
// indistinguishable from a working one right up until the bot posts something the
// operator has to answer for.
var gate *safety.Gate

// guard is the outbound chokepoint. Assigned by Service.Init once the session exists,
// and never nil afterwards.
//
// Every Discord call that sends, edits, deletes or reacts goes through it, which is what
// makes finding 8 unwritable rather than fixed: nothing in this package holds a path to
// discordgo's send helpers any more except the three adapters below, and a test enforces
// that. The reason it has to be structural is that peregrine's output is Markov text
// built from arbitrary user messages, so every send is untrusted-input-shaped and a
// mention that got learned pings its target forever.
var guard *discordguard.Guard

// emitGate adapts *safety.Gate to discordguard.EmitGate.
//
// The guard wants a bool and the gate returns a Verdict carrying the reason, the
// category and whether to alert. The adapter throws away everything except the decision
// on purpose: the gate has already logged and counted by the time it returns, and having
// the guard re-log the reason would put the same incident in the log twice under two
// different spellings.
type emitGate struct{ g *safety.Gate }

func (e emitGate) CheckEmit(text string) bool { return e.g.CheckEmit(text).Allowed }

// botMentionPattern strips the bot's own mention from a message before learning it,
// so the corpus does not fill with the bot's user ID.
//
// Cached because it used to be regexp.MustCompile'd inside learnMessage, which
// means once per message per caller, with botID interpolated into the pattern every
// time (SPEC.md section 8, finding 16). botID is fixed for the life of the process,
// so one compile is enough.
var (
	botMentionOnce  sync.Once
	botMentionCache *regexp.Regexp
)

func botMentionPattern(botID string) *regexp.Regexp {
	botMentionOnce.Do(func() {
		botMentionCache = regexp.MustCompile(fmt.Sprintf(`(?i)<@!?%s>|@peregrine`, regexp.QuoteMeta(botID)))
	})
	return botMentionCache
}

var botID string

var transcriptionQueue chan TranscriptionJob // A queue for voice transcription jobs

// games owns every live word-game puzzle and the activity tracking that starts one.
//
// This replaces four package-level variables and two mutexes: activeWordGames,
// channelActivity, wordGameMutex and activityMutex, plus a wordGamesAvailable flag. Those
// two locks were taken in a fixed order at one call site and separately at another, which
// is a deadlock waiting for a third caller, and the state they guarded had no owner: the
// lifecycle lived in three hand-copied goroutines that slept and then acted.
//
// The Manager takes no session and performs no I/O. It returns what should be said, and
// the caller sends it through the guard, so a word-game announcement cannot skip mention
// suppression or the emit gate.
var games *wordgame.Manager

// leaderboard is the weekly win tally. Its mutex is inside it now, and its marshalling
// happens under that mutex, which is the fix for a real data race rather than a tidy-up:
// the old struct exported its mutex and was JSON-marshalled outside it while AddWin held
// it, and a concurrent map read and write is a FATAL runtime error in Go.
var leaderboard *wordgame.Leaderboard

// channelActivity is where people are talking and who is around, counted from the
// messages the gateway already delivers.
//
// One tracker for three features that each used to answer the question their own way.
// The two that asked Discord (autonomous posting choosing a channel, aggro choosing a
// target) paged every text channel in every guild and then a hundred messages per active
// channel, which was hundreds of REST calls for information that arrived free on the
// websocket and was thrown away (SPEC.md section 8, finding 14).
var channelActivity *activity.Tracker

// ----- BIRD AGGRO -----
var birdAggroTargetID string
var birdAggroEndTime time.Time

// AggroState for persisting bird aggro status
type AggroState struct {
	TargetID string    `json:"target_id"`
	EndTime  time.Time `json:"end_time"`
}

// saveAggroState persists the bird aggro state.
//
// Through the opaque blob API rather than a bucket of its own, because the shape of
// this value belongs to the aggro feature and not to storage: making storage hold a
// type definition for every scrap of state a feature persists is how it ends up
// importing the features it is meant to serve.
func saveAggroState(w *storage.Writer, state AggroState) error {
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return w.PutBlob(storage.BlobConfig, "aggroState", encoded)
}

// loadAggroState loads the bird aggro state.
func loadAggroState(r *storage.Reader) (AggroState, error) {
	v, err := r.GetBlob(storage.BlobConfig, "aggroState")
	if err != nil {
		return AggroState{}, err
	}
	if v == nil {
		return AggroState{}, nil // No state saved yet
	}
	var state AggroState
	if err := json.Unmarshal(v, &state); err != nil {
		return AggroState{}, err
	}
	return state, nil
}

// saveLastIngestionTime persists the last ingestion time to the ConfigBucket.
var birdAggroMutex sync.Mutex

// ----- IMAGE REPOSTING -----
var recentImageURLs []string
var imageURLMutex sync.Mutex

var discordCDNRegex = regexp.MustCompile(`^https?:\/\/cdn\.discordapp\.com\/\S+$`)
var tenorRegex = regexp.MustCompile(`^https?:\/\/tenor\.com\/view\/\S+$`)

var stopWords = map[string]struct{}{
	"a": {}, "about": {}, "above": {}, "after": {}, "again": {}, "against": {}, "all": {}, "am": {}, "an": {}, "and": {}, "any": {}, "are": {}, "as": {}, "at": {},
	"be": {}, "because": {}, "been": {}, "before": {}, "being": {}, "below": {}, "between": {}, "both": {}, "but": {}, "by": {},
	"can": {}, "could": {}, "did": {}, "do": {}, "does": {}, "doing": {}, "down": {}, "during": {},
	"each": {}, "few": {}, "for": {}, "from": {}, "further": {},
	"had": {}, "has": {}, "have": {}, "having": {}, "he": {}, "he'd": {}, "he'll": {}, "he's": {}, "her": {}, "here": {}, "here's": {}, "hers": {}, "herself": {}, "him": {}, "himself": {}, "his": {}, "how": {}, "how's": {},
	"i": {}, "i'd": {}, "i'll": {}, "i'm": {}, "i've": {}, "if": {}, "in": {}, "into": {}, "is": {}, "it": {}, "it's": {}, "its": {}, "itself": {},
	"let's": {}, "me": {}, "more": {}, "most": {}, "my": {}, "myself": {},
	"no": {}, "nor": {}, "not": {}, "of": {}, "off": {}, "on": {}, "once": {}, "only": {}, "or": {}, "other": {}, "ought": {}, "our": {}, "ours": {}, "ourselves": {}, "out": {}, "over": {}, "own": {},
	"same": {}, "she": {}, "she'd": {}, "she'll": {}, "she's": {}, "should": {}, "so": {}, "some": {}, "such": {},
	"than": {}, "that": {}, "that's": {}, "the": {}, "their": {}, "theirs": {}, "them": {}, "themselves": {}, "then": {}, "there": {}, "there's": {}, "these": {}, "they": {}, "they'd": {}, "they'll": {}, "they're": {}, "they've": {}, "this": {}, "those": {}, "through": {}, "to": {}, "too": {},
	"under": {}, "until": {}, "up": {}, "very": {},
	"was": {}, "we": {}, "we'd": {}, "we'll": {}, "we're": {}, "we've": {}, "were": {}, "what": {}, "what's": {}, "when": {}, "when's": {}, "where": {}, "where's": {}, "which": {}, "while": {}, "who": {}, "who's": {}, "whom": {}, "why": {}, "why's": {}, "with": {}, "would": {},
	"you": {}, "you'd": {}, "you'll": {}, "you're": {}, "you've": {}, "your": {}, "yours": {}, "yourself": {}, "yourselves": {},
}

// ----- ENTRYPOINT -----
// There is deliberately no package-level *rand.Rand any more.
//
// There used to be one, seeded once in main and then called from every
// per-message goroutine, the aggro ticker, the autonomous poster and the image
// reposter. A *rand.Rand is not safe for concurrent use, so that was a live data
// race on the bot's hottest path (SPEC.md section 8, finding 3): its internal
// state could be read and written simultaneously, which corrupts the generator
// and, under the race detector CI runs, fails the build.
//
// math/rand/v2's top-level functions are goroutine-safe and auto-seeded, so the
// fix is to have no shared generator at all rather than to wrap one in a mutex.
// Auto-seeding also removes the time.Now().UnixNano() seed, which was the
// conventional but wrong way to get randomness that differs per process.

// lastWordGameTime is gone. Interval mode is a core.Loop now, so the interval is the
// ticker's rather than a timestamp compared by hand inside an unrelated function: it used
// to pace the autonomous poster, which does not post word games (finding 30).

// Service is everything peregrine does, as one core.Service.
//
// One service, deliberately. The registry is load-bearing from the first commit
// rather than a seam waiting for a user, and each later milestone lifts one
// subsystem out of here into its own service registered alongside this one, so
// this service shrinks the same way the package does.
//
// It holds almost no state of its own because the code it wraps still uses
// package-level variables (db, dg, cfg, leaderboard, and the rest). Those move
// onto real types as their subsystems move out. What the type does own is the
// lifecycle: what starts, in what order, and what has to finish before the corpus
// closes.
type Service struct {
	deps core.Deps

	// loops tracks the background tickers so Shutdown can wait for them. Only
	// RunLoop ever Adds to it, and only during Start, which is the invariant that
	// makes the finding-4 panic impossible here as well as in the Dispatcher.
	loops       sync.WaitGroup
	cancelLoops context.CancelFunc
}

// New returns the legacy service. It does no work: everything that can fail
// happens in Init, where a failure aborts startup with an explanation.
func New() *Service { return &Service{} }

// EnsureBuckets is gone. It was exported so that both the bot and the -clean-db
// mode could make the corpus well formed, which meant a hand-maintained slice of
// bucket names in this package that a new bucket could be forgotten from.
// storage.Open does it now, from one list, and in the same transaction that checks
// schema_version: "these buckets exist" and "this file is the layout this binary
// understands" are different questions, and only the second one can refuse to run.

func (s *Service) Name() string { return "legacy" }

// Init loads persistent state and registers the gateway handler. No gateway or
// REST calls belong here: the session is not connected yet.
func (s *Service) Init(deps core.Deps) error {
	s.deps = deps
	cfg = deps.Config
	store = deps.Store
	dispatcher = deps.Dispatcher
	gate = deps.Gate
	dg = deps.Session

	// The guard is built here rather than in cmd/bot because it is legacy's own
	// chokepoint and moves out with the handler in M10b. Built in Init rather than
	// Start, because the gateway handler registered below can fire as soon as
	// session.Open returns and it replies through the guard: a guard assigned in Start
	// would be nil for every message that arrived in that window.
	guard = discordguard.New(dg, emitGate{g: gate}, deps.Logger, cfg.IgnoreChannels)

	// Load the word game dictionary. Deliberately not fatal: word games are one
	// optional feature, and taking the whole bot down because a 64 KB word list
	// would not load meant an unrelated asset problem killed learning,
	// generation, and every other behavior with it. A failure here disables word
	// games and says so. PEREGRINE_WORDGAME_DICTIONARY overrides the embedded
	// list; empty means use the embedded one.
	dict, err := wordgame.LoadDictionary(cfg.WordGameDictionary, wordgame.DictionaryOptions{
		MinLength: cfg.WordGameMinLength,
		MaxLength: cfg.WordGameMaxLength,
	})
	if err != nil {
		// A nil dictionary is fine: Manager.Available reports false and every entry point
		// declines. That is the general rule here, and it is why this is a warning rather
		// than a fatal: peregrine is a bag of loosely related engagement behaviours and
		// exactly one of them failing should disable that one, never the process.
		log.Printf("[WARN] Word game dictionary failed to load, word games disabled: %v", err)
	} else {
		log.Printf("[INFO] Word game dictionary loaded, %d usable words.", dict.Len())
	}
	// One activity tracker, shared. Word games ask it whether a channel is busy enough
	// for a puzzle, autonomous posting and interval-mode word games ask it where people
	// are talking, and aggro asks it who is around. All three used to answer their own
	// version of that question: the Manager kept a per-channel activity map of its own,
	// and the other two paged Discord's REST API for it (SPEC.md section 8, finding 14).
	channelActivity = activity.New(activity.Options{})

	games = wordgame.NewManager(dict, nil, channelActivity, wordgame.Options{
		Timeout:           cfg.WordGameTimeout,
		AnnounceTTL:       cfg.WordGameAnnounceTTL,
		ActivityWindow:    cfg.WordGameActivityWindow,
		ActivityThreshold: cfg.WordGameActivityThreshold,
		TriggerChance:     cfg.WordGameTriggerChance,
	})

	// Load persistent states from DB
	_ = store.View(func(r *storage.Reader) error {
		var err error
		state, err := loadAggroState(r)
		if err != nil {
			log.Printf("[WARN] Failed to load aggro state: %v", err)
			return err
		}
		leaderboard, err = loadLeaderboard(r)
		if err != nil {
			log.Printf("[WARN] Failed to load leaderboard, starting fresh: %v", err)
			leaderboard = wordgame.NewLeaderboard(time.Now())
		} else {
			log.Println("[INFO] Leaderboard loaded.")
		}
		birdAggroMutex.Lock()
		birdAggroTargetID = state.TargetID
		birdAggroEndTime = state.EndTime
		birdAggroMutex.Unlock()
		if birdAggroTargetID != "" && time.Now().Before(birdAggroEndTime) {
			log.Printf("[AGGRO] Loaded active aggro on %s until %s", birdAggroTargetID, birdAggroEndTime.Format(time.RFC3339))
		} else if birdAggroTargetID != "" && time.Now().After(birdAggroEndTime) {
			log.Printf("[AGGRO] Loaded expired aggro on %s, clearing...", birdAggroTargetID)
			go func() {
				birdAggroMutex.Lock()
				defer birdAggroMutex.Unlock()
				birdAggroTargetID = ""
				birdAggroEndTime = time.Time{}
				_ = store.Update(func(w *storage.Writer) error {
					return saveAggroState(w, AggroState{})
				})
			}()
		}
		return nil
	})

	// Load recent image URLs from DB into memory cache
	loadedURLs, err := loadImageURLsFromDB()
	if err != nil {
		log.Printf("[ERR] Failed to load image URLs from DB: %v", err)
	} else {
		imageURLMutex.Lock()
		recentImageURLs = loadedURLs
		imageURLMutex.Unlock()
		log.Printf("[INFO] Loaded %d image URLs from DB.", len(loadedURLs))
	}

	// The gateway handler is registered here rather than in Start, because
	// discordgo begins dispatching inside Open and Open happens between Init and
	// Start. Registering in Start would drop every message that arrived in that
	// window.
	dg.AddHandler(messageCreate)

	// The deletion handlers, registered here for the same reason and gated on the feature
	// that reads their result. Reposting off means nothing is cached, so there is nothing
	// to revoke and no reason to take a write transaction on every deletion the bot sees.
	if cfg.EnableImageRepost {
		dg.AddHandler(messageDelete)
		dg.AddHandler(messageDeleteBulk)
	}

	// Buffered so a burst of voice notes does not block the handler that queues
	// them. M12 gives this a real bound and a context.
	transcriptionQueue = make(chan TranscriptionJob, 100)

	return nil
}

// Start launches the background work. The gateway is connected and READY by the
// time this runs, which is why the bot's own identity is resolved here rather
// than in Init.
func (s *Service) Start(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancelLoops = cancel

	user, err := dg.User("@me")
	if err != nil {
		return fmt.Errorf("get bot user: %w", err)
	}
	botID = user.ID
	log.Printf("[INFO] Bot ID: %s", botID)

	// Every one of these was a hand-rolled goroutine building its own ticker and
	// selecting on a shared stop channel: nine copies of the same three lines,
	// each subtly different, and any panic inside one of them killed the process.
	// core.RunLoop owns the shape now, and each iteration is panic-isolated so an
	// optional behavior failing disables that behavior and nothing else.
	loops := []core.Loop{
		{
			Name:      "status",
			Every:     cfg.StatusTick,
			Immediate: true, // wanted at startup: it is the first sign of life
			Fn:        func(context.Context) { printLibraryStatus() },
		},
		{
			Name:      "ingest",
			Every:     cfg.IngestTick,
			Immediate: true, // the original code ran one backfill before the loop
			Fn: func(ctx context.Context) {
				log.Println("[AUTO] Starting autonomous ingestion...")
				runIngest(ctx, dg)
				log.Println("[AUTO] Autonomous ingestion finished.")
			},
		},
		{
			Name:  "aggro",
			Every: cfg.AggroTick,
			Fn:    func(context.Context) { maybeTriggerAggro() },
		},
		{
			Name:  "leaderboard-reset",
			Every: cfg.LeaderboardTick,
			// Immediate, which the NTP version could not usefully be: it only returned
			// true inside one hour on Monday, so a startup check almost always answered
			// no. Comparing week boundaries makes a check at startup the useful one,
			// because that is when a bot returning from downtime notices the week turned
			// while it was off.
			Immediate: true,
			Fn:        func(context.Context) { maybeResetLeaderboard() },
		},
	}
	if cfg.EnableWordGames {
		// One sweep replaces up to three goroutines per game. Each of those slept and then
		// acted with no context, so after shutdown they woke against a closed session; the
		// count was bounded only by how often people played.
		loops = append(loops, core.Loop{
			Name:  "wordgame-sweep",
			Every: cfg.WordGameSweepTick,
			Fn:    func(context.Context) { sweepWordGames(dg) },
		})

		// Interval mode gets its own loop, which is what makes the setting mean what its
		// name says. It used to pace the AUTONOMOUS POSTER, which posts Markov text, so
		// PEREGRINE_WORDGAME_INTERVAL controlled how often the bot said something that was
		// not a word game (finding 30). Activity mode has no loop because its trigger is
		// per message, in the reactor.
		if cfg.WordGameMode == config.WordGameModeInterval {
			loops = append(loops, core.Loop{
				Name:  "wordgame-interval",
				Every: cfg.WordGameInterval,
				Fn:    func(ctx context.Context) { startIntervalWordGame(ctx, dg) },
			})
		}
	}
	if cfg.EnableAutonomousPost {
		loops = append(loops, core.Loop{
			Name:  "autonomous-post",
			Every: cfg.AutonomousPostTick,
			Fn:    func(ctx context.Context) { autonomousPost(ctx, dg) },
		})
	}
	// There is no clustering loop any more, and its absence is a finding rather
	// than a regression.
	//
	// The pass took a *bbolt.DB, which nothing outside internal/storage can hold, so
	// it could not be called from here even if it worked. It does not work: its
	// members are persisted string-keyed and unmarshalled into map[int]float32, so
	// every cluster fails to decode, and both consumers guarded that with
	// `if err := json.Unmarshal(...); err == nil` and no else, making the failure
	// completely silent. It has never once produced data anything read
	// (SPEC.md section 8, finding 27), which is why PEREGRINE_ENABLE_CLUSTERING has
	// defaulted to false since M4.
	//
	// It also read the name-topic bucket as a JSON map per name, which the composite
	// key layout no longer stores, so under M6 it would find nothing regardless of
	// the codec bug. M8 rebuilds it against storage's blob API with content-hashed
	// IDs, and PEREGRINE_ENABLE_CLUSTERING is a deferred variable until then.
	for _, l := range loops {
		core.RunLoop(loopCtx, &s.loops, s.deps.Logger, l)
	}

	// Not a RunLoop: this one blocks on a queue rather than a ticker. It is
	// registered with the same WaitGroup so Shutdown waits for an in-flight
	// transcription the same way it waits for a tick.
	s.loops.Add(1)
	go func() {
		defer s.loops.Done()
		transcriptionWorker(loopCtx, dg)
	}()

	// Kept separate from the status loop because it reports on a different cadence
	// and M13 turns it into a real health service.
	s.loops.Add(1)
	go func() {
		defer s.loops.Done()
		monitorPerformance(loopCtx, dg)
	}()

	log.Println("[INFO] Bot running")
	return nil
}

// Shutdown stops the loops and waits for them, then saves the leaderboard.
//
// The wait is what the old code got wrong. It closed a stop channel and called
// wg.Wait on a WaitGroup the message handler was also Adding to, so shutdown
// could panic, and when it did not, wg.Wait returning still did not mean handlers
// were done: they kept using the corpus and the session while shutdown moved on
// to closing both. Here the only Adds happened during Start, the wait is bounded
// by ctx, and cmd/bot closes the store strictly after this returns.
func (s *Service) Shutdown(ctx context.Context) error {
	if s.cancelLoops != nil {
		s.cancelLoops()
	}

	done := make(chan struct{})
	go func() {
		s.loops.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Println("[INFO] Background loops finished.")
	case <-ctx.Done():
		// Reported rather than waited out. The container has a fixed budget
		// before SIGKILL, and losing an orderly close of the corpus to a stuck
		// loop is the worse trade.
		log.Println("[WARN] Shutdown deadline reached with background loops still running.")
	}

	// Last write before cmd/bot closes the store.
	if leaderboard != nil {
		if err := store.Update(func(w *storage.Writer) error {
			return saveLeaderboard(w, leaderboard)
		}); err != nil {
			return fmt.Errorf("save leaderboard: %w", err)
		}
	}
	return nil
}

// maybeTriggerAggro is the body of what was an inline ticker goroutine, extracted so the
// loop table above reads as a list of behaviors.
func maybeTriggerAggro() {
	birdAggroMutex.Lock()
	busy := birdAggroTargetID != "" && !time.Now().After(birdAggroEndTime)
	birdAggroMutex.Unlock()
	if busy {
		return
	}
	if rand.Float64() >= cfg.AggroChance {
		return
	}

	// Chosen outside the aggro lock. Picking a target takes the activity tracker's mutex,
	// and holding one package's lock while acquiring another's is how a lock-ordering
	// deadlock gets built; the old version did it while ALSO making hundreds of REST
	// calls, so the aggro state was locked for the length of a Discord page walk.
	target := findRandomActiveUser()
	if target == "" {
		return
	}

	birdAggroMutex.Lock()
	// Re-checked, because the state could have changed while the lock was released. There
	// is only one aggro loop so this cannot happen today, and that is exactly why it is
	// worth two lines: the next caller will not know.
	if birdAggroTargetID != "" && !time.Now().After(birdAggroEndTime) {
		birdAggroMutex.Unlock()
		return
	}
	birdAggroTargetID = target
	birdAggroEndTime = time.Now().Add(cfg.AggroDuration)
	state := AggroState{TargetID: birdAggroTargetID, EndTime: birdAggroEndTime}
	birdAggroMutex.Unlock()

	log.Printf("[AGGRO] Bird aggro triggered on user %s for %v.", target, cfg.AggroDuration)
	if err := store.Update(func(w *storage.Writer) error {
		return saveAggroState(w, state)
	}); err != nil {
		log.Printf("[ERR] Failed to persist aggro state: %v", err)
	}
}

// maybeResetLeaderboard clears the weekly tally when the week has turned over.
//
// THE NTP QUERY IS GONE (SPEC.md section 8, finding 17). The old version asked
// pool.ntp.org what day it was, hourly, and reset only when the answer was Monday between
// 00:00 and 00:59 UTC. Three things were wrong with that:
//
//   - It went to the network for something time.Now() answers. A bot whose clock is an
//     hour out has a much larger problem than a stale leaderboard.
//   - A failed query inside that one-hour window skipped the reset for a WHOLE WEEK, and
//     logged an error nobody would connect to a stale board six days later.
//   - The reset had to be observed within a specific hour, so it depended on the tick
//     landing there. A restart moved the tick's phase, and downtime across Monday morning
//     meant it never happened at all.
//
// Comparing week boundaries has none of those properties, and the important one is that it
// CATCHES UP: a bot that was off all Monday resets on its first tick back, because the
// week it holds is still the old one. The decision itself lives in the leaderboard, next
// to the field it reads, so the display and the reset cannot disagree about when the week
// turns.
func maybeResetLeaderboard() {
	if !leaderboard.MaybeReset(time.Now()) {
		return
	}
	log.Printf("[LEADERBOARD] New week starting %s, leaderboard reset.",
		leaderboard.WeekStart().Format(time.DateOnly))

	if err := store.Update(func(w *storage.Writer) error {
		return saveLeaderboard(w, leaderboard)
	}); err != nil {
		log.Printf("[ERR] Failed to persist leaderboard reset: %v", err)
	}
}

// learnOrUpdateName finds the canonical name for a user and updates aliases.
func learnOrUpdateName(w *storage.Writer, name, discordUserID, username string) (string, error) {
	canonicalName := toLowerCaseExceptURLs(username)
	nameKey := toLowerCaseExceptURLs(name)

	// Update the canonical name entry first.
	canonicalData, _, err := w.Name(canonicalName)
	if err != nil {
		return "", err
	}
	canonicalData.Count++
	canonicalData.DiscordUserID = discordUserID
	if err := w.PutName(canonicalName, canonicalData); err != nil {
		return "", err
	}

	// If the current name is an alias (nickname), create/update its entry to point to the canonical name.
	if nameKey != canonicalName {
		aliasData, _, err := w.Name(nameKey)
		if err != nil {
			return "", err
		}
		aliasData.DiscordUserID = discordUserID
		aliasData.Canonical = canonicalName // Link to the primary username.
		if err := w.PutName(nameKey, aliasData); err != nil {
			return "", err
		}
	}

	return canonicalName, nil
}

// isRecognizedName checks if a token is a known name or alias and returns its
// canonical form.
//
// It takes the Reader it is already inside, and that signature is half of finding
// 1's fix. It used to open its own db.View, and its only caller runs inside the read
// transaction that wraps generation, so every recognized-name lookup was a nested
// bbolt transaction: an outer read holds mmaplock.RLock for its whole life, a writer
// waiting to grow the mmap queues for the write lock, and Go's RWMutex then queues
// the inner read behind that writer. Unrecoverable, no timeout, and likelier the
// bigger the file gets.
//
// Passing the Reader is not merely the fix but the only thing that compiles now:
// a Reader has no method that opens a transaction.
func isRecognizedName(r *storage.Reader, token string) (canonicalName string, exists bool) {
	if token == "" {
		return "", false
	}
	lower := toLowerCaseExceptURLs(token)
	data, ok, err := r.Name(lower)
	if err != nil || !ok {
		return "", false
	}
	if data.Canonical != "" {
		return data.Canonical, true // It's an alias, return the canonical name.
	}
	return lower, true // It's a canonical name itself.
}

// ----------------------------
// Utilities & DB helpers
// ----------------------------
// Seven helpers used to live here and all seven are gone, because each of them was
// a bucket operation that internal/storage now owns and does better:
//
//   - getMapFromBucket and putJSONToBucket read and wrote a JSON map[string]int as
//     the VALUE of a prefix key, which is the write amplification at the root of the
//     old layout: learning one occurrence of "the cat" rewrote every successor "the"
//     had ever had. Writer.LearnNgram replaces both with one 12-byte put.
//   - incTopicInTx silently dropped words shorter than three characters, so the
//     topic count for "ok", "no" and "wtf" was permanently zero in a server whose
//     register is short interjections (finding G10). Writer.IncTopic has no minimum.
//   - trimHistoryInTx and trimImageCacheInTx called Bucket.Stats() in their LOOP
//     CONDITION. Stats() walks every page in the bucket, so evicting a thousand keys
//     walked the whole bucket a thousand times (finding 11). Both trims are counter
//     driven now, and history eviction is chronological because the keys are
//     fixed-width big-endian snowflakes rather than decimal strings (finding 10).
//   - saveImageURLToDB stored the literal byte "1" as a value that nothing read.
//     Writer.AddImageURL stores a timestamp, which is what makes eviction by age
//     possible at all.
//
// loadImageURLsFromDB survives only as the one line below, because the store now
// answers the question directly.
func loadImageURLsFromDB() ([]string, error) {
	var urls []string
	err := store.View(func(r *storage.Reader) error {
		var err error
		urls, err = r.ImageURLs()
		return err
	})
	return urls, err
}

// saveLeaderboard persists the leaderboard.
//
// json.Marshal is safe to call concurrently with AddWin now, because Leaderboard
// implements MarshalJSON and takes its own lock. It was NOT safe before: the mutex was an
// exported field of the marshalled struct and the marshalling ran outside it, so a win
// landing during a save was a concurrent map read and write, which in Go is a fatal
// runtime error rather than a recoverable panic.
func saveLeaderboard(w *storage.Writer, l *wordgame.Leaderboard) error {
	encoded, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return w.PutBlob(storage.BlobLeaderboard, "current", encoded)
}

// loadLeaderboard restores the leaderboard, or starts a fresh one.
//
// The pre-M11 field names are still read, and that asymmetry with the corpus is
// deliberate: storage.Open refuses a corpus written by an older layout outright, because a
// corpus is re-derivable from Discord history. A week of wins is not re-derivable from
// anything, so discarding it would lose data nobody can rebuild.
func loadLeaderboard(r *storage.Reader) (*wordgame.Leaderboard, error) {
	v, err := r.GetBlob(storage.BlobLeaderboard, "current")
	if err != nil {
		return nil, err
	}
	if v == nil {
		return wordgame.NewLeaderboard(time.Now()), nil
	}
	var l wordgame.Leaderboard
	if err := json.Unmarshal(v, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// startOfWeekUTC is the Monday 00:00 UTC that the current week began at.
//
// Hoisted because the same five lines were open-coded in two places, the reader that
// filters the chat leaderboard and the writer that resets a user's count, and those
// two disagreeing would mean a user's stats were counted for a week the leaderboard
// was not showing.
func startOfWeekUTC(now time.Time) time.Time {
	now = now.UTC()
	daysSinceMonday := (now.Weekday() - time.Monday + 7) % 7
	return now.Truncate(24 * time.Hour).Add(-time.Duration(daysSinceMonday) * 24 * time.Hour)
}

// loadAllUserStats returns each user's message count for the current week.
//
// Two pieces of tolerance went away with the layout. It no longer skips keys that
// are not numeric, because the stats bucket no longer holds a non-user key:
// total_messages_learned is a meta counter now. And it no longer falls back to
// decoding a bare integer for the "old format", because there is no old format to
// read: storage.Open refuses a corpus written before M6 outright rather than
// half-understanding it.
func loadAllUserStats(r *storage.Reader) (map[string]int, error) {
	all, err := r.AllUserStats()
	if err != nil {
		return nil, err
	}

	startOfWeek := startOfWeekUTC(time.Now())
	scores := make(map[string]int, len(all))
	for userID, stat := range all {
		if !stat.LastTimestamp.Before(startOfWeek) {
			scores[userID] = int(stat.Count)
		}
	}
	return scores, nil
}

// getNextMap is gone, and it was the other half of finding 1.
//
// It opened its own db.View to read one prefix's successor map, and it was called
// from inside the read transaction that wraps generation, once per candidate per
// backoff step per word. So the hottest path in the bot opened a nested bbolt
// transaction thousands of times per reply. Its caller now takes the *storage.Reader
// it is already inside and calls Reader.Successors, which is also a range scan over
// fixed-width values instead of unmarshalling a map that can hold thousands of
// entries.

// learnMessage ingests and learns from a single message.
func learnMessage(w *storage.Writer, msg, msgID, botID string, author MentionedUser, mentionedUsers []MentionedUser) error {
	msg = botMentionPattern(botID).ReplaceAllString(msg, "")
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return nil
	}

	// THE LEARNING GATE. This is inside learnMessage, not at any of its callers,
	// and that placement is the whole point.
	//
	// This function has four callers and until M5 only ONE of them filtered: the
	// live message handler. The historical backfill, self-learning and voice
	// transcripts all passed content straight through. Since the backfill re-read
	// the trailing 24 hours every ten minutes, a message the live path blocked was
	// learned anyway, unfiltered, minutes later, which defeated the live filter
	// entirely. That was the highest-value finding in the review (SPEC.md section 4,
	// A1).
	//
	// A check at one of four call sites is not a check. Here, a fifth caller is
	// covered without anyone remembering to cover it, which is the difference
	// between fixing a bug and making it unwritable. Do not hoist this to the call
	// sites for performance: the normalizer is cheap and the corpus is forever.
	//
	// The verdict is to DROP THE MESSAGE WHOLE. Never launder: a rewritten message
	// is still learned, with its structure intact and a harmless token in the
	// offending word's grammatical position.
	if v := gate.CheckLearn(msg); !v.Allowed {
		return nil
	}

	words := tokenize(msg)
	if len(words) == 0 {
		return nil
	}
	words = append(words, "<end>") // Append a token to signify the end of a thought.

	startTime := time.Now()

	// The dedup window. Checked before anything is written, because the backfill
	// re-reads recent history and without this every pass would re-learn the same
	// messages and double-count their n-grams (finding 13).
	//
	// A message ID that is not a snowflake is an error rather than a miss: it means
	// a caller invented one, and silently learning it twice is worse than saying so.
	seen, err := w.Seen(msgID)
	if err != nil {
		return fmt.Errorf("dedup check for message %s: %w", msgID, err)
	}
	if seen {
		return nil
	}

	// Update user stats
	if author.UserID != "" {
		if _, err := learnOrUpdateName(w, author.Name, author.UserID, author.Username); err == nil {
			stat, _, err := w.UserStat(author.UserID)
			if err != nil {
				return fmt.Errorf("read stats for %s: %w", author.UserID, err)
			}
			now := time.Now().UTC()
			if stat.LastTimestamp.Before(startOfWeekUTC(now)) {
				stat.Count = 1 // It's a new week for this user
			} else {
				stat.Count++
			}
			stat.LastTimestamp = now
			if err := w.PutUserStat(author.UserID, stat); err != nil {
				return fmt.Errorf("write stats for %s: %w", author.UserID, err)
			}
		}
	}

	if err := w.IncMessagesLearned(); err != nil {
		return fmt.Errorf("bump learned counter: %w", err)
	}

	// Increment global topic counts. This is also where unigram frequency lives, one
	// key per word, which is where it always belonged: the old ingestion loop tried
	// to keep it in the n-gram bucket under an empty prefix and that key held a map
	// of the entire vocabulary (finding 5).
	for _, word := range words {
		if err := w.IncTopic(toLowerCaseExceptURLs(word)); err != nil {
			return fmt.Errorf("increment topic: %w", err)
		}
	}

	// Create a set of canonical names to process for this message.
	canonicalNames := make(map[string]struct{})
	for _, user := range mentionedUsers {
		canonical, err := learnOrUpdateName(w, user.Name, user.UserID, user.Username)
		if err != nil {
			log.Printf("[WARN] Failed to learn name '%s': %v", user.Name, err)
			continue
		}
		canonicalNames[canonical] = struct{}{}
	}

	// Update associations for each unique canonical name.
	//
	// One composite-key put per (name, word) pair, where this used to read a JSON map
	// of every word ever associated with the name, mutate it in memory, and write the
	// whole thing back. On a name that has been discussed a lot that map is thousands
	// of entries rewritten per message.
	for canonicalName := range canonicalNames {
		for i, word := range words {
			lw := toLowerCaseExceptURLs(word)
			if lw == "<end>" {
				continue
			}
			// CRITICAL: Exclude stop words from all associative learning.
			if _, isStop := stopWords[lw]; isStop {
				continue
			}
			if err := w.AddNameTopic(canonicalName, lw, float64(i)/float64(len(words))); err != nil {
				return fmt.Errorf("associate %q with %q: %w", canonicalName, lw, err)
			}
		}
	}

	// If any names were involved, update global word-to-word associations. This
	// ensures the global graph is built from user-provided context.
	//
	// WINDOWED as of M7b, which closes the rest of finding 12. This was all-pairs and
	// therefore quadratic in message length, running inside the single write
	// transaction that serializes every other write in the process: a 200-word message
	// produced nearly 40,000 read-add-write pairs and blocked all ingestion while it
	// did. With a window of 5 the same message produces about 2,000.
	//
	// The window is a genuine model change and not only an optimization, and it is a
	// change in the right direction. "Co-occurs anywhere in the same message" is a
	// weak claim that gets weaker as messages get longer, since a 200-word message
	// links every word in it to every other. Proximity is what association actually
	// means, and bounding it makes long messages contribute proportionally rather than
	// quadratically. A window of 0 restores the old behaviour and config warns.
	//
	// Both directions of each pair are still recorded, because the index is
	// direction-sensitive: it stores the position of the ASSOCIATE, so (a, b) and
	// (b, a) carry different position sums and the readers use that.
	//
	// The separate topic-cluster pass that used to follow this is gone, and it was
	// pure duplication: it recorded the same word pairs from the same messages under
	// the same stop-word exclusion, canonicalised into one order and WITHOUT the
	// position sum, into a second bucket. Everything it stored is derivable from what
	// this loop stores, so the layout has one co-occurrence index rather than two
	// that could disagree (SPEC.md section 8, finding 28).
	if len(canonicalNames) > 0 {
		window := cfg.CooccurrenceWindow
		for i, wordA := range words {
			lwA := toLowerCaseExceptURLs(wordA)
			if lwA == "<end>" {
				continue
			}
			if _, isStop := stopWords[lwA]; isStop {
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
				lwB := toLowerCaseExceptURLs(words[j])
				if lwB == "<end>" {
					continue
				}
				if _, isStop := stopWords[lwB]; isStop {
					continue
				}
				if err := w.AddTopicWord(lwA, lwB, float64(j)/float64(len(words))); err != nil {
					return fmt.Errorf("associate %q with %q: %w", lwA, lwB, err)
				}
			}
		}
	}

	// Markov n-gram ingestion.
	//
	// The loop starts at 2, not 1, and that single bound is finding 5. At n == 1 the
	// prefix slice is empty, so the key was "" and every unigram in the corpus
	// accumulated into ONE bbolt key whose value was a JSON map of the entire
	// vocabulary, unmarshalled and re-marshalled once per word per message. Nothing
	// ever read it, because every reader builds a prefix of at least one word. It was
	// pure write amplification and the dominant reason the file reached 128 MB.
	//
	// Writer.LearnNgram refuses an empty prefix as well, so the bug cannot come back
	// through a different caller rather than only being avoided here.
	//
	// authorID is empty for the bot's own output, which is what keeps self-learning
	// out of the author-diversity counts: if the bot counted as an author, anything it
	// said once would bootstrap itself toward eligibility to be said again
	// (SPEC.md section 4, A6).
	authorID := author.UserID
	if authorID == botID {
		authorID = ""
	}

	totalNgrams := 0
	for n := cfg.MaxNGram; n >= 2; n-- {
		if len(words) < n {
			continue
		}
		for i := 0; i <= len(words)-n; i++ {
			prefix := toLowerCaseExceptURLs(strings.Join(words[i:i+n-1], " "))
			next := toLowerCaseExceptURLs(words[i+n-1])
			if err := w.LearnNgram(prefix, next, authorID); err != nil {
				return fmt.Errorf("learn %q -> %q: %w", prefix, next, err)
			}
			totalNgrams++
		}
	}

	// Recorded last, so a failure anywhere above rolls the whole message back rather
	// than marking it seen and then not learning it.
	if err := w.MarkSeen(msgID, time.Now(), cfg.MaxHistory); err != nil {
		return fmt.Errorf("mark message %s seen: %w", msgID, err)
	}

	// The history size comes from a counter rather than Bucket.Stats(), which walked
	// every page in the bucket once per message to fill this one log field
	// (finding 11).
	log.Printf("[LEARNED] msg=%q | words=%d | ngrams=%d | history=%d | names=%d | took=%s",
		msg, len(words), totalNgrams, w.HistoryCount(), len(canonicalNames), time.Since(startTime))

	return nil
} // ----------------------------
// Ingestion / Learning
// ----------------------------
// MentionedUser holds the name and user details of a mentioned user.
type MentionedUser struct {
	Name     string
	UserID   string
	Username string
}

// MemoryEntry stores a message's content and its associated decay factor.
type MemoryEntry struct {
	Content     string
	DecayFactor float64
}

// ConversationMemory holds a list of recent messages with their decay factors.
type ConversationMemory struct {
	Entries []MemoryEntry
	Mutex   sync.Mutex
}

// AddMessage adds a new message to the memory, maintaining the decay order.
func (cm *ConversationMemory) AddMessage(content string) {
	cm.Mutex.Lock()
	defer cm.Mutex.Unlock()

	// Apply decay to existing entries
	for i := range cm.Entries {
		cm.Entries[i].DecayFactor *= 0.8 // Decay factor for older messages
	}

	// Add new message with full weight
	cm.Entries = append(cm.Entries, MemoryEntry{Content: content, DecayFactor: 1.0})

	// Trim old entries if memory exceeds a certain size (e.g., 50 messages)
	if len(cm.Entries) > 50 {
		cm.Entries = cm.Entries[len(cm.Entries)-50:]
	}
}

// GetWeightedWords returns a flattened list of words from memory, weighted by decay factor.
func (cm *ConversationMemory) GetWeightedWords() []string {
	cm.Mutex.Lock()
	defer cm.Mutex.Unlock()

	var weightedWords []string
	for _, entry := range cm.Entries {
		words := tokenize(entry.Content)
		reps := int(math.Max(1, entry.DecayFactor*5)) // Amplify decay for repetition
		for _, w := range words {
			for i := 0; i < reps; i++ {
				weightedWords = append(weightedWords, w)
			}
		}
	}
	return weightedWords
}

// Conversation memory is PER CHANNEL as of M7b, closing finding G8.
//
// There used to be one package-level ConversationMemory shared by every channel in
// every guild, so a reply in one channel was steered by whatever had been said in an
// unrelated one. That is not chaos, which would be fine, it is simply wrong context:
// the reply reads as a non-sequitur to the thread it is in, and the bot looks like it
// is not paying attention rather than like it is being funny.
//
// The map is bounded, which the old single memory did not need to be. An unbounded map
// keyed by channel ID is a slow leak in a bot that can be added to any number of
// guilds, and it is the kind that never shows up in testing because a test only ever
// uses one channel. Eviction is oldest-touched-first rather than a real LRU: the
// difference does not matter for a cap in the hundreds, and a timestamp per channel is
// cheaper to reason about than an intrusive list.
const maxRememberedChannels = 200

var (
	convMemories   = map[string]*channelMemory{}
	convMemoriesMu sync.Mutex
)

type channelMemory struct {
	mem      ConversationMemory
	lastUsed time.Time
}

// memoryFor returns the memory for one channel, creating it if needed.
func memoryFor(channelID string) *ConversationMemory {
	convMemoriesMu.Lock()
	defer convMemoriesMu.Unlock()

	if cm, ok := convMemories[channelID]; ok {
		cm.lastUsed = time.Now()
		return &cm.mem
	}

	if len(convMemories) >= maxRememberedChannels {
		evictOldestChannelMemory()
	}
	cm := &channelMemory{lastUsed: time.Now()}
	convMemories[channelID] = cm
	return &cm.mem
}

// evictOldestChannelMemory drops the least recently used entry. Caller holds the lock.
func evictOldestChannelMemory() {
	var oldestID string
	var oldest time.Time
	for id, cm := range convMemories {
		if oldestID == "" || cm.lastUsed.Before(oldest) {
			oldestID, oldest = id, cm.lastUsed
		}
	}
	if oldestID != "" {
		delete(convMemories, oldestID)
	}
}

// extractNamesFromMessage returns Discord usernames and display names mentioned in
// a message.
//
// It is deliberately split into two phases, and the split is about transaction
// duration rather than tidiness. Phase one makes REST calls (GuildMember, to resolve
// nicknames) and touches no corpus. Phase two reads the corpus and makes no network
// call. It used to be one pass, which held a bbolt read transaction open across N
// HTTP round trips: a read transaction holds mmaplock.RLock for its entire life, so
// every writer waiting to grow the mmap was waiting on Discord's latency.
//
// No caller may invoke this from inside a transaction. That is not a convention here
// but a consequence: the store is the only thing that can open one, and a caller that
// already holds a Reader has namesFromContent below instead.
func extractNamesFromMessage(s *discordgo.Session, m *discordgo.MessageCreate, guildID string) []MentionedUser {
	users, seenIDs := mentionedUsersFromMentions(s, m, guildID)

	var fromContent []MentionedUser
	if err := store.View(func(r *storage.Reader) error {
		fromContent = namesFromContent(r, m.Content, seenIDs)
		return nil
	}); err != nil {
		log.Printf("[WARN] name lookup for message %s failed, using @mentions only: %v", m.ID, err)
		return users
	}
	return append(users, fromContent...)
}

// mentionedUsersFromMentions is phase one: explicit @mentions, resolved to their
// server nicknames over REST. It returns the set of user IDs it consumed so phase two
// does not add anyone twice.
func mentionedUsersFromMentions(s *discordgo.Session, m *discordgo.MessageCreate, guildID string) ([]MentionedUser, map[string]struct{}) {
	users := []MentionedUser{}
	seenIDs := make(map[string]struct{})

	for _, u := range m.Mentions {
		if _, ok := seenIDs[u.ID]; ok {
			continue
		}
		seenIDs[u.ID] = struct{}{}

		// Add the base username.
		users = append(users, MentionedUser{Name: u.Username, UserID: u.ID, Username: u.Username})

		// Add the server-specific nickname, if it exists.
		if guildID != "" {
			member, err := s.GuildMember(guildID, u.ID)
			if err == nil && member.Nick != "" {
				users = append(users, MentionedUser{Name: member.Nick, UserID: u.ID, Username: u.Username})
			}
		}
	}
	return users, seenIDs
}

// namesFromContent is phase two: known names appearing in the text that were not
// @mentioned. It takes the Reader rather than opening a transaction, so it is usable
// from a caller that already holds one, and adds to seenIDs as it goes.
func namesFromContent(r *storage.Reader, content string, seenIDs map[string]struct{}) []MentionedUser {
	var users []MentionedUser
	for _, word := range tokenize(content) {
		lw := toLowerCaseExceptURLs(word)
		data, ok, err := r.Name(lw)
		if err != nil || !ok {
			continue
		}

		// Determine the canonical name for the found word, which could be an alias.
		canonicalName := lw
		if data.Canonical != "" {
			canonicalName = data.Canonical
		}

		// Fetch the full record for the canonical name to get the user ID.
		canonicalData, ok, err := r.Name(canonicalName)
		if err != nil || !ok || canonicalData.DiscordUserID == "" {
			continue
		}
		if _, dup := seenIDs[canonicalData.DiscordUserID]; dup {
			continue
		}
		seenIDs[canonicalData.DiscordUserID] = struct{}{}
		users = append(users, MentionedUser{
			Name:     canonicalName,
			UserID:   canonicalData.DiscordUserID,
			Username: canonicalName,
		})
	}
	return users
}

// The guild and channel walk is internal/ingest as of M9.
//
// What was here was three functions and about 150 lines: an unbounded goroutine per
// channel per guild, a pre-scan that paged every channel to COUNT recent messages and
// then threw them away so the real pass could page the same channels again, and a
// re-read of the trailing lookback window on every tick. That last one is finding 13, and
// it was corrupting the corpus rather than merely wasting calls: the history bucket it
// relied on for dedup is capped, so on a busy guild the older half of each window had
// already been evicted and was learned again, counting its n-grams twice.
//
// What stayed here is learning, which is the part with a safety gate in front of it.
// ingest asks what is new; legacy decides what to do with it.

// ingestLearner adapts learnMessage to ingest.Learner.
//
// The historical path and the live path therefore go through the same function, which is
// the whole point of A1's fix: CheckLearn is inside learnMessage, so a backfilled message
// cannot bypass a filter the live handler applies. This was the exact path that used to,
// and it was the worst finding in the review.
type ingestLearner struct{ s *discordgo.Session }

func (l ingestLearner) Learn(m *discordgo.Message, guildID string) error {
	names := extractNamesFromMessage(l.s, &discordgo.MessageCreate{Message: m}, guildID)

	author := MentionedUser{
		Name:     m.Author.Username,
		UserID:   m.Author.ID,
		Username: m.Author.Username,
	}
	if m.Member != nil && m.Member.Nick != "" {
		author.Name = m.Member.Nick
	}

	return store.Update(func(w *storage.Writer) error {
		return learnMessage(w, m.Content, m.ID, botID, author, names)
	})
}

// storeCursors adapts the corpus to ingest.Cursors.
//
// One transaction per call rather than one per pass, and that is the right trade here:
// the alternative is holding a write transaction open across every REST round trip of an
// ingest pass, and bbolt has a single writer process-wide, so it would block all live
// learning for the length of the walk. A read to fetch a cursor and a write to advance it
// are both a handful of bytes.
type storeCursors struct{}

func (storeCursors) Cursor(channelID string) (string, error) {
	var id string
	err := store.View(func(r *storage.Reader) error {
		id = r.Cursor(channelID)
		return nil
	})
	return id, err
}

func (storeCursors) SetCursor(channelID, messageID string) error {
	return store.Update(func(w *storage.Writer) error {
		return w.SetCursor(channelID, messageID)
	})
}

// runIngest performs one pass. Called from the ingest loop in Start.
func runIngest(ctx context.Context, s *discordgo.Session) {
	in := ingest.New(s, storeCursors{}, ingestLearner{s: s}, slog.Default(), ingest.Options{
		Lookback:           cfg.IngestLookback,
		GuildConcurrency:   cfg.IngestGuildConcurrency,
		ChannelConcurrency: cfg.IngestChannelConcurrency,
		BatchDelay:         cfg.IngestBatchDelay,
	})
	if _, err := in.Run(ctx); err != nil {
		log.Printf("[ERR] ingest pass failed to start: %v", err)
	}
}

// -----------------------------------------------------------------------------
//  Tokenizing & generation
// -----------------------------------------------------------------------------

// The tokenizer, the similarity measure and the sentence cleaner now live in
// internal/text, which is a leaf: no bbolt, no discordgo, no config. These
// wrappers exist so the roughly seventy call sites in this package did not all
// have to change in the same commit that moved the logic out. They disappear as
// each subsystem moves.
//
// The regexes moved with them and are compiled once at package scope. Two were
// being compiled per call, and the punctuation stripper inside the sentence
// cleaner was compiled once per token per sentence, which is a regex compile on
// the hot path of every single reply (SPEC.md section 8, finding 16).

func tokenize(msg string) []string { return text.Tokenize(msg) }

func toLowerCaseExceptURLs(s string) string { return text.LowerExceptURLs(s) }

// The sentenceSimilarity wrapper that sat here is gone. Its only caller was the
// prompt-gravity term in the old scorer, which is internal/markov's now and calls
// text.Similarity directly. The linter reporting it unused is how that was confirmed
// rather than assumed, which is the same way M6b established that filter.go's last two
// wrappers existed only for the cleanup pass.

// sessionEmoji resolves a :shortcode: against the guilds the session can see.
//
// This is the seam that took discordgo out of the sentence cleaner: internal/text
// declares the minimal EmojiResolver interface it needs, and this satisfies it
// structurally, so the cleaner is testable with a two-line fake instead of a
// gateway connection.
//
// It walks s.State.Guilds, which was empty for the entire life of this bot because
// the session never requested IntentsGuilds, so the resolver had never once
// succeeded and peregrine had never spoken in the server's own emotes. M3 requests
// the intent; this is the code that finally benefits (SPEC.md section 8, finding 7).
type sessionEmoji struct{ s *discordgo.Session }

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

func cleanSentence(s *discordgo.Session, str string) string {
	return text.CleanSentence(str, sessionEmoji{s: s})
}

// Three functions that used to sit here are gone, and all three went to the same
// place. findBestSeed and findJumpWord are markov.Seed and markov.Generator.Jump,
// which is what let their two dead concept-cluster tiers be replaced by one bounded
// query-time two-hop expansion rather than by a rebuilt cluster bucket (SPEC.md
// section 8, finding 29). applyEdgyStyle is markov.Style, which shares its Persona and
// its lexicon with the in-sampler bias instead of deciding independently of it
// (finding G6), and which chooses where to insert filler by position weight rather
// than by a flat draw over the sentence interior.

// generateSentenceWithContext builds a sentence using the markov DB, prompt, and recent context.
func generateSentenceWithContext(s *discordgo.Session, prompt string, isRoast bool, convMemory *ConversationMemory) (string, error) {
	promptWords := tokenize(prompt)
	if len(promptWords) == 0 {
		promptWords = []string{"<START>"}
	}

	recentWords := convMemory.GetWeightedWords() // Get weighted words from the conversation memory
	var sentence []string
	var recognizedNames []string

	// ONE read transaction for the whole attempt, and now genuinely one: everything
	// inside used to reach back for its own. Reader.CorpusEmpty replaces a
	// Bucket.Stats() call, which walked every page in the largest bucket in the
	// database on every reply purely to answer "is there anything in here".
	err := store.View(func(r *storage.Reader) error {
		if r.CorpusEmpty() {
			// Nothing learned yet, so there is nothing to say. This used to emit a
			// three-dot placeholder, which the two-word floor below would now discard
			// anyway; returning nothing states the intent instead of relying on that.
			// CorpusEmpty is one cursor First(), replacing a Bucket.Stats() call that
			// walked every page in the largest bucket on every reply (finding 11).
			return nil
		}

		// Up to three attempts, keeping the longest, and this is a RE-SEED rather than
		// the discard-and-retry that M7b deleted. The distinction matters because they
		// look alike. The old mechanism threw away an end token and continued from the
		// same prefix, which fought the length decision; this abandons the whole attempt
		// and draws a different seed, which is the only response available to the real
		// failure mode.
		//
		// That failure mode showed up in the golden samples rather than in reasoning,
		// which is the point of reading them: a seed drawn from a non-prompt tier can
		// dead-end on its very first step, because the length floor is a logit penalty
		// on the end token and a penalty does nothing when there are no eligible
		// candidates at all. The result was one-word replies like "roof" and "coping".
		// A short reply lands; a one-word non-sequitur reads as the bot malfunctioning.
		//
		// Attempts are cheap: they share this transaction, and the corpus reads they
		// repeat are the ones storage has cheap answers for.
		const attempts = 3
		for i := range attempts {
			words, names, _ := generateSentenceAttempt(r, promptWords, recentWords, prompt, isRoast)
			if len(words) > len(sentence) {
				sentence, recognizedNames = words, names
			}
			if len(sentence) >= cfg.MinWords {
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

	// Below two words there is nothing worth posting, and silence is the safe default
	// this bot already uses when the emit gate refuses. One word is not a punchy reply,
	// it is a reply that looks broken, and an unexplained silence is indistinguishable
	// from the bot choosing not to answer, which it does all the time anyway.
	if len(sentence) < 2 {
		return "", nil
	}

	// Clean final sentence
	final := strings.Join(sentence, " ")
	final = strings.ReplaceAll(final, markov.EndToken, "") // Remove any end sentinels
	final = cleanSentence(s, final)

	// The persona post-pass. One mechanism with the in-sampler lexicon bias now, rather
	// than applyEdgyStyle deciding independently of whatever the scorer had decided
	// (SPEC.md finding G6). The comment this replaced said "only apply edgy style if no
	// recognized name seed was used" and then passed isAboutName straight through, which
	// does the opposite; the behaviour was right and the comment was wrong.
	persona := markov.PersonaNeutral
	if isRoast {
		persona = markov.PersonaRoast
	}
	final = markov.Style(nil, markov.DefaultWeights(), final, persona, len(recognizedNames) > 0)

	// THE EMIT GATE.
	//
	// This is not redundant with the learning gate, and that is the architectural
	// point rather than caution. A Markov chain composes novel sequences from
	// n-grams that were learned separately, so fragments which were each innocuous
	// can join into something the operator has to answer for. No amount of input
	// filtering prevents that: input filtering lowers the rate, only an output gate
	// bounds the result (SPEC.md section 4, A3). Removing either gate because the
	// other exists would be wrong.
	//
	// Until M5 the only check here tested length, character repetition and character
	// class. There was no slur check and no illegal-content check on output at all,
	// so anything in the corpus could come back out verbatim (A2).
	//
	// On rejection the bot RETURNS EMPTY AND STAYS SILENT rather than substituting a
	// fallback. Silence is always safe; a fallback is a new output that has to be
	// reasoned about, and in a bot that already replies selectively an unexplained
	// silence is indistinguishable from it choosing not to answer.
	//
	// This sits at the single exit from generation, which covers it for now. M10
	// moves it into internal/discordguard so that all thirteen send sites are
	// covered structurally rather than this one being the only path that generates.
	if v := gate.CheckEmit(final); !v.Allowed {
		return "", nil
	}

	return final, nil
}

// generateSentenceAttempt drives one sentence out of the engine.
//
// What used to be here was 470 lines: a prefix-shrink loop that took the first
// non-empty result from the longest prefix, and a 290-line scorer that multiplied a
// raw n-gram count by an unbounded topic term and a dozen more ad-hoc factors before
// raising it to a power. All of that is internal/markov now, and what is left is the
// part that is genuinely legacy's job: turning a Discord prompt into a markov.Step and
// walking the loop.
//
// Three things that were tangled together here are now separate, and it is worth
// saying which because the diff is large. The probability model is Kneser-Ney inside
// the engine. The heuristics are additive logits inside the engine. The length bounds
// and the stuck-jump are still here, because they are M7b's row.
func generateSentenceAttempt(r *storage.Reader, promptWords, recentWords []string, originalPrompt string, isRoast bool) ([]string, []string, map[string]corpus.TopicAssoc) {
	g := markov.New(r, genParams(), nil)

	var recognizedNames []string
	for _, word := range promptWords {
		if name, exists := isRecognizedName(r, word); exists {
			recognizedNames = append(recognizedNames, name)
		}
	}

	// Core topics: the prompt's non-stop words, weighted by the log of their global
	// count so significance does not grow too quickly.
	//
	// Keyed by the word itself rather than by a text.Interner id. The interner is gone
	// from this function, and that is a simplification the engine bought rather than a
	// change in behavior: the ids only ever existed to key these two maps within one
	// attempt, they were never persisted, and the engine's Corpus interface speaks in
	// strings. Nothing may start persisting an interner id, for the reason recorded on
	// text.Interner, and having no id here is the strongest form of that.
	coreTopics := make(map[string]float64, len(promptWords))
	for _, word := range promptWords {
		if _, isStop := stopWords[word]; isStop {
			continue
		}
		coreTopics[word] = math.Log(float64(r.TopicCount(word)) + 1)
	}

	aggregatedAssoc := make(map[string]corpus.TopicAssoc)
	for _, nm := range recognizedNames {
		nmKey := toLowerCaseExceptURLs(nm)
		assoc, err := r.NameTopicsFor(nmKey)
		if err != nil {
			log.Printf("[WARN] name-topic lookup for %s failed: %v", nmKey, err)
			continue
		}
		for word, data := range assoc {
			existing := aggregatedAssoc[word]
			existing.Count += data.Count
			existing.PosSum += data.PosSum
			aggregatedAssoc[word] = existing
		}
	}

	// Seed selection is the engine's now, and the two-hop tier inside it is what
	// replaces the concept-cluster tier that had never once fired (SPEC.md finding 29).
	// The text.Interner that used to key the candidate map went with it: the engine
	// works in strings, so there is no longer an id anywhere near this path, which is
	// the strongest available form of "nothing may persist an interner id".
	seedIn := markov.SeedInput{
		PromptWords: promptWords,
		RecentWords: recentWords,
		Names:       recognizedNames,
	}
	seed := g.Seed(seedIn)
	if seed == "" {
		// The corpus offered nothing. Falling back to a prompt word is better than a
		// sentinel, because at worst it echoes and at best the prompt word has
		// continuations the seed tiers happened not to rank.
		if len(promptWords) == 0 {
			return nil, recognizedNames, aggregatedAssoc
		}
		seed = promptWords[0]
	}

	words := tokenize(seed)

	// ONE length model, replacing three mechanisms that competed: an end-token
	// multiplier below 40% progress, a discard-and-retry, and a 30 + rand(15) loop
	// bound. The target is sampled per sentence and skewed short, and the engine shifts
	// the end-token logit against it, so there is a single answer to how long this
	// should be (SPEC.md finding G7).
	length := markov.NewLength(markov.DefaultSource{}, cfg.MinWords, cfg.MaxWords)

	step := &markov.Step{
		Prefix:       append([]string{}, words...),
		Sentence:     append([]string{}, words...),
		Prompt:       originalPrompt,
		PromptSet:    wordSet(promptWords),
		RecentSet:    wordSet(recentWords),
		Used:         make(map[string]int, length.Max),
		Ngrams:       make(map[string]struct{}, length.Max*3),
		Length:       length,
		CoreTopics:   coreTopics,
		CurrentTopic: seed,
		NameAssoc:    aggregatedAssoc,
	}
	if isRoast {
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

		next, err := g.Next(step)
		if err != nil {
			log.Printf("[WARN] generation step failed: %v", err)
			break
		}

		if next == markov.EndToken {
			// Unconditional. The floor lives in the length model as a logit penalty, so
			// there is no discard-and-retry here: if the model chose to end despite that
			// penalty, the alternatives were worse and overriding it would put the
			// decision back in two places.
			break
		}

		if next == "" {
			// A dead end. Either the prefix has no continuation, or the
			// author-diversity gate refused everything that did, which on a young
			// corpus is the common case and is the gate working.
			jump := g.Jump(seedIn, step.Sentence)
			if jump == "" {
				break
			}
			next = jump
		}

		step.Sentence = append(step.Sentence, next)
		lower := toLowerCaseExceptURLs(next)
		step.Used[lower]++
		step.Ngrams[lower] = struct{}{}
		if len(step.Sentence) >= 2 {
			step.Ngrams[toLowerCaseExceptURLs(strings.Join(step.Sentence[len(step.Sentence)-2:], " "))] = struct{}{}
		}
		if len(step.Sentence) >= 3 {
			step.Ngrams[toLowerCaseExceptURLs(strings.Join(step.Sentence[len(step.Sentence)-3:], " "))] = struct{}{}
		}

		step.Prefix = append(step.Prefix, next)
		if len(step.Prefix) > cfg.MaxNGram-1 {
			step.Prefix = step.Prefix[1:]
		}
	}

	return step.Sentence, recognizedNames, aggregatedAssoc
}

// wordSet builds a normalized presence set.
//
// Once per sentence, where the old scorer rebuilt the prompt set inside the
// per-candidate loop: it tokenized the whole prompt and allocated a map once per
// candidate per step per generated word.
func wordSet(words []string) map[string]struct{} {
	out := make(map[string]struct{}, len(words))
	for _, w := range words {
		out[toLowerCaseExceptURLs(w)] = struct{}{}
	}
	return out
}

// -----------------------------------------------------------------------------
//  Discord handlers / channel utils
// -----------------------------------------------------------------------------

// messageCreate handles incoming messages: optionally reply and always learn.
//
// The package-level globalConvMemory that used to be declared here is gone. It was one
// 50-entry decayed memory shared by every channel in every guild, so a reply in one
// channel was steered by an unrelated conversation in another. Conversation memory is
// keyed by channel ID now, in a bounded map (finding G8).

// sendMessage, editMessage and deleteMessage are now three-line adapters onto
// internal/discordguard, which owns mention suppression, the outbound safety gate, the
// ignore list and the logging.
//
// They survive as functions rather than being inlined at their call sites because they
// take a *discordgo.Session that the guard does not need, and rewriting fourteen call
// sites in the same commit that introduces the chokepoint would make the diff hard to
// check for exactly the thing it has to be checked for: that nothing still reaches
// s.ChannelMessage* directly. `TestNothingBypassesTheGuard` is what enforces that, and
// these three are the only functions it permits to name the raw session.
//
// The session parameter is ignored. It is kept so the call sites do not all have to
// change shape twice, once here and once when M10b splits the handler into plugins that
// carry a guard rather than a session.
func sendMessage(s *discordgo.Session, channelID, content string) {
	_ = s
	guard.Send(channelID, content)
}

func editMessage(s *discordgo.Session, channelID, messageID, content string) {
	_ = s
	guard.Edit(channelID, messageID, content)
}

func deleteMessage(s *discordgo.Session, channelID, messageID string) {
	_ = s
	guard.Delete(channelID, messageID)
}

// scrambleMessage and timeUpMessage exist because the word-game announcements were
// composed inline at two call sites each, identically, and the duplication is what let
// the interval-mode and activity-mode branches drift apart in the first place. Naming
// them also keeps the guard call on one line at each site, which is what makes
// TestNothingBypassesTheGuard readable.
func scrambleMessage(scrambled string) string {
	return fmt.Sprintf("✨ **Word Scramble!** ✨\n\nUnscramble this word: **%s**", scrambled)
}

func timeUpMessage(original string) string {
	return fmt.Sprintf("Time is up! The word was **%s**.", original)
}

// winnerMessage announces a solved scramble.
//
// The nickname is interpolated into it, which is why this goes through the guard like any
// other send: a nickname is user-controlled text and can contain a role mention, so even
// a string the bot composes itself is untrusted-input-shaped.
func winnerMessage(winner, word string, solveTime time.Duration) string {
	return fmt.Sprintf("🎉 **%s** guessed the word **%s** in %.2f seconds!", winner, word, solveTime.Seconds())
}

// messageCreate is the gateway handler. It does the cheapest possible rejection
// and then hands the work to the dispatcher.
//
// It used to call wg.Add(1) on the same WaitGroup the shutdown path waited on and
// then spawn a goroutine per message, which was two bugs and a cost: an Add
// racing a Wait at zero panics, wg.Wait returning did not mean handlers had
// finished with the database, and goroutines-per-message is unbounded on a channel
// that can burst (SPEC.md section 8, finding 4).
//
// A dropped message is logged rather than retried. discordgo dispatches every
// event on its own goroutine, so blocking here to wait for queue space would grow
// goroutines without bound and turn a slow corpus into unbounded memory use.
// Best-effort is the honest semantics for chat.
func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.Bot {
		return
	}

	if !dispatcher.Submit(func(context.Context) { handleMessage(s, m) }) {
		log.Printf("[QUEUE] dropped message from %s: work queue full (%d dropped so far)",
			m.Author.Username, dispatcher.Dropped())
	}
}

// messageDelete revokes anything the deleted message contributed to the repost cache.
//
// Through the dispatcher rather than inline, like every other gateway event. A mass
// deletion (a moderator clearing a spam raid) arrives as a burst, and each one of these
// opens a write transaction: doing that on discordgo's own goroutine would put an
// unbounded number of them in line for bbolt's single writer.
//
// Unlike messageCreate this does NOT skip bots, and the difference is deliberate. It
// skips them because a bot's message must not be learned; here the question is whether a
// cached URL's source still exists, and the answer does not depend on who posted it. The
// bot's own reposts are in nobody's cache, so they simply match nothing.
func messageDelete(_ *discordgo.Session, m *discordgo.MessageDelete) {
	if m.ID == "" {
		return
	}
	if !dispatcher.Submit(func(context.Context) { forgetImagesFromMessage(m.ID) }) {
		log.Printf("[QUEUE] dropped a message deletion in %s: work queue full (%d dropped so far)",
			m.ChannelID, dispatcher.Dropped())
	}
}

// messageDeleteBulk is the same for Discord's bulk deletion event, which is what a
// moderator purging a channel produces. Handled as one unit of work rather than one per
// ID, so a hundred-message purge is one write transaction instead of a hundred.
func messageDeleteBulk(_ *discordgo.Session, m *discordgo.MessageDeleteBulk) {
	if len(m.Messages) == 0 {
		return
	}
	ids := append([]string(nil), m.Messages...)
	if !dispatcher.Submit(func(context.Context) { forgetImagesFromMessage(ids...) }) {
		log.Printf("[QUEUE] dropped a bulk deletion of %d messages in %s: work queue full (%d dropped so far)",
			len(ids), m.ChannelID, dispatcher.Dropped())
	}
}

// captureImageURLs caches one image or Tenor URL from a message so a later
// repost has something to post. Extracted from messageCreate only so the
// EnableImageRepost gate could wrap it as one statement; the body is unchanged.
//
// The NSFW check reads the state cache as of M10a, not REST.
//
// This used to be s.Channel, a REST request on every message carrying a candidate URL,
// purely to read one boolean. It was probably the largest rate-limit consumer in the bot.
// s.State.Channel answers from the cache the gateway already maintains, which
// core.NewSession populates now that it requests IntentsGuilds (SPEC.md section 8,
// finding 7): the same missing intent that meant custom emotes had never worked was also
// forcing this call.
//
// It FAILS CLOSED. A cache miss means no caching of the URL, not caching it anyway: the
// check exists to keep the bot from reposting NSFW media into a channel of its choosing,
// and "we could not tell" has to mean "do not" for that to be worth anything. A miss is
// rare and transient (the cache fills on READY), and the cost of being wrong in the safe
// direction is one image not being remembered.
func captureImageURLs(s *discordgo.Session, m *discordgo.MessageCreate) {
	ch, err := s.State.Channel(m.ChannelID)
	if err != nil {
		log.Printf("[WARN] channel %s is not in the state cache, not caching this URL: %v", m.ChannelID, err)
		return
	}
	if ch.NSFW || strings.Contains(strings.ToLower(ch.Name), "nsfw") {
		log.Printf("[INFO] Skipping image cache for NSFW-flagged or named channel #%s", ch.Name)
		return
	}

	var candidateURLs []string

	// 1. Check attachments first.
	for _, att := range m.Attachments {
		if strings.HasPrefix(att.ContentType, "image/") && discordCDNRegex.MatchString(att.URL) {
			candidateURLs = append(candidateURLs, att.URL)
		}
	}

	// 2. Scan message content for any URLs.
	for _, word := range tokenize(m.Content) {
		if discordCDNRegex.MatchString(word) || tenorRegex.MatchString(word) {
			candidateURLs = append(candidateURLs, word)
		}
	}
	if len(candidateURLs) == 0 {
		return
	}
	urlToCache := candidateURLs[rand.IntN(len(candidateURLs))]

	// Persist and then reload the cache. The reload happens through the Writer, which
	// embeds Reader, so it sees its own write without opening a second transaction: that
	// is the reason the Writer embeds the Reader rather than being a separate type.
	//
	// The URL is attributed to its message and its author, which is what lets
	// AddImageURL cap one author's share of the cache and lets a later deletion of the
	// message revoke it (SPEC.md section 4, A7). Trimming and both caps are the store's,
	// so a second caller cannot skip them.
	//
	// NOT holding imageURLMutex across this. It used to wrap the whole function including
	// the store.Update, so one goroutine's bbolt write transaction (which serializes
	// against every other write in the process) also blocked every other capture from
	// even looking at the cache. The lock protects the slice; the store protects itself.
	var newURLList []string
	if err := store.Update(func(w *storage.Writer) error {
		if err := w.AddImageURL(urlToCache, m.ID, m.Author.ID, cfg.ImageCacheSize, cfg.ImageMaxPerAuthor); err != nil {
			return fmt.Errorf("failed to save image URL: %w", err)
		}
		var err error
		newURLList, err = w.ImageURLs()
		return err
	}); err != nil {
		log.Printf("[WARN] DB operation for image cache failed: %v", err)
		return
	}

	// In-memory cache updated ONLY after the write succeeded, so the two cannot disagree
	// about what is repostable.
	imageURLMutex.Lock()
	recentImageURLs = newURLList
	size := len(recentImageURLs)
	imageURLMutex.Unlock()
	log.Printf("[IMG] Captured URL: %s, cache size: %d", urlToCache, size)
}

// forgetImagesFromMessage drops every cached URL a deleted message contributed.
//
// This is the deleted-message half of SPEC.md section 4, A7. A deletion is a strong
// signal that the content should not be republished, and the bot reposting something a
// moderator or its own author has just removed is the failure that finding describes.
// The rule was written into the spec a milestone ago and could not be implemented,
// because the image cache was keyed by URL and there was no way to ask which entries a
// message had contributed.
//
// It is deliberately silent about IDs it does not hold, which is almost all of them:
// every message deletion in every channel the bot can see arrives here.
func forgetImagesFromMessage(messageIDs ...string) {
	removed := 0
	var newURLList []string
	if err := store.Update(func(w *storage.Writer) error {
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
		newURLList, err = w.ImageURLs()
		return err
	}); err != nil {
		log.Printf("[WARN] could not revoke cached images for deleted messages: %v", err)
		return
	}
	if removed == 0 {
		return
	}

	// Outside the transaction, for the same reason capture is: the lock guards the slice
	// and taking it inside a bbolt write transaction couples it to the process-wide
	// writer.
	imageURLMutex.Lock()
	recentImageURLs = newURLList
	imageURLMutex.Unlock()
	log.Printf("[IMG] Dropped %d cached URL(s) whose source message was deleted.", removed)
}

// -----------------------------------------------------------------------------
//  Bird Aggro
// -----------------------------------------------------------------------------

// findRandomActiveUser picks someone who has spoken recently, or "".
//
// It reads the in-process activity tracker. It used to call s.UserGuilds, then
// getActiveChannels for each guild (which paged every text channel fifty messages at a
// time with a 50ms sleep between pages), then another hundred messages per active channel
// to collect authors. Hundreds of REST calls per aggro tick, on an hourly ticker, to
// answer a question the gateway had already answered for free (SPEC.md section 8, finding
// 14).
//
// One consequence is worth knowing before it looks like a bug: the candidates are people
// the bot has SEEN, so for the first minutes after a restart there is no target at all.
// The old version could reach six hours into history and pick someone who had since left
// the conversation, which is the worse answer for a feature whose whole point is poking
// somebody who is present.
func findRandomActiveUser() string {
	candidates := channelActivity.RecentAuthors(cfg.AggroActivityWindow)

	// Never the bot itself. It cannot reach here (messageCreate rejects bots before
	// anything is recorded), but aggro on our own messages would be a loop of the bot
	// reacting to itself, so the check costs nothing and closes it structurally.
	filtered := candidates[:0]
	for _, id := range candidates {
		if id != botID {
			filtered = append(filtered, id)
		}
	}
	if len(filtered) == 0 {
		log.Println("[AGGRO] No recently active users to pick a target from.")
		return ""
	}
	return filtered[rand.IntN(len(filtered))]
}

// -----------------------------------------------------------------------------
//  Autonomous posting
// -----------------------------------------------------------------------------

// busiestChannel returns the ID of the most active channel the bot can see, or "".
//
// Two callers want it: autonomous posting and interval-mode word games. It was inline in
// the first, so the second would have had to copy it, and a copy is how the two would
// eventually disagree about what "busiest" means.
//
// It reads the activity tracker for volume and the STATE CACHE for everything else, which
// is the division the two sources force: the gateway tells us how much traffic a channel
// has had, and State tells us what the channel is. Neither costs a REST call. The version
// this replaces paged every channel in every guild to count messages.
//
// allow, when non-empty, restricts the choice. Filtering while CHOOSING rather than after
// is the fix for a real bug: the old code scored every channel, picked the winner, and then
// rejected it if it was not on the allowlist, so a bot whose busiest channel was not listed
// posted nothing and logged a rejection every single cycle. Now the busiest ALLOWED channel
// wins, which is what an allowlist means.
//
// The "general" bonus is preserved and is a judgement about where a bot is welcome to speak
// unprompted rather than a measurement.
func busiestChannel(s *discordgo.Session, allow []string) string {
	allowed := make(map[string]struct{}, len(allow))
	for _, id := range allow {
		if id != "" {
			allowed[id] = struct{}{}
		}
	}

	best := ""
	bestScore := 0.0
	for _, c := range channelActivity.Busiest(cfg.ActiveChannelWindow) {
		if len(allowed) > 0 {
			if _, ok := allowed[c.ID]; !ok {
				continue
			}
		}
		ch, err := s.State.Channel(c.ID)
		if err != nil || ch == nil {
			// Not in the cache, so we cannot check what it is. Skipped rather than used,
			// for the same reason the image capture fails closed on a cache miss: this
			// decides where the bot speaks unprompted, and "we could not tell" has to
			// mean "not here".
			continue
		}
		if ch.Type != discordgo.ChannelTypeGuildText {
			continue
		}

		score := float64(c.Count)
		if strings.Contains(strings.ToLower(ch.Name), "general") {
			score *= 1.5
		}
		// Ties break on channel ID, so the choice does not depend on map iteration order.
		// Tracker.Busiest sorts its result for the same reason.
		if score > bestScore || (score == bestScore && ch.ID < best) {
			bestScore = score
			best = ch.ID
		}
	}

	if best == "" {
		// The cold-start case, and the one an operator will see first: the tracker is
		// empty for the first window after a restart, so there is nowhere to speak yet.
		// Falling back to the state cache's LastMessageID would give recency without
		// volume, and a channel whose last message was three hours ago is not somewhere
		// to start talking unprompted.
		log.Printf("[ACTIVE] No channel has had traffic in the last %s.", cfg.ActiveChannelWindow)
	}
	return best
}

// autonomousPost picks an active channel and posts a generated message occasionally.
func autonomousPost(ctx context.Context, dg *discordgo.Session) {
	start := time.Now()
	log.Println("[AUTONOMOUS] Starting autonomous post cycle...")

	channelID := busiestChannel(dg, cfg.AutonomousPostChannels)
	if channelID == "" {
		log.Println("[AUTONOMOUS] No active channel found, skipping post")
		return
	}

	// Chance to skip for "natural pacing" (applies to ALL autonomous posts)
	skipChance := cfg.AutonomousSkipChance
	if rand.Float64() < skipChance {
		log.Printf("[AUTONOMOUS] Skipping this cycle for natural pacing (chance %.2f)", skipChance)
		return
	}

	// THE WORD-GAME GATE THAT USED TO BE HERE IS GONE, and it was a wiring error rather
	// than a policy (SPEC.md section 8, finding 30).
	//
	// This function posts a MARKOV SENTENCE. It always has: the call below is
	// generateSentenceWithContext. But it returned early unless word games were enabled
	// AND their dictionary had loaded, and then paced itself with
	// PEREGRINE_WORDGAME_MODE and PEREGRINE_WORDGAME_INTERVAL, while two comments
	// claimed it posted word games.
	//
	// The consequence is the one that matters: setting
	// PEREGRINE_ENABLE_AUTONOMOUS_POST=true produced NOTHING unless word games happened
	// to be on too, which they were not by default. That is the third distinct way this
	// feature has been dead, after the compile-time constant and the empty allowlist.
	//
	// Pacing is the skip chance above plus the tick, both of which are autonomous
	// posting's own configuration. Word games have their own trigger in the reactor, and
	// PEREGRINE_WORDGAME_MODE and PEREGRINE_WORDGAME_INTERVAL are what M11 rewires to
	// mean what they say.

	// The allowlist check that used to be here is gone, because busiestChannel applies it
	// while CHOOSING rather than after. Checking afterwards meant the busiest channel in
	// the server won the scoring and was then rejected, so a bot whose allowlist did not
	// happen to contain its busiest channel posted nothing and logged a rejection every
	// cycle. Filtering first picks the busiest ALLOWED channel, which is what the
	// allowlist means.

	// The conversation memory is seeded with the channel's own recent context rather than
	// with its last message ID, which is what the old code passed: a snowflake, tokenized
	// into a meaningless number and fed to the generator as if it were something somebody
	// said.
	msg, err := generateSentenceWithContext(dg, "autonomous thought", false, memoryFor(channelID))
	if err != nil {
		log.Println("[AUTONOMOUS] Error generating message:", err)
		return
	}
	if msg == "" {
		log.Println("[AUTONOMOUS] Generated message is empty, skipping")
		return
	}

	// Send message. Through the guard, which is a change in behaviour and not only in
	// plumbing: until M10 this path reached Discord without passing CheckEmit at all,
	// because the gate sat at the generation exit and the autonomous poster called a
	// different one. An unprompted post is the output with the least human context
	// around it, so it is the worst one to have been ungated.
	if sentMsg, ok := guard.Send(channelID, msg); ok {
		log.Printf("[AUTONOMOUS] Sent message in %s: %s (ID: %s)", channelID, msg, sentMsg.ID)
	}

	log.Printf("[AUTONOMOUS] Cycle finished in %s", time.Since(start))
}

// transcriptionWorker processes voice transcription jobs from a queue.
func transcriptionWorker(ctx context.Context, s *discordgo.Session) {
	log.Println("[INFO] Transcription worker started.")
	for {
		select {
		case job := <-transcriptionQueue:
			log.Printf("[VOICE] Starting transcription for message %s...", job.MsgID)
			transcript, err := voicenotes.TranscribeVoiceNote(job.URL)
			if err != nil {
				log.Printf("[VOICE] Transcription failed for message %s: %v", job.MsgID, err)
				editMessage(s, job.ChannelID, job.PlaceholderID, "❌ Transcription failed.")
				continue
			}
			if transcript == "" {
				deleteMessage(s, job.ChannelID, job.PlaceholderID)
				continue
			}

			log.Printf("[VOICE] Transcript for %s: %s", job.MsgID, transcript)
			// Through the guard, and this is the send whose content is least under
			// anyone's control: a Whisper transcript of arbitrary audio, posted by the
			// bot. Until M10 it passed neither CheckEmit nor mention suppression, so
			// someone could have had the bot say anything by saying it out loud, and a
			// transcript containing a mention would have pinged.
			//
			// The code fence does not help. Discord parses mentions inside code blocks
			// for notification purposes even though it does not render them as links.
			editMessage(s, job.ChannelID, job.PlaceholderID, fmt.Sprintf("```\n%s\n```", transcript))

			// Learn transcript
			authorInfo := MentionedUser{
				Name:     job.Author.Username,
				UserID:   job.AuthorID,
				Username: job.Author.Username,
			}
			if job.Member != nil && job.Member.Nick != "" {
				authorInfo.Name = job.Member.Nick
			}
			err = store.Update(func(w *storage.Writer) error {
				return learnMessage(w, transcript, job.MsgID, botID, authorInfo, []MentionedUser{})
			})
			if err != nil {
				log.Printf("[VOICE] Failed to learn transcript for message %s: %v", job.MsgID, err)
			}
		case <-ctx.Done():
			log.Println("[INFO] Transcription worker stopped.")
			return
		}
	}
}

// -----------------------------------------------------------------------------
//  Monitoring & status
// -----------------------------------------------------------------------------

// printLibraryStatus logs the size of each index.
//
// It calls Bucket.Stats() through Reader.Status, which walks every page, so it is
// genuinely expensive and belongs on this ticker and nowhere else. The same call used
// to sit in learnMessage, once per message, purely to fill a log field
// (SPEC.md section 8, finding 11).
//
// Two of the counts here come from meta counters instead: the history window and the
// image cache are the two buckets that get trimmed, and their counters are what made
// the trims stop being quadratic.
func printLibraryStatus() {
	start := time.Now()
	err := store.View(func(r *storage.Reader) error {
		st := r.Status()
		log.Printf(
			"Library status: ngrams=%d | author-entries=%d | topics=%d | topic-word=%d | "+
				"name-topic=%d | names=%d | clusters=%d | history=%d | images=%d | "+
				"total-learned=%d | checked in %s",
			st.Ngrams, st.AuthorEntries, st.Topics, st.TopicWords, st.NameTopics,
			st.Names, st.Clusters, st.HistoryWindow, st.ImageCache, st.Learned,
			time.Since(start),
		)
		return nil
	})

	if err != nil {
		log.Printf("[ERR] checking library status: %v", err)
	}
}

// monitorPerformance periodically probes the Discord API and logs notable latency.
func monitorPerformance(ctx context.Context, dg *discordgo.Session) {
	// small startup jitter so multiple instances don't align
	time.Sleep(time.Duration(rand.IntN(1000)) * time.Millisecond)

	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	// only log if latency is above this or on error (reduces spam)
	const latencyLogThreshold = 500 * time.Millisecond

	for {
		select {
		case <-ticker.C:
			start := time.Now()
			_, err := dg.User("@me")
			latency := time.Since(start)

			if err != nil {
				log.Printf("[HEALTH] ⚠️ Discord API ping failed: %v", err)
				continue
			}

			// Only log notable latencies to avoid noisy output
			if latency > latencyLogThreshold {
				log.Printf("[HEALTH] ⚠️ Discord API latency high: %s", latency)
			}
		case <-ctx.Done():
			log.Println("[INFO] Performance monitor stopped by shutdown signal.")
			return
		}
	}
}

// stringContains is gone. It reimplemented slices.Contains, and its last caller was the
// autonomous poster's allowlist check, which busiestChannel now applies while choosing
// rather than after (SPEC.md section 8, the folded-in list). The linter reporting it unused
// is how that was confirmed rather than assumed, which is the same way M6b established that
// filter.go's last two wrappers existed only for the cleanup pass.

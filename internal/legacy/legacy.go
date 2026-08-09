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
	"math"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/6586x57890143/peregrine/internal/config"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/markov"
	"github.com/6586x57890143/peregrine/internal/safety"
	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/text"
	"github.com/6586x57890143/peregrine/voicenotes"
	"github.com/6586x57890143/peregrine/wordgames"
	"github.com/beevik/ntp"
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

var activeWordGames = make(map[string]*wordgames.ScrambleGame)
var wordGameMutex = &sync.Mutex{}

// wordGamesAvailable records whether the dictionary actually loaded. Checked
// alongside cfg.EnableWordGames so that a failed load degrades to "word games off"
// rather than to a panic on an empty word list.
var wordGamesAvailable bool
var channelActivity = make(map[string][]time.Time)
var activityMutex = &sync.Mutex{}
var leaderboard *wordgames.Leaderboard

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

// lastWordGameTime tracks the last time a word game was posted in interval mode.
var lastWordGameTime time.Time

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

	lastWordGameTime = time.Now() // Initialize with current time on startup

	// Load the word game dictionary. Deliberately not fatal: word games are one
	// optional feature, and taking the whole bot down because a 64 KB word list
	// would not load meant an unrelated asset problem killed learning,
	// generation, and every other behavior with it. A failure here disables word
	// games and says so. PEREGRINE_WORDGAME_DICTIONARY overrides the embedded
	// list; empty means use the embedded one.
	if err := wordgames.LoadDictionary(cfg.WordGameDictionary); err != nil {
		log.Printf("[WARN] Word game dictionary failed to load, word games disabled: %v", err)
		wordGamesAvailable = false
	} else {
		log.Println("[INFO] Word game dictionary loaded.")
		wordGamesAvailable = true
	}

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
			leaderboard = wordgames.NewLeaderboard()
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
				ingestRecentMessagesIncremental(ctx, dg)
				log.Println("[AUTO] Autonomous ingestion finished.")
			},
		},
		{
			Name:  "aggro",
			Every: cfg.AggroTick,
			Fn:    func(ctx context.Context) { maybeTriggerAggro(ctx, dg) },
		},
		{
			Name:  "leaderboard-reset",
			Every: cfg.LeaderboardTick,
			Fn:    func(context.Context) { maybeResetLeaderboard() },
		},
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

// maybeTriggerAggro is the body of what was an inline ticker goroutine. Extracted
// unchanged so the loop table above reads as a list of behaviors.
func maybeTriggerAggro(ctx context.Context, dg *discordgo.Session) {
	birdAggroMutex.Lock()
	defer birdAggroMutex.Unlock()

	// Only trigger if there's no current aggro
	if birdAggroTargetID != "" && !time.Now().After(birdAggroEndTime) {
		return
	}
	if rand.Float64() >= cfg.AggroChance {
		return
	}
	target := findRandomActiveUser(ctx, dg)
	if target == "" {
		return
	}
	birdAggroTargetID = target
	birdAggroEndTime = time.Now().Add(cfg.AggroDuration)
	log.Printf("[AGGRO] Bird aggro triggered on user %s for %v.", target, cfg.AggroDuration)
	// Persist the new aggro state
	if err := store.Update(func(w *storage.Writer) error {
		return saveAggroState(w, AggroState{TargetID: birdAggroTargetID, EndTime: birdAggroEndTime})
	}); err != nil {
		log.Printf("[ERR] Failed to persist aggro state: %v", err)
	}
}

// maybeResetLeaderboard is likewise the body of a former inline ticker.
func maybeResetLeaderboard() {
	reset, err := isTimeToResetLeaderboard()
	if err != nil {
		log.Printf("[LEADERBOARD] Error checking reset time: %v", err)
		return
	}
	if !reset {
		return
	}
	leaderboard.Mutex.Lock()
	defer leaderboard.Mutex.Unlock()

	now := time.Now().UTC()
	// Final check to ensure we only reset once per week
	if now.Sub(leaderboard.LastReset) <= 24*time.Hour {
		return
	}
	log.Println("[LEADERBOARD] It's a new week! Resetting the leaderboard.")
	leaderboard.WeekStartDate = now
	leaderboard.LastReset = now
	leaderboard.Scores = make(map[string]wordgames.LeaderboardEntry)
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

// getActiveChannels returns all text channels in a guild that have recent activity.
func getActiveChannels(ctx context.Context, s *discordgo.Session, guildID string, cutoff time.Time) []ActiveChannelInfo {
	channels, err := s.GuildChannels(guildID)
	if err != nil {
		if restErr, ok := err.(*discordgo.RESTError); ok && restErr.Response != nil && restErr.Response.StatusCode == 403 {
			return nil // Can't view channels in this guild, just return silently.
		}
		log.Printf("[ERR] Failed to fetch channels for guild %s: %v", guildID, err)
		return nil
	}

	log.Printf("[INFO] Checking %d channels for activity in guild %s...", len(channels), guildID)

	var wg sync.WaitGroup
	activeCh := make(chan ActiveChannelInfo, len(channels))

	for _, c := range channels {
		select {
		case <-ctx.Done():
			log.Printf("[INFO] Channel scanning stopped by shutdown signal.")
			return nil // Return early if shutdown signal received
		default:
		}
		if c.Type != discordgo.ChannelTypeGuildText {
			continue
		}

		wg.Add(1)
		go func(ch *discordgo.Channel) {
			defer wg.Done()
			count := countRecentMessages(s, ch, cutoff)
			if count > 0 {
				log.Printf("[INFO] Channel #%s has %d recent messages, adding to active list.", ch.Name, count)
				activeCh <- ActiveChannelInfo{Channel: ch, MessageCount: count}
			}
		}(c)
	}

	// Close channel when all goroutines finish
	go func() {
		wg.Wait()
		close(activeCh)
	}()

	var active []ActiveChannelInfo
	for chInfo := range activeCh {
		active = append(active, chInfo)
	}

	if len(active) == 0 {
		log.Printf("[INFO] No active channels found in guild %s", guildID)
	} else {
		log.Printf("[INFO] Found %d active channels in guild %s", len(active), guildID)
	}

	return active
}

// countRecentMessages counts non-bot messages newer than cutoff in a channel.
func countRecentMessages(s *discordgo.Session, ch *discordgo.Channel, cutoff time.Time) int {
	count := 0
	beforeID := ""
	batchSize := 50 // Discord API page size

	for {
		batch, err := s.ChannelMessages(ch.ID, batchSize, beforeID, "", "")
		if err != nil {
			if restErr, ok := err.(*discordgo.RESTError); ok && restErr.Response != nil && restErr.Response.StatusCode == 403 {
				return 0 // It's a permissions error, just skip this channel silently.
			}
			log.Printf("[ERR] Fetch messages from #%s failed: %v", ch.Name, err)
			return 0 // Return 0 on error to prevent further processing of this channel
		}
		if len(batch) == 0 {
			break
		}

		for _, m := range batch {
			if m.Author.Bot || m.Timestamp.IsZero() {
				continue
			}
			if m.Timestamp.After(cutoff) {
				count++
			}
		}

		// Stop if we've reached messages older than cutoff
		oldestTs := batch[len(batch)-1].Timestamp
		if len(batch) < batchSize || oldestTs.Before(cutoff) {
			break
		}

		beforeID = batch[len(batch)-1].ID
		time.Sleep(50 * time.Millisecond) // mild pacing to avoid rate limits
	}

	return count
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

// saveLeaderboard saves the current leaderboard state.
func saveLeaderboard(w *storage.Writer, l *wordgames.Leaderboard) error {
	encoded, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return w.PutBlob(storage.BlobLeaderboard, "current", encoded)
}

// loadLeaderboard loads the leaderboard state.
func loadLeaderboard(r *storage.Reader) (*wordgames.Leaderboard, error) {
	v, err := r.GetBlob(storage.BlobLeaderboard, "current")
	if err != nil {
		return nil, err
	}
	if v == nil {
		return wordgames.NewLeaderboard(), nil // No state saved, create a new one
	}
	var l wordgames.Leaderboard
	if err := json.Unmarshal(v, &l); err != nil {
		return nil, err
	}
	// Ensure the map is initialized
	if l.Scores == nil {
		l.Scores = make(map[string]wordgames.LeaderboardEntry)
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

// isTimeToResetLeaderboard checks an NTP server to see if it's Monday morning UTC.
func isTimeToResetLeaderboard() (bool, error) {
	ntpTime, err := ntp.Time("pool.ntp.org")
	if err != nil {
		return false, fmt.Errorf("failed to query NTP server: %w", err)
	}

	utcTime := ntpTime.UTC()
	// Reset between 00:00 and 01:00 on Monday UTC
	if utcTime.Weekday() == time.Monday && utcTime.Hour() == 0 {
		return true, nil
	}
	return false, nil
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
	// Still all-pairs and still O(n^2) in message length, which is finding 12 and
	// belongs to M7's co-occurrence window. What changed is the cost per pair: each
	// one is now a 16-byte read-add-write on its own key instead of a member of a
	// JSON map that grows without bound.
	//
	// The separate topic-cluster pass that used to follow this is gone, and it was
	// pure duplication: it recorded the same word pairs from the same messages under
	// the same stop-word exclusion, canonicalised into one order and WITHOUT the
	// position sum, into a second bucket. Everything it stored is derivable from what
	// this loop stores, so the layout has one co-occurrence index rather than two
	// that could disagree (SPEC.md section 8, finding 28).
	if len(canonicalNames) > 0 {
		for i, wordA := range words {
			lwA := toLowerCaseExceptURLs(wordA)
			if lwA == "<end>" {
				continue
			}
			if _, isStop := stopWords[lwA]; isStop {
				continue
			}
			for j, wordB := range words {
				if i == j {
					continue
				}
				lwB := toLowerCaseExceptURLs(wordB)
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

// ingestRecentMessagesIncremental walks guilds and ingests recent messages in parallel.
func ingestRecentMessagesIncremental(ctx context.Context, s *discordgo.Session) {
	start := time.Now().UTC() // Use UTC for consistency
	// ALWAYS look back the full PEREGRINE_INGEST_LOOKBACK window.
	ingestionCutoff := time.Now().UTC().Add(-cfg.IngestLookback)

	guilds, err := s.UserGuilds(100, "", "", false)
	if err != nil {
		log.Println("[ERR] failed to fetch guilds:", err)
		return
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, 5) // limit concurrent guild workers

	for _, gptr := range guilds {
		if gptr == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{} // acquire slot

		// copy value for closure
		g := *gptr
		go func(guild discordgo.UserGuild) {
			defer wg.Done()
			defer func() { <-sem }()
			processGuildIncremental(ctx, s, guild, ingestionCutoff)
		}(g)
	}

	wg.Wait()
	log.Printf("[DONE] autonomous ingestion complete in %s", time.Since(start))
}

// processGuildIncremental processes active channels for a guild.
func processGuildIncremental(ctx context.Context, s *discordgo.Session, g discordgo.UserGuild, ingestionCutoff time.Time) {
	var guildWg sync.WaitGroup // WaitGroup for channels within this guild

	// Use a wider window to find channels that are generally active.
	activeChannelCutoff := time.Now().UTC().Add(-cfg.IngestLookback)
	for _, chInfo := range getActiveChannels(ctx, s, g.ID, activeChannelCutoff) {
		select {
		case <-ctx.Done():
			log.Printf("[INFO] Guild %s ingestion stopped by shutdown signal.", g.Name)
			guildWg.Wait() // Wait for any already launched channel goroutines
			return
		default:
		}
		guildWg.Add(1)
		go func(ch *discordgo.Channel) {
			defer guildWg.Done()
			// Pass the more precise ingestionCutoff for fetching messages.
			processChannelIncremental(ctx, s, ch, ingestionCutoff)
		}(chInfo.Channel)
	}
	guildWg.Wait() // Wait for all channels in this guild to finish
}

// processChannelIncremental fetches messages incrementally and learns them.
func processChannelIncremental(ctx context.Context, s *discordgo.Session, ch *discordgo.Channel, cutoff time.Time) {
	start := time.Now()
	total, errors, skipped := 0, 0, 0
	var allMessages []*discordgo.Message
	beforeID := ""

	// Loop to fetch all messages *before* the last oldest one we've seen.
	for {
		select {
		case <-ctx.Done():
			log.Printf("[INFO] Channel #%s processing stopped by shutdown signal.", ch.Name)
			return
		default:
		}
		batch, err := s.ChannelMessages(ch.ID, 100, beforeID, "", "")
		if err != nil {
			if restErr, ok := err.(*discordgo.RESTError); ok && restErr.Response != nil && restErr.Response.StatusCode == 403 {
				// Permissions error, stop processing this channel.
			} else {
				log.Printf("[ERR] fetch #%s: %v", ch.Name, err)
			}
			break
		}
		if len(batch) == 0 {
			break // No more new messages.
		}

		allMessages = append(allMessages, batch...)

		// Check the oldest message in this batch. If it's older than cutoff, we can stop.
		oldest := batch[len(batch)-1]
		if oldest.Timestamp.Before(cutoff) {
			break
		}

		// If we received less than 100 messages, we've reached the oldest messages available.
		if len(batch) < 100 {
			break
		}

		// Prepare for next page: fetch messages older than the current oldest.
		beforeID = batch[len(batch)-1].ID
		time.Sleep(cfg.IngestBatchDelay) // Pacing to avoid rate limits
	}

	// If no messages were pulled, nothing to do.
	if len(allMessages) == 0 {
		return
	}

	// Reverse in-place to chronological order (oldest -> newest) before processing.
	for i, j := 0, len(allMessages)-1; i < j; i, j = i+1, j-1 {
		allMessages[i], allMessages[j] = allMessages[j], allMessages[i]
	}

	// Process messages in chronological order.
	for _, m := range allMessages {
		if m.Timestamp.Before(cutoff) {
			skipped++
			continue
		}
		if m.Author.Bot || m.Timestamp.IsZero() {
			skipped++
			continue
		}

		names := extractNamesFromMessage(s, &discordgo.MessageCreate{Message: m}, ch.GuildID)
		authorInfo := MentionedUser{
			Name:     m.Author.Username,
			UserID:   m.Author.ID,
			Username: m.Author.Username,
		}
		if m.Member != nil && m.Member.Nick != "" {
			authorInfo.Name = m.Member.Nick
		}

		err := store.Update(func(w *storage.Writer) error {
			return learnMessage(w, m.Content, m.ID, botID, authorInfo, names)
		})

		if err != nil {
			errors++
		} else {
			total++
		}
	}

	if total > 0 || errors > 0 || skipped > 0 {
		log.Printf("[DONE] #%s: %d new, %d skipped, %d errors, duration: %s", ch.Name, total, skipped, errors, time.Since(start))
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

// applyEdgyStyle adds a configurable, context-aware "edgy" flavor to sentences.
func applyEdgyStyle(s string, isAboutName bool) string {
	fields := strings.Fields(s)
	// Only apply style to sentences of a reasonable length to avoid nonsensical short phrases.
	if len(fields) < 4 {
		return s
	}

	s = strings.ToLower(s)
	openers := []string{"ngl", "tbh", "bruh", "like", "i guess", "idk but", "listen", "ok so", "fr tho", "no cap", "deadass", "lowkey", "bet", "sheesh", "valid"}
	closers := []string{"lol", "lmao", "whatever", "i guess", "or something", "...", "smh", "for real", "periodt", "iykyk", "no cap", "fr fr", "ong"}
	interjections := []string{"ngl", "fr", "tbh", "like", "i mean", "lowkey", "bet"}
	metaComments := []string{" (i think)", " (just saying)", " (don't quote me)", " (or so they say)", " (allegedly)"}

	// Base chance to apply style is lower.
	chance := 0.35
	// If the sentence is about a recognized person, be more likely to be edgy (for roasting).
	if isAboutName {
		chance = 0.65
	}

	// Adjust chance based on sentence length for contextual intensity.
	lengthFactor := math.Min(1.0, float64(len(fields))/20.0) // Max intensity at ~20 words
	chance *= (0.7 + 0.6*lengthFactor)                       // Scale chance between 70% and 130% of base

	if rand.Float64() < chance {
		styleType := rand.IntN(4) // 0: opener, 1: closer, 2: interjection, 3: meta-comment

		switch styleType {
		case 0:
			s = openers[rand.IntN(len(openers))] + " " + s
		case 1:
			s += " " + closers[rand.IntN(len(closers))]
		case 2:
			// Insert a mid-sentence interjection
			if len(fields) > 3 {
				insertPos := 1 + rand.IntN(len(fields)-2) // Avoid very beginning or end
				interjection := interjections[rand.IntN(len(interjections))]
				fields = append(fields[:insertPos], append([]string{interjection}, fields[insertPos:]...)...)
				s = strings.Join(fields, " ")
			}
		case 3:
			// Insert a meta-comment
			if len(fields) > 2 {
				insertPos := 1 + rand.IntN(len(fields)-1) // Avoid very beginning
				metaComment := metaComments[rand.IntN(len(metaComments))]
				// Append to the previous word to avoid extra spaces
				fields[insertPos-1] += metaComment
				s = strings.Join(fields, " ")
			}
		}
	}
	return s
}

// findBestSeed analyzes the prompt and context to find the highest-quality starting
// seed.
//
// Two of the seven priority tiers it used to have are gone, and both for the same
// reason: they read buckets that recorded nothing the surviving indexes do not.
//
// The concept-cluster tier scored at weight 50, higher than everything else here, and
// had never once fired. Cluster members were persisted with string keys and decoded
// into map[int]float32, which fails for every cluster, and the decode was guarded by
// `err == nil` with no else, so the tier silently contributed nothing for the whole
// life of the bot (SPEC.md section 8, finding 27). It is also unfixable as written:
// the members are text.Interner ids, and those depend on insertion order, so an id
// written to disk means a different word to the next process. M8 rebuilds it.
//
// The two topic-cluster tiers read a bucket that stored the same word pairs as the
// topic-word index, minus the position data, so tiers 2 and 4 asked the topic-word
// index a question it could already answer. They are folded into the name-topic and
// topic-word tiers below, keeping their weights (finding 28).
func findBestSeed(r *storage.Reader, in *text.Interner, promptWords, recentWords, recognizedNames []string) string {
	type candidate struct {
		key    int
		weight float64
	}
	candidateMap := make(map[int]float64)

	addCandidate := func(key int, weight float64) {
		if existingWeight, ok := candidateMap[key]; !ok || weight > existingWeight {
			candidateMap[key] = weight
		}
	}

	// 1. High Priority: Multi-word n-grams from the prompt.
	//
	// n starts at 1 rather than at MaxNGram-1 downward being cut off, because a
	// single-word prefix is a legitimate n-gram context in the new layout: the key is
	// <prefix> NUL <next>, so "bird" has successors of its own.
	for n := cfg.MaxNGram - 1; n >= 1; n-- {
		for i := 0; i <= len(promptWords)-n; i++ {
			key := toLowerCaseExceptURLs(strings.Join(promptWords[i:i+n], " "))
			if r.HasSuccessors(key) {
				addCandidate(in.Intern(key), float64(n*30))
			}
		}
	}

	// 2. Name Expansion: topics this message's names are associated with. This is the
	// former name-cluster tier, answered from the name-topic index at its old weight.
	for _, name := range recognizedNames {
		assocs, err := r.NameTopicsFor(toLowerCaseExceptURLs(name))
		if err != nil {
			log.Printf("[WARN] findBestSeed: name-topic lookup for %s failed: %v", name, err)
			continue
		}
		for topic, data := range assocs {
			addCandidate(in.Intern(topic), 25.0+math.Sqrt(float64(data.Count)))
		}
	}

	// 3. Associative Expansion: Find words related to the prompt words.
	for _, word := range promptWords {
		topic := toLowerCaseExceptURLs(word)
		assocs, err := r.TopicWordsFor(topic)
		if err != nil {
			log.Printf("[WARN] findBestSeed: topic-word lookup for %s failed: %v", topic, err)
			continue
		}
		for associatedWord, data := range assocs {
			if associatedWord != topic && data.Count > 1 {
				addCandidate(in.Intern(associatedWord), 18.0+math.Sqrt(float64(data.Count)))
			}
		}
	}

	// 4. Medium-High Priority: Topics directly associated with the most recent
	// recognized name, restricted to ones the chain can actually continue from.
	if len(recognizedNames) > 0 {
		name := toLowerCaseExceptURLs(recognizedNames[len(recognizedNames)-1])
		assocs, err := r.NameTopicsFor(name)
		if err != nil {
			log.Printf("[WARN] findBestSeed: name-topic lookup for %s failed: %v", name, err)
		} else {
			for topic, data := range assocs {
				if r.HasSuccessors(topic) {
					addCandidate(in.Intern(topic), 10.0+math.Sqrt(float64(data.Count)))
				}
			}
		}
	}

	// 5. Medium Priority: Single words from the prompt.
	for _, word := range promptWords {
		lw := toLowerCaseExceptURLs(word)
		if r.HasSuccessors(lw) {
			addCandidate(in.Intern(lw), 15.0)
		}
	}

	// 6. Low Priority: Recent context fallback.
	for n := cfg.MaxNGram - 1; n >= 1; n-- {
		for i := 0; i <= len(recentWords)-n; i++ {
			key := toLowerCaseExceptURLs(strings.Join(recentWords[i:i+n], " "))
			if r.HasSuccessors(key) {
				addCandidate(in.Intern(key), float64(n))
			}
		}
	}

	if len(candidateMap) == 0 {
		// Absolute fallback: any real prefix beats a sentinel with no continuations.
		if prefix, ok := r.FirstPrefix(); ok {
			return prefix
		}
		return "<START>"
	}

	candidates := make([]candidate, 0, len(candidateMap))
	for key, weight := range candidateMap {
		candidates = append(candidates, candidate{key, weight})
	}

	// Weighted random selection.
	total := 0.0
	for _, c := range candidates {
		total += c.weight
	}
	if total > 0 {
		r_val := rand.Float64() * total
		for _, c := range candidates {
			r_val -= c.weight
			if r_val <= 0 {
				return in.Word(c.key)
			}
		}
		return in.Word(candidates[len(candidates)-1].key)
	}

	return in.Word(candidates[0].key)
}

// findJumpWord attempts to find a related word when generation hits a dead end.
//
// The concept-cluster pivot that used to sit between the two surviving tiers is gone
// for the reasons on findBestSeed: it had never fired, and its members are interner
// ids that cannot mean anything across processes. M8 owns the replacement.
//
// The interner parameter went with it. Nothing left here needs one.
func findJumpWord(r *storage.Reader, promptWords, currentWords, recognizedNames []string) string {
	// 1. Name-Centric Pivot: Prioritize finding a topic related to a recognized name.
	if len(recognizedNames) > 0 {
		// Use the most recently mentioned name as the primary context.
		name := toLowerCaseExceptURLs(recognizedNames[len(recognizedNames)-1])
		if topicAssoc, err := r.NameTopicsFor(name); err == nil {
			bestJump := ""
			closestDist := 0.5 // Target the middle of the sentence for a pivot
			for topic, data := range topicAssoc {
				if data.Count > 1 { // Require at least two occurrences to be a stable topic
					dist := math.Abs(data.MeanPosition() - 0.5)
					if dist < closestDist {
						closestDist = dist
						bestJump = topic
					}
				}
			}
			if bestJump != "" {
				log.Printf("[INFO] Generation stuck, jumping via name->topic: '%s'", bestJump)
				return bestJump
			}
		}
	}

	// 2. Topic-Centric Pivot: Fallback to finding a word related to the last topic.
	// Cap the context words to a reasonable limit before combining.
	const maxContext = 5
	var searchContext []string
	if len(currentWords) > maxContext {
		searchContext = currentWords[len(currentWords)-maxContext:]
	} else {
		searchContext = currentWords
	}
	searchContext = append(promptWords, searchContext...)

	// Create a set of words already in the sentence to avoid repetition.
	sentenceSet := make(map[string]struct{})
	for _, w := range currentWords {
		sentenceSet[toLowerCaseExceptURLs(w)] = struct{}{}
	}

	bestJump := ""
	bestScore := 0.0

	for i := len(searchContext) - 1; i >= 0; i-- {
		topic := toLowerCaseExceptURLs(searchContext[i])
		wordAssoc, err := r.TopicWordsFor(topic)
		if err != nil {
			log.Printf("[WARN] findJumpWord: topic-word lookup for %s failed: %v", topic, err)
			continue
		}

		for word, data := range wordAssoc {
			// Don't jump to the topic word itself or a word already used.
			if _, exists := sentenceSet[word]; exists || word == topic || data.Count <= 1 {
				continue
			}

			// Combine association strength (primary) with positional preference
			// (secondary).
			strengthScore := math.Sqrt(float64(data.Count))
			posScore := 1.0 - math.Abs(data.MeanPosition()-0.5) // value from 0.5 to 1.0

			score := strengthScore * posScore
			if score > bestScore {
				bestScore = score
				bestJump = word
			}
		}
	}

	if bestJump != "" {
		log.Printf("[INFO] Generation stuck, jumping via topic->word: '%s'", bestJump)
		return bestJump
	}

	return ""
}

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
			sentence = append(sentence, "...")
			return nil
		}

		// Initial generation attempt
		sentence, recognizedNames, _ = generateSentenceAttempt(r, promptWords, recentWords, prompt, isRoast)
		return nil
	})

	if err != nil {
		return "", err
	}

	// Clean final sentence
	final := strings.Join(sentence, " ")
	final = strings.ReplaceAll(final, "<end>", "") // Remove any <end> tokens
	final = cleanSentence(s, final)
	// Only apply edgy style if no recognized name seed was used
	isAboutName := len(recognizedNames) > 0
	final = applyEdgyStyle(final, isAboutName)

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

	// The interner is constructed inline because findBestSeed is now its only user: it
	// keys its candidate map by id. The scorer used to share this one, which is why it
	// was a variable. M7b moves seed selection into the engine and the interner goes
	// with it. Per-call is still the requirement, not merely the convention, for the
	// reason recorded on text.Interner: ids depend on insertion order, so one that
	// outlived a call would mean different words to different callers.
	seed := findBestSeed(r, text.NewInterner(), promptWords, recentWords, recognizedNames)
	if seed == "" || seed == "<START>" {
		if len(promptWords) > 0 {
			seed = promptWords[0]
		} else {
			seed = "<START>"
		}
	}

	words := tokenize(seed)
	sentence := append([]string{}, words...)

	// Length bounds, unchanged from M6b and deliberately so: M7b replaces all three
	// competing length mechanisms with one model, and changing them here would mean
	// tuning the engine against bounds that are about to move.
	minWords := 5 + rand.IntN(4)
	maxWords := 30 + rand.IntN(15)

	step := &markov.Step{
		Prefix:       append([]string{}, words...),
		Sentence:     sentence,
		Prompt:       originalPrompt,
		PromptSet:    wordSet(promptWords),
		RecentSet:    wordSet(recentWords),
		Used:         make(map[string]int, maxWords),
		Ngrams:       make(map[string]struct{}, maxWords*3),
		MinWords:     minWords,
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

	for range maxWords {
		if len(step.Prefix) == 0 {
			break
		}
		step.Position = float64(len(step.Sentence)) / float64(maxWords)

		next, err := g.Next(step)
		if err != nil {
			log.Printf("[WARN] generation step failed: %v", err)
			break
		}

		if next == markov.EndToken {
			// The floor is still enforced here rather than in the engine, which only
			// penalizes the sentinel. M7b makes this one length model.
			if len(step.Sentence) >= minWords {
				break
			}
			next = ""
		}

		if next == "" {
			// A dead end. Either the prefix has no continuation, or the
			// author-diversity gate refused everything that did, which on a young
			// corpus is the common case and is the gate working.
			jump := findJumpWord(r, promptWords, step.Sentence, recognizedNames)
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
var globalConvMemory ConversationMemory // Declare a global instance of ConversationMemory

// sendMessage, editMessage and deleteMessage exist because every Discord call
// below used to discard its error, so a send Discord refused (missing
// permission, rate limit, channel deleted mid-flight) was indistinguishable
// from one that worked: the bot simply appeared to ignore people at random,
// with nothing in the log to say why.
//
// M10 replaces these with internal/discordguard, which owns the same logging
// plus mention suppression and the outbound safety gate at a single chokepoint.
// They are deliberately thin so that migration is a mechanical rename.
func sendMessage(s *discordgo.Session, channelID, content string) {
	if _, err := s.ChannelMessageSend(channelID, content); err != nil {
		log.Printf("[DISCORD] send to channel %s failed: %v", channelID, err)
	}
}

func editMessage(s *discordgo.Session, channelID, messageID, content string) {
	if _, err := s.ChannelMessageEdit(channelID, messageID, content); err != nil {
		log.Printf("[DISCORD] edit of message %s failed: %v", messageID, err)
	}
}

// deleteMessage logs at a lower urgency than the others: failing to delete is
// routinely benign (someone already removed the message, or the bot lacks
// Manage Messages in that channel) and is not worth alarming about.
func deleteMessage(s *discordgo.Session, channelID, messageID string) {
	if err := s.ChannelMessageDelete(channelID, messageID); err != nil {
		log.Printf("[DISCORD] delete of message %s failed (often benign): %v", messageID, err)
	}
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

func handleMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	start := time.Now()
	log.Printf("[MSG] from %s: %q", m.Author.Username, m.Content)

	// Reject early, so the bot neither replies to nor reacts to a message it will
	// not learn from either. This is a convenience, not the protection: the
	// protection is CheckLearn inside learnMessage, which every path reaches. If
	// this block were deleted the corpus would still be safe; the bot would just
	// waste work replying to spam.
	if v := gate.CheckLearn(m.Content); !v.Allowed {
		log.Printf("[FILTER] Message from %s dropped, no processing will occur: %s", m.Author.Username, v.Reason)
		return
	}

	// There used to be a `m.Content = filterSlurs(m.Content)` here, and removing it
	// is load-bearing rather than tidying.
	//
	// It replaced matches in place, which had two effects. The message was learned
	// anyway, with its structure intact and a harmless token sitting in the slur's
	// grammatical position, so the bot had been taught the sentence and merely said
	// "ninja" where the slur went (SPEC.md section 4, A5). And now that CheckLearn
	// exists, laundering here would be strictly worse than useless: the gate would
	// receive the already-cleaned text, find nothing, and allow it. The launder
	// would defeat the gate.
	//
	// The verdict on the learning path is to drop the whole message, and it is made
	// above and again inside learnMessage. Replacement remains available in
	// internal/filter for display paths that want it; nothing on this path does.

	// Add message to conversation memory
	globalConvMemory.AddMessage(m.Content)

	// --- Word Game Event Logic ---
	if cfg.EnableWordGames && wordGamesAvailable {
		wordGameMutex.Lock()
		if game, gameExists := activeWordGames[m.ChannelID]; gameExists {
			// A game is active, check for a winning guess
			if game.CheckGuess(m.Content) {
				solveTime := time.Since(game.StartTime)
				winnerUsername := m.Author.Username
				if m.Member != nil && m.Member.Nick != "" {
					winnerUsername = m.Member.Nick
				}

				// Announce winner and set it for delayed deletion
				winMessage, err := s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("🎉 **%s** guessed the word **%s** in %.2f seconds!", winnerUsername, game.OriginalWord, solveTime.Seconds()))
				if err == nil {
					go func(channelID, messageID string) {
						time.Sleep(30 * time.Second)
						deleteMessage(s, channelID, messageID)
					}(m.ChannelID, winMessage.ID)
				}

				// Update and save leaderboard
				leaderboard.AddWin(m.Author.ID, winnerUsername)
				_ = store.Update(func(w *storage.Writer) error {
					return saveLeaderboard(w, leaderboard)
				})

				// Clean up messages
				_ = s.ChannelMessageDelete(m.ChannelID, game.MessageID) // Delete original puzzle
				_ = s.ChannelMessageDelete(m.ChannelID, m.ID)           // Delete the winning message

				// End the game
				delete(activeWordGames, m.ChannelID)
			}
		} else {
			// No game is active, check if we should start one based on activity
			activityMutex.Lock()
			now := time.Now()
			// Prune old timestamps
			newTimestamps := []time.Time{}
			for _, ts := range channelActivity[m.ChannelID] {
				if now.Sub(ts) < 5*time.Minute {
					newTimestamps = append(newTimestamps, ts)
				}
			}
			newTimestamps = append(newTimestamps, now)
			channelActivity[m.ChannelID] = newTimestamps

			// Check for trigger condition
			// Needs at least 5 messages in the last 5 minutes, then a 2.5% chance per message
			if len(newTimestamps) >= 5 && rand.Float64() < 0.025 {
				game, err := wordgames.NewScrambleGame()
				if err != nil {
					log.Printf("[WORDGAME] Failed to create new game: %v", err)
				} else {
					msg, err := s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("✨ **Word Scramble!** ✨\n\nUnscramble this word: **%s**", game.ScrambledWord))
					if err == nil {
						game.MessageID = msg.ID
						activeWordGames[m.ChannelID] = game
						channelActivity[m.ChannelID] = []time.Time{} // Reset activity after starting
						log.Printf("[WORDGAME] Started a new game in channel %s.", m.ChannelID)
						go func(channelID, messageID, originalWord string) {
							time.Sleep(60 * time.Second)
							wordGameMutex.Lock()
							defer wordGameMutex.Unlock()
							if g, exists := activeWordGames[channelID]; exists && g.MessageID == messageID {
								timeoutMsg, err := s.ChannelMessageSend(channelID, fmt.Sprintf("Time's up! The word was **%s**.", originalWord))
								if err == nil {
									go func(cid, mid string) {
										time.Sleep(30 * time.Second)
										deleteMessage(s, cid, mid)
									}(channelID, timeoutMsg.ID)
								}
								deleteMessage(s, channelID, messageID)
								delete(activeWordGames, channelID)
								log.Printf("[WORDGAME] Game timed out in channel %s.", channelID)
							}
						}(m.ChannelID, msg.ID, game.OriginalWord)
					}
				}
			}
			activityMutex.Unlock()
		}
		wordGameMutex.Unlock()
	}

	// --- Leaderboard Command ---
	if strings.ToLower(m.Content) == "!leaderboard" {
		var userScores map[string]int
		err := store.View(func(r *storage.Reader) error {
			var loadErr error
			userScores, loadErr = loadAllUserStats(r)
			return loadErr
		})

		if err != nil {
			log.Printf("[LEADERBOARD] Error loading user stats: %v", err)
			sendMessage(s, m.ChannelID, "Could not generate chat leaderboard.")
			return
		}

		nameScores := make(map[string]int)
		for userID, score := range userScores {
			// Try to get member to use nickname
			member, err := s.GuildMember(m.GuildID, userID)
			var displayName string
			if err == nil {
				if member.Nick != "" {
					displayName = member.Nick
				} else {
					displayName = member.User.Username
				}
			} else {
				// Fallback to fetching user if member not found (e.g., user left server)
				user, userErr := s.User(userID)
				if userErr != nil {
					log.Printf("[LEADERBOARD] Could not find user/member for ID %s: %v", userID, userErr)
					displayName = userID // fallback to ID
				} else {
					displayName = user.Username
				}
			}
			nameScores[displayName] = score
		}

		chatLeaderboard := wordgames.FormatChatLeaderboard(nameScores)
		sendMessage(s, m.ChannelID, leaderboard.Format()+"\n\n"+chatLeaderboard)
	}
	// --- Word Game Admin Command (for testing) ---
	if cfg.EnableWordGames && wordGamesAvailable && strings.ToLower(m.Content) == "!wordgame" {
		// The only authorization check in the codebase, and until M2 it was a
		// user ID hardcoded in the source. It now fails CLOSED: an unset
		// PEREGRINE_BOOTSTRAP_ADMIN_USER_ID refuses this command for
		// everyone, never allows it for everyone. Getting that direction
		// wrong on an empty string is how a missing variable turns an
		// operator-only command into a public one.
		if cfg.AdminUserID != "" && m.Author.ID == cfg.AdminUserID {
			wordGameMutex.Lock()
			if _, gameExists := activeWordGames[m.ChannelID]; gameExists {
				sendMessage(s, m.ChannelID, "A word game is already in progress in this channel!")
			} else {
				game, err := wordgames.NewScrambleGame()
				if err != nil {
					log.Printf("[WORDGAME] Failed to create new game: %v", err)
				} else {
					msg, err := s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("✨ **Word Scramble!** ✨\n\nUnscramble this word: **%s**", game.ScrambledWord))
					if err == nil {
						game.MessageID = msg.ID
						activeWordGames[m.ChannelID] = game
						log.Printf("[WORDGAME] Started a new game in channel %s.", m.ChannelID)
						go func(channelID, messageID, originalWord string) {
							time.Sleep(60 * time.Second)
							wordGameMutex.Lock()
							defer wordGameMutex.Unlock()
							if g, exists := activeWordGames[channelID]; exists && g.MessageID == messageID {
								timeoutMsg, err := s.ChannelMessageSend(channelID, fmt.Sprintf("Time's up! The word was **%s**.", originalWord))
								if err == nil {
									go func(cid, mid string) {
										time.Sleep(30 * time.Second)
										deleteMessage(s, cid, mid)
									}(channelID, timeoutMsg.ID)
								}
								deleteMessage(s, channelID, messageID)
								delete(activeWordGames, channelID)
								log.Printf("[WORDGAME] Game timed out in channel %s.", channelID)
							}
						}(m.ChannelID, msg.ID, game.OriginalWord)
					}
				}
			}
			wordGameMutex.Unlock()
		}
	}

	// --- Bird Aggro Check ---
	birdAggroMutex.Lock()
	isTarget := m.Author.ID == birdAggroTargetID
	isExpired := time.Now().After(birdAggroEndTime)
	birdAggroMutex.Unlock() // Unlock immediately after reading shared state.

	if isTarget && !isExpired {
		// Aggro is active. Perform API calls outside the lock.
		err := s.MessageReactionAdd(m.ChannelID, m.ID, cfg.AggroEmoji)
		if err != nil {
			log.Printf("[AGGRO] Failed to add reaction: %v", err)
		}
	} else if isTarget && isExpired {
		// Aggro has expired. Update shared state and persist.
		log.Printf("[AGGRO] Aggro expired for %s, clearing...", m.Author.Username)
		birdAggroMutex.Lock()
		birdAggroTargetID = ""
		birdAggroEndTime = time.Time{}
		birdAggroMutex.Unlock()

		// Perform DB update and reaction removal outside the lock.
		_ = store.Update(func(w *storage.Writer) error {
			return saveAggroState(w, AggroState{})
		})
		_ = s.MessageReactionRemove(m.ChannelID, m.ID, cfg.AggroEmoji, s.State.User.ID)
	}

	// --- Determine flags ---
	flags := make(map[string]bool)

	// Mentioned the bot
	if strings.Contains(m.Content, "<@"+botID+">") || strings.Contains(m.Content, "<@!"+botID+">") {
		flags["MENTIONED"] = true
	}

	// Reply to bot
	if m.MessageReference != nil && m.MessageReference.MessageID != "" && m.MessageReference.ChannelID != "" {
		if refMsg, err := s.ChannelMessage(m.MessageReference.ChannelID, m.MessageReference.MessageID); err == nil && refMsg.Author.ID == botID {
			flags["REPLY_TO_BOT"] = true
		}
	}

	// Check attachments for voice/audio
	flags["VOICE"] = false
	for _, att := range m.Attachments {
		// Discord voice messages are always .ogg and named "voice-message.ogg"
		if strings.ToLower(att.Filename) == "voice-message.ogg" && strings.ToLower(filepath.Ext(att.Filename)) == ".ogg" {
			flags["VOICE"] = true
			break
		}
	}

	// Check if the message contains text
	flags["TEXT"] = len(strings.TrimSpace(m.Content)) > 0

	// Check for self-mention keywords in text messages
	if flags["TEXT"] && cfg.SelfMention.MatchString(m.Content) {
		flags["SELF_MENTION_KEYWORD"] = true
	}

	// --- Handle text messages ---
	if flags["TEXT"] && (flags["MENTIONED"] || flags["REPLY_TO_BOT"] || flags["SELF_MENTION_KEYWORD"]) {
		replyStart := time.Now()

		promptForGeneration := m.Content
		isRoast := false

		// If a self-mention keyword is detected and the bot wasn't directly addressed,
		// override the prompt to encourage a self-referential roast.
		if flags["SELF_MENTION_KEYWORD"] && !flags["MENTIONED"] && !flags["REPLY_TO_BOT"] {
			promptForGeneration = "<START> peregrine"
			isRoast = true // Always roast when self-mentioning without direct address
			log.Printf("[INFO] Activating 'roast' mode due to self-mention keyword. Using prompt: %q", promptForGeneration)
		} else if flags["MENTIONED"] || flags["REPLY_TO_BOT"] {
			// If directly mentioned or replied to, there's still a chance for a roast.
			roastChance := 0.10 // Base 10% chance for a roast
			// (Optional: Add logic here to increase roastChance if the message sentiment is negative)
			if rand.Float64() < roastChance {
				isRoast = true
				log.Printf("[INFO] Activating 'roast' mode for direct interaction.")
			}
		}

		reply, err := generateSentenceWithContext(s, promptForGeneration, isRoast, &globalConvMemory)
		if err != nil {
			log.Printf("[ERR] reply generation failed: %v", err)
		} else if reply != "" {
			// Always reply directly when the bot is triggered by a message.
			if _, err := s.ChannelMessageSendReply(m.ChannelID, reply, &discordgo.MessageReference{MessageID: m.ID, ChannelID: m.ChannelID}); err != nil {
				log.Printf("[ERR] sending reply failed: %v", err)
			} else {
				log.Printf("[RESP] replied to %s in %s: %q", m.Author.Username, time.Since(replyStart), reply)

				// --- Reinforcement Learning ---
				// The bot learns its own successful, contextually-generated replies.
				// This creates a feedback loop, reinforcing the associations that led to a good response.
				go func(replyContent string) {
					botUser, err := s.User("@me")
					if err != nil {
						log.Printf("[WARN] Could not get bot user for self-learning: %v", err)
						return
					}
					botAsMention := MentionedUser{
						Name:     botUser.Username,
						UserID:   botUser.ID,
						Username: botUser.Username,
					}
					// Learn the bot's own reply, associating it with the users mentioned
					// in the original prompt. This teaches the bot what a good,
					// on-topic response looks like.
					//
					// The name extraction is hoisted OUT of the write transaction, and
					// that is a fix rather than a tidy-up: it used to run inside the
					// db.Update, which meant a read transaction and a series of REST
					// calls nested inside a write transaction while bbolt's single
					// writer lock was held.
					//
					// The bot's own user ID reaching learnMessage is deliberate and
					// safe: learnMessage compares it to botID and passes an empty
					// author to LearnNgram, so self-learning contributes nothing to
					// author diversity (SPEC.md section 4, A6).
					mentionedInPrompt := extractNamesFromMessage(s, m, m.GuildID)
					_ = store.Update(func(w *storage.Writer) error {
						if err := learnMessage(w, replyContent, m.ID, botID, botAsMention, mentionedInPrompt); err != nil {
							return fmt.Errorf("self-learning failed: %w", err)
						}
						return nil
					})
				}(reply)
			}
		}
	}

	// --- Extract and store names ---
	if flags["TEXT"] {
		mentionedUsers := extractNamesFromMessage(s, m, m.GuildID)
		if len(mentionedUsers) > 0 {
			_ = store.Update(func(w *storage.Writer) error {
				for _, user := range mentionedUsers {
					_, err := learnOrUpdateName(w, user.Name, user.UserID, user.Username)
					if err != nil {
						log.Printf("[WARN] Failed to learn name '%s' during extraction: %v", user.Name, err)
					}
				}
				return nil
			})
		}
	}

	// --- Learn the message ---
	if flags["TEXT"] {
		mentionedUsers := extractNamesFromMessage(s, m, m.GuildID)

		// Create a representation of the author to learn their own message content.
		authorAsMention := MentionedUser{
			Name:     m.Author.Username,
			UserID:   m.Author.ID,
			Username: m.Author.Username,
		}
		if m.Member != nil && m.Member.Nick != "" {
			// Prefer nickname for association if available.
			authorAsMention.Name = m.Member.Nick
		}

		// Avoid duplicating the author if they were already processed (e.g., mentioned themselves).
		isAuthorProcessed := false
		for _, u := range mentionedUsers {
			if u.UserID == m.Author.ID {
				isAuthorProcessed = true
				break
			}
		}
		if !isAuthorProcessed {
			mentionedUsers = append(mentionedUsers, authorAsMention)
		}

		// Learn the message in a new transaction to avoid conflicts with ongoing processing
		_ = store.Update(func(w *storage.Writer) error {
			if err := learnMessage(w, m.Content, m.ID, botID, authorAsMention, mentionedUsers); err != nil {
				return fmt.Errorf("learning failed: %w", err)
			}
			return nil
		})
	}

	// --- Handle voice attachments ---
	if cfg.EnableTranscription && flags["VOICE"] {
		for _, att := range m.Attachments {
			ext := strings.ToLower(filepath.Ext(att.Filename))
			if ext == ".ogg" || ext == ".mp3" || ext == ".wav" {
				// Send placeholder immediately
				placeholder, err := s.ChannelMessageSendReply(
					m.ChannelID,
					"🔊 transcription in progress...",
					&discordgo.MessageReference{MessageID: m.ID, ChannelID: m.ChannelID},
				)
				if err != nil {
					log.Printf("[VOICE] Failed to send placeholder message: %v", err)
					continue
				}

				// Add a job to the transcription queue
				transcriptionQueue <- TranscriptionJob{
					URL:           att.URL,
					AuthorID:      m.Author.ID,
					MsgID:         m.ID,
					ChannelID:     m.ChannelID,
					PlaceholderID: placeholder.ID,
					Author:        m.Author,
					Member:        m.Member,
				}
			}
		}
	}

	// --- Capture image and Tenor URLs ---
	// Gated on the same switch as reposting, not just on the repost itself.
	// The cache exists only to feed reposts, so with reposting off this would
	// be storing other people's media URLs in the operator's database for no
	// consumer at all: liability with no upside.
	if cfg.EnableImageRepost {
		captureImageURLs(s, m)
	}

	// --- Spontaneous image repost ---
	// Applies to all messages, but the ambient rate is deliberately the
	// higher of the two: when the bot is already answering a mention it is
	// contributing to the channel anyway, so an unrelated image on top of the
	// reply is noise rather than chaos.
	repostChance := cfg.ImageRepostDirect
	if !flags["MENTIONED"] && !flags["REPLY_TO_BOT"] {
		repostChance = cfg.ImageRepostChance
	}

	if cfg.EnableImageRepost && rand.Float64() < repostChance {
		imageURLMutex.Lock()
		if len(recentImageURLs) > 0 {
			urlToPost := recentImageURLs[rand.IntN(len(recentImageURLs))]
			imageURLMutex.Unlock() // Unlock before sending to avoid holding lock during network call

			// Logged before the send, and worded as an attempt, because
			// sendMessage returns nothing: it logs its own failure. Claiming
			// success after a void call would report a repost that Discord
			// refused as one that happened.
			log.Printf("[REPOST] Reposting image: %s", urlToPost)
			sendMessage(s, m.ChannelID, urlToPost)
		} else {
			imageURLMutex.Unlock()
		}
	}

	log.Printf("[OK] handled msg from %s in %s | flags: %+v", m.Author.Username, time.Since(start), flags)
}

// captureImageURLs caches one image or Tenor URL from a message so a later
// repost has something to post. Extracted from messageCreate only so the
// EnableImageRepost gate could wrap it as one statement; the body is unchanged.
//
// The s.Channel call here is a REST request on every message that carries a
// candidate URL, purely to read the NSFW flag. It becomes a free s.State.Channel
// lookup in M10 once IntentsGuilds is requested (SPEC.md section 8, finding 7).
func captureImageURLs(s *discordgo.Session, m *discordgo.MessageCreate) {
	ch, err := s.Channel(m.ChannelID)
	if err != nil {
		log.Printf("[WARN] Could not fetch channel info to check for NSFW status: %v", err)
	} else if !ch.NSFW && !strings.Contains(strings.ToLower(ch.Name), "nsfw") {
		imageURLMutex.Lock()

		var candidateURLs []string

		// 1. Check attachments first.
		for _, att := range m.Attachments {
			if strings.HasPrefix(att.ContentType, "image/") && discordCDNRegex.MatchString(att.URL) {
				candidateURLs = append(candidateURLs, att.URL)
			}
		}

		// 2. Scan message content for any URLs.
		contentWords := tokenize(m.Content)
		for _, word := range contentWords {
			if discordCDNRegex.MatchString(word) || tenorRegex.MatchString(word) {
				candidateURLs = append(candidateURLs, word)
			}
		}

		var urlToCache string
		if len(candidateURLs) > 0 {
			// Randomly select one URL from the candidates to cache
			urlToCache = candidateURLs[rand.IntN(len(candidateURLs))]
		}

		// 3. If a URL was found (either from attachment or content), save it.
		if urlToCache != "" {
			var newUrlList []string
			// Persist and then reload the cache to ensure consistency. The reload
			// happens through the Writer, which embeds Reader, so it sees its own
			// write without opening a second transaction: that is the reason the
			// Writer embeds the Reader rather than being a separate type.
			//
			// Trimming is part of AddImageURL now, and evicts the OLDEST entry by
			// timestamp. The old trim deleted the lexicographically first URL, which
			// has nothing to do with age, so which cached image got dropped was a
			// function of how the URL happened to be spelled.
			err := store.Update(func(w *storage.Writer) error {
				if err := w.AddImageURL(urlToCache, time.Now(), cfg.ImageCacheSize); err != nil {
					return fmt.Errorf("failed to save image URL: %w", err)
				}
				var err error
				newUrlList, err = w.ImageURLs()
				return err
			})

			if err != nil {
				log.Printf("[WARN] DB operation for image cache failed: %v", err)
			} else {
				// Update in-memory cache ONLY if DB operation was successful
				recentImageURLs = newUrlList
				log.Printf("[IMG] Captured URL: %s, cache size: %d", urlToCache, len(recentImageURLs))
			}
		}
		imageURLMutex.Unlock()
	} else if ch != nil {
		log.Printf("[INFO] Skipping image cache for NSFW-flagged or named channel #%s", ch.Name)
	}
}

// -----------------------------------------------------------------------------
//  Bird Aggro
// -----------------------------------------------------------------------------

// findRandomActiveUser finds a random user who has posted in the most active channel recently.
func findRandomActiveUser(ctx context.Context, s *discordgo.Session) string {
	guilds, err := s.UserGuilds(100, "", "", false)
	if err != nil || len(guilds) == 0 {
		log.Println("[AGGRO] No guilds available to find active user:", err)
		return ""
	}

	var activeUsers []string
	userSet := make(map[string]struct{})

	// Find all active users across all guilds in the last 6 hours
	activityCutoff := time.Now().Add(-6 * time.Hour)
	for _, gptr := range guilds {
		if gptr == nil {
			continue
		}
		channels := getActiveChannels(ctx, s, gptr.ID, activityCutoff)
		for _, chInfo := range channels {
			batch, err := s.ChannelMessages(chInfo.Channel.ID, 100, "", "", "")
			if err != nil {
				continue
			}
			for _, msg := range batch {
				if msg.Author.Bot {
					continue
				}
				if msg.Timestamp.After(activityCutoff) {
					if _, exists := userSet[msg.Author.ID]; !exists {
						userSet[msg.Author.ID] = struct{}{}
						activeUsers = append(activeUsers, msg.Author.ID)
					}
				}
			}
		}
	}

	if len(activeUsers) == 0 {
		log.Println("[AGGRO] No active users found to select a target.")
		return ""
	}

	// Select a random user from the pool of active users
	return activeUsers[rand.IntN(len(activeUsers))]
}

// -----------------------------------------------------------------------------
//  Autonomous posting
// -----------------------------------------------------------------------------

// autonomousPost picks an active channel and posts a generated message occasionally.
func autonomousPost(ctx context.Context, dg *discordgo.Session) {
	start := time.Now()
	log.Println("[AUTONOMOUS] Starting autonomous post cycle...")

	// Fetch guilds
	guilds, err := dg.UserGuilds(100, "", "", false)
	if err != nil || len(guilds) == 0 {
		log.Println("[AUTONOMOUS] No guilds available or error:", err)
		return
	}

	var bestChannel *discordgo.Channel
	maxScore := 0.0

	for _, gptr := range guilds {
		if gptr == nil {
			continue
		}
		channels := getActiveChannels(ctx, dg, gptr.ID, time.Now().Add(-cfg.IngestLookback))
		if len(channels) == 0 {
			log.Printf("[AUTONOMOUS] Guild %s has no active channels", gptr.Name)
			continue
		}
		for _, chInfo := range channels {
			score := float64(chInfo.MessageCount)
			if strings.Contains(strings.ToLower(chInfo.Channel.Name), "general") {
				score *= 1.5 // Apply a bonus to general channels
			}

			log.Printf("[AUTONOMOUS] Channel #%s has %d recent messages, score: %.2f", chInfo.Channel.Name, chInfo.MessageCount, score)
			if score > maxScore {
				maxScore = score
				bestChannel = chInfo.Channel
			}
		}
	}

	if bestChannel == nil {
		log.Println("[AUTONOMOUS] No active channel found, skipping post")
		return
	}

	// Chance to skip for "natural pacing" (applies to ALL autonomous posts)
	skipChance := cfg.AutonomousSkipChance
	if rand.Float64() < skipChance {
		log.Printf("[AUTONOMOUS] Skipping this cycle for natural pacing (chance %.2f)", skipChance)
		return
	}

	// Only post word games as autonomous posts for now
	if !cfg.EnableWordGames || !wordGamesAvailable {
		return
	}

	// Word game pacing. The outer `if EnableWordGames` that used to wrap this was
	// unreachable-false: the guard above already returned when word games are
	// off. Activity mode intentionally has no branch here, because for an
	// autonomous post the global skip chance above already provides the pacing;
	// only interval mode needs its own clock.
	if cfg.WordGameMode == config.WordGameModeInterval {
		if time.Since(lastWordGameTime) < cfg.WordGameInterval {
			log.Printf("[AUTONOMOUS] Not time for a word game yet (next in %v)", cfg.WordGameInterval-time.Since(lastWordGameTime))
			return
		}
		lastWordGameTime = time.Now()
	}

	// Only post word games in allowed channels
	if !stringContains(cfg.AutonomousPostChannels, bestChannel.ID) {
		log.Printf("[AUTONOMOUS] Channel %s not in allowed list for autonomous posting, skipping word game", bestChannel.Name)
		return
	}

	// Generate message (will be a word game in this current implementation)
	// For autonomous posts, we'll create a temporary memory instance based on the most active channel
	tempMemory := &ConversationMemory{}
	tempMemory.AddMessage(bestChannel.LastMessageID) // Seed with the last message ID
	msg, err := generateSentenceWithContext(dg, "autonomous thought", false, tempMemory)
	if err != nil {
		log.Println("[AUTONOMOUS] Error generating message:", err)
		return
	}
	if msg == "" {
		log.Println("[AUTONOMOUS] Generated message is empty, skipping")
		return
	}

	// Send message
	sentMsg, err := dg.ChannelMessageSend(bestChannel.ID, msg)
	if err != nil {
		log.Println("[AUTONOMOUS] Failed to send message:", err)
	} else {
		log.Printf("[AUTONOMOUS] Sent message in #%s: %s (ID: %s)", bestChannel.Name, msg, sentMsg.ID)
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
			finalMsg := fmt.Sprintf("```\n%s\n```", transcript)
			if _, err := s.ChannelMessageEdit(job.ChannelID, job.PlaceholderID, finalMsg); err != nil {
				log.Printf("[VOICE] Failed to edit placeholder for message %s: %v", job.MsgID, err)
			}

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

// stringContains is a helper function to check if a string is in a slice of strings.
func stringContains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

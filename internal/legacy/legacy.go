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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/6586x57890143/peregrine/clustering"
	"github.com/6586x57890143/peregrine/internal/config"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/voicenotes"
	"github.com/6586x57890143/peregrine/wordgames"
	"github.com/beevik/ntp"
	"github.com/bwmarrin/discordgo"
	"go.etcd.io/bbolt"
)

// Bucket names. These are the only members of the old CONFIG block that are
// genuinely constant: they are the on-disk schema, not a tuning decision, and
// changing one at runtime would just mean reading from a bucket nothing wrote.
// M6 moves them into internal/storage unexported, which is also what stops
// clustering owning storage-layer names as it does here.
const (
	MarkovBucket         = "markov"
	TopicBucket          = "topics"
	HistoryBucket        = "history"
	NameBucket           = "names"
	NameTopicBucket      = clustering.NameTopicBucket
	TopicWordBucket      = "TopicWordBucket"
	TopicClusterBucket   = clustering.TopicClusterBucket
	ImageCacheBucket     = "ImageCacheBucket"
	ConfigBucket         = "ConfigBucket"
	ConceptClusterBucket = clustering.ConceptClusterBucket
	StatsBucket          = "stats"
	LeaderboardBucket    = "LeaderboardBucket"
)

// cfg is the loaded configuration, set once by Run before anything reads it.
//
// A package-level variable rather than a parameter threaded through 40 functions,
// because those functions are being deleted milestone by milestone and rewriting
// each signature twice is churn for no reader's benefit. As each subsystem moves
// out of this package it takes its own config fields with it as struct fields on
// a real type. Nothing here writes to it after Run sets it.
//
// Creativity is deliberately NOT here and gets no environment variable. It is
// applied as an exponent of 1/(Creativity+0.01), so at its 0.75 default the
// exponent is 1.316, which sharpens the distribution: the knob's arithmetic
// inverts its own name and cannot reach the interesting half of its own range.
// Exposing that to an operator would invite tuning something broken. M7 replaces
// it with PEREGRINE_TEMPERATURE once the scoring is normalized and the dial
// actually moves. ContextWindow and CoherencyBalance are gone entirely: they were
// declared and never read.
var cfg *config.Config

// Creativity is the old inverse-temperature exponent, left as a constant because
// it is not fit to be configuration. See the comment on cfg.
const Creativity = 0.75

// WordPosData tracks word statistics for positional weighting and name/topic associations
type WordPosData struct {
	Word       string             `json:"word,omitempty"`       // the actual word/token
	Position   int                `json:"position,omitempty"`   // optional absolute position
	IsName     bool               `json:"is_name,omitempty"`    // true if recognized as a name
	Associated []string           `json:"associated,omitempty"` // associated names/topics
	Count      int                `json:"count"`                // how many times this word occurred
	PosSum     float64            `json:"pos_sum"`              // sum of relative positions for averaging
	TopicBias  map[string]float64 `json:"topic_bias,omitempty"` //	 topic bias scores
	Sentiment  float64            `json:"sentiment,omitempty"`  // 	sentiment score
}

// TopicAssociationData tracks topic statistics for positional weighting and name associations
type TopicAssociationData struct {
	Count  int     `json:"count"`   // how many times this topic occurred with the name
	PosSum float64 `json:"pos_sum"` // sum of relative positions for averaging
}

type TopicClusterData = clustering.TopicClusterData

// NameData stores information about a recognized name, including its canonical form.
type NameData struct {
	Count         int    `json:"count"`
	DiscordUser   string `json:"discord_user,omitempty"`
	CanonicalName string `json:"canonical_name,omitempty"` // The primary name this alias points to (e.g., the username).
}

// ActiveChannelInfo holds a channel and its recent message count.
type ActiveChannelInfo struct {
	Channel      *discordgo.Channel
	MessageCount int
}

type ConceptCluster = clustering.ConceptCluster

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

// WeeklyStat holds the message count for a user for the current week.
type WeeklyStat struct {
	Count         int       `json:"count"`
	LastTimestamp time.Time `json:"last_timestamp"`
}

var (
	vocab    = make(map[string]int)
	revVocab = make([]string, 0)
)

func addToVocab(s string) int {
	if idx, ok := vocab[s]; ok {
		return idx
	}
	idx := len(revVocab)
	vocab[s] = idx
	revVocab = append(revVocab, s)
	return idx
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
var db *bbolt.DB
var dg *discordgo.Session
var dispatcher *core.Dispatcher
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

// saveAggroState persists the bird aggro state to the ConfigBucket.
func saveAggroState(tx *bbolt.Tx, state AggroState) error {
	b := tx.Bucket([]byte(ConfigBucket))
	if b == nil {
		return fmt.Errorf("ConfigBucket not found")
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return b.Put([]byte("aggroState"), encoded)
}

// loadAggroState loads the bird aggro state from the ConfigBucket.
func loadAggroState(tx *bbolt.Tx) (AggroState, error) {
	b := tx.Bucket([]byte(ConfigBucket))
	if b == nil {
		return AggroState{}, fmt.Errorf("ConfigBucket not found")
	}
	v := b.Get([]byte("aggroState"))
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

// EnsureBuckets creates the buckets this package expects, if they do not already
// exist.
//
// Exported and called from cmd/bot rather than from Init because both the bot and
// the -clean-db maintenance mode need the corpus to be well formed, and only one
// of them runs a Service. M6 moves this inside internal/storage.Open, where it
// belongs, along with the schema_version key that makes "these buckets exist" and
// "these buckets are the layout this binary understands" different questions.
func EnsureBuckets(store *bbolt.DB) error {
	if err := store.Update(func(tx *bbolt.Tx) error {
		for _, b := range []string{
			MarkovBucket, TopicBucket, HistoryBucket, NameBucket, NameTopicBucket,
			TopicWordBucket, TopicClusterBucket, ImageCacheBucket, ConfigBucket,
			ConceptClusterBucket, StatsBucket, LeaderboardBucket,
		} {
			if _, err := tx.CreateBucketIfNotExists([]byte(b)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("create buckets: %w", err)
	}
	return nil
}

func (s *Service) Name() string { return "legacy" }

// Init loads persistent state and registers the gateway handler. No gateway or
// REST calls belong here: the session is not connected yet.
func (s *Service) Init(deps core.Deps) error {
	s.deps = deps
	cfg = deps.Config
	db = deps.Store
	dispatcher = deps.Dispatcher
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
	_ = db.View(func(tx *bbolt.Tx) error {
		var err error
		state, err := loadAggroState(tx)
		if err != nil {
			log.Printf("[WARN] Failed to load aggro state: %v", err)
			return err
		}
		leaderboard, err = loadLeaderboard(tx)
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
				_ = db.Update(func(tx *bbolt.Tx) error {
					return saveAggroState(tx, AggroState{})
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
	if cfg.EnableClustering {
		// Skippable because it walks the whole corpus inside a write transaction
		// and bbolt has one writer process-wide, making it the pass most worth
		// turning off while diagnosing stalled ingestion.
		loops = append(loops, core.Loop{
			Name:      "clustering",
			Every:     cfg.ClusteringTick,
			Immediate: true,
			Fn:        func(context.Context) { performClustering() },
		})
	}
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
		if err := db.Update(func(tx *bbolt.Tx) error {
			return saveLeaderboard(tx, leaderboard)
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
	if err := db.Update(func(tx *bbolt.Tx) error {
		return saveAggroState(tx, AggroState{TargetID: birdAggroTargetID, EndTime: birdAggroEndTime})
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
	if err := db.Update(func(tx *bbolt.Tx) error {
		return saveLeaderboard(tx, leaderboard)
	}); err != nil {
		log.Printf("[ERR] Failed to persist leaderboard reset: %v", err)
	}
}

// learnOrUpdateName finds the canonical name for a user and updates aliases.
func learnOrUpdateName(tx *bbolt.Tx, name, discordUserID, username string) (string, error) {
	nameB := tx.Bucket([]byte(NameBucket))
	if nameB == nil {
		return "", fmt.Errorf("name bucket not found")
	}

	canonicalName := toLowerCaseExceptURLs(username)
	nameKey := toLowerCaseExceptURLs(name)

	// Update the canonical name entry first.
	var canonicalData NameData
	if v := nameB.Get([]byte(canonicalName)); v != nil {
		_ = json.Unmarshal(v, &canonicalData)
	}
	canonicalData.Count++
	canonicalData.DiscordUser = discordUserID
	out, err := json.Marshal(canonicalData)
	if err != nil {
		return "", err
	}
	if err := nameB.Put([]byte(canonicalName), out); err != nil {
		return "", err
	}

	// If the current name is an alias (nickname), create/update its entry to point to the canonical name.
	if nameKey != canonicalName {
		var aliasData NameData
		if v := nameB.Get([]byte(nameKey)); v != nil {
			_ = json.Unmarshal(v, &aliasData)
		}
		aliasData.DiscordUser = discordUserID
		aliasData.CanonicalName = canonicalName // Link to the primary username.
		out, err := json.Marshal(aliasData)
		if err != nil {
			return "", err
		}
		if err := nameB.Put([]byte(nameKey), out); err != nil {
			return "", err
		}
	}

	return canonicalName, nil
}

// isRecognizedName checks if a token is a known name or alias and returns its canonical form.
func isRecognizedName(token string) (canonicalName string, exists bool) {
	if token == "" {
		return "", false
	}
	lower := toLowerCaseExceptURLs(token)
	_ = db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(NameBucket))
		if b != nil {
			if v := b.Get([]byte(lower)); v != nil {
				var data NameData
				if json.Unmarshal(v, &data) == nil {
					exists = true
					if data.CanonicalName != "" {
						canonicalName = data.CanonicalName // It's an alias, return the canonical name.
					} else {
						canonicalName = lower // It's a canonical name itself.
					}
				}
			}
		}
		return nil
	})
	return
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
func getMapFromBucket(b *bbolt.Bucket, key string) (map[string]int, error) {
	if b == nil {
		return nil, fmt.Errorf("nil bucket")
	}
	v := b.Get([]byte(key))
	if v == nil {
		return nil, nil
	}
	var out map[string]int
	if err := json.Unmarshal(v, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func putJSONToBucket(b *bbolt.Bucket, key string, val interface{}) error {
	if b == nil {
		return fmt.Errorf("nil bucket")
	}
	data, err := json.Marshal(val)
	if err != nil {
		return err
	}
	return b.Put([]byte(key), data)
}

func incTopicInTx(topicB *bbolt.Bucket, word string) error {
	if topicB == nil {
		return fmt.Errorf("nil topic bucket")
	}
	word = strings.ToLower(word)
	if len(word) < 3 {
		return nil
	}
	var count int
	if v := topicB.Get([]byte(word)); v != nil {
		if err := json.Unmarshal(v, &count); err != nil {
			return fmt.Errorf("failed to unmarshal topic count: %w", err)
		}
	}
	count++
	data, _ := json.Marshal(count)
	return topicB.Put([]byte(word), data)
}

func trimHistoryInTx(historyB *bbolt.Bucket, max int) error {
	if historyB == nil {
		return nil
	}
	for historyB.Stats().KeyN > max {
		c := historyB.Cursor()
		k, _ := c.First()
		if k == nil {
			break
		}
		if err := historyB.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// saveImageURLToDB stores an image URL in the ImageCacheBucket.
func saveImageURLToDB(tx *bbolt.Tx, url string) error {
	bucket := tx.Bucket([]byte(ImageCacheBucket))
	if bucket == nil {
		return fmt.Errorf("ImageCacheBucket not found")
	}
	return bucket.Put([]byte(url), []byte("1")) // Store URL as key, value doesn't matter much
}

// trimImageCacheInTx removes oldest image URLs from the ImageCacheBucket if it exceeds max.
func trimImageCacheInTx(tx *bbolt.Tx, max int) error {
	bucket := tx.Bucket([]byte(ImageCacheBucket))
	if bucket == nil {
		return nil
	}

	for bucket.Stats().KeyN > max {
		c := bucket.Cursor()
		k, _ := c.First()
		if k == nil {
			break
		}
		if err := bucket.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// loadImageURLsFromDB loads image URLs from the ImageCacheBucket into memory.
func loadImageURLsFromDB() ([]string, error) {
	var urls []string
	err := db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(ImageCacheBucket))
		if bucket == nil {
			return fmt.Errorf("ImageCacheBucket not found")
		}
		_ = bucket.ForEach(func(k, v []byte) error {
			urls = append(urls, string(k))
			return nil
		})
		return nil
	})
	return urls, err
}

// saveLeaderboard saves the current leaderboard state to the database.
func saveLeaderboard(tx *bbolt.Tx, l *wordgames.Leaderboard) error {
	b := tx.Bucket([]byte(LeaderboardBucket))
	if b == nil {
		return fmt.Errorf("LeaderboardBucket not found")
	}
	encoded, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return b.Put([]byte("current"), encoded)
}

// loadLeaderboard loads the leaderboard state from the database.
func loadLeaderboard(tx *bbolt.Tx) (*wordgames.Leaderboard, error) {
	b := tx.Bucket([]byte(LeaderboardBucket))
	if b == nil {
		return nil, fmt.Errorf("LeaderboardBucket not found")
	}
	v := b.Get([]byte("current"))
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

// loadAllUserStats loads all user message counts from the StatsBucket for the current week.
func loadAllUserStats(tx *bbolt.Tx) (map[string]int, error) {
	statsB := tx.Bucket([]byte(StatsBucket))
	if statsB == nil {
		return nil, fmt.Errorf("StatsBucket not found")
	}

	scores := make(map[string]int)

	now := time.Now().UTC()
	// Determine the start of the current week (Monday).
	weekday := now.Weekday()
	daysSinceMonday := (weekday - time.Monday + 7) % 7
	startOfWeek := now.Truncate(24 * time.Hour).Add(-time.Duration(daysSinceMonday) * 24 * time.Hour)

	err := statsB.ForEach(func(k, v []byte) error {
		// Skip non-user ID stats like "total_messages_learned"
		if _, err := strconv.ParseInt(string(k), 10, 64); err != nil {
			return nil
		}
		var stat WeeklyStat
		if err := json.Unmarshal(v, &stat); err != nil {
			// Could be old format (just an int). Try to unmarshal that.
			var count int
			if err2 := json.Unmarshal(v, &count); err2 == nil {
				// It's the old format. Include it for now.
				// It will be converted to the new format when the user next speaks.
				scores[string(k)] = count
			}
			return nil // Skip to the next item
		}

		// Only include stats from the current week.
		if !stat.LastTimestamp.Before(startOfWeek) {
			scores[string(k)] = stat.Count
		}

		return nil
	})

	return scores, err
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

func getNextMap(prefix string) (map[string]int, error) {
	var nextMap map[string]int
	err := db.View(func(tx *bbolt.Tx) error {
		markovB := tx.Bucket([]byte(MarkovBucket))
		if markovB == nil {
			return nil
		}
		if v := markovB.Get([]byte(prefix)); v != nil {
			_ = json.Unmarshal(v, &nextMap)
		}
		return nil
	})
	return nextMap, err
}

// learnMessage ingests and learns from a single message.
func learnMessage(tx *bbolt.Tx, msg, msgID, botID string, author MentionedUser, mentionedUsers []MentionedUser) error {
	// ... (message cleaning logic is the same)
	msg = regexp.MustCompile(fmt.Sprintf(`(?i)<@!?%s>|@peregrine`, botID)).ReplaceAllString(msg, "")
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return nil
	}
	if isSpammyContent(msg) {
		return nil
	}

	words := tokenize(msg)
	if len(words) == 0 {
		return nil
	}
	words = append(words, "<end>") // Append a token to signify the end of a thought.

	startTime := time.Now()

	// Get buckets within the existing transaction.
	// Note: `tx` is already provided by the calling function (e.g., processChannelIncremental)
	// so we do not need to call `db.Update` here.

	// ... (bucket retrieval is the same)
	historyB := tx.Bucket([]byte(HistoryBucket))
	markovB := tx.Bucket([]byte(MarkovBucket))
	topicB := tx.Bucket([]byte(TopicBucket))
	nameTopicB := tx.Bucket([]byte(NameTopicBucket))
	statsB := tx.Bucket([]byte(StatsBucket))

	if historyB == nil || markovB == nil || topicB == nil || nameTopicB == nil || statsB == nil {
		return fmt.Errorf("missing bucket")
	}

	if historyB.Get([]byte(msgID)) != nil {
		return nil
	}

	// Update user stats
	if author.UserID != "" {
		if _, err := learnOrUpdateName(tx, author.Name, author.UserID, author.Username); err == nil {
			var stat WeeklyStat
			if v := statsB.Get([]byte(author.UserID)); v != nil {
				_ = json.Unmarshal(v, &stat)
			}

			now := time.Now().UTC()
			// Determine the start of the current week (Monday).
			weekday := now.Weekday()
			daysSinceMonday := (weekday - time.Monday + 7) % 7
			startOfWeek := now.Truncate(24 * time.Hour).Add(-time.Duration(daysSinceMonday) * 24 * time.Hour)

			if stat.LastTimestamp.Before(startOfWeek) {
				// It's a new week for this user
				stat.Count = 1
			} else {
				stat.Count++
			}
			stat.LastTimestamp = now

			statBytes, _ := json.Marshal(stat)
			_ = statsB.Put([]byte(author.UserID), statBytes)
		}
	}

	// Increment total messages learned counter
	var totalLearnedCount int
	if v := statsB.Get([]byte("total_messages_learned")); v != nil {
		_ = json.Unmarshal(v, &totalLearnedCount)
	}
	totalLearnedCount++
	totalLearnedBytes, _ := json.Marshal(totalLearnedCount)
	_ = statsB.Put([]byte("total_messages_learned"), totalLearnedBytes)

	// Increment global topic counts
	for _, w := range words {
		_ = incTopicInTx(topicB, toLowerCaseExceptURLs(w)) // Use helper
	}

	// Create a set of canonical names to process for this message.
	canonicalNames := make(map[string]struct{})
	for _, user := range mentionedUsers {
		canonical, err := learnOrUpdateName(tx, user.Name, user.UserID, user.Username)
		if err != nil {
			log.Printf("[WARN] Failed to learn name '%s': %v", user.Name, err)
			continue
		}
		canonicalNames[canonical] = struct{}{}
	}

	// Update associations for each unique canonical name.
	for canonicalName := range canonicalNames {
		var nameTopicAssoc map[string]WordPosData
		if v := nameTopicB.Get([]byte(canonicalName)); v != nil {
			_ = json.Unmarshal(v, &nameTopicAssoc)
		} else {
			nameTopicAssoc = make(map[string]WordPosData)
		}

		for i, w := range words {
			lw := toLowerCaseExceptURLs(w) // Use helper
			if lw == "<end>" {
				continue
			}
			// CRITICAL: Exclude stop words from all associative learning.
			if _, isStop := stopWords[lw]; isStop {
				continue
			}
			pos := float64(i) / float64(len(words))

			// Update NameTopicBucket
			topicData, ok := nameTopicAssoc[lw]
			if !ok {
				topicData = WordPosData{Count: 0, PosSum: 0}
			}
			topicData.Count++
			topicData.PosSum += pos
			nameTopicAssoc[lw] = topicData
		}
		out, _ := json.Marshal(nameTopicAssoc)
		_ = nameTopicB.Put([]byte(canonicalName), out)
	}

	// If any names were involved, update global word/topic associations.
	// This ensures the global graph is built from user-provided context.
	if len(canonicalNames) > 0 {
		// Update TopicWordBucket for word-to-word associations
		topicWordB := tx.Bucket([]byte(TopicWordBucket))
		if topicWordB != nil {
			topicWordUpdates := make(map[string]map[string]WordPosData)
			for _, word := range words {
				lw := toLowerCaseExceptURLs(word) // Use helper
				if lw == "<end>" {
					continue
				}
				if _, exists := topicWordUpdates[lw]; !exists {
					var wordAssoc map[string]WordPosData
					if v := topicWordB.Get([]byte(lw)); v != nil {
						_ = json.Unmarshal(v, &wordAssoc)
					} else {
						wordAssoc = make(map[string]WordPosData)
					}
					topicWordUpdates[lw] = wordAssoc
				}
			}

			for i, wordA := range words {
				lwA := toLowerCaseExceptURLs(wordA) // Use helper
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
					lwB := toLowerCaseExceptURLs(wordB) // Use helper
					if lwB == "<end>" {
						continue
					}
					if _, isStop := stopWords[lwB]; isStop {
						continue
					}
					assocMapA := topicWordUpdates[lwA]
					dataB := assocMapA[lwB]
					dataB.Word = lwB
					dataB.Count++
					dataB.PosSum += float64(j) / float64(len(words))
					assocMapA[lwB] = dataB
				}
			}

			for topic, wordAssocMap := range topicWordUpdates {
				out, _ := json.Marshal(wordAssocMap)
				_ = topicWordB.Put([]byte(topic), out)
			}
		}

		// Update Topic Clusters based on all words in the message
		topicClusterB := tx.Bucket([]byte(TopicClusterBucket))
		if topicClusterB != nil {
			uniqueWords := make(map[string]struct{})
			for _, word := range words {
				lw := toLowerCaseExceptURLs(word) // Use helper
				if lw != "<end>" {
					uniqueWords[lw] = struct{}{}
				}
			}
			topicList := make([]string, 0, len(uniqueWords))
			for topic := range uniqueWords {
				topicList = append(topicList, topic)
			}

			for i := 0; i < len(topicList); i++ {
				for j := i + 1; j < len(topicList); j++ {
					topicA, topicB := topicList[i], topicList[j]
					if _, isStopA := stopWords[topicA]; isStopA {
						continue
					}
					if _, isStopB := stopWords[topicB]; isStopB {
						continue
					}
					var key string
					if topicA < topicB {
						key = fmt.Sprintf("%s|%s", topicA, topicB)
					} else {
						key = fmt.Sprintf("%s|%s", topicB, topicA)
					}
					var clusterData TopicClusterData
					if v := topicClusterB.Get([]byte(key)); v != nil {
						if err := json.Unmarshal(v, &clusterData); err != nil {
							// Skip rather than fall through: on a decode error
							// clusterData stays zero, so the Count++ below would
							// write 1 and silently discard however many
							// co-occurrences had already accumulated for this pair.
							log.Printf("[WARN] topic cluster %q has an undecodable value, skipping: %v", key, err)
							continue
						}
					}
					clusterData.Count++
					out, _ := json.Marshal(clusterData)
					_ = topicClusterB.Put([]byte(key), out)
				}
			}
		}
	}

	// ... (Markov n-gram ingestion is the same)
	totalNgrams := 0
	for n := cfg.MaxNGram; n >= 1; n-- {
		if len(words) < n {
			continue
		}
		for i := 0; i <= len(words)-n; i++ {
			key := toLowerCaseExceptURLs(strings.Join(words[i:i+n-1], " ")) // Use helper
			next := toLowerCaseExceptURLs(words[i+n-1])                     // Use helper
			ng, _ := getMapFromBucket(markovB, key)
			if ng == nil {
				ng = make(map[string]int)
			}
			ng[next]++
			_ = putJSONToBucket(markovB, key, ng)
			totalNgrams++
		}
	}

	_ = historyB.Put([]byte(msgID), []byte(msg))
	_ = trimHistoryInTx(historyB, cfg.MaxHistory)

	log.Printf("[LEARNED] msg=%q | words=%d | ngrams=%d | history=%d | names=%d | took=%s",
		msg, len(words), totalNgrams, historyB.Stats().KeyN, len(canonicalNames), time.Since(startTime))

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

// extractNamesFromMessage returns Discord usernames and display names mentioned in a message
func extractNamesFromMessage(s *discordgo.Session, m *discordgo.MessageCreate, guildID string) []MentionedUser {
	users := []MentionedUser{}
	seenIDs := make(map[string]struct{})

	// 1. Process explicit @mentions first.
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

	// 2. Scan message content for any other known names that weren't @mentioned.
	words := tokenize(m.Content)
	_ = db.View(func(tx *bbolt.Tx) error {
		nameB := tx.Bucket([]byte(NameBucket))
		if nameB == nil {
			return nil
		}

		for _, word := range words {
			lw := toLowerCaseExceptURLs(word)
			if v := nameB.Get([]byte(lw)); v != nil {
				var data NameData
				if err := json.Unmarshal(v, &data); err != nil {
					continue // Skip malformed data.
				}

				// Determine the canonical name for the found word (which could be an alias).
				canonicalName := lw
				if data.CanonicalName != "" {
					canonicalName = data.CanonicalName
				}

				// Fetch the full data for the canonical name to get the UserID.
				var canonicalData NameData
				if vCanonical := nameB.Get([]byte(canonicalName)); vCanonical != nil {
					if err := json.Unmarshal(vCanonical, &canonicalData); err == nil {
						// If we haven't already processed this user via @mention, add them.
						if _, ok := seenIDs[canonicalData.DiscordUser]; !ok && canonicalData.DiscordUser != "" {
							seenIDs[canonicalData.DiscordUser] = struct{}{}
							users = append(users, MentionedUser{
								Name:     canonicalName,
								UserID:   canonicalData.DiscordUser,
								Username: canonicalName,
							})
						}
					}
				}
			}
		}
		return nil
	})

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

		err := db.Update(func(tx *bbolt.Tx) error {
			return learnMessage(tx, m.Content, m.ID, botID, authorInfo, names)
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

// tokenRegex tokenizes words, punctuation, URLs, mentions, and emojis.
var tokenRegex = regexp.MustCompile(`(?:https?|steam):\/\/\S+|<@!?&?\d+>|<#\d+>|<a?:\w+:\d+>|:\w+:|[\p{L}\p{N}\p{So}'’]+`)
var urlRegex = regexp.MustCompile(`^(?:https?|steam):\/\/\S+$`)
var emoteRegex = regexp.MustCompile(`^<a?:\w+:\d+>$`)
var shortcodeRegex = regexp.MustCompile(`^:(\w+):$`)

// tokenize splits a message into tokens, preserving URL casing and lowercasing others.
// It uses a bytes.Buffer for efficient string building.
func tokenize(msg string) []string {
	tokens := tokenRegex.FindAllString(msg, -1)
	processedTokens := make([]string, 0, len(tokens))

	for _, token := range tokens {
		if urlRegex.MatchString(token) {
			processedTokens = append(processedTokens, token) // Keep URL case for URLs
		} else {
			processedTokens = append(processedTokens, strings.ToLower(token)) // Lowercase other tokens
		}
	}
	return processedTokens
}

// toLowerCaseExceptURLs converts a string to lowercase, but preserves the case of identified URLs.
// This is an optimized helper to avoid repeated regex matching and string allocations
// when only part of a token needs lowercasing.
func toLowerCaseExceptURLs(input string) string {
	if urlRegex.MatchString(input) {
		return input
	}
	return strings.ToLower(input)
}

// sentenceSimilarity computes a simple Jaccard-like similarity on token sets.
func sentenceSimilarity(a, b string) float64 {
	wa := tokenize(a)
	wb := tokenize(b)
	setA := make(map[string]struct{})
	setB := make(map[string]struct{})
	for _, w := range wa {
		setA[w] = struct{}{}
	}
	for _, w := range wb {
		setB[w] = struct{}{}
	}
	inter := 0
	for w := range setA {
		if _, ok := setB[w]; ok {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// cleanSentence preserves special tokens (URLs, mentions, emojis) while cleaning punctuation from regular words.
func cleanSentence(s *discordgo.Session, str string) string {
	str = strings.TrimSpace(str)
	if str == "" {
		return str
	}

	tokens := tokenize(str)
	cleanedTokens := make([]string, 0, len(tokens))
	var lastToken string

	for _, token := range tokens {
		// Resolve emote shortcodes
		if matches := shortcodeRegex.FindStringSubmatch(token); len(matches) == 2 {
			emojiName := matches[1]
			var foundEmoji string
			// Search for the emoji in all guilds the bot is in.
			for _, guild := range s.State.Guilds {
				for _, emoji := range guild.Emojis {
					if emoji.Name == emojiName {
						foundEmoji = emoji.MessageFormat()
						break
					}
				}
				if foundEmoji != "" {
					break
				}
			}

			resolvedToken := toLowerCaseExceptURLs(token) // Use helper to process token
			if foundEmoji != "" {
				resolvedToken = foundEmoji
			}

			if resolvedToken != lastToken {
				cleanedTokens = append(cleanedTokens, resolvedToken)
				lastToken = resolvedToken
			}
			continue
		}

		// Preserve URLs, mentions, and full emotes.
		isSpecial := urlRegex.MatchString(token) || strings.HasPrefix(token, "<@") || strings.HasPrefix(token, "<#") || emoteRegex.MatchString(token)
		if isSpecial {
			transformedToken := token
			// FxEmbed transformation for Twitter/X links.
			if urlRegex.MatchString(token) {
				if strings.Contains(transformedToken, "://x.com/") || strings.Contains(transformedToken, "://twitter.com/") {
					transformedToken = strings.Replace(transformedToken, "://x.com/", "://fxtwitter.com/", 1)
					transformedToken = strings.Replace(transformedToken, "://twitter.com/", "://fxtwitter.com/", 1)
				}
			}

			// Avoid immediate duplicates of special tokens.
			if transformedToken != lastToken {
				cleanedTokens = append(cleanedTokens, transformedToken)
				lastToken = transformedToken
			}
			continue
		}

		// For regular words, remove any punctuation.
		cleanedWord := regexp.MustCompile(`[.,!?]`).ReplaceAllString(token, "")
		if cleanedWord == "" {
			continue
		}

		// Avoid immediate duplicates of regular words.
		if cleanedWord != lastToken {
			cleanedTokens = append(cleanedTokens, cleanedWord)
			lastToken = cleanedWord
		}
	}

	return strings.Join(cleanedTokens, " ")
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

// findBestSeed analyzes the prompt and context to find the highest-quality starting seed.
func findBestSeed(tx *bbolt.Tx, promptWords, recentWords, recognizedNames []string) string {
	type candidate struct {
		key    int
		weight float64
	}
	candidateMap := make(map[int]float64)

	markovB := tx.Bucket([]byte(MarkovBucket))
	if markovB == nil {
		log.Printf("[WARN] findBestSeed: MarkovBucket not found")
		return "<START>"
	}
	nameTopicB := tx.Bucket([]byte(clustering.NameTopicBucket))
	topicWordB := tx.Bucket([]byte(TopicWordBucket))
	topicClusterB := tx.Bucket([]byte(clustering.TopicClusterBucket))

	addCandidate := func(key int, weight float64) {
		if existingWeight, ok := candidateMap[key]; !ok || weight > existingWeight {
			candidateMap[key] = weight
		}
	}

	// 1. High Priority: Use Concept Clusters for seed generation
	if conceptClusterB := tx.Bucket([]byte(clustering.ConceptClusterBucket)); conceptClusterB != nil {
		for _, word := range promptWords {
			wordIdx, ok := vocab[word]
			if !ok {
				continue
			}
			// Find which cluster the prompt word belongs to
			c := conceptClusterB.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var cluster ConceptCluster
				if err := json.Unmarshal(v, &cluster); err == nil {
					if _, ok := cluster.Members[wordIdx]; ok {
						// Add all members of the cluster as high-weight candidates
						for memberIdx := range cluster.Members {
							addCandidate(memberIdx, 50.0) // High weight for being in a relevant cluster
						}
						break // Move to the next prompt word
					}
				}
			}
		}
	}

	// 2. High Priority: Multi-word n-grams from the prompt.
	for n := cfg.MaxNGram - 1; n >= 1; n-- {
		for i := 0; i <= len(promptWords)-n; i++ {
			key := toLowerCaseExceptURLs(strings.Join(promptWords[i:i+n], " "))
			if markovB.Get([]byte(key)) != nil {
				addCandidate(addToVocab(key), float64(n*30))
			}
		}
	}

	// 2. Name-Cluster Expansion: Find topics clustered with recognized names.
	if topicClusterB != nil && len(recognizedNames) > 0 {
		for _, name := range recognizedNames {
			prefix := []byte(toLowerCaseExceptURLs(name) + "|")
			c := topicClusterB.Cursor()
			for k, v := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, v = c.Next() {
				parts := strings.Split(string(k), "|")
				if len(parts) == 2 {
					associatedTopic := parts[1]
					var clusterData TopicClusterData
					if err := json.Unmarshal(v, &clusterData); err != nil {
						log.Printf("[WARN] findBestSeed: failed to unmarshal cluster data for key %s: %v", k, err)
						continue
					}
					addCandidate(addToVocab(associatedTopic), 25.0+math.Sqrt(float64(clusterData.Count)))
				}
			}
		}
	}

	// 3. Associative Expansion: Find words related to the prompt words.
	if topicWordB != nil {
		for _, word := range promptWords {
			topic := toLowerCaseExceptURLs(word)
			if v := topicWordB.Get([]byte(topic)); v != nil {
				var wordAssoc map[string]WordPosData
				if err := json.Unmarshal(v, &wordAssoc); err != nil {
					log.Printf("[WARN] findBestSeed: failed to unmarshal word association for topic %s: %v", topic, err)
					continue
				}
				for associatedWord, data := range wordAssoc {
					if associatedWord != topic && data.Count > 1 {
						addCandidate(addToVocab(associatedWord), 18.0+math.Sqrt(float64(data.Count)))
					}
				}
			}
		}
	}

	// 4. Topic Cluster Expansion: Find words from related topic clusters.
	if topicClusterB != nil {
		for _, word := range promptWords {
			lw := toLowerCaseExceptURLs(word)
			prefix := []byte(lw + "|")
			c := topicClusterB.Cursor()
			for k, v := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, v = c.Next() {
				parts := strings.Split(string(k), "|")
				if len(parts) == 2 {
					associatedTopic := parts[1]
					var clusterData TopicClusterData
					if err := json.Unmarshal(v, &clusterData); err != nil {
						log.Printf("[WARN] findBestSeed: failed to unmarshal cluster data for key %s: %v", k, err)
						continue
					}
					addCandidate(addToVocab(associatedTopic), 15.0+math.Sqrt(float64(clusterData.Count)))
				}
			}
		}
	}

	// 5. Medium-High Priority: Topics directly associated with recognized names.
	if len(recognizedNames) > 0 && nameTopicB != nil {
		name := toLowerCaseExceptURLs(recognizedNames[len(recognizedNames)-1])
		if v := nameTopicB.Get([]byte(name)); v != nil {
			var topicAssoc map[string]TopicAssociationData
			if err := json.Unmarshal(v, &topicAssoc); err != nil {
				log.Printf("[WARN] findBestSeed: failed to unmarshal topic association for name %s: %v", name, err)
			} else {
				for topic, data := range topicAssoc {
					if markovB.Get([]byte(topic)) != nil {
						addCandidate(addToVocab(topic), 10.0+math.Sqrt(float64(data.Count)))
					}
				}
			}
		}
	}

	// 6. Medium Priority: Single words from the prompt.
	for _, word := range promptWords {
		lw := toLowerCaseExceptURLs(word)
		if markovB.Get([]byte(lw)) != nil {
			addCandidate(addToVocab(lw), 15.0)
		}
	}

	// 7. Low Priority: Recent context fallback.
	for n := cfg.MaxNGram - 1; n >= 1; n-- {
		for i := 0; i <= len(recentWords)-n; i++ {
			key := toLowerCaseExceptURLs(strings.Join(recentWords[i:i+n], " "))
			if markovB.Get([]byte(key)) != nil {
				addCandidate(addToVocab(key), float64(n))
			}
		}
	}

	if len(candidateMap) == 0 {
		// Absolute fallback.
		c := markovB.Cursor()
		k, _ := c.First()
		if k != nil {
			return string(k)
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
				return revVocab[c.key]
			}
		}
		return revVocab[candidates[len(candidates)-1].key]
	}

	return revVocab[candidates[0].key]
}

// performClustering runs an agglomerative clustering algorithm to merge and refine concepts.
func performClustering() {
	err := clustering.PerformClusteringOptimized(db, nil)
	if err != nil {
		log.Printf("[ERR] Optimized clustering pass failed: %v", err)
	}
}

// findJumpWord attempts to find a related word when generation hits a dead end.
func findJumpWord(tx *bbolt.Tx, promptWords, currentWords, recognizedNames []string) string {
	nameTopicB := tx.Bucket([]byte(clustering.NameTopicBucket))
	topicWordB := tx.Bucket([]byte(TopicWordBucket))
	if topicWordB == nil || nameTopicB == nil {
		return ""
	}

	// 1. Name-Centric Pivot: Prioritize finding a topic related to a recognized name.
	if len(recognizedNames) > 0 {
		// Use the most recently mentioned name as the primary context.
		name := toLowerCaseExceptURLs(recognizedNames[len(recognizedNames)-1])
		if v := nameTopicB.Get([]byte(name)); v != nil {
			var topicAssoc map[string]TopicAssociationData
			if json.Unmarshal(v, &topicAssoc) == nil {
				bestJump := ""
				closestDist := 0.5 // Target the middle of the sentence for a pivot
				for topic, data := range topicAssoc {
					if data.Count > 1 { // Require at least two occurrences to be considered a stable topic
						avgPos := data.PosSum / float64(data.Count)
						dist := math.Abs(avgPos - 0.5) // Find topic closest to the middle of a sentence
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
	}

	// 2. Concept Cluster Pivot: If we're stuck, try to jump to a related concept.
	if conceptClusterB := tx.Bucket([]byte(clustering.ConceptClusterBucket)); conceptClusterB != nil {
		for _, word := range currentWords {
			wordIdx, ok := vocab[word]
			if !ok {
				continue
			}
			c := conceptClusterB.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var cluster ConceptCluster
				if err := json.Unmarshal(v, &cluster); err == nil {
					if _, ok := cluster.Members[wordIdx]; ok {
						// Found a cluster, pick a random member to jump to
						for memberIdx := range cluster.Members {
							if memberIdx != wordIdx { // Avoid jumping to the same word
								log.Printf("[INFO] Generation stuck, jumping via concept cluster: '%s' -> '%s'", word, revVocab[memberIdx])
								return revVocab[memberIdx]
							}
						}
					}
				}
			}
		}
	}

	// 3. Topic-Centric Pivot: Fallback to finding a word related to the last topic.
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
		if v := topicWordB.Get([]byte(topic)); v != nil {
			var wordAssoc map[string]WordPosData
			if err := json.Unmarshal(v, &wordAssoc); err != nil {
				log.Printf("[WARN] findJumpWord: failed to unmarshal word association for topic %s: %v", topic, err)
				continue
			}

			for word, data := range wordAssoc {
				// Don't jump to the topic word itself or a word already used.
				if _, exists := sentenceSet[word]; exists || word == topic || data.Count <= 1 {
					continue
				}

				// New scoring: Combine association strength (primary) with positional preference (secondary).
				strengthScore := math.Sqrt(float64(data.Count))
				avgPos := data.PosSum / float64(data.Count)
				posScore := 1.0 - math.Abs(avgPos-0.5) // value from 0.5 to 1.0

				score := strengthScore * posScore
				if score > bestScore {
					bestScore = score
					bestJump = word
				}
			}
		}
	}

	if bestJump != "" {
		log.Printf("[INFO] Generation stuck, jumping via topic->word: '%s'", bestJump)
		return bestJump
	}

	return ""
}

// pickPromptAwareNextWithSimilarity picks the next word considering prompt similarity and context.
func pickPromptAwareNextWithSimilarity(
	prefix string,
	topicB, nameB, topicWordB *bbolt.Bucket,
	fullPrompt string,
	recentWords []string,
	used map[string]int,
	generatedNgrams map[string]struct{},
	aggregatedAssoc map[string]WordPosData,
	isRoast bool,
	sentenceLength int, // NEW: pass current sentence length
	maxWords int, // NEW: pass max words for this sentence
	currentTopic string, // NEW: pass current topic
	coreTopics map[int]float64, // For Topic Gravity
	currentSentence []string, // NEW: Pass current sentence for immediate repetition check
) string {
	nextMap, _ := getNextMap(prefix)
	if len(nextMap) == 0 {
		return ""
	}

	type cand struct {
		word   string
		weight float64
	}
	cands := make([]cand, 0, len(nextMap))

	promptWords := tokenize(fullPrompt)
	promptSet := make(map[string]struct{}, len(promptWords))
	for _, w := range promptWords {
		promptSet[w] = struct{}{}
	}

	recentSet := make(map[string]struct{}, len(recentWords))
	for _, w := range recentWords {
		recentSet[toLowerCaseExceptURLs(w)] = struct{}{} // Use helper
	}

	curPos := float64(sentenceLength) / float64(maxWords) // Use actual sentence progress
	// NEW: momentum factor, grows as we move forward to encourage linguistic continuity
	momentum := math.Min(1.0, curPos*1.2+0.1)

	// Pre-load all topic-word associations needed for this generation round to avoid repeated DB access
	topicWordAssocCache := make(map[string]map[string]WordPosData)
	if topicWordB != nil {
		// Cache associations for aggregated name topics
		for topic := range aggregatedAssoc {
			if _, exists := topicWordAssocCache[topic]; !exists {
				if v := topicWordB.Get([]byte(topic)); v != nil {
					var wordAssoc map[string]WordPosData
					if err := json.Unmarshal(v, &wordAssoc); err == nil {
						topicWordAssocCache[topic] = wordAssoc
					}
				}
			}
		}
		// Cache associations for core prompt topics (for Topic Gravity)
		for topicIdx := range coreTopics {
			topic := revVocab[topicIdx]
			if _, exists := topicWordAssocCache[topic]; !exists {
				if v := topicWordB.Get([]byte(topic)); v != nil {
					var wordAssoc map[string]WordPosData
					if err := json.Unmarshal(v, &wordAssoc); err == nil {
						topicWordAssocCache[topic] = wordAssoc
					}
				}
			}
		}
	}

	for w, base := range nextMap {
		lw := toLowerCaseExceptURLs(w) // Use helper
		score := float64(base)

		// --- Topic Gravity (New Sophisticated Version) ---
		topicGravity := 1.0
		for topicIdx, significance := range coreTopics {
			topic := revVocab[topicIdx]
			if wordAssoc, ok := topicWordAssocCache[topic]; ok {
				if data, ok := wordAssoc[lw]; ok {
					// 1. Strength Score: How strong is the link between the topic and this candidate word?
					strengthScore := math.Sqrt(float64(data.Count))

					// 2. Positional Score: Does this word tend to appear in the right place relative to the topic?
					avgPos := data.PosSum / float64(data.Count)
					distance := math.Abs(curPos - avgPos)
					posScore := math.Exp(-distance * 5.0) // Exponential decay based on distance

					// Combine scores: strength * position * topic significance
					topicGravity += strengthScore * posScore * significance
				}
			}
		}
		score *= topicGravity
		// --- End Topic Gravity ---

		// Roasty flavor boost
		if isRoast {
			roastyWords := map[string]float64{
				toLowerCaseExceptURLs("dumbass"):  5.0,
				toLowerCaseExceptURLs("idiot"):    5.0,
				toLowerCaseExceptURLs("loser"):    4.0,
				toLowerCaseExceptURLs("clown"):    4.0,
				toLowerCaseExceptURLs("cringe"):   3.0,
				toLowerCaseExceptURLs("pathetic"): 3.0,
				toLowerCaseExceptURLs("weak"):     2.0,
				toLowerCaseExceptURLs("sad"):      2.0,
				toLowerCaseExceptURLs("cope"):     2.0,
				toLowerCaseExceptURLs("seethe"):   2.0,
				toLowerCaseExceptURLs("mald"):     2.0,
				toLowerCaseExceptURLs("ratio"):    1.5,
				toLowerCaseExceptURLs("lmao"):     1.0,
				toLowerCaseExceptURLs("lol"):      1.0,
			}
			if boost, ok := roastyWords[lw]; ok {
				score *= (1.0 + boost)
			}
		}

		// recognized name boost (moderated)
		if nameB != nil && nameB.Get([]byte(lw)) != nil {
			score *= 1.2
		}

		// global topic boost (moderated)
		if topicB != nil {
			if v := topicB.Get([]byte(lw)); len(v) > 0 {
				var count int
				_ = json.Unmarshal(v, &count)
				score += math.Sqrt(float64(count)) * 0.3
			}
		}

		// aggregated name-topic weighting (now hierarchical and cached)
		if len(aggregatedAssoc) > 0 {
			for topic, topicData := range aggregatedAssoc {
				avgTopicPos := topicData.PosSum / float64(topicData.Count)
				distanceToTopic := math.Abs(curPos - avgTopicPos)
				topicPosScore := math.Exp(-distanceToTopic * 3)

				if topicPosScore > 0.1 {
					if wordAssoc, ok := topicWordAssocCache[topic]; ok {
						if wordPosData, ok := wordAssoc[lw]; ok {
							avgWordPos := wordPosData.PosSum / float64(wordPosData.Count)
							distanceToWordInTopic := math.Abs(curPos - avgWordPos)
							wordPosScore := math.Exp(-distanceToWordInTopic * 4)
							score += math.Sqrt(float64(wordPosData.Count)) * 0.8 * topicPosScore * wordPosScore
						}
					}
				}
			}
		}

		// Smooth topic transition: bias towards words in the current topic cluster
		if currentTopic != "" {
			if wordAssoc, ok := topicWordAssocCache[currentTopic]; ok {
				if _, ok := wordAssoc[lw]; ok {
					score *= 1.5 // Boost words within the current topic
				}
			}
		}

		// prompt relevance boost (unchanged)
		if _, ok := promptSet[lw]; ok {
			score += cfg.PromptRelevanceBoost
		}

		// recent context nudge (stronger)
		if _, ok := recentSet[lw]; ok {
			score *= 1.25 // Increased multiplier from 1.05 to 1.25
		}

		// repetition penalty (stronger)
		if used[lw] > 0 {
			score /= math.Pow(2.2, float64(used[lw])) // Increased penalty from 1.6 to 2.2
		}

		// Immediate sentence repetition check (NEW)
		for i := len(currentSentence) - 1; i >= 0 && i >= len(currentSentence)-5; i-- { // Check last 5 words
			if toLowerCaseExceptURLs(currentSentence[i]) == lw {
				score *= 0.01 // Heavily penalize immediate repetition within the sentence
				break
			}
		}

		// N-gram repetition penalty
		// Check for 1-gram (word) repetition
		if _, found := generatedNgrams[lw]; found {
			score *= 0.5 // Penalty for repeating a word
		}
		// Check for 2-gram repetition
		if len(prefix) > 0 {
			twoGram := toLowerCaseExceptURLs(prefix + " " + lw)
			if _, found := generatedNgrams[twoGram]; found {
				score *= 0.1 // Heavy penalty for repeating a 2-gram
			}
		}
		// Check for 3-gram repetition
		if len(prefix) > 0 {
			words := strings.Fields(prefix)
			if len(words) >= 2 {
				threeGram := toLowerCaseExceptURLs(strings.Join(words[len(words)-2:], " ") + " " + lw)
				if _, found := generatedNgrams[threeGram]; found {
					score *= 0.01 // Very heavy penalty for repeating a 3-gram
				}
			}
		}

		// Semantic grounding: encourage words that maintain semantic flow
		// Positional grounding: encourage words that fit typical sentence structure at current position
		// These are implicitly handled by the markov chain probabilities and topic/name associations
		// as well as the prompt gravity and momentum factors. Further explicit implementation
		// would require more advanced NLP techniques (e.g., word embeddings, part-of-speech tagging)
		// which are beyond the scope of a simple Markov bot without additional libraries.

		// slight punctuation penalty
		// Removed as punctuation is no longer desired in output.

		// Prompt Gravity: Dynamically pull the sentence back towards the prompt's topic.
		currentSentenceFragment := prefix + " " + lw
		gravity := sentenceSimilarity(currentSentenceFragment, fullPrompt)
		if gravity > 0.05 { // Only apply if there's a meaningful similarity
			score *= (1.0 + gravity*2.5) // Apply a strong, compounding bonus for staying on topic.
		}

		// stochastic jitter to reduce deterministic loops
		score *= 0.9 + 0.2*rand.Float64() // ±10% variation

		// Penalize the <end> token heavily if the sentence is short.
		if lw == "<end>" && curPos < 0.4 { // curPos is an approximation of sentence progress (0.0 to 1.0)
			score *= 0.1 // Drastically reduce the chance of ending early
		}

		// early <END> penalty
		// Removed as punctuation is no longer desired in output.

		// === NEW NATURAL FLOW HEURISTICS ===
		// discourage early punctuation / closure
		// Removed as punctuation is no longer desired in output.
		// discourage mid-sequence stop words if too repetitive
		if curPos > 0.3 && curPos < 0.7 && (lw == "and" || lw == "but" || lw == "so") && used[lw] > 0 {
			score *= 0.8
		}
		// gentle encouragement for connective continuity in mid-sentence
		if curPos > 0.2 && curPos < 0.8 && (lw == "and" || lw == "but" || lw == "then" || lw == "because") {
			score *= 1.1 * momentum
		}
		// encourage conclusive / emotional tokens near the end
		// Removed as punctuation is longer desired in output.
		// lightly dampen excessive connectors at very end
		if curPos > 0.85 && (lw == "and" || lw == "but" || lw == "because") {
			score *= 0.7
		}

		if score > 0 {
			cands = append(cands, cand{w, score})
		}
	}

	if len(cands) == 0 {
		return ""
	}

	// stochastic selection
	total := 0.0
	weights := make([]float64, len(cands))
	for i := range cands {
		w := math.Pow(cands[i].weight, 1.0/(Creativity+0.01))
		if i == 0 {
			w *= 1.0 + 0.05*rand.Float64()
		}
		weights[i] = w
		total += w
	}

	if total <= 0 {
		return cands[len(cands)-1].word
	}

	r_val := rand.Float64() * total
	for i := range cands {
		r_val -= weights[i]
		if r_val <= 0 {
			if cands[i].word == "<END>" && len(prefix) < 10 && len(cands) > 1 {
				return cands[1].word
			}
			return cands[i].word
		}
	}

	return cands[len(cands)-1].word
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

	err := db.View(func(tx *bbolt.Tx) error {
		markovB := tx.Bucket([]byte(MarkovBucket))
		if markovB == nil || markovB.Stats().KeyN == 0 {
			sentence = append(sentence, "...")
			return nil
		}

		// Initial generation attempt
		sentence, recognizedNames, _ = generateSentenceAttempt(tx, promptWords, recentWords, prompt, isRoast)
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

	// Check if the generated output is spammy before returning.
	if isSpammyContent(final) {
		log.Printf("[FILTER] Generated message was blocked as spam: %q", final)
		return "", nil // Return an empty string to prevent sending.
	}

	return final, nil
}

// generateSentenceAttempt is a helper that contains the core sentence generation logic.
func generateSentenceAttempt(tx *bbolt.Tx, promptWords, recentWords []string, originalPrompt string, isRoast bool) ([]string, []string, map[string]WordPosData) {
	sentence := []string{}
	usedWords := make(map[string]int)
	generatedNgrams := make(map[string]struct{}) // Track generated n-grams
	var recognizedNames []string
	var aggregatedAssoc map[string]WordPosData
	var currentTopic string // Track the current topic cluster

	topicB := tx.Bucket([]byte(TopicBucket))
	nameB := tx.Bucket([]byte(NameBucket))
	nameTopicB := tx.Bucket([]byte(NameTopicBucket))
	topicWordB := tx.Bucket([]byte(TopicWordBucket))

	// populate recognizedNames inside closure
	for _, w := range promptWords {
		if name, exists := isRecognizedName(w); exists {
			recognizedNames = append(recognizedNames, name)
		}
	}

	// --- Topic Gravity ---
	// Extract core non-stop-words from the prompt and weigh them by significance (global count).
	coreTopics := make(map[int]float64)
	for _, w := range promptWords {
		if _, isStop := stopWords[w]; !isStop {
			var count int
			if v := topicB.Get([]byte(w)); v != nil {
				_ = json.Unmarshal(v, &count)
			}
			// Use the log of the count to get a significance score that doesn't grow too quickly.
			coreTopics[addToVocab(w)] = math.Log(float64(count) + 1)
		}
	}

	// NEW: aggregate all recognized names' associations
	aggregatedAssoc = make(map[string]WordPosData)
	if nameTopicB != nil && len(recognizedNames) > 0 {
		for _, nm := range recognizedNames {
			nmKey := toLowerCaseExceptURLs(nm)
			if assocV := nameTopicB.Get([]byte(nmKey)); len(assocV) > 0 {
				var assoc map[string]WordPosData
				if err := json.Unmarshal(assocV, &assoc); err != nil {
					log.Printf("[WARN] failed to unmarshal name-topic association for %s: %v", nmKey, err)
					continue
				}
				for w, data := range assoc {
					if existing, ok := aggregatedAssoc[w]; ok {
						existing.Count += data.Count
						existing.PosSum += data.PosSum
						aggregatedAssoc[w] = existing
					} else {
						aggregatedAssoc[w] = data
					}
				}
			}
		}
	}

	// Determine seed using the new intelligent algorithm
	seed := findBestSeed(tx, promptWords, recentWords, recognizedNames)
	if seed == "" || seed == "<START>" {
		if len(promptWords) > 0 {
			seed = promptWords[0]
		} else {
			seed = "<START>" // Should be rare
		}
	}
	currentTopic = seed // Initialize current topic with the seed

	// Initialize sentence with seed
	words := tokenize(seed)
	sentence = append(sentence, words...)
	for _, w := range words {
		usedWords[w]++
	}
	lastWords := append([]string{}, words...)

	// Set generation boundaries
	minWords := 5 + rand.IntN(4) // Set a dynamic minimum length (5-8 words)
	maxWords := 30 + rand.IntN(15)

	// Generate words iteratively
	for i := 0; i < maxWords; i++ {
		if len(lastWords) == 0 {
			break
		}

		n := len(lastWords)
		if n > cfg.MaxNGram-1 {
			n = cfg.MaxNGram - 1
		}

		var next string
		// Shrink prefix if no match
		for shrink := n; shrink >= 1; shrink-- {
			prefix := strings.Join(lastWords[len(lastWords)-shrink:], " ")
			next = pickPromptAwareNextWithSimilarity(
				prefix,
				topicB,
				nameB,
				topicWordB,
				originalPrompt,
				recentWords,
				usedWords,
				generatedNgrams, // NEW: pass generated n-grams map
				aggregatedAssoc,
				isRoast,
				len(sentence), // NEW: pass current sentence length
				maxWords,      // NEW: pass max words
				currentTopic,  // NEW: pass current topic
				coreTopics,    // Pass core topics for Topic Gravity
				sentence,      // Pass current sentence for immediate repetition check
			)
			if next != "" {
				break
			}
		} // If we generate an end token but the sentence is too short, discard it and try to find another word.
		if next == "<end>" && len(sentence) < minWords {
			next = "" // Discard the <end> token
		}

		if next == "<end>" {
			break // Stop generating if we hit the end token and sentence is long enough
		}

		if next == "" {
			// Creative Jump: If we hit a dead end, try to find a related word to jump to.
			jumpWord := findJumpWord(tx, promptWords, sentence, recognizedNames)
			if jumpWord != "" {
				next = jumpWord
			} else {
				// If we still can't find a jump, then it's a true dead end.
				break
			}
		}

		sentence = append(sentence, next)
		lastWords = append(lastWords, next)
		if len(lastWords) > cfg.MaxNGram-1 {
			lastWords = lastWords[1:]
		}
		usedWords[toLowerCaseExceptURLs(next)]++

		// Track generated n-grams (1, 2, and 3-grams)
		generatedNgrams[toLowerCaseExceptURLs(next)] = struct{}{}
		if len(sentence) >= 2 {
			twoGram := toLowerCaseExceptURLs(strings.Join(sentence[len(sentence)-2:], " "))
			generatedNgrams[twoGram] = struct{}{}
		}
		if len(sentence) >= 3 {
			threeGram := toLowerCaseExceptURLs(strings.Join(sentence[len(sentence)-3:], " "))
			generatedNgrams[threeGram] = struct{}{}
		}
	}

	return sentence, recognizedNames, aggregatedAssoc
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

	// CRITICAL: Block spam and illegal content first.
	if isSpammyContent(m.Content) {
		log.Printf("[FILTER] Spammy message from %s blocked, no processing will occur.", m.Author.Username)
		return // Silently drop the message.
	}
	if filterIllegalContent(m.Content) {
		return
	}

	// Apply the slur filter (replacement).
	m.Content = filterSlurs(m.Content)

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
				_ = db.Update(func(tx *bbolt.Tx) error {
					return saveLeaderboard(tx, leaderboard)
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
		err := db.View(func(tx *bbolt.Tx) error {
			var loadErr error
			userScores, loadErr = loadAllUserStats(tx)
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
		_ = db.Update(func(tx *bbolt.Tx) error {
			return saveAggroState(tx, AggroState{})
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
					// Learn the bot's own reply, associating it with the users mentioned in the original prompt.
					// This teaches the bot what a good, on-topic response looks like.
					_ = db.Update(func(tx *bbolt.Tx) error {
						mentionedInPrompt := extractNamesFromMessage(s, m, m.GuildID)
						if err := learnMessage(tx, replyContent, m.ID, botID, botAsMention, mentionedInPrompt); err != nil {
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
			_ = db.Update(func(tx *bbolt.Tx) error {
				for _, user := range mentionedUsers {
					_, err := learnOrUpdateName(tx, user.Name, user.UserID, user.Username)
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
		_ = db.Update(func(tx *bbolt.Tx) error {
			if err := learnMessage(tx, m.Content, m.ID, botID, authorAsMention, mentionedUsers); err != nil {
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
			// Persist to DB and then reload the cache to ensure consistency
			err := db.Update(func(tx *bbolt.Tx) error {
				if err := saveImageURLToDB(tx, urlToCache); err != nil {
					return fmt.Errorf("failed to save image URL to DB: %w", err)
				}
				if err := trimImageCacheInTx(tx, cfg.ImageCacheSize); err != nil {
					return fmt.Errorf("failed to trim image cache: %w", err)
				}

				// After trimming, reload all URLs to ensure the in-memory slice is in sync
				bucket := tx.Bucket([]byte(ImageCacheBucket))
				if bucket == nil {
					return fmt.Errorf("ImageCacheBucket not found during reload")
				}
				return bucket.ForEach(func(k, v []byte) error {
					newUrlList = append(newUrlList, string(k))
					return nil
				})
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
			err = db.Update(func(tx *bbolt.Tx) error {
				return learnMessage(tx, transcript, job.MsgID, botID, authorInfo, []MentionedUser{})
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

// printLibraryStatus logs counts of markov/topic/history keys.
func printLibraryStatus() {
	start := time.Now()
	err := db.View(func(tx *bbolt.Tx) error {
		buckets := []string{MarkovBucket, TopicBucket, HistoryBucket, NameBucket, NameTopicBucket, TopicWordBucket, TopicClusterBucket, ImageCacheBucket, ConceptClusterBucket}
		status := make(map[string]int)

		for _, bName := range buckets {
			b := tx.Bucket([]byte(bName))
			if b != nil {
				status[bName] = b.Stats().KeyN
			} else {
				status[bName] = 0
			}
		}

		// Fetch total_messages_learned separately if it exists
		totalMessagesLearned := 0
		if statsB := tx.Bucket([]byte(StatsBucket)); statsB != nil {
			if v := statsB.Get([]byte("total_messages_learned")); v != nil {
				_ = json.Unmarshal(v, &totalMessagesLearned)
			}
		}

		log.Printf(
			"Library status: markov=%d | topics=%d | history=%d | names=%d | name-topic=%d | topic-word=%d | raw-clusters=%d | concepts=%d | images=%d | total-learned=%d | checked in %s",
			status[MarkovBucket],
			status[TopicBucket],
			status[HistoryBucket],
			status[NameBucket],
			status[NameTopicBucket],
			status[TopicWordBucket],
			status[TopicClusterBucket],
			status[ConceptClusterBucket],
			status[ImageCacheBucket],
			totalMessagesLearned, // NEW: Include total messages learned
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

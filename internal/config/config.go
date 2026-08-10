// Package config turns the environment into one validated value.
//
// Environment variables only, deliberately: peregrine has no config.yaml and
// wants none. Merlin needs one because its settings are per guild and edited
// live from Discord; peregrine's are per process and change on a deploy, which
// is exactly what an env var models. Adding a file format would mean two sources
// of truth and a reload path for a bot that has neither.
//
// Two rules hold here, and both come from a specific failure.
//
// Every field in Config is read by code that exists. A knob wired to nothing is
// worse than no knob: it looks like a supported control, an operator tunes it,
// nothing changes, and the bot gets blamed for ignoring configuration. That is
// why ContextWindow and CoherencyBalance were deleted rather than promoted (they
// were declared and never read), and why the variables that later milestones
// will read are documented in .env.example but absent from this struct. Setting
// one of those today produces a warning naming the milestone, not silence.
//
// Load reports every problem it finds, not the first. A container that fails on
// one bad variable per restart makes an operator debug a six-variable mistake in
// six deploys.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Config is the whole of peregrine's configuration. Zero value is not usable;
// call Load.
type Config struct {
	// Credentials and identity.
	Token       string // DISCORD_BOT_TOKEN
	AdminUserID string // PEREGRINE_BOOTSTRAP_ADMIN_USER_ID
	LogLevel    string // LOG_LEVEL

	// Storage.
	DBPath     string // PEREGRINE_DB_PATH
	MaxHistory int    // PEREGRINE_MAX_HISTORY

	// Safety. BlocklistPath is validated here only as a string; the file itself is
	// loaded in cmd/bot, where a failure is fatal. It is deliberately allowed to be
	// empty, because a developer running against a scratch corpus should not need to
	// invent a slur list first, and the built-in baseline in internal/filter still
	// applies. An empty value logs a prominent warning rather than passing silently.
	BlocklistPath string // PEREGRINE_BLOCKLIST_PATH

	// PauseAllWrites refuses every outbound message process-wide while leaving
	// reading and learning alone. The host-level lever for when the bot is actively
	// saying something awful and waiting for a deploy is not acceptable.
	PauseAllWrites bool // PEREGRINE_PAUSE_ALL_WRITES

	// IgnoreChannels are channel IDs the bot must never post in.
	//
	// Enforced inside internal/discordguard rather than in the reply logic, because an
	// operator setting this means "not in there" and not "not in reply to a message in
	// there": the autonomous poster and the word games have to respect it too. It does
	// NOT stop learning from those channels, which is the same asymmetry
	// PauseAllWrites has and for the same reason: the output is what causes trouble.
	IgnoreChannels []string // PEREGRINE_IGNORE_CHANNELS

	// Generation. Everything here is read by internal/markov as of M7a.
	//
	// Creativity is deliberately absent and there is no PEREGRINE_CREATIVITY. It was
	// the one tuning constant M2 refused to promote, because its arithmetic
	// contradicted its name, and M7a replaces it with Temperature in the same change
	// that normalizes the scoring so the dial actually moves (SPEC.md section 5.3).
	MaxNGram             int     // PEREGRINE_MAX_NGRAM
	PromptRelevanceBoost float64 // PEREGRINE_PROMPT_RELEVANCE_BOOST
	Temperature          float64 // PEREGRINE_TEMPERATURE
	TopK                 int     // PEREGRINE_TOP_K
	TopP                 float64 // PEREGRINE_TOP_P
	KNDiscount           float64 // PEREGRINE_KN_DISCOUNT
	KNRawMix             float64 // PEREGRINE_KN_RAW_MIX
	MinDistinctAuthors   int     // PEREGRINE_MIN_DISTINCT_AUTHORS
	MinWords             int     // PEREGRINE_MIN_WORDS
	MaxWords             int     // PEREGRINE_MAX_WORDS
	CooccurrenceWindow   int     // PEREGRINE_COOCCURRENCE_WINDOW
	RoastChance          float64 // PEREGRINE_ROAST_CHANCE
	SelfMention          *regexp.Regexp

	// Ingestion.
	//
	// IngestLookback stopped being a re-read window in M9 and is now a BOOTSTRAP bound:
	// it applies to the first pass over a channel and never again, because subsequent
	// passes resume from a stored per-channel cursor. The old meaning was the mechanism
	// behind finding 13, since re-reading a window whose dedup record had been evicted
	// counted the same n-grams twice.
	IngestTick               time.Duration // PEREGRINE_INGEST_TICK
	IngestLookback           time.Duration // PEREGRINE_INGEST_LOOKBACK
	IngestBatchDelay         time.Duration // PEREGRINE_INGEST_BATCH_DELAY
	IngestGuildConcurrency   int           // PEREGRINE_INGEST_GUILD_CONCURRENCY
	IngestChannelConcurrency int           // PEREGRINE_INGEST_CHANNEL_CONCURRENCY

	// History repair. RepairJobs names the jobs to run, or "all"; empty runs none.
	//
	// RepairBefore is an override used only by a job whose learn generation predates
	// generation stamping, because the corpus records when each generation first ran and that
	// is the boundary a repair should use. It exists for exactly one case and should not grow
	// a second (SPEC.md section 8, finding 46).
	//
	// The two variables these replace, PEREGRINE_ASSOC_BACKFILL and
	// PEREGRINE_ASSOC_BACKFILL_BEFORE, are REFUSED at startup rather than ignored. The rule
	// in this file is prefer rescale-and-refuse over rename, precisely because a rename lets
	// an old value stop being read without saying so; refusing is how a rename honours it.
	RepairJobs   []string  // PEREGRINE_REPAIR_JOBS
	RepairBefore time.Time // PEREGRINE_REPAIR_BEFORE

	// Runtime. The worker pool that every incoming message goes through, replacing
	// an unbounded goroutine per message that shared a WaitGroup with the shutdown
	// path. A full queue drops and counts rather than blocking, because
	// best-effort chat is the honest semantics and an unbounded queue is only a
	// slower crash.
	//
	// Workers is capped low on purpose: bbolt serializes every write transaction
	// process-wide, so past a handful of workers the extra concurrency buys
	// contention rather than throughput.
	MessageWorkers int // PEREGRINE_MESSAGE_WORKERS
	MessageQueue   int // PEREGRINE_MESSAGE_QUEUE

	// Housekeeping loops.
	//
	// EnableClustering and ClusteringTick were here until M6b, became deferred
	// variables, and are gone entirely as of M7b: clustering is deleted rather than
	// rebuilt, so there is no milestone left to defer them to (SPEC.md section 8,
	// findings 27 and 29).
	StatusTick time.Duration // PEREGRINE_STATUS_TICK

	// The Discord presence line, rotated on the status tick. There is no interval of its own,
	// because the numbers it shows come from the page walk that tick already pays for.
	//
	// PresenceCorpusWordChance is how often the line quotes a WORD FROM THE CORPUS instead of
	// reporting a count. That is user-typed text on public display, so it goes through the
	// emit gate like every other emission and falls back to a count when the gate refuses it.
	// Zero disables the variant entirely, for an operator who does not want the bot's status
	// to be user-derived at all.
	EnablePresence           bool          // PEREGRINE_ENABLE_PRESENCE
	PresenceCorpusWordChance float64       // PEREGRINE_PRESENCE_CORPUS_WORD_CHANCE
	LeaderboardTick          time.Duration // PEREGRINE_LEADERBOARD_CHECK_TICK

	// Engagement: bird aggro.
	AggroTick     time.Duration // PEREGRINE_AGGRO_TICK
	AggroChance   float64       // PEREGRINE_AGGRO_CHANCE
	AggroDuration time.Duration // PEREGRINE_AGGRO_DURATION
	AggroEmoji    string        // PEREGRINE_AGGRO_EMOJI

	// AggroActivityWindow is how far back a user counts as "around" when a target is
	// picked. It was a hardcoded six hours inside findRandomActiveUser, which reached
	// that far back by paging Discord; it now reads the in-process activity tracker, so
	// the practical bound is also how long the process has been up.
	AggroActivityWindow time.Duration // PEREGRINE_AGGRO_ACTIVITY_WINDOW

	// ActiveChannelWindow is how recent traffic must be for a channel to count as
	// somewhere worth speaking unprompted. Read by autonomous posting and by
	// interval-mode word games, which are the two features that choose a channel for
	// themselves.
	//
	// It replaces PEREGRINE_INGEST_LOOKBACK in that role, which was a misuse: the
	// lookback is a bootstrap bound for the history walk and has nothing to say about
	// where people are talking now.
	ActiveChannelWindow time.Duration // PEREGRINE_ACTIVE_CHANNEL_WINDOW

	// Engagement: image reposting.
	EnableImageRepost bool    // PEREGRINE_ENABLE_IMAGE_REPOST
	ImageRepostChance float64 // PEREGRINE_IMAGE_REPOST_CHANCE
	ImageRepostDirect float64 // PEREGRINE_IMAGE_REPOST_CHANCE_DIRECT
	ImageCacheSize    int     // PEREGRINE_IMAGE_CACHE_SIZE

	// ImageMaxPerAuthor caps how much of the cache one person can own.
	//
	// This is the anti-poisoning half of SPEC.md section 4, A7: image reposting has the
	// bot republish user-supplied media under its own name in a channel of its choosing,
	// so a hostile user seeding the cache is the attack. Enforced inside
	// storage.Writer.AddImageURL rather than at the caller, for the same reason
	// CheckLearn lives inside learnMessage.
	ImageMaxPerAuthor int // PEREGRINE_IMAGE_MAX_PER_AUTHOR

	// Engagement: autonomous posting.
	EnableAutonomousPost   bool          // PEREGRINE_ENABLE_AUTONOMOUS_POST
	AutonomousPostChannels []string      // PEREGRINE_AUTONOMOUS_POST_CHANNELS
	AutonomousPostTick     time.Duration // PEREGRINE_AUTONOMOUS_POST_TICK
	AutonomousSkipChance   float64       // PEREGRINE_AUTONOMOUS_SKIP_CHANCE

	// Engagement: word games.
	//
	// Every number here was a literal in the middle of the message handler until M11.
	// They are not tuning knobs for their own sake: the timeout and the announcement TTL
	// govern how much of the channel the game occupies, and the activity trigger governs
	// whether the bot interrupts a conversation or joins one.
	EnableWordGames           bool          // PEREGRINE_ENABLE_WORD_GAMES
	WordGameMode              string        // PEREGRINE_WORDGAME_FREQUENCY_MODE
	WordGameInterval          time.Duration // PEREGRINE_WORDGAME_INTERVAL
	WordGameDictionary        string        // PEREGRINE_WORDGAME_DICTIONARY
	WordGameTimeout           time.Duration // PEREGRINE_WORDGAME_TIMEOUT
	WordGameAnnounceTTL       time.Duration // PEREGRINE_WORDGAME_ANNOUNCE_TTL
	WordGameActivityWindow    time.Duration // PEREGRINE_WORDGAME_ACTIVITY_WINDOW
	WordGameActivityThreshold int           // PEREGRINE_WORDGAME_ACTIVITY_THRESHOLD
	WordGameTriggerChance     float64       // PEREGRINE_WORDGAME_TRIGGER_CHANCE
	WordGameMinLength         int           // PEREGRINE_WORDGAME_MIN_LENGTH
	WordGameMaxLength         int           // PEREGRINE_WORDGAME_MAX_LENGTH
	WordGameSweepTick         time.Duration // PEREGRINE_WORDGAME_SWEEP_TICK
	WordGameHintAfter         time.Duration // PEREGRINE_WORDGAME_HINT_AFTER

	// Corpus snapshots. Off by default, because there is no safe guess for a path and
	// writing megabytes somewhere the operator did not choose is worse than not backing up.
	//
	// In a container BackupDir MUST be on the mounted volume: the image runs read_only, so
	// anywhere else fails on the first write, and a relative path resolves against the
	// distroless working directory. Each snapshot is a full copy, so the disk cost is
	// BackupKeep times the corpus size on the same volume as the corpus.
	BackupDir  string        // PEREGRINE_BACKUP_DIR
	BackupTick time.Duration // PEREGRINE_BACKUP_TICK
	BackupKeep int           // PEREGRINE_BACKUP_KEEP

	// The tuning export. Off by default and for the same reason as the backups above: no
	// safe guess for a path, and the same read_only container caveat applies.
	//
	// This one writes MESSAGE TEXT, which the backups also do (the corpus is made of it) but
	// in a form nobody would read by accident. Prompts and replies land in the export as
	// plain lines, so the directory deserves the same handling as the corpus rather than
	// being treated as logs. What it does NOT write is anything the emit gate refused: a
	// sample whose send was turned down carries no text at all.
	TuningDir              string        // PEREGRINE_TUNING_DIR
	TuningRotate           time.Duration // PEREGRINE_TUNING_ROTATE
	TuningKeep             int           // PEREGRINE_TUNING_KEEP
	TuningSample           float64       // PEREGRINE_TUNING_SAMPLE
	TuningEngagementWindow time.Duration // PEREGRINE_TUNING_ENGAGEMENT_WINDOW
	TuningTrackMax         int           // PEREGRINE_TUNING_TRACK_MAX

	// Version is stamped on every exported record so two archives can be told apart.
	//
	// It is an environment variable rather than a build stamp because .dockerignore excludes
	// .git, so debug.ReadBuildInfo has no vcs revision inside the image. The deploy already
	// knows the answer: docker-compose.prod.yml passes the image tag, which CI sets to the
	// commit SHA it built.
	Version string // PEREGRINE_VERSION

	// Transcription. Off by default, and that default deliberately differs from
	// the old in-code constant, which was true: transcription shells out to
	// ffmpeg and whisper-cli and needs a 465 MiB model, none of which exist in a
	// distroless image that has no shell at all. On Linux the binary lookup fails
	// and every voice note produces a failure reply, so shipping it on by default
	// meant the container's only visible transcription behavior was an error.
	EnableTranscription bool // PEREGRINE_ENABLE_TRANSCRIPTION

	// TranscriptionQueue bounds work in flight. Transcription is slow and voice notes
	// arrive in bursts, so this is what stops a burst becoming unbounded memory. A full
	// queue drops the note and says so in the channel, which is the same honest semantics
	// the message dispatcher uses.
	TranscriptionQueue int // PEREGRINE_TRANSCRIPTION_QUEUE
}

// Word game pacing modes.
const (
	WordGameModeInterval = "interval"
	WordGameModeActivity = "activity"
)

// maxActivityThreshold is the ceiling on PEREGRINE_WORDGAME_ACTIVITY_THRESHOLD.
//
// It is a ceiling rather than an opinion: the activity tracker keeps a fixed ring of
// timestamps per channel, so a count saturates at activity.PerChannelHistory and a
// threshold above that could never be met. The knob would accept the value, the trigger
// would never fire, and nothing would say why. TestTheActivityCeilingFitsTheTracker
// asserts the relationship, because two files agreeing by comment is how it drifts.
const maxActivityThreshold = 100

// deferredVars are documented in .env.example but read by no code yet, mapped to
// the milestone that starts reading them. Setting one today does nothing, and
// silence about that is how an operator concludes the bot ignores its own
// documentation. Load warns instead. Delete an entry when its milestone lands and
// the field appears in Config above.
var deferredVars = map[string]string{
	// PEREGRINE_ENABLE_CLUSTERING and PEREGRINE_CLUSTERING_TICK were here until M7b
	// and are now GONE rather than promoted, because clustering is deleted rather than
	// rebuilt (SPEC.md section 8, finding 29). A variable naming a feature that will
	// never exist is worse than a deferred one: the deferred warning at least promises
	// a milestone. An operator who still has either set gets no warning now, which is
	// correct, because there is nothing left for them to be waiting for.
	// These three configure a transcription ENGINE, and M12 shipped the seam rather than an
	// engine: no implementation ships in this repository, because the one peregrine had needed
	// a 465 MiB model and platform binaries that exist in no deployed environment. They stay
	// deferred rather than being deleted, because unlike the clustering variables above there
	// is a milestone that will read them.
	"PEREGRINE_WHISPER_MODEL":         "M12b",
	"PEREGRINE_VOICENOTES_DIR":        "M12b",
	"PEREGRINE_TRANSCRIPTION_TIMEOUT": "M12b",
}

// Load reads and validates the environment. The returned error, if any, names
// every problem found rather than the first.
//
// It deliberately does not require DISCORD_BOT_TOKEN. Maintenance modes
// (-clean-db) operate on the corpus and never touch Discord, so requiring a
// token to run one would mean an operator cleaning a poisoned corpus needs a
// live credential to do it. RequireToken is the check the bot path makes.
func Load() (*Config, error) {
	l := &loader{}
	cfg := &Config{
		Token:       os.Getenv("DISCORD_BOT_TOKEN"),
		AdminUserID: l.str("PEREGRINE_BOOTSTRAP_ADMIN_USER_ID", ""),
		LogLevel:    l.enum("LOG_LEVEL", "info", "debug", "info", "warn", "error"),

		// The relative default is kept because it is what a `go run ./cmd/bot`
		// from the repo root has always used. It is wrong in a container and
		// .env.example says so: the image has a read-only root filesystem, so a
		// relative path resolves against the distroless working directory and
		// bbolt.Open fails.
		DBPath:     l.str("PEREGRINE_DB_PATH", "markov.db"),
		MaxHistory: l.intVal("PEREGRINE_MAX_HISTORY", 10000, 100, 10_000_000),

		BlocklistPath:  l.str("PEREGRINE_BLOCKLIST_PATH", ""),
		PauseAllWrites: l.boolVal("PEREGRINE_PAUSE_ALL_WRITES", false),
		IgnoreChannels: l.csv("PEREGRINE_IGNORE_CHANNELS"),

		// Minimum 2. Order 1 makes the prefix empty, and under the old layout an
		// empty prefix was one bbolt key holding a map of the entire vocabulary,
		// rewritten once per word per message, that nothing ever read
		// (SPEC.md section 8, finding 5).
		//
		// This bound is now one of three things stopping that. M6b's ingestion loop
		// descends to 2 rather than 1, and storage.Writer.LearnNgram refuses an empty
		// prefix outright, so a new caller cannot reintroduce it either.
		MaxNGram: l.intVal("PEREGRINE_MAX_NGRAM", 5, 2, 8),

		// The units of this one CHANGED in M7a, and the narrowed range is the point
		// rather than a side effect.
		//
		// It used to be added to an unnormalized score whose scale was raw n-gram
		// counts, so 15.0 was a sensible default. The score is now a log-probability,
		// where an additive 15.0 multiplies a candidate's odds by three million and
		// makes echoing the prompt the only thing the bot ever does. Keeping the
		// variable's NAME and refusing the old value is deliberate: an operator with
		// 15.0 still in their .env gets a startup error naming the new range, whereas
		// renaming it would have let the stale value quietly stop being read, which is
		// the exact failure this package exists to prevent.
		PromptRelevanceBoost: l.float("PEREGRINE_PROMPT_RELEVANCE_BOOST", 0.6, 0, 5),

		// Generation dials, live as of M7a (SPEC.md section 5.3).
		//
		// Temperature replaces the Creativity constant, which was applied as an
		// exponent of 1/(Creativity+0.01) and therefore sharpened at its own default
		// and could never pass an exponent of 1.0. There is deliberately no
		// PEREGRINE_CREATIVITY. The upper bound of 10 is generous rather than
		// meaningful: past about 3 the output is word salad even with top-k on, and the
		// point of allowing more is that the operator can see that for themselves.
		Temperature: l.float("PEREGRINE_TEMPERATURE", 1.0, 0, 10),
		TopK:        l.intVal("PEREGRINE_TOP_K", 40, 0, 10_000),
		TopP:        l.float("PEREGRINE_TOP_P", 0.95, 0, 1),

		// D below 1 is required: absolute discounting subtracts D from each observed
		// count, so a D at or above 1 erases every count-1 continuation, and in a
		// corpus this sparse that is nearly all of them.
		KNDiscount: l.float("PEREGRINE_KN_DISCOUNT", 0.75, 0, 0.99),
		KNRawMix:   l.float("PEREGRINE_KN_RAW_MIX", 0.25, 0, 1),

		// Author diversity, the anti-poisoning control (SPEC.md section 4, A6). 0
		// disables it, which is the right value on a scratch corpus and the wrong one
		// on a live server: the default is 2 so that the safe direction is what an
		// operator gets by doing nothing.
		MinDistinctAuthors: l.intVal("PEREGRINE_MIN_DISTINCT_AUTHORS", 2, 0, 100),

		// Length, live as of M7b. The old bound was 30 + rand(15) words, which is a
		// paragraph. The cap's upper limit of 100 is deliberately well above anything
		// sensible: it is a guard against a typo, not a recommendation.
		MinWords: l.intVal("PEREGRINE_MIN_WORDS", 4, 1, 100),
		MaxWords: l.intVal("PEREGRINE_MAX_WORDS", 18, 1, 100),

		// The co-occurrence window, live as of M7b. 0 means unbounded, which restores
		// the all-pairs behaviour and warns, because quadratic work inside bbolt's
		// single write transaction blocks all ingestion.
		CooccurrenceWindow: l.intVal("PEREGRINE_COOCCURRENCE_WINDOW", 5, 0, 1000),

		// Roast persona probability, live as of M7b.
		RoastChance: l.float("PEREGRINE_ROAST_CHANCE", 0.10, 0, 1),

		SelfMention: l.regex("PEREGRINE_SELF_MENTION_PATTERN", `(?i)\b(peregrine|bird)\b`),

		// History repair (finding 46). Default OFF: a repair re-reads the whole of history,
		// which is an operator's decision rather than a deploy's.
		RepairJobs:   l.csv("PEREGRINE_REPAIR_JOBS"),
		RepairBefore: l.timestamp("PEREGRINE_REPAIR_BEFORE"),

		IngestTick:       l.dur("PEREGRINE_INGEST_TICK", 10*time.Minute, time.Minute, 24*time.Hour),
		IngestLookback:   l.dur("PEREGRINE_INGEST_LOOKBACK", 24*time.Hour, time.Minute, 30*24*time.Hour),
		IngestBatchDelay: l.dur("PEREGRINE_INGEST_BATCH_DELAY", 500*time.Millisecond, 0, time.Minute),

		// Both capped low, and the cap is the point rather than caution. The fan-out was
		// unbounded (finding 14): one goroutine per channel per guild, each paging
		// Discord, which earns rate limits whose retries then make it worse. bbolt also
		// serializes every write process-wide, so past a handful of workers the extra
		// concurrency buys contention rather than throughput.
		IngestGuildConcurrency:   l.intVal("PEREGRINE_INGEST_GUILD_CONCURRENCY", 4, 1, 64),
		IngestChannelConcurrency: l.intVal("PEREGRINE_INGEST_CHANNEL_CONCURRENCY", 4, 1, 64),

		MessageWorkers: l.intVal("PEREGRINE_MESSAGE_WORKERS", 4, 1, 64),
		MessageQueue:   l.intVal("PEREGRINE_MESSAGE_QUEUE", 256, 1, 100_000),

		StatusTick: l.dur("PEREGRINE_STATUS_TICK", 5*time.Minute, time.Minute, 24*time.Hour),

		// On by default, unlike almost everything else here, because it is the one feature in
		// this bot whose failure mode is purely cosmetic: it cannot ping, cannot post, and
		// says nothing an operator has to answer for. A blank status line on a bot that is
		// running is a small lie an operator has to work to see through.
		EnablePresence:           l.boolVal("PEREGRINE_ENABLE_PRESENCE", true),
		PresenceCorpusWordChance: l.float("PEREGRINE_PRESENCE_CORPUS_WORD_CHANCE", 0.25, 0, 1),

		LeaderboardTick: l.dur("PEREGRINE_LEADERBOARD_CHECK_TICK", time.Hour, time.Minute, 24*time.Hour),

		AggroTick:     l.dur("PEREGRINE_AGGRO_TICK", time.Hour, time.Minute, 30*24*time.Hour),
		AggroChance:   l.float("PEREGRINE_AGGRO_CHANCE", 0.20, 0, 1),
		AggroDuration: l.dur("PEREGRINE_AGGRO_DURATION", 20*time.Minute, time.Minute, 30*24*time.Hour),
		AggroEmoji:    l.str("PEREGRINE_AGGRO_EMOJI", "\U0001F426"),

		AggroActivityWindow: l.dur("PEREGRINE_AGGRO_ACTIVITY_WINDOW", 6*time.Hour, time.Minute, 7*24*time.Hour),
		ActiveChannelWindow: l.dur("PEREGRINE_ACTIVE_CHANNEL_WINDOW", time.Hour, time.Minute, 7*24*time.Hour),

		// Reposting had no flag at all: it was unconditionally on. The default is
		// true so this milestone changes no behavior, but it now has an off
		// switch, which matters because the bot republishes user media under its
		// own name (SPEC.md section 4, A7).
		EnableImageRepost: l.boolVal("PEREGRINE_ENABLE_IMAGE_REPOST", true),
		ImageRepostChance: l.float("PEREGRINE_IMAGE_REPOST_CHANCE", 0.015, 0, 1),
		ImageRepostDirect: l.float("PEREGRINE_IMAGE_REPOST_CHANCE_DIRECT", 0.01, 0, 1),
		ImageCacheSize:    l.intVal("PEREGRINE_IMAGE_CACHE_SIZE", 100, 1, 100_000),
		ImageMaxPerAuthor: l.intVal("PEREGRINE_IMAGE_MAX_PER_AUTHOR", 5, 1, 100_000),

		EnableAutonomousPost:   l.boolVal("PEREGRINE_ENABLE_AUTONOMOUS_POST", false),
		AutonomousPostChannels: l.csv("PEREGRINE_AUTONOMOUS_POST_CHANNELS"),
		// Its own variable, defaulting to the ingestion cadence it used to share.
		// Both loops were driven by one AutonomyTick constant, so tuning how often
		// the bot backfills history also changed how often it speaks unprompted:
		// two unrelated decisions on one dial, and the second one invisible.
		AutonomousPostTick:   l.dur("PEREGRINE_AUTONOMOUS_POST_TICK", 10*time.Minute, time.Minute, 30*24*time.Hour),
		AutonomousSkipChance: l.float("PEREGRINE_AUTONOMOUS_SKIP_CHANCE", 0.90, 0, 1),

		// Word games are ON by default as of M11. They were switched off by a
		// compile-time constant, then by a flag defaulting false, and the whole point of
		// the feature is engagement: a game nobody can play is not a conservative
		// default, it is a feature that does not exist. The failure mode of having it on
		// is that the bot occasionally posts a puzzle, which is what it is for.
		EnableWordGames: l.boolVal("PEREGRINE_ENABLE_WORD_GAMES", true),
		WordGameMode:    l.enum("PEREGRINE_WORDGAME_FREQUENCY_MODE", WordGameModeActivity, WordGameModeInterval, WordGameModeActivity),

		// 30 minutes, not the 2 minutes that stood here. Two was plainly a leftover from
		// testing: a puzzle every two minutes in a busy channel is the bot talking over
		// the conversation rather than joining it. The minimum is raised to 5 for the
		// same reason, since anything under that is the old value by another name.
		WordGameInterval:   l.dur("PEREGRINE_WORDGAME_INTERVAL", 30*time.Minute, 5*time.Minute, 24*time.Hour),
		WordGameDictionary: l.str("PEREGRINE_WORDGAME_DICTIONARY", ""),

		// The numbers that used to be literals inside the handler.
		//
		// The timeout is how long people get to answer, and 60 seconds is what it was.
		// The announce TTL is how long a win or timeout message survives before the bot
		// tidies it away; 0 keeps it, which an operator who wants a visible history
		// should set.
		WordGameTimeout:     l.dur("PEREGRINE_WORDGAME_TIMEOUT", time.Minute, 10*time.Second, time.Hour),
		WordGameAnnounceTTL: l.dur("PEREGRINE_WORDGAME_ANNOUNCE_TTL", 30*time.Second, 0, 24*time.Hour),

		// A game starts when a channel has seen THRESHOLD messages within WINDOW, then on
		// a TRIGGER_CHANCE roll per message. All three were literals: 5, 5 minutes and
		// 0.025.
		WordGameActivityWindow:    l.dur("PEREGRINE_WORDGAME_ACTIVITY_WINDOW", 5*time.Minute, time.Minute, 24*time.Hour),
		WordGameActivityThreshold: l.intVal("PEREGRINE_WORDGAME_ACTIVITY_THRESHOLD", 5, 1, maxActivityThreshold),
		WordGameTriggerChance:     l.float("PEREGRINE_WORDGAME_TRIGGER_CHANCE", 0.025, 0, 1),

		// Word length in RUNES. The old filter was `len(word) > 4` on bytes, so an
		// accented five-letter word counted as six.
		WordGameMinLength: l.intVal("PEREGRINE_WORDGAME_MIN_LENGTH", 5, 3, 40),
		WordGameMaxLength: l.intVal("PEREGRINE_WORDGAME_MAX_LENGTH", 12, 3, 40),

		// How often expired games and due deletions are swept. This is the resolution of
		// the timeout, so a game can outlive its deadline by up to one tick, which is a
		// trade worth making: a puzzle ending a few seconds late is invisible, whereas the
		// goroutine per game this replaces outlived shutdown.
		WordGameSweepTick: l.dur("PEREGRINE_WORDGAME_SWEEP_TICK", 5*time.Second, time.Second, time.Minute),

		// How far into a puzzle the first letter is revealed. 0 turns hints off, which is why
		// the range starts there rather than at the sweep tick: "no hints" is a real choice and
		// has to be expressible.
		WordGameHintAfter: l.dur("PEREGRINE_WORDGAME_HINT_AFTER", 30*time.Second, 0, time.Hour),

		BackupDir:  l.str("PEREGRINE_BACKUP_DIR", ""),
		BackupTick: l.dur("PEREGRINE_BACKUP_TICK", 24*time.Hour, time.Minute, 30*24*time.Hour),
		BackupKeep: l.intVal("PEREGRINE_BACKUP_KEEP", 7, 1, 1000),

		TuningDir:    l.str("PEREGRINE_TUNING_DIR", ""),
		TuningRotate: l.dur("PEREGRINE_TUNING_ROTATE", 24*time.Hour, time.Minute, 30*24*time.Hour),
		TuningKeep:   l.intVal("PEREGRINE_TUNING_KEEP", 14, 1, 1000),

		// 1.0 records every generation attempt, which is the right default because a reply is
		// rare compared to a message: the bot answers when addressed. Lower it only if the
		// file is genuinely too large to move, and know what it costs, since a sampled
		// archive cannot answer questions about rare outcomes.
		TuningSample: l.float("PEREGRINE_TUNING_SAMPLE", 1.0, 0.001, 1.0),

		// Ten minutes is long enough for a reaction and short enough that a restart does not
		// truncate most windows. A truncated window is not lost, since the record carries the
		// window it actually got, but a file full of two-minute windows measures less.
		TuningEngagementWindow: l.dur("PEREGRINE_TUNING_ENGAGEMENT_WINDOW", 10*time.Minute, time.Minute, 6*time.Hour),

		// Bounds the map of replies being watched. Keyed by message ID, so this is the bound
		// that stops a leak this repository has shipped twice before.
		TuningTrackMax: l.intVal("PEREGRINE_TUNING_TRACK_MAX", 500, 10, 100_000),

		Version: l.str("PEREGRINE_VERSION", "dev"),

		EnableTranscription: l.boolVal("PEREGRINE_ENABLE_TRANSCRIPTION", false),
		TranscriptionQueue:  l.intVal("PEREGRINE_TRANSCRIPTION_QUEUE", 32, 1, 10_000),
	}

	// Cross-field checks. Each of these was previously a way to get silence.

	// The old arrangement had the feature const false AND the channel list
	// empty, so flipping either one alone produced no posts and no explanation
	// of why. Naming both variables in the error is the entire point.
	// THE RENAMED VARIABLES ARE REFUSED, NOT IGNORED. A rename that silently stops reading an
	// old value is the failure this whole file is written against, and an operator who set
	// PEREGRINE_ASSOC_BACKFILL=true would otherwise get a bot that cheerfully repairs nothing.
	for old, replacement := range map[string]string{
		"PEREGRINE_ASSOC_BACKFILL":        "PEREGRINE_REPAIR_JOBS",
		"PEREGRINE_ASSOC_BACKFILL_BEFORE": "PEREGRINE_REPAIR_BEFORE",
	} {
		if v, ok := os.LookupEnv(old); ok && v != "" {
			l.errs = append(l.errs, fmt.Errorf(
				"%s was renamed to %s in M18, when the one-shot association backfill became a "+
					"general repair mechanism. Unset %s and set %s instead", old, replacement, old, replacement))
		}
	}
	if cfg.EnableAutonomousPost && len(cfg.AutonomousPostChannels) == 0 {
		l.errs = append(l.errs, errors.New(
			"PEREGRINE_ENABLE_AUTONOMOUS_POST is true but PEREGRINE_AUTONOMOUS_POST_CHANNELS is empty: "+
				"a bot that speaks unprompted must never do so in a channel nobody named, so set the channel IDs or turn the feature off"))
	}
	// A per-author cap at or above the cache size is not a cap. One person can still own
	// every entry, so the operator believes the A7 protection is on when it does nothing,
	// which is the exact shape of silence this file exists to refuse. Naming both
	// variables is the point, because the mistake is in their relationship rather than in
	// either value.
	if cfg.EnableImageRepost && cfg.ImageMaxPerAuthor >= cfg.ImageCacheSize {
		l.errs = append(l.errs, fmt.Errorf(
			"PEREGRINE_IMAGE_MAX_PER_AUTHOR=%d is not below PEREGRINE_IMAGE_CACHE_SIZE=%d, "+
				"so one author can still fill the repost cache and the per-author cap protects nothing",
			cfg.ImageMaxPerAuthor, cfg.ImageCacheSize))
	}

	// A hint due at or after the deadline never fires, so the knob would be wired to nothing:
	// the operator sets it, no hint ever appears, and the feature gets blamed for ignoring
	// configuration. Same relationship mistake as the image cap above, and named the same way.
	if cfg.WordGameHintAfter > 0 && cfg.WordGameHintAfter >= cfg.WordGameTimeout {
		l.errs = append(l.errs, fmt.Errorf(
			"PEREGRINE_WORDGAME_HINT_AFTER=%v is not below PEREGRINE_WORDGAME_TIMEOUT=%v, so the "+
				"hint would be due after the puzzle has already ended and would never appear. "+
				"Set it lower, or to 0 to turn hints off",
			cfg.WordGameHintAfter, cfg.WordGameTimeout))
	}

	if len(l.errs) > 0 {
		return nil, errors.Join(l.errs...)
	}
	return cfg, nil
}

// RequireToken is the bot path's check, kept out of Load so maintenance modes do
// not need a credential.
func (c *Config) RequireToken() error {
	if c.Token == "" {
		return errors.New("DISCORD_BOT_TOKEN environment variable not set")
	}
	return nil
}

// Level maps LOG_LEVEL onto a slog level. LOG_LEVEL is validated as an enum in
// Load, so the default branch here is unreachable rather than lenient.
func (c *Config) Level() slog.Level {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// DeferredSet returns the variables that are set in the environment but read by
// no code yet, each with the milestone that will read it. Sorted for a stable
// log line.
// A variable is "set" here only if it has a non-empty value. os.LookupEnv alone
// reports true for an empty assignment, and .env.example deliberately ships
// several of these with no value (PEREGRINE_PAUSE_ALL_WRITES is the emergency
// stop and must default to off), so keying off presence would warn about every
// blank line an operator copied and never touched. A warning that fires on the
// stock configuration is a warning people learn to skip.
func DeferredSet() []string {
	var out []string
	for key, milestone := range deferredVars {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			out = append(out, key+" ("+milestone+")")
		}
	}
	slices.Sort(out)
	return out
}

// loader accumulates parse and range failures so Load can report all of them.
type loader struct {
	errs []error
}

func (l *loader) str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// enum rejects an unrecognized value rather than falling back to the default. A
// silent fallback means a typo in LOG_LEVEL=dbeug reads as "info" forever and
// the operator concludes debug logging is broken.
func (l *loader) enum(key, def string, allowed ...string) string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	lower := strings.ToLower(strings.TrimSpace(v))
	if slices.Contains(allowed, lower) {
		return lower
	}
	l.errs = append(l.errs, fmt.Errorf("%s=%q is not one of %s", key, v, strings.Join(allowed, ", ")))
	return def
}

func (l *loader) intVal(key string, def, minimum, maximum int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q is not an integer", key, v))
		return def
	}
	if n < minimum || n > maximum {
		l.errs = append(l.errs, fmt.Errorf("%s=%d is outside the supported range %d to %d", key, n, minimum, maximum))
		return def
	}
	return n
}

func (l *loader) float(key string, def, minimum, maximum float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q is not a number", key, v))
		return def
	}
	if f < minimum || f > maximum {
		l.errs = append(l.errs, fmt.Errorf("%s=%v is outside the supported range %v to %v", key, f, minimum, maximum))
		return def
	}
	return f
}

func (l *loader) dur(key string, def, minimum, maximum time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q is not a duration (want a form like 30s, 10m, 24h)", key, v))
		return def
	}
	if d < minimum || d > maximum {
		l.errs = append(l.errs, fmt.Errorf("%s=%v is outside the supported range %v to %v", key, d, minimum, maximum))
		return def
	}
	return d
}

// boolVal treats an unrecognized value as an error, not as false. Every bool
// here is a feature switch, and "the value you typed was not understood so the
// feature is off" is indistinguishable from "the feature is broken". This is the
// same class of silence that kept autonomous posting dark.
func (l *loader) boolVal(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		l.errs = append(l.errs, fmt.Errorf("%s=%q is not a boolean (want 1/true/yes/on or 0/false/no/off)", key, v))
		return def
	}
}

// csv splits a comma-separated list, dropping empty entries so a trailing comma
// or a value of "," does not produce a channel ID of "" that matches nothing and
// looks like a configured channel.
// timestamp reads an RFC3339 instant, or the zero Time when unset.
//
// Unset is not an error here, because the variable is only meaningful when
// PEREGRINE_ASSOC_BACKFILL is on, and Load reports THAT combination rather than this
// field in isolation. A value that does not parse is always an error, per the rule that a
// bad value must never fall back to a default.
func (l *loader) timestamp(key string) time.Time {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q is not an RFC3339 timestamp (want 2026-08-09T21:00:00Z)", key, raw))
		return time.Time{}
	}
	return t
}

func (l *loader) csv(key string) []string {
	v := os.Getenv(key)
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// regex compiles at startup so a broken pattern is a startup error rather than a
// feature that silently never matches. The self-mention pattern is what makes
// the bot answer unprompted, so a pattern that fails to compile would present as
// the bot ignoring its own name.
func (l *loader) regex(key, def string) *regexp.Regexp {
	v := l.str(key, def)
	re, err := regexp.Compile(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q does not compile as a Go regexp: %w", key, v, err))
		// Fall back to the default so the rest of validation can proceed and
		// report its own findings in the same pass.
		return regexp.MustCompile(def)
	}
	return re
}

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

	// Generation.
	MaxNGram             int     // PEREGRINE_MAX_NGRAM
	PromptRelevanceBoost float64 // PEREGRINE_PROMPT_RELEVANCE_BOOST
	SelfMention          *regexp.Regexp

	// Ingestion.
	IngestTick       time.Duration // PEREGRINE_INGEST_TICK
	IngestLookback   time.Duration // PEREGRINE_INGEST_LOOKBACK
	IngestBatchDelay time.Duration // PEREGRINE_INGEST_BATCH_DELAY

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
	StatusTick       time.Duration // PEREGRINE_STATUS_TICK
	EnableClustering bool          // PEREGRINE_ENABLE_CLUSTERING
	ClusteringTick   time.Duration // PEREGRINE_CLUSTERING_TICK
	LeaderboardTick  time.Duration // PEREGRINE_LEADERBOARD_CHECK_TICK

	// Engagement: bird aggro.
	AggroTick     time.Duration // PEREGRINE_AGGRO_TICK
	AggroChance   float64       // PEREGRINE_AGGRO_CHANCE
	AggroDuration time.Duration // PEREGRINE_AGGRO_DURATION
	AggroEmoji    string        // PEREGRINE_AGGRO_EMOJI

	// Engagement: image reposting.
	EnableImageRepost bool    // PEREGRINE_ENABLE_IMAGE_REPOST
	ImageRepostChance float64 // PEREGRINE_IMAGE_REPOST_CHANCE
	ImageRepostDirect float64 // PEREGRINE_IMAGE_REPOST_CHANCE_DIRECT
	ImageCacheSize    int     // PEREGRINE_IMAGE_CACHE_SIZE

	// Engagement: autonomous posting.
	EnableAutonomousPost   bool          // PEREGRINE_ENABLE_AUTONOMOUS_POST
	AutonomousPostChannels []string      // PEREGRINE_AUTONOMOUS_POST_CHANNELS
	AutonomousPostTick     time.Duration // PEREGRINE_AUTONOMOUS_POST_TICK
	AutonomousSkipChance   float64       // PEREGRINE_AUTONOMOUS_SKIP_CHANCE

	// Engagement: word games.
	EnableWordGames    bool          // PEREGRINE_ENABLE_WORD_GAMES
	WordGameMode       string        // PEREGRINE_WORDGAME_FREQUENCY_MODE
	WordGameInterval   time.Duration // PEREGRINE_WORDGAME_INTERVAL
	WordGameDictionary string        // PEREGRINE_WORDGAME_DICTIONARY

	// Transcription. Off by default, and that default deliberately differs from
	// the old in-code constant, which was true: transcription shells out to
	// ffmpeg and whisper-cli and needs a 465 MiB model, none of which exist in a
	// distroless image that has no shell at all. On Linux the binary lookup fails
	// and every voice note produces a failure reply, so shipping it on by default
	// meant the container's only visible transcription behavior was an error.
	EnableTranscription bool // PEREGRINE_ENABLE_TRANSCRIPTION
}

// Word game pacing modes.
const (
	WordGameModeInterval = "interval"
	WordGameModeActivity = "activity"
)

// deferredVars are documented in .env.example but read by no code yet, mapped to
// the milestone that starts reading them. Setting one today does nothing, and
// silence about that is how an operator concludes the bot ignores its own
// documentation. Load warns instead. Delete an entry when its milestone lands and
// the field appears in Config above.
var deferredVars = map[string]string{
	"PEREGRINE_BACKUP_DIR":                 "M13",
	"PEREGRINE_BACKUP_TICK":                "M13",
	"PEREGRINE_BACKUP_KEEP":                "M13",
	"PEREGRINE_BLOCKLIST_PATH":             "M5",
	"PEREGRINE_PAUSE_ALL_WRITES":           "M5",
	"PEREGRINE_MIN_DISTINCT_AUTHORS":       "M7",
	"PEREGRINE_TEMPERATURE":                "M7",
	"PEREGRINE_TOP_K":                      "M7",
	"PEREGRINE_TOP_P":                      "M7",
	"PEREGRINE_KN_DISCOUNT":                "M7",
	"PEREGRINE_KN_RAW_MIX":                 "M7",
	"PEREGRINE_MIN_WORDS":                  "M7",
	"PEREGRINE_MAX_WORDS":                  "M7",
	"PEREGRINE_COOCCURRENCE_WINDOW":        "M7",
	"PEREGRINE_ROAST_CHANCE":               "M7",
	"PEREGRINE_IMAGE_MAX_PER_AUTHOR":       "M11",
	"PEREGRINE_IGNORE_CHANNELS":            "M10",
	"PEREGRINE_INGEST_GUILD_CONCURRENCY":   "M9",
	"PEREGRINE_INGEST_CHANNEL_CONCURRENCY": "M9",
	"PEREGRINE_WHISPER_MODEL":              "M12",
	"PEREGRINE_VOICENOTES_DIR":             "M12",
	"PEREGRINE_TRANSCRIPTION_TIMEOUT":      "M12",
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

		// Minimum 2. Order 1 makes the prefix empty, and an empty prefix is one
		// bbolt key holding a map of the entire vocabulary, rewritten once per
		// word per message. Nothing reads it. The ingestion loop still descends
		// to 1 today, so this validation does not yet stop that write happening
		// (SPEC.md section 8, finding 5); M6 changes the loop.
		MaxNGram:             l.intVal("PEREGRINE_MAX_NGRAM", 5, 2, 8),
		PromptRelevanceBoost: l.float("PEREGRINE_PROMPT_RELEVANCE_BOOST", 15.0, 0, 1000),
		SelfMention:          l.regex("PEREGRINE_SELF_MENTION_PATTERN", `(?i)\b(peregrine|bird)\b`),

		IngestTick:       l.dur("PEREGRINE_INGEST_TICK", 10*time.Minute, time.Minute, 24*time.Hour),
		IngestLookback:   l.dur("PEREGRINE_INGEST_LOOKBACK", 24*time.Hour, time.Minute, 30*24*time.Hour),
		IngestBatchDelay: l.dur("PEREGRINE_INGEST_BATCH_DELAY", 500*time.Millisecond, 0, time.Minute),

		MessageWorkers: l.intVal("PEREGRINE_MESSAGE_WORKERS", 4, 1, 64),
		MessageQueue:   l.intVal("PEREGRINE_MESSAGE_QUEUE", 256, 1, 100_000),

		StatusTick: l.dur("PEREGRINE_STATUS_TICK", 5*time.Minute, time.Minute, 24*time.Hour),

		// Defaults to FALSE as of M4, which is a change from the value the code
		// previously had, and the reason is that the pass currently cannot affect
		// output at all.
		//
		// Clustering writes its members string-keyed and the generation path reads
		// them into a map[int]float32, so every unmarshal fails and both consumers
		// silently skip the cluster (SPEC.md section 8, finding 27). So the pass
		// walks the whole corpus every 24 hours inside a write transaction, against
		// bbolt's single writer, ends in a destructive bucket rebuild, and produces
		// data nothing can read. Leaving that on by default once it is known to be a
		// no-op is not defensible; the observable behavior is unchanged because the
		// output was never readable.
		//
		// M8 fixes the codec and re-enables this default, after M7's golden samples
		// exist to judge whether the clusters actually improve output. Turning it on
		// before then would add a seed branch firing at weight 50 inside a scorer
		// that is not yet normalized, with no way to evaluate the result.
		EnableClustering: l.boolVal("PEREGRINE_ENABLE_CLUSTERING", false),
		ClusteringTick:   l.dur("PEREGRINE_CLUSTERING_TICK", 24*time.Hour, time.Minute, 30*24*time.Hour),
		LeaderboardTick:  l.dur("PEREGRINE_LEADERBOARD_CHECK_TICK", time.Hour, time.Minute, 24*time.Hour),

		AggroTick:     l.dur("PEREGRINE_AGGRO_TICK", time.Hour, time.Minute, 30*24*time.Hour),
		AggroChance:   l.float("PEREGRINE_AGGRO_CHANCE", 0.20, 0, 1),
		AggroDuration: l.dur("PEREGRINE_AGGRO_DURATION", 20*time.Minute, time.Minute, 30*24*time.Hour),
		AggroEmoji:    l.str("PEREGRINE_AGGRO_EMOJI", "\U0001F426"),

		// Reposting had no flag at all: it was unconditionally on. The default is
		// true so this milestone changes no behavior, but it now has an off
		// switch, which matters because the bot republishes user media under its
		// own name (SPEC.md section 4, A7).
		EnableImageRepost: l.boolVal("PEREGRINE_ENABLE_IMAGE_REPOST", true),
		ImageRepostChance: l.float("PEREGRINE_IMAGE_REPOST_CHANCE", 0.015, 0, 1),
		ImageRepostDirect: l.float("PEREGRINE_IMAGE_REPOST_CHANCE_DIRECT", 0.01, 0, 1),
		ImageCacheSize:    l.intVal("PEREGRINE_IMAGE_CACHE_SIZE", 100, 1, 100_000),

		EnableAutonomousPost:   l.boolVal("PEREGRINE_ENABLE_AUTONOMOUS_POST", false),
		AutonomousPostChannels: l.csv("PEREGRINE_AUTONOMOUS_POST_CHANNELS"),
		// Its own variable, defaulting to the ingestion cadence it used to share.
		// Both loops were driven by one AutonomyTick constant, so tuning how often
		// the bot backfills history also changed how often it speaks unprompted:
		// two unrelated decisions on one dial, and the second one invisible.
		AutonomousPostTick:   l.dur("PEREGRINE_AUTONOMOUS_POST_TICK", 10*time.Minute, time.Minute, 30*24*time.Hour),
		AutonomousSkipChance: l.float("PEREGRINE_AUTONOMOUS_SKIP_CHANCE", 0.90, 0, 1),

		EnableWordGames: l.boolVal("PEREGRINE_ENABLE_WORD_GAMES", false),
		WordGameMode:    l.enum("PEREGRINE_WORDGAME_FREQUENCY_MODE", WordGameModeInterval, WordGameModeInterval, WordGameModeActivity),
		// 2 minutes is today's value and it is a leftover from testing rather
		// than a decision. Kept so this milestone changes no behavior; M11 picks
		// a real one when word games are turned on deliberately.
		WordGameInterval:   l.dur("PEREGRINE_WORDGAME_INTERVAL", 2*time.Minute, time.Minute, 24*time.Hour),
		WordGameDictionary: l.str("PEREGRINE_WORDGAME_DICTIONARY", ""),

		EnableTranscription: l.boolVal("PEREGRINE_ENABLE_TRANSCRIPTION", false),
	}

	// Cross-field checks. Each of these was previously a way to get silence.

	// The old arrangement had the feature const false AND the channel list
	// empty, so flipping either one alone produced no posts and no explanation
	// of why. Naming both variables in the error is the entire point.
	if cfg.EnableAutonomousPost && len(cfg.AutonomousPostChannels) == 0 {
		l.errs = append(l.errs, errors.New(
			"PEREGRINE_ENABLE_AUTONOMOUS_POST is true but PEREGRINE_AUTONOMOUS_POST_CHANNELS is empty: "+
				"a bot that speaks unprompted must never do so in a channel nobody named, so set the channel IDs or turn the feature off"))
	}
	if cfg.EnableClustering && cfg.ClusteringTick < time.Minute {
		l.errs = append(l.errs, errors.New("PEREGRINE_CLUSTERING_TICK is below one minute: clustering walks the whole corpus and would starve ingestion, which shares bbolt's single writer"))
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

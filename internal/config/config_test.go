package config

import (
	"strings"
	"testing"
	"time"
)

// clearEnv unsets every variable this package reads, so a test never inherits
// the developer's own .env through the process environment. t.Setenv restores
// values automatically, and it also fails the test if the package is running in
// parallel, which is the behavior we want for something that mutates global
// state.
func clearEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"DISCORD_BOT_TOKEN", "PEREGRINE_BOOTSTRAP_ADMIN_USER_ID", "LOG_LEVEL",
		"PEREGRINE_DB_PATH", "PEREGRINE_MAX_HISTORY", "PEREGRINE_MAX_NGRAM",
		"PEREGRINE_BLOCKLIST_PATH", "PEREGRINE_PAUSE_ALL_WRITES",
		"PEREGRINE_PROMPT_RELEVANCE_BOOST", "PEREGRINE_SELF_MENTION_PATTERN",
		"PEREGRINE_INGEST_TICK", "PEREGRINE_INGEST_LOOKBACK", "PEREGRINE_INGEST_BATCH_DELAY",
		"PEREGRINE_STATUS_TICK", "PEREGRINE_ENABLE_CLUSTERING", "PEREGRINE_CLUSTERING_TICK",
		"PEREGRINE_LEADERBOARD_CHECK_TICK",
		"PEREGRINE_AGGRO_TICK", "PEREGRINE_AGGRO_CHANCE", "PEREGRINE_AGGRO_DURATION", "PEREGRINE_AGGRO_EMOJI",
		"PEREGRINE_ENABLE_IMAGE_REPOST", "PEREGRINE_IMAGE_REPOST_CHANCE",
		"PEREGRINE_IMAGE_REPOST_CHANCE_DIRECT", "PEREGRINE_IMAGE_CACHE_SIZE",
		"PEREGRINE_ENABLE_AUTONOMOUS_POST", "PEREGRINE_AUTONOMOUS_POST_CHANNELS",
		"PEREGRINE_AUTONOMOUS_POST_TICK", "PEREGRINE_AUTONOMOUS_SKIP_CHANCE",
		"PEREGRINE_ENABLE_WORD_GAMES", "PEREGRINE_WORDGAME_FREQUENCY_MODE",
		"PEREGRINE_WORDGAME_INTERVAL", "PEREGRINE_WORDGAME_DICTIONARY",
		"PEREGRINE_ENABLE_TRANSCRIPTION",
	}
	for k := range deferredVars {
		keys = append(keys, k)
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

// TestLoadDefaults pins the defaults that M2 promised not to change. Every one of
// these is the value the corresponding constant held before it became
// configuration, so a diff here means this milestone changed behavior it said it
// would not.
func TestLoadDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with an empty environment must succeed, got: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"DBPath", cfg.DBPath, "markov.db"},
		{"MaxHistory", cfg.MaxHistory, 10000},
		{"MaxNGram", cfg.MaxNGram, 5},
		{"PromptRelevanceBoost", cfg.PromptRelevanceBoost, 15.0},
		{"IngestTick", cfg.IngestTick, 10 * time.Minute},
		{"IngestLookback", cfg.IngestLookback, 24 * time.Hour},
		{"IngestBatchDelay", cfg.IngestBatchDelay, 500 * time.Millisecond},
		{"StatusTick", cfg.StatusTick, 5 * time.Minute},
		{"ClusteringTick", cfg.ClusteringTick, 24 * time.Hour},
		{"LeaderboardTick", cfg.LeaderboardTick, time.Hour},
		{"AggroTick", cfg.AggroTick, time.Hour},
		{"AggroChance", cfg.AggroChance, 0.20},
		{"AggroDuration", cfg.AggroDuration, 20 * time.Minute},
		{"ImageRepostChance", cfg.ImageRepostChance, 0.015},
		{"ImageRepostDirect", cfg.ImageRepostDirect, 0.01},
		{"ImageCacheSize", cfg.ImageCacheSize, 100},
		{"AutonomousPostTick", cfg.AutonomousPostTick, 10 * time.Minute},
		{"AutonomousSkipChance", cfg.AutonomousSkipChance, 0.90},
		{"WordGameMode", cfg.WordGameMode, WordGameModeInterval},
		{"WordGameInterval", cfg.WordGameInterval, 2 * time.Minute},
		{"EnableClustering", cfg.EnableClustering, false},
		// Both safety defaults are permissive-looking and are checked here so that
		// changing either is a visible decision. An unset blocklist path is allowed
		// (cmd/bot warns loudly), and writes are not paused by default because the
		// pause is an incident lever, not a posture.
		{"BlocklistPath", cfg.BlocklistPath, ""},
		{"PauseAllWrites", cfg.PauseAllWrites, false},
		{"EnableImageRepost", cfg.EnableImageRepost, true},
		{"EnableAutonomousPost", cfg.EnableAutonomousPost, false},
		{"EnableWordGames", cfg.EnableWordGames, false},
		// Deliberately differs from the old in-code constant, which was true.
		// See the field comment: none of the transcription toolchain exists in a
		// distroless image, so on by default meant the only visible behavior in
		// production was a failure reply per voice note.
		{"EnableTranscription", cfg.EnableTranscription, false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	if cfg.AggroEmoji != "\U0001F426" {
		t.Errorf("AggroEmoji = %q, want the bird emoji", cfg.AggroEmoji)
	}
	if cfg.SelfMention == nil || !cfg.SelfMention.MatchString("that BIRD again") {
		t.Error("default self-mention pattern must match its own name case-insensitively")
	}
	if len(cfg.AutonomousPostChannels) != 0 {
		t.Errorf("AutonomousPostChannels = %v, want empty", cfg.AutonomousPostChannels)
	}
}

// TestLoadReportsEveryError is the regression pin for the reason Load
// accumulates: a container that reports one bad variable per restart makes an
// operator debug a multi-variable mistake one deploy at a time.
func TestLoadReportsEveryError(t *testing.T) {
	clearEnv(t)
	t.Setenv("PEREGRINE_MAX_NGRAM", "not-a-number")
	t.Setenv("PEREGRINE_AGGRO_CHANCE", "7")           // out of range
	t.Setenv("PEREGRINE_INGEST_TICK", "10 parsecs")   // not a duration
	t.Setenv("PEREGRINE_ENABLE_WORD_GAMES", "ture")   // typo, must not read as false
	t.Setenv("LOG_LEVEL", "dbeug")                    // typo, must not read as info
	t.Setenv("PEREGRINE_SELF_MENTION_PATTERN", "a(b") // does not compile

	_, err := Load()
	if err == nil {
		t.Fatal("Load must fail when variables are invalid")
	}
	msg := err.Error()
	for _, want := range []string{
		"PEREGRINE_MAX_NGRAM",
		"PEREGRINE_AGGRO_CHANCE",
		"PEREGRINE_INGEST_TICK",
		"PEREGRINE_ENABLE_WORD_GAMES",
		"LOG_LEVEL",
		"PEREGRINE_SELF_MENTION_PATTERN",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must name %s; got:\n%s", want, msg)
		}
	}
}

// TestBadBoolIsAnErrorNotFalse is called out separately because it is the exact
// shape of the bug that kept autonomous posting dark: a value the code did not
// understand became "feature off", which is indistinguishable from "feature
// broken".
func TestBadBoolIsAnErrorNotFalse(t *testing.T) {
	for _, v := range []string{"ture", "enabled", "y", "2", "-1"} {
		t.Run(v, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("PEREGRINE_ENABLE_IMAGE_REPOST", v)
			if _, err := Load(); err == nil {
				t.Errorf("PEREGRINE_ENABLE_IMAGE_REPOST=%q must be an error, not a silent default", v)
			}
		})
	}
}

func TestBoolAcceptedForms(t *testing.T) {
	cases := map[string]bool{
		"1": true, "true": true, "TRUE": true, "yes": true, "on": true, " True ": true,
		"0": false, "false": false, "FALSE": false, "no": false, "off": false,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("PEREGRINE_ENABLE_WORD_GAMES", in)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", in, err)
			}
			if cfg.EnableWordGames != want {
				t.Errorf("EnableWordGames for %q = %v, want %v", in, cfg.EnableWordGames, want)
			}
		})
	}
}

// TestAutonomousPostWithoutChannelsFails pins the cross-field check. The previous
// arrangement had the feature constant false AND the channel list empty, so
// flipping either one alone produced no posts and no explanation of why.
func TestAutonomousPostWithoutChannelsFails(t *testing.T) {
	clearEnv(t)
	t.Setenv("PEREGRINE_ENABLE_AUTONOMOUS_POST", "true")

	_, err := Load()
	if err == nil {
		t.Fatal("enabling autonomous posting with no channels must be a startup error")
	}
	// Both variables must be named. Naming only one leaves the operator to guess
	// which half of the pair to change, which is how this stayed broken.
	msg := err.Error()
	if !strings.Contains(msg, "PEREGRINE_ENABLE_AUTONOMOUS_POST") || !strings.Contains(msg, "PEREGRINE_AUTONOMOUS_POST_CHANNELS") {
		t.Errorf("error must name both variables; got:\n%s", msg)
	}
}

func TestAutonomousPostWithChannelsSucceeds(t *testing.T) {
	clearEnv(t)
	t.Setenv("PEREGRINE_ENABLE_AUTONOMOUS_POST", "yes")
	t.Setenv("PEREGRINE_AUTONOMOUS_POST_CHANNELS", " 123 , 456 ,, ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty entries are dropped rather than kept: a channel ID of "" matches
	// nothing and would read in a log line as a configured channel.
	want := []string{"123", "456"}
	if len(cfg.AutonomousPostChannels) != len(want) {
		t.Fatalf("channels = %v, want %v", cfg.AutonomousPostChannels, want)
	}
	for i := range want {
		if cfg.AutonomousPostChannels[i] != want[i] {
			t.Errorf("channels[%d] = %q, want %q", i, cfg.AutonomousPostChannels[i], want[i])
		}
	}
}

// TestMaxNGramFloorIsTwo pins the floor. Order 1 makes the n-gram prefix empty,
// and an empty prefix is a single bbolt key holding a map of the entire
// vocabulary, rewritten once per word per message, that nothing ever reads.
func TestMaxNGramFloorIsTwo(t *testing.T) {
	for _, v := range []string{"1", "0", "-3"} {
		t.Run(v, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("PEREGRINE_MAX_NGRAM", v)
			if _, err := Load(); err == nil {
				t.Errorf("PEREGRINE_MAX_NGRAM=%s must be rejected", v)
			}
		})
	}
}

// TestLoadDoesNotRequireToken is the pin for maintenance modes. -clean-db runs
// against the corpus and never touches Discord, so requiring a token would mean
// an operator cleaning a poisoned corpus needs a live credential to do it.
func TestLoadDoesNotRequireToken(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load must succeed without a token: %v", err)
	}
	if err := cfg.RequireToken(); err == nil {
		t.Error("RequireToken must fail when DISCORD_BOT_TOKEN is unset")
	}

	t.Setenv("DISCORD_BOT_TOKEN", "not-a-real-token")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := cfg.RequireToken(); err != nil {
		t.Errorf("RequireToken must pass when the token is set: %v", err)
	}
}

// TestAdminUserIDFailsClosed guards the direction of the only authorization check
// in the codebase. Until M2 it was a user ID hardcoded in the source; an empty
// value must refuse everyone rather than admit everyone.
func TestAdminUserIDFailsClosed(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AdminUserID != "" {
		t.Fatalf("AdminUserID = %q, want empty by default", cfg.AdminUserID)
	}
	// The call site is `cfg.AdminUserID != "" && author == cfg.AdminUserID`. This
	// asserts the property that makes that correct: an empty configured ID never
	// equals a real Discord snowflake, so the guard cannot be satisfied by an
	// unset variable even if a caller forgets the emptiness check.
	if cfg.AdminUserID == "184693515748507648" {
		t.Error("an unset admin ID must not match any real user ID")
	}
}

func TestDeferredSet(t *testing.T) {
	clearEnv(t)
	if got := DeferredSet(); len(got) != 0 {
		t.Fatalf("DeferredSet with nothing set = %v, want empty", got)
	}

	t.Setenv("PEREGRINE_TOP_K", "40")
	t.Setenv("PEREGRINE_KN_RAW_MIX", "0.25")

	got := DeferredSet()
	if len(got) != 2 {
		t.Fatalf("DeferredSet = %v, want 2 entries", got)
	}
	// Sorted, so the log line is stable rather than reordering per run on Go's
	// randomized map iteration.
	if got[0] != "PEREGRINE_KN_RAW_MIX (M7)" || got[1] != "PEREGRINE_TOP_K (M7)" {
		t.Errorf("DeferredSet = %v, want sorted entries carrying their milestone", got)
	}
}

// TestDeferredVarsAreNotAlsoLive catches the mistake this table invites: a
// milestone starts reading a variable, adds the field, and forgets to delete the
// deferred entry, so the bot warns that a variable it now honors is ignored.
func TestDeferredVarsAreNotAlsoLive(t *testing.T) {
	// This value parses as none of the supported types: not an integer, not a
	// float, not a duration, not a bool, and not a member of any enum. So if Load
	// reads the variable at all it must report an error, and if it does not read
	// it, Load succeeds. Either outcome is a fact about whether the key is
	// genuinely deferred. (A NUL byte would be a stronger probe but Go refuses to
	// put one in the environment.)
	const notValidAsAnything = "!!! not a valid value for anything !!!"

	// Prove the probe discriminates before trusting it. Without this the test
	// would pass just as happily if Load stopped validating anything at all.
	for _, liveKey := range []string{
		"PEREGRINE_MAX_NGRAM", "PEREGRINE_AGGRO_CHANCE", "PEREGRINE_INGEST_TICK",
		"PEREGRINE_ENABLE_IMAGE_REPOST", "PEREGRINE_WORDGAME_FREQUENCY_MODE",
	} {
		clearEnv(t)
		t.Setenv(liveKey, notValidAsAnything)
		if _, err := Load(); err == nil {
			t.Fatalf("probe value is not discriminating: Load accepted %s=%q, so this test cannot detect a deferred key that is actually live", liveKey, notValidAsAnything)
		}
	}

	for key := range deferredVars {
		clearEnv(t)
		t.Setenv(key, notValidAsAnything)
		if _, err := Load(); err != nil {
			t.Errorf("%s is listed as deferred but Load validates it (%v): remove it from deferredVars", key, err)
		}
	}
}

func TestLevel(t *testing.T) {
	for in, wantString := range map[string]string{
		"debug": "DEBUG", "info": "INFO", "warn": "WARN", "error": "ERROR",
	} {
		clearEnv(t)
		t.Setenv("LOG_LEVEL", in)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("LOG_LEVEL=%q: %v", in, err)
		}
		if got := cfg.Level().String(); got != wantString {
			t.Errorf("LOG_LEVEL=%q gave level %s, want %s", in, got, wantString)
		}
	}
}

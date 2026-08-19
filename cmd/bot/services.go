package main

import (
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/activity"
	"github.com/6586x57890143/peregrine/internal/channels"
	"github.com/6586x57890143/peregrine/internal/config"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/discordguard"
	"github.com/6586x57890143/peregrine/internal/generate"
	"github.com/6586x57890143/peregrine/internal/learn"
	"github.com/6586x57890143/peregrine/internal/names"
	"github.com/6586x57890143/peregrine/internal/plugins/aggro"
	"github.com/6586x57890143/peregrine/internal/plugins/autopost"
	"github.com/6586x57890143/peregrine/internal/plugins/backup"
	"github.com/6586x57890143/peregrine/internal/plugins/chat"
	"github.com/6586x57890143/peregrine/internal/plugins/games"
	"github.com/6586x57890143/peregrine/internal/plugins/health"
	"github.com/6586x57890143/peregrine/internal/plugins/images"
	"github.com/6586x57890143/peregrine/internal/plugins/ingest"
	"github.com/6586x57890143/peregrine/internal/plugins/repair"
	"github.com/6586x57890143/peregrine/internal/plugins/tuning"
	"github.com/6586x57890143/peregrine/internal/plugins/voicenote"
	"github.com/6586x57890143/peregrine/internal/safety"
	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/wordgame"
)

// registerServices builds every feature and registers it.
//
// This is the only place in the program where the features are named together, which is
// deliberate: internal/core owns the mechanisms and imports no feature package, so
// registration cannot happen anywhere else (SPEC.md section 2).
//
// # The order is behaviour
//
// Init runs in registration order and Shutdown in reverse, so this list decides two things
// that matter. chat is registered LAST among the message-handling features, because its Init
// registers the gateway handler and Start resolves the bot's own identity: everything it
// calls into has to have finished loading its state before a message can arrive. And its
// position means it shuts down FIRST, before the features it calls, so a step cannot run
// against a half-stopped plugin.
func registerServices(
	registry *core.Registry,
	cfg *config.Config,
	session *discordgo.Session,
	corpora *storage.Set,
	gate *safety.Gate,
	dispatcher *core.Dispatcher,
	log *slog.Logger,
) error {
	// The guard is the single chokepoint every outbound call goes through. Built here, once,
	// and handed to every feature that sends anything: a rule applied at thirteen of fourteen
	// send sites is not a rule (SPEC.md section 8, finding 8).
	guard := discordguard.New(session, emitGate{gate: gate}, log, cfg.IgnoreChannels)

	// Where people are talking and who is around, counted from the messages the gateway
	// already delivers. One tracker for four consumers, each bringing its own window.
	tracker := activity.New(activity.Options{})
	resolver := channels.FromSession(session)

	// The corpus writer. One per process, and the only way anything enters the corpus.
	learner := learn.New(gate, learn.Options{
		MaxNGram:           cfg.MaxNGram,
		MaxHistory:         cfg.MaxHistory,
		CooccurrenceWindow: cfg.CooccurrenceWindow,
	})

	// Generation, plus the per-channel conversation memory it reads. The memory is shared
	// between the reply path and the autonomous poster, which is why it is built here rather
	// than owned by either.
	memories := generate.NewMemories(0)
	dials := generate.Options{
		MaxNGram:           cfg.MaxNGram,
		MinWords:           cfg.MinWords,
		MaxWords:           cfg.MaxWords,
		Temperature:        cfg.Temperature,
		TopK:               cfg.TopK,
		TopP:               cfg.TopP,
		KNDiscount:         cfg.KNDiscount,
		KNRawMix:           cfg.KNRawMix,
		MinDistinctAuthors: cfg.MinDistinctAuthors,
		SoloRepeatLimit:    cfg.SoloRepeatLimit,
		SoloMaxOrder:       cfg.SoloMaxOrder,
		PromptRelevance:    cfg.PromptRelevanceBoost,
		RoastChance:        cfg.RoastChance,
	}
	speaker := generate.New(corpora, dials)
	emoji := core.SessionEmoji(session)

	// One member cache for every path that resolves a nickname. Bounded and TTL'd, because a
	// permanent one makes the bot use a stale name forever and the map is keyed by person,
	// which is a leak this repository has shipped twice (M18).
	members := names.NewCachedSession(session, memberCacheTTL, memberCacheMax)

	// The tuning export. Named as a variable before the features that record into it,
	// because three of them take it: the reactor, the autonomous poster and the health
	// report. It is a real service with a nil-safe off state rather than a nil interface, so
	// nothing downstream has to know whether PEREGRINE_TUNING_DIR was set.
	//
	// The SAME dials the generator was built with go into it, deliberately: an archive that
	// recorded output without recording the numbers that produced it cannot be compared to
	// the next one, which is the entire point of exporting anything.
	tuningSvc := tuning.New(session, tuning.Options{
		Dir:              cfg.TuningDir,
		Rotate:           cfg.TuningRotate,
		Keep:             cfg.TuningKeep,
		Sample:           cfg.TuningSample,
		EngagementWindow: cfg.TuningEngagementWindow,
		TrackMax:         cfg.TuningTrackMax,
		Version:          cfg.Version,
		Dials:            dials,
	})

	// The word-game dictionary. Deliberately not fatal: word games are one optional feature,
	// and taking the whole bot down because a 64 KB word list would not load meant an
	// unrelated asset problem killed learning, generation and every other behaviour with it.
	// A nil dictionary makes Manager.Available report false and every entry point decline.
	dict, err := wordgame.LoadDictionary(cfg.WordGameDictionary, wordgame.DictionaryOptions{
		MinLength: cfg.WordGameMinLength,
		MaxLength: cfg.WordGameMaxLength,
	})
	if err != nil {
		log.Warn("word game dictionary failed to load, word games disabled", "err", err)
	} else {
		log.Info("word game dictionary loaded", "words", dict.Len())
	}
	manager := wordgame.NewManager(dict, nil, tracker, wordgame.Options{
		Timeout:           cfg.WordGameTimeout,
		AnnounceTTL:       cfg.WordGameAnnounceTTL,
		ActivityWindow:    cfg.WordGameActivityWindow,
		ActivityThreshold: cfg.WordGameActivityThreshold,
		TriggerChance:     cfg.WordGameTriggerChance,
		HintAfter:         cfg.WordGameHintAfter,
		HintLevels:        cfg.WordGameHintLevels,
		GauntletMax:       cfg.WordGameGauntletMax,
		GauntletGap:       cfg.WordGameGauntletGap,
	})

	// The reactor is built before the features it calls, because it hands them nothing: they
	// are constructed here and passed in. It is REGISTERED after them, which is what the
	// ordering note above is about.
	aggroSvc := aggro.New(corpora, guard, tracker, aggro.Options{
		Chance:   cfg.AggroChance,
		Duration: cfg.AggroDuration,
		Tick:     cfg.AggroTick,
		Emoji:    cfg.AggroEmoji,
		Window:   cfg.AggroActivityWindow,
	})
	imagesSvc := images.New(corpora, guard, resolver, images.Options{
		Chance:       cfg.ImageRepostChance,
		Direct:       cfg.ImageRepostDirect,
		CacheSize:    cfg.ImageCacheSize,
		MaxPerAuthor: cfg.ImageMaxPerAuthor,
	})
	// Word games take their own channel allowlist, falling back to the autonomous-post one:
	// that list is what restricted interval mode before PEREGRINE_WORDGAME_CHANNELS existed,
	// so an operator who set only the old one keeps the behaviour they had.
	wordGameChannels := cfg.WordGameChannels
	if len(wordGameChannels) == 0 {
		wordGameChannels = cfg.AutonomousPostChannels
	}
	gamesSvc := games.New(corpora, guard, manager, tracker, resolver, games.Options{
		Enabled:             cfg.EnableWordGames,
		Mode:                games.Mode(cfg.WordGameMode),
		Interval:            cfg.WordGameInterval,
		LeaderboardTick:     cfg.LeaderboardTick,
		SweepTick:           cfg.WordGameSweepTick,
		ActiveChannelWindow: cfg.ActiveChannelWindow,
		AllowChannels:       wordGameChannels,
		AdminUserID:         cfg.AdminUserID,
		PointsBase:          cfg.WordGamePointsBase,
	})
	autopostSvc := autopost.New(guard, speaker, memories, tracker, resolver, emoji, tuningSvc, autopost.Options{
		Enabled:             cfg.EnableAutonomousPost,
		Tick:                cfg.AutonomousPostTick,
		SkipChance:          cfg.AutonomousSkipChance,
		Channels:            cfg.AutonomousPostChannels,
		ActiveChannelWindow: cfg.ActiveChannelWindow,
	})

	// Transcription, over an Engine seam whose only implementation in this repository is a
	// stub. StubEngine is named rather than passed as nil, so what is being wired is visible
	// here: swapping in a real engine is this one argument.
	voiceSvc := voicenote.New(voicenote.StubEngine(), guard, voicenote.Options{
		Enabled:   cfg.EnableTranscription,
		QueueSize: cfg.TranscriptionQueue,
	})

	reactor := chat.New(chat.Deps{
		Session:  session,
		Corpora:  corpora,
		Gate:     gate,
		Guard:    guard,
		Learner:  learner,
		Speaker:  speaker,
		Memories: memories,
		Emoji:    emoji,
		Activity: tracker,
		Aggro:    aggroSvc,
		Images:   imagesSvc,
		Games:    gamesSvc,
		Voice:    voiceSvc,
		Recorder: tuningSvc,
		// The member cache, shared with the ingest and mention paths. discordgo's GuildMember
		// is an unconditional REST GET, and the leaderboard's name lookups were the last call
		// site in the module that did not go through this.
		Members: members,
		Options: chat.Options{
			SelfMention:  cfg.SelfMention,
			RoastChance:  cfg.RoastChance,
			EnableImages: cfg.EnableImageRepost,
			EnableVoice:  cfg.EnableTranscription,
		},
	})

	// The aggro feature needs the bot's own ID to take a reaction back, and only the reactor
	// learns it (from READY). Wired as a closure rather than a value for that reason: at
	// registration time nobody knows it yet.
	aggroSvc.SetBotID(reactor.BotID)

	// Ingestion, the corpus snapshots and the health report. These three were the last things
	// in internal/legacy, which M13 deleted.
	ingestSvc := ingest.New(session, corpora, learner, ingest.Options{
		Tick:               cfg.IngestTick,
		Lookback:           cfg.IngestLookback,
		GuildConcurrency:   cfg.IngestGuildConcurrency,
		ChannelConcurrency: cfg.IngestChannelConcurrency,
		BatchDelay:         cfg.IngestBatchDelay,
	})
	// History repair. Registered unconditionally and gated inside Start, so an operator
	// turning it on is a restart rather than a different binary, and so the service can
	// report what it decided.
	repairSvc := repair.New(session, corpora, learner, repair.Options{
		Enabled:  cfg.RepairJobs,
		Override: cfg.RepairBefore,
		// Gentler than the live pass on purpose: a repair has no deadline and the bot does,
		// so it yields REST budget rather than competing for it.
		GuildConcurrency:   1,
		ChannelConcurrency: 2,
		BatchDelay:         time.Second,
		Retry:              time.Hour,
	})
	backupSvc := backup.New(corpora, backup.Options{
		Dir:   cfg.BackupDir,
		Every: cfg.BackupTick,
		Keep:  cfg.BackupKeep,
	})
	healthSvc := health.New(health.Deps{
		Corpora:  corpora,
		Queue:    dispatcher,
		Gate:     gate,
		Latency:  health.SessionLatency(session),
		Reporter: tuningSvc,
		Presence: guard,
		// The corpus word source, wired only when the operator wants that variant. Passing it
		// unconditionally would be harmless and would also mean the chance dial had two ways
		// to be off, which is one more than a knob should have.
		Topics: health.CorpusTopics(corpora),
	}, health.Options{
		StatusTick:  cfg.StatusTick,
		LatencyTick: latencyTick,
		Threshold:   latencyThreshold,
	}, health.PresenceOptions{
		Enabled:          cfg.EnablePresence,
		CorpusWordChance: cfg.PresenceCorpusWordChance,
	})

	// Registration order, and it is behaviour rather than taste. See the note above this
	// function: chat goes last among the message-handling features because its Init arms the
	// gateway handler, and health goes last overall so its Shutdown report is the final word.
	// FIRST, so it shuts down LAST. Shutdown runs in reverse registration order, and the
	// export has to outlive everything that records into it: a flush that happened before
	// the reactor stopped would lose the last replies, which are the ones an operator
	// investigating a shutdown most wants.
	registry.Register(tuningSvc)
	registry.Register(aggroSvc)
	registry.Register(imagesSvc)
	registry.Register(gamesSvc)
	registry.Register(autopostSvc)
	registry.Register(voiceSvc)
	registry.Register(reactor)
	registry.Register(ingestSvc)
	// After ingest, so a restart that resumes both starts the live pass first: the walk that
	// keeps the corpus current matters more than the one repairing history, and they compete
	// for the same REST budget.
	registry.Register(repairSvc)
	registry.Register(backupSvc)
	registry.Register(healthSvc)
	return nil
}

// The two health dials that are not configurable, and why.
//
// An operator has no reason to tune either: the latency check is cheap (it reads a value
// discordgo already maintains rather than making a request) and the threshold is the line between
// "worth a log line" and "noise that trains you to stop reading it". Neither is a decision an
// incident would change, which is the test internal/config applies to whether something deserves
// a variable.
const (
	latencyTick      = 2 * time.Minute
	latencyThreshold = 500 * time.Millisecond
)

// The member cache's bounds, and neither is caution.
//
// A permanent cache would make the bot address somebody by a nickname they changed weeks ago,
// which is the opposite of what the name path is for. And the map is keyed by person, so
// without a size bound it grows with everyone the bot ever meets: this repository has shipped
// that exact leak twice, in the conversation memory before M7b and in the word-game activity
// map in M11a.
//
// Not configurable for the same reason the two health dials above are not: an operator has no
// incident in which they would change either number.
const (
	memberCacheTTL = 15 * time.Minute
	memberCacheMax = 2000
)

// emitGate adapts the safety gate to the guard's narrower interface.
//
// The guard needs one boolean and the gate returns a verdict with a reason, so the adapter
// throws the reason away here and the guard logs its own refusal. That asymmetry is
// deliberate: internal/safety returns a reason so the caller can decide what to record, and
// the caller here has already decided.
type emitGate struct{ gate *safety.Gate }

func (e emitGate) CheckEmit(text string) bool { return e.gate.CheckEmit(text).Allowed }

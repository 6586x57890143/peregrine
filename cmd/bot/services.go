package main

import (
	"log/slog"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/activity"
	"github.com/6586x57890143/peregrine/internal/channels"
	"github.com/6586x57890143/peregrine/internal/config"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/discordguard"
	"github.com/6586x57890143/peregrine/internal/generate"
	"github.com/6586x57890143/peregrine/internal/learn"
	"github.com/6586x57890143/peregrine/internal/legacy"
	"github.com/6586x57890143/peregrine/internal/plugins/aggro"
	"github.com/6586x57890143/peregrine/internal/plugins/autopost"
	"github.com/6586x57890143/peregrine/internal/plugins/chat"
	"github.com/6586x57890143/peregrine/internal/plugins/games"
	"github.com/6586x57890143/peregrine/internal/plugins/images"
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
	store *storage.Store,
	gate *safety.Gate,
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
	speaker := generate.New(store, generate.Options{
		MaxNGram:           cfg.MaxNGram,
		MinWords:           cfg.MinWords,
		MaxWords:           cfg.MaxWords,
		Temperature:        cfg.Temperature,
		TopK:               cfg.TopK,
		TopP:               cfg.TopP,
		KNDiscount:         cfg.KNDiscount,
		KNRawMix:           cfg.KNRawMix,
		MinDistinctAuthors: cfg.MinDistinctAuthors,
		PromptRelevance:    cfg.PromptRelevanceBoost,
		RoastChance:        cfg.RoastChance,
	})
	emoji := core.SessionEmoji(session)

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
	})

	// The reactor is built before the features it calls, because it hands them nothing: they
	// are constructed here and passed in. It is REGISTERED after them, which is what the
	// ordering note above is about.
	aggroSvc := aggro.New(store, guard, tracker, aggro.Options{
		Chance:   cfg.AggroChance,
		Duration: cfg.AggroDuration,
		Tick:     cfg.AggroTick,
		Emoji:    cfg.AggroEmoji,
		Window:   cfg.AggroActivityWindow,
	})
	imagesSvc := images.New(store, guard, resolver, images.Options{
		Chance:       cfg.ImageRepostChance,
		Direct:       cfg.ImageRepostDirect,
		CacheSize:    cfg.ImageCacheSize,
		MaxPerAuthor: cfg.ImageMaxPerAuthor,
	})
	gamesSvc := games.New(store, guard, manager, tracker, resolver, games.Options{
		Enabled:             cfg.EnableWordGames,
		Mode:                games.Mode(cfg.WordGameMode),
		Interval:            cfg.WordGameInterval,
		LeaderboardTick:     cfg.LeaderboardTick,
		SweepTick:           cfg.WordGameSweepTick,
		ActiveChannelWindow: cfg.ActiveChannelWindow,
		AllowChannels:       cfg.AutonomousPostChannels,
		AdminUserID:         cfg.AdminUserID,
	})
	autopostSvc := autopost.New(guard, speaker, memories, tracker, resolver, emoji, autopost.Options{
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
		Store:    store,
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

	// Registration order. legacy still owns ingestion and the status line, and M13 is what
	// deletes it.
	registry.Register(aggroSvc)
	registry.Register(imagesSvc)
	registry.Register(gamesSvc)
	registry.Register(autopostSvc)
	registry.Register(voiceSvc)
	registry.Register(reactor)
	registry.Register(legacy.New(learner))
	return nil
}

// emitGate adapts the safety gate to the guard's narrower interface.
//
// The guard needs one boolean and the gate returns a verdict with a reason, so the adapter
// throws the reason away here and the guard logs its own refusal. That asymmetry is
// deliberate: internal/safety returns a reason so the caller can decide what to record, and
// the caller here has already decided.
type emitGate struct{ gate *safety.Gate }

func (e emitGate) CheckEmit(text string) bool { return e.gate.CheckEmit(text).Allowed }

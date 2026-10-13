# CLAUDE.md

Guidance for Claude Code working in this repository.

## What this is

**Start here after a context reset: [`HANDOFF.md`](./HANDOFF.md)** (branches, last milestone, what
is deliberately not done). Update it at the end of a milestone.

`peregrine` is a Markov-chain Discord engagement bot (Go 1.25, `discordgo` v0.29, `bbolt` v1.5)
that learns from channel messages and replies in the server's own voice. Design doc:
[`SPEC.md`](./SPEC.md); read §4 (safety) and §5 (generation) before any non-trivial change.
"Finding N" and milestone numbers ("M24") below and in code comments refer to the findings
catalogue and milestone table cut from the public SPEC; read them with
`git show 4aaf753:SPEC.md` (§8 and §9). Do not re-add history to SPEC, README or HANDOFF. The bot lives in a meme server: **output that lands matters more than output that
is grammatical**, and several things that look like bugs are deliberate register.

Layout: `cmd/bot` (entrypoint: `main.go` picks the mode, `services.go` is the only place features
are named together) plus `internal/`. `internal/legacy` is deleted (M13); a comment referring to it
is stale.

## Commands

```sh
go build ./...
go vet ./...
golangci-lint run                             # CI pins v2.12.2
go test ./... -cover
go test ./... -race -cover -covermode=atomic  # CI exactly; needs cgo + gcc, so CI-only on stock Windows
go test ./internal/markov/ -run TestGenerateGolden -v  # print golden samples and READ them
govulncheck ./...
```

Judge an engine change on real text with `tools/replay`, not only the golden fixture. Average a
few seeds before believing a one or two point difference:
```sh
go run ./tools/replay -db ./snapshot.db -tuning ./tuning -n 400 -q -seed 1
go run ./tools/replay -db ./snapshot.db -tuning ./tuning -n 100 -misses
```

CI's prose check fails on em dashes, ellipsis characters and curly quotes. Run before pushing:
```sh
em=$'\342\200\224' ell=$'\342\200\246' ldq=$'\342\200\234' rdq=$'\342\200\235'
grep -rnI --exclude-dir=.git -e "$em" -e "$ell" -e "$ldq" -e "$rdq" .
```
**Exception:** `tokenRegex` in `internal/text/text.go` contains a literal right single quote so
curly-apostrophe contractions tokenize. CI deliberately does not scan for it. Do not remove it;
`TestTokenizeCurlyApostropheRegression` guards it.

Run locally:
```sh
cp .env.example .env      # DISCORD_BOT_TOKEN is the only required variable
go run ./cmd/bot          # build ./cmd/bot, not . (the root is not a main package)
docker compose up --build
```

Corpus maintenance modes (no Discord, no token, exclusive lock, one at a time, operator picks the
order):
```sh
go run ./cmd/bot -clean-db
go run ./cmd/bot -compact ./markov.new      # bbolt never shrinks its file
go run ./cmd/bot -purge-author 123456789
```
Read-only modes:
```sh
PEREGRINE_DB_PATH=./snapshot.db go run ./cmd/bot -corpus-report  # shared lock: use a snapshot, not a live corpus
go run ./cmd/bot -tuning-report ./tuning                         # reads JSONL only; no .env, token or corpus
```

## Conventions

- Plain punctuation only (see the exception above).
- Comments explain *why* and name the failure mode that motivated the code.
- `main` is protected: PR review plus CI, no direct or force pushes.
- Before a PR: `go vet`, `go test ./... -cover`, `golangci-lint run`, the prose grep.
- One milestone (or slice) per PR per `SPEC.md` §9; green at every commit.
- When a fix changes generation input, make the golden harness exercise it in the same change. A
  harness that silently omits a code path reports on a bot that does not exist (this has happened
  three times: names, `aboutName`, conversation memory).

## Architecture invariants

### Process and lifecycle

- `cmd/bot` owns the process; `internal/core` owns the mechanisms and imports no feature package.
  Registration happens only in `cmd/bot`'s `registerServices`. Order there is behaviour: `Init` in
  registration order, `Shutdown` in reverse; `chat` is registered last.
- Startup: open corpus, build session, `InitAll`, arm `WatchReady`, `session.Open()`, wait for
  READY, start dispatcher, `StartAll`. `WatchReady` must be armed **before** `Open`, and gateway
  handlers register in `Init`, not `Start`, because discordgo dispatches inside `Open`. `Open`
  returning nil only means identify was sent; `WatchReady` turns a rejected identify (4014,
  missing privileged intent) into a startup error.
- Shutdown: `session.Close()`, `dispatcher.Shutdown()`, `registry.ShutdownAll()`, then
  `store.Close()` via defer, strictly last. One 8 s budget covers the middle two (under Docker's
  10 s).
- No `os.Exit`/`log.Fatal` outside `main`: it skips defers and leaves the bbolt flock held.
  `log.Fatal`-class failures are only "token missing" and "corpus will not open". One feature
  failing (e.g. the word list) disables that feature, not the bot.
- No goroutine per event outside `core`. `core.Dispatcher` is a bounded pool; `Submit` never
  blocks, a full queue drops and counts. Background work uses `core.RunLoop` (panic-isolated,
  context-bound), never a hand-rolled ticker. No package-level stop channel or WaitGroup.
- No `*rand.Rand` anywhere. Use `math/rand/v2` top-level functions; generation draws go through
  `markov.Source` (tests pass a seeded PCG).
- Logging: `slog.SetLogLoggerLevel` must be called **before** `slog.SetDefault`, or bridged
  `log.Printf` records all arrive at Info. Older plugins still use `log.Printf`; newer ones use
  slog directly.
- Any per-channel, per-user or per-message map must be bounded. This repo has shipped that leak
  more than twice.

### Configuration (`internal/config`)

Environment variables only, validated once into one `*Config`. No `config.yaml`.
- Every `Config` field is read by code that exists. Variables for future milestones live in
  `.env.example` and `deferredVars` (startup warns); `TestDeferredVarsAreNotAlsoLive` guards it.
  Deleted variables are deleted, not deferred.
- When a variable's units change, keep the name and narrow the range so a stale value is a startup
  error (rescale-and-refuse over rename).
- `Load` reports every problem (`errors.Join`, logged one record each). An unparseable value is an
  error, never the default. Booleans accept `1/true/yes/on`, `0/false/no/off`.
- `Load` does not require the token; `cfg.RequireToken()` is the bot path's check.
- Paths are never relative to the working directory. `PEREGRINE_DB_PATH` is mandatory in a
  container (`read_only: true`); the word list is `go:embed`ded.

### Storage (`internal/storage`)

- **Only `internal/storage` imports bbolt**; `TestOnlyThisPackageReachesBbolt` walks the module.
  Everything else gets a `*storage.Reader`/`*storage.Writer` bound to one transaction. `Reader` has
  no method that opens a transaction, so nested transactions (which deadlock bbolt: finding 1)
  cannot be written. `Writer` embeds `Reader`.
- **One corpus per guild (M31)**, chosen where the transaction opens. `storage.Set` owns them,
  `storage.Corpora` is the interface consumers take, `storage.Single` wraps one store for tests.
  Fan-out services (`games`, `aggro`, `health`, `repair`, `backup`) take `*storage.Set`.
  `PEREGRINE_DB_PATH` is a directory of `<guild id>.db`; a file path is a startup error.
- `For("")` is an error (DMs are not learned, no fallback corpus). REST-fetched messages
  (ingest, repair) have an empty `GuildID`: thread the guild explicitly, never read `m.GuildID`.
  Non-numeric guild IDs are refused (path component). `For` after `Close` is an error.
- Per guild: corpus, weekly leaderboard, word-game settings, aggro target, image pool, ingest and
  repair cursors/state, backups. Process-wide: safety gate and blocklist, `PAUSE_ALL_WRITES`,
  ignore list, dispatcher, presence. A dial that needs per-guild values follows
  `/wordgame-config`: a blob in that guild's corpus seeded from env, owned by a command afterwards.
- Channel allowlists are read per guild via `channels.Allows`: a list restricts the guilds it
  names and says nothing about others. A list resolving to no guild falls back to plain
  membership; blank entries are skipped.
- Key layout is composite `<prefix> 0x00 <next>` with a fixed 12-byte value (count `uint64`,
  distinct authors `uint32`). Empty prefixes are refused in `LearnNgram`. Presence sets store one
  byte, not zero (bbolt `Get` cannot distinguish empty from missing). Snowflakes are fixed-width
  big-endian so key order is chronological; a non-snowflake ID is an error.
- Use the cheap-answer methods on the hot path (`HasSuccessors`, `CorpusEmpty`, `IsName`,
  `TopicCount`), not the decoding ones.
- Never `Bucket.Stats()` on any regular path. Counts are counters in `meta` updated in the same
  transaction (`keyCounters`/`addKeyCount`). `Reader.Status` is counter lookups only; a long read
  transaction stops bbolt reusing freed pages and grows the file (finding 58).
  `TestStatusCountersMatchAWalk` compares counters to a real walk.
- `schema_version` is 2. Version 1 (pre-M6) is refused, not migrated: the corpus is re-derivable
  from Discord history. `upgradeToV2` only empties the image cache. Migrate what is cheap to lose,
  refuse what is not.
- `PurgeAuthor` reduces author diversity, not counts (counts carry no attribution).
- `storage.OpenReadOnly` writes nothing (`TestOpenReadOnlyWritesNothing`). `CorpusStats` is a
  `Store` method, never a `Reader` one: unbounded walks must not be reachable from generation.
- Storage never learns what a word means: filters (stop words, sentinel) are parameters or live in
  the caller.
- `internal/dbtest` gives a real corpus in `t.TempDir()` and has no skip path.
- Nothing may persist a `text.Interner` id (ids depend on insertion order).

bbolt facts that bite: one writer process-wide (slow work inside `Update` blocks all ingestion);
exclusive flock, opened with a 5 s timeout; the file never shrinks; pure Go, which is what keeps
`CGO_ENABLED=0` and the distroless image. Do not add a cgo dependency.

### Leaves: `internal/text`, `internal/filter`

Import nothing from this module and never log (filters return a reason string). `CleanSentence`
takes an `EmojiResolver`, not a session. The slur list is an ordered slice. `internal/filter`
matches raw text and is evadable on purpose (`TestSlurRulesAreEvadable`); do not add evasion
variants there, `internal/safety` normalizes first.

### Safety (`SPEC.md` §4)

Assume users try to make the bot say gross or illegal things; the operator wears the result.
- Two gates. Input filtering cannot bound Markov output; only the emit gate does. Do not remove
  either as redundant.
- **Gates live at chokepoints.** `gate.CheckLearn` is called inside `learn.Learner.Message`
  (AST test in `internal/learn`); `CheckEmit` is in `internal/discordguard`. Do not hoist either to
  call sites.
- Reject, never launder, on the learning path. `Verdict` has no rewritten-text field (denylist test
  on its field names). Replacement is display-only.
- Match on `safety.Normalize` output, which is lossy and matching-only: never store, emit or show
  it. Patterns must not re-enumerate evasions the normalizer removes. Thresholds pinned by tests:
  joining needs three spaced single letters; a separator next to a real word ends a run.
- Blocklist is data: `PEREGRINE_BLOCKLIST_PATH`, real file gitignored, `blocklist.example.txt`
  committed. Once set it fails closed (missing, unreadable, malformed, bad regex or empty file are
  startup errors). Categories: `slur`, `illegal` (plus alert), `spam` (learn only).
- `PEREGRINE_PAUSE_ALL_WRITES` refuses every send but not learning.
- Author diversity (`PEREGRINE_MIN_DISTINCT_AUTHORS`, default 2) is a **filter, not a penalty**,
  and never relaxes when it empties the candidate set. It applies to all three word producers
  (sampler `eligible()`, `Jump`, `Seed`) through one `admissible` in `score.go`. Since M24 an edge
  is also admissible with one real author, low count, short context
  (`PEREGRINE_SOLO_REPEAT_LIMIT=0` restores the strict gate). Zero-author edges (the bot's own
  output) never qualify; `learnMessage` passes an empty author when the author is `botID`
  (`TestTheBotIsExcludedFromAuthorDiversity`). End sentinel exempt in `eligible()`, not
  in `attested`; prompt-derived seeds exempt. A young corpus is nearly mute: that is the gate
  working.
- Authorization is one function, `games.Service.Authorized` (bootstrap admin env var, or guild
  Administrator from the payload/state cache). It fails closed. Never inline a second check: the
  empty case fails open.

### Outbound Discord: `internal/discordguard`

- Every outbound call goes through the guard. `TestNothingBypassesTheGuard` scans the module for
  session methods that speak; when the guard grows a method, add it to that list **before** writing
  the handler.
- `allowedMentions()` sets `Parse`, `Roles`, `Users` to empty slices and `RepliedUser` false.
  Never rely on the zero value (`"parse":null`). Test asserts marshalled JSON.
- Sends, edits, embeds (`CheckEmit` walks every text field), interaction responses (ephemeral
  exempts nothing), `UpdateEmbed`, `EditEmbed`, `RespondModal` and reactions are content- and
  pause-gated and honour `PEREGRINE_IGNORE_CHANNELS`. Deletes, `Unreact` and `RegisterCommands`
  (bulk overwrite) are not. `Presence` is content-gated, not pause-gated. Button labels are repo
  text and not gated.
- `EditEmbed` always sends the components pointer (nil is omitted and leaves dead buttons).
- Errors are logged, never discarded: failed send/edit/response at Error, failed delete/reaction
  at Info.
- `MessageComponentData`/`ApplicationCommandData` type-assert: always read via comma-ok helpers
  (`games.componentID`, `subcommand()`).
- Names render as plain text in embeds, never `<@id>`.
- Not ported from merlin's guard: per-guild pause, dry-run, write governor, audit journal.
  Peregrine's dangerous operation is speaking.

### Plugins and shared packages

Shared, stateless: `names` (who a message is about), `learn` (the only way into the corpus),
`generate` (prompt to sentence plus per-channel memory, no emit gate), `channels` (state cache,
never REST), `activity` (traffic). Features: `internal/plugins/{chat,aggro,images,games,autopost,
health,backup,ingest,repair,tuning,voicenote}`, each a `core.Service`.

- Interface type identity forces some cross-imports (`channels.Counter` names `activity.Channel`,
  etc.). That is intended; no renaming adapters.
- **Reactor (`chat`)**: a table of steps; consumed means "addressed to me". Only commands consume
  (`TestOnlyCommandsConsume`). Learn gate first, commands before reply and learn. Commands match
  the whole trimmed message, never a prefix (`commandFor`: exactly the command, or command plus one
  argument). Self-learning keys on the reply's own message ID. `reaction.names()` is memoized; copy
  before appending.
- **Names**: `names.Spellings` (nickname, `GlobalName`, username; display-first order is
  load-bearing), `names.Primary` for one value. `names.Substitute` replaces mention markup in
  prompt and corpus, before the gate. `Learner.Message` merges the author into associations itself;
  never at a caller (finding 33). `names.Display` resolves state cache, then
  `names.NewCachedSession` (bounded, TTL), then `User`, then raw ID. Any long walk or per-mention
  lookup must use the cached session.
- **Activity**: `internal/activity` is the one source of channel busyness and recent users, from
  gateway traffic plus state cache. Never page Discord REST for it. Keeps timestamps so each caller
  brings its window; per-channel ring caps counts (`activity.PerChannelHistory`, and the word-game
  threshold is capped below it). `Note` is called after the learn gate. Cold start means no aggro
  target and no busiest channel, by design.
- **Images**: cache key `<message snowflake> NUL <url>`, value author. Caps enforced inside
  `AddImageURL`; at the per-author cap evict that author's oldest. Per-author cap at or above
  `PEREGRINE_IMAGE_CACHE_SIZE` is a startup error. `MESSAGE_DELETE` revokes URLs, never unlearns
  n-grams. Do not hold `imageURLMutex` across `store.Update`.
- **Ingest**: resumes from per-channel cursors; `PEREGRINE_INGEST_LOOKBACK` bounds only the first
  pass (re-reading is finding 13, double-counting). `SetCursor` is monotonic in the writer and
  advances past bot messages. `ingest.SnowflakeAt` clamps at the Discord epoch. A channel worker
  swallows its error (errgroup would cancel the rest); failing to list guilds is fatal.
- **Repair**: additive and time-bounded, never drop-and-rebuild. A new repair is a row in
  `jobs.go`. Bump `learn.Generation` only when already-written data becomes wrong, and add the
  repair job in the same commit.
- **Health**: reports dispatcher drops/queue and gate rejections as totals and deltas; nonzero
  deltas log loudly. Latency is `Session.HeartbeatLatency()`, not a REST call. Reports once more at
  shutdown, before waiting. It feeds the presence line (round robin, no loop of its own) and the
  tuning `Reporter`. `Reader.ScanTopics` is caller-bounded and unfiltered.
- **Backup**: in-process `Store.Backup` (`tx.WriteTo`); `cp markov.db` is not a backup and a
  sidecar cannot take the flock. Write temp then rename; prune only files it named (prefix and
  suffix); never prune after a failed snapshot; not `Immediate` and no shutdown snapshot.
  `PEREGRINE_BACKUP_DIR` must be a writable mount outside the corpus volume.
- **Tuning export**: `internal/tuning` is a stdlib-only leaf with wire types duplicated on purpose
  (mapping in `internal/plugins/tuning/map.go`). `markov.Trace` hangs off `Step`/`SeedInput`, never
  the `Generator`; nil trace changes nothing (`TestANilTraceChangesNothing`). Seed tiers recorded by
  name. Every reply/autopost exit goes through one `record` closure. Refused sends carry no text.
  The recorder drops rather than blocks and reports drops. Engagement watches are bounded and evict
  oldest early. `IntentsGuildMessageReactions` is requested unconditionally. The bot never uploads;
  bind mounts need `sudo chgrp 65532`.
- **Voicenote**: a plugin over an `Engine` interface whose only shipped implementation reports
  unavailable; enabling it warns. Transcripts are never learned. Read the package comment before
  writing an engine.
- **Clustering is deleted** and stays deleted (finding 29). Transitive association is the bounded
  query-time two-hop tier in `markov.Seed`. Before building any derived index, ask whether the
  source can answer directly. Two indexes written from the same loop that differ only in what one
  discards are one index (finding 28).

### Games (`internal/plugins/games`, `internal/wordgame`, `internal/wheel`)

- Engines (`wordgame.Manager`, `wheel`) take no session and do no I/O; they return what to say and
  the service sends through the guard. One sweep `RunLoop`, no goroutine or timer per game.
- Every game is a `/game` subcommand (M37). `/wordgame-config`, `/leaderboard`, `/wallet` stay
  top-level. `TestTheRegisteredCommandMatchesWhatTheHandlerReads` guards registration.
  `!wordgame` still works and refuses non-admins silently in channel (logged).
- Every interaction exit answers (ephemeral acks; puzzles are normal channel posts). Handlers
  register in `Init`, commands in `Start`.
- Settings are a stored blob (M30), seeded from `PEREGRINE_WORDGAME_*` env once; `Init` logs which
  source won, `reset:true` reapplies env. The config command is not gated on the allowlist it
  edits; an empty list means anywhere and the reply says so. `WheelChannels` is not `omitempty`
  (absent key inherits the scramble list; present key, even null, is kept).
- Word game: the manager asks a shared `Counter` (outside its own lock); per-channel cooldown
  replaces clearing counts. Dictionary rejects words with fewer than two distinct letters and
  `scramble` bounds attempts. Planted words go through `Dictionary.Usable`.
- Hints are a ladder priced against points: reveal order fixed at start, rungs dropped when they
  add nothing, at least two letters stay hidden. `DueHints` returns copies and does not mutate;
  `HintDelivered` acknowledges. Hint is a repost: new card up before old card down. `announce` does
  not abandon; the caller decides. Config cross-checks (`HINT_AFTER < TIMEOUT`,
  `POINTS_BASE > HINT_LEVELS`) are startup errors.
- Gauntlets advance on the previous puzzle concluding, in memory only, bounded by `MaxChannels`,
  cancelled on `Abandon`. The run tally lives on the `Manager`, stores names at win time, is taken
  not read, and records against the game's `Rounds`. Both endings go through `finishRun`.
- Puzzles are plain messages (H2 scramble header, mask not in backticks, deadpan lowercase chrome).
  The leaderboard and the wheel are embeds. Tests assert payload structure, not copy or decoration.
- Leaderboard: rank (competition ranking, ties by user ID) before resolving names; resolve at most
  the rendered rows (`TestTheLeaderboardResolvesOnlyTheNamesItRenders`). Keyed by user ID. Paged
  via `custom_id` `lb:<scope>:<page>`, guild never in it; out-of-range pages clamp; press
  re-renders for the presser. Global boards skip unreachable guilds. `/leaderboard` registers
  regardless of the games flag. Persisted fields after `Wins` are `omitempty`; the pre-M11 format
  is still read. `corpus.StartOfWeekUTC` is the only week boundary; weekly reset compares
  boundaries and catches up.
- Leaderboard mutex is unexported and marshalling happens under it.
- Wheel: a `Result` is returned exactly once; engine errors change nothing and every one has a row
  in `wheelRefusals`; every action carries the Turn token; wrong solve text is never stored. Gold
  goes to board and wallet in one transaction; the wallet is never cached. Board tiles are each
  followed by a space (no flags) and rows are `boardCells` wide. Strip PNGs are compared pixel for
  pixel. `MaxChannels` refuses new matches rather than evicting. Three failed repaints abandon.

## Generation (`internal/markov`, `SPEC.md` §5)

- `internal/markov` imports neither bbolt nor discordgo; it declares `Corpus`, satisfied by
  `*storage.Reader` (assertion in external test package). A `Generator` must not outlive its
  transaction: no package-level generator.
- Scoring is `log P` plus bounded additive logits. Every term summing evidence is `tanh`-squashed
  to its weight. New heuristics go in `Weights`, bounded. `DefaultWeights` are constants tuned by
  reading golden samples and replay, not env vars.
- Interpolated Kneser-Ney with lambda from the discount (`TestInterpolatedKNSumsToOne`).
  `PEREGRINE_KN_RAW_MIX` (default 0.25) deliberately mixes raw counts back in because memes look
  like "Francisco"; do not set it to 0 on principle. `Reader.PrefixTotal` is a memoized cursor scan
  on purpose (a counter would cost the single writer).
- Candidate enumeration is bounded with a `minCandidates` floor. Candidates arrive in sorted order:
  never give index position meaning.
- Prefix lookups lowercase at the lookup.
- `Step.Position` is a fraction of the sentence's target, set only from `Length.Progress`.
- One length model in `length.go`; do not add another. When a term reads a corpus-wide count, check
  the end sentinel's value of it (finding 53: `Significance` exempts it).
- `Continuity` rewards longer matched context (finding 61). Lowering the KN discount does not
  substitute.
- Scorer uses point lookups into association indexes; never decode a whole range per step
  (finding 59). Decoding once per sentence in `Seed` is fine.
- Seeds: seven weighted tiers. Name tier 40 must beat prompt word 30; association bonuses are
  bounded by `assocWeight`; seeds prefer early positions (`bestPivot` deliberately prefers middle).
  A lone function word is never a seed. Two-hop candidates must pass `HasSuccessors`.
- `Jump` is bounded in the engine: none past `Length.Min`, at most one per sentence, never after a
  function word. `TrimDangling` trims only sentences that ran out of chain, not ones that ended on
  the sentinel.
- `generate.Sentence` returns a typed `Outcome`; callers log silence reasons. New reasons go in the
  enum. Below two words nothing is posted; up to three re-seeds keep the longest attempt.
- Context: only the prompt may SEED; reply chain, channel topics and recalled names may only STEER.
  Referenced messages are never re-learned and the bot's own messages are not context. `Memory` has
  one accessor per question; per-channel, bounded at 200 channels.
- Persona: one `Persona` drives lexicon bias and post-pass filler; filler only as opener or closer.
  Extend the lexicon by listing inflected forms, not stemming.
- Co-occurrence is windowed (`PEREGRINE_COOCCURRENCE_WINDOW`, default 5), both directions written.
- `PEREGRINE_TEMPERATURE` replaced `Creativity`; do not bring it back. `PEREGRINE_PROMPT_RELEVANCE_BOOST`
  is a logit, range 0 to 5.

## Deployment

CI (`.github/workflows/ci.yml`): vet, golangci-lint, `go test -race`, govulncheck, gitleaks, prose
check, Docker build. On `main` it pushes a multi-arch image to GHCR and deploys over SSH with
`docker-compose.prod.yml`. The host pins the image in `deployed-tag.env` with `previous-tag.env`
for rollback; image prune is scoped `until=168h` so the rollback image survives.

- Image is distroless static, `read_only: true`; every writable path is a mount.
- `mem_limit` must stay above the combined corpus size: below it the mmap thrashes rather than
  OOMs and looks like a slow engine (finding 59).
- Gitignored and never committed: `markov.db`, the whisper model (465 MiB, over GitHub's limit),
  ffmpeg. `.dockerignore` keeps the build context small.

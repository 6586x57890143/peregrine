# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`peregrine` is a Markov-chain Discord engagement bot (Go 1.24, `discordgo` v0.29, `bbolt` v1.4). It learns from channel messages and replies in the server's own voice. Full design doc: [`SPEC.md`](./SPEC.md). Read it before any non-trivial change, especially §4 (safety and threat model) and §5 (the generation pipeline). The bot lives in a meme-heavy server and exists to cause engagement, fun and chaos, so **output that lands matters more than output that is grammatical**, and several things that look like bugs are deliberate register. The things that are actually bugs are catalogued in §8.

**The repository is mid-restructure.** It is being taken from a single 3,200-line `package main` to merlin's `cmd/` plus `internal/` layout, one subsystem per milestone (`SPEC.md` §9). The entrypoint is now `cmd/bot`, and everything it calls still lives in `internal/legacy/legacy.go`, which is the old `main.go` moved verbatim. A comment saying "M6 replaces this" means exactly that. Do not treat the current shape as the intended shape, and in particular do not add anything new to `internal/legacy`: it is a holding pen that only shrinks.

## Commands

```sh
go build ./...                                # compile everything
go vet ./...
golangci-lint run                             # matches CI's pinned v2.12.2
go test ./... -cover                          # full suite
go test ./... -race -cover -covermode=atomic  # matches CI exactly (needs CGO_ENABLED=1 and a C toolchain)
go test ./wordgames/ -run TestTruncateRunes -v # single package, single test
govulncheck ./...
```

The `-race` line needs a C toolchain, which a stock Windows checkout does not have. Without gcc it fails with `cgo: C compiler "gcc" not found`, which is a missing toolchain and not a broken test. CI runs on `ubuntu-latest` where cgo works, so the race detector is effectively CI-only unless you install one locally.

There is a prose check in CI that fails the build on em dashes, ellipsis characters and curly quotes anywhere in the repo. Run it before pushing, because it is easy to trip in a comment:

```sh
em=$'\342\200\224' ell=$'\342\200\246' ldq=$'\342\200\234' rdq=$'\342\200\235'
grep -rnI --exclude-dir=.git -e "$em" -e "$ell" -e "$ldq" -e "$rdq" .
```

**One exception, and it is load-bearing:** `tokenRegex` in `internal/legacy/legacy.go` contains a literal right single quote inside its character class so the tokenizer can handle curly-apostrophe contractions. The CI check deliberately does not scan for that character. Do not "clean it up": removing it silently changes what the bot learns.

Local run:
```sh
cp .env.example .env      # DISCORD_BOT_TOKEN is the only variable required today
go run ./cmd/bot
docker compose up --build # or in a container
```

Maintenance modes, which operate on the corpus and never touch Discord:
```sh
go run ./cmd/bot -clean-db   # remove spammy and slur-bearing keys
```

## Architecture

### The entrypoint, and why `internal/legacy` exists

`cmd/bot/main.go` is thin on purpose and mirrors merlin's: `main` builds the logger and loads `.env`, `runGuarded` turns a panic into a logged fatal instead of a bare stderr trace, and `run` parses flags, creates the signal context, and calls `legacy.Run(ctx)`. Nothing else belongs there. Build it as `./cmd/bot`, not `.`; the root is no longer a main package.

`internal/legacy` holds the old `main.go`, `filter.go` and `cleanup.go` unchanged. It exists because **two `main` packages cannot share code**: making `cmd/bot` the entrypoint required the 3,200 lines it calls to live somewhere importable, and moving them verbatim was the only sequencing that keeps `go build ./...` green at every commit while ending at merlin's layout. Each later milestone moves one subsystem *out*, so the package only shrinks; M13 deletes it. Add nothing to it.

Two behaviors changed in that move, and both are about `log.Fatal`. `main()` became `Run(ctx) error`, so its six `log.Fatal` calls are returned errors, and `performDatabaseCleanup` became `CleanDatabase() error` for the same reason. `log.Fatal` calls `os.Exit`, which **skips every deferred function**, so any startup failure after `bbolt.Open` left the exclusive flock on `markov.db` held by a dying process, and the operator's natural next attempt then failed on the five-second `Open` timeout instead of on the original problem. There is now no `os.Exit` anywhere in `internal/legacy`.

Logging is bridged rather than converted. `cmd/bot` calls `slog.SetDefault`, which routes the stdlib `log` package through the slog handler, so `internal/legacy`'s ~200 `log.Printf` calls emit structured records without 200 call-site edits. `slog.SetLogLoggerLevel` must be called **before** `SetDefault` or every bridged record arrives at Info regardless of the handler's level. Convert call sites when their subsystem moves out, not before.

`stopSignal` is still the internal shutdown broadcast, because a dozen functions take it as a `<-chan struct{}` parameter. `Run` waits on `ctx.Done()` and then closes it, which is what translates the one cancellation `cmd/bot` owns into the one every goroutine selects on. M3 replaces both with a real lifecycle.

### `internal/text` and `internal/filter` are leaves and neither logs

Both import nothing from this module and must stay that way, so the tokenizer that decides what the bot learns and the filters that decide what it drops are testable with no database, no session and no config.

**Neither package logs.** The filters return a reason string alongside the verdict and the caller decides what to record. A leaf that writes to the log is a leaf you cannot test quietly, and the reason belongs to whoever chose to act on it.

**`tokenRegex` contains a literal right single quote and that is load-bearing.** Discord clients substitute a curly apostrophe as you type, so without it `don't` tokenizes as `don` plus `t` and the corpus fills with fragments. It is the one deliberate exception to the plain-punctuation rule and CI's prose check is configured not to scan for it. `TestTokenizeCurlyApostropheRegression` is what fails if someone cleans it up.

**`CleanSentence` takes an `EmojiResolver`, not a session.** That was the only thing it needed discordgo for, and taking the session would have dragged discordgo into a leaf and made the sentence cleaner untestable. `legacy.sessionEmoji` satisfies the interface structurally.

**The slur list is an ordered slice, not a map.** As a map, iteration order was randomized, so overlapping patterns produced different output on different runs and the corpus recorded whichever won that time. Today's patterns are all `\b`-anchored whole words so none actually overlap; the slice is what makes a future overlapping addition predictable instead of random.

**`internal/filter` is not the safety gate.** It is the mechanism. Its patterns match **raw** text, so they are evadable by construction: intra-word spacing, zero-width characters, combining marks and homoglyphs all pass. `TestSlurRulesAreEvadable` asserts that weakness on purpose, so nobody reads a green suite as evidence that raw matching suffices. Do not add evasion variants here; M5's normalizer folds the input first, which is what makes these patterns adequate rather than a sieve.

**Replacement is a display operation and must never touch the learning path.** A laundered message still carries its structure, so learning it teaches the bot the sentence with a harmless word where the slur went. On the learning path the verdict has to be "drop the whole message".

**Nothing may persist a `text.Interner` id.** Ids depend on insertion order, so an id written to the database means something different to the next process. This is not hypothetical: see the clustering note below.

### Clustering currently does nothing, and that is a finding, not a design

`clustering` persists cluster members **string-keyed**; the generation path unmarshals them into `map[int]float32`. Go parses integer map keys with `strconv`, so every cluster fails to unmarshal, and both consumers guard with `if err := json.Unmarshal(...); err == nil` and no `else`. The failure is completely silent.

So the pass runs a full similarity walk over the corpus every 24 hours, inside a write transaction against bbolt's single writer, ending in a destructive `DeleteBucket` plus `CreateBucket`, and produces data that **has never once been read**. Both consumers are dead code: the concept-cluster seed branch and the cluster-based jump. `PEREGRINE_ENABLE_CLUSTERING` therefore defaults to **false** since M4. `SPEC.md` §8 finding 27 has the detail.

The codec fix is small and deliberately deferred to M8, because turning the path on adds a seed branch firing at weight 50 inside a scorer that is not yet normalized, and M7's golden samples are what can judge whether it helps. The general lesson is worth keeping: an unmarshal guarded by `err == nil` with no `else` is indistinguishable from one that works, and only round-tripping the two real types tells you which you have.

### The storage seam, and why it is a seam rather than a fix

`internal/storage` is the only package that knows a bucket exists. Everything above it gets a `*storage.Reader` or `*storage.Writer`, handed to a callback and bound to one transaction.

That shape exists to make the worst bug in the review **unwritable**. Generation used to run inside a `db.View` and call helpers that each opened their own `db.View`; bbolt holds `mmaplock.RLock` for a read transaction's whole life and takes the write lock to grow the mmap, and Go's `RWMutex` queues new readers behind a waiting writer, so outer-read plus waiting-writer plus inner-read is a deadlock with no timeout and no recovery. **`Reader` has no method that starts a transaction**, so a consumer cannot nest one even by accident. A `Writer` embeds `Reader`, which is why nesting is never necessary: a write path can read its own writes.

**The key layout is composite, not map-valued.** `<prefix> 0x00 <next>` to a fixed 12 bytes (count `uint64`, distinct authors `uint32`). The old layout put a JSON `map[string]int` of every successor in the *value* of each prefix key, so learning one occurrence of "the cat" rewrote every successor "the" had ever had. `0x00` is asserted, not assumed: it sorts below space, so `Seek("the\x00")` cannot wander into `"the cat\x00"` keys, and a NUL inside a token would produce a key that stores and retrieves fine under a prefix the caller did not mean.

**An empty prefix is refused by the writer.** The old ingestion loop descended to order 1, where the prefix is empty, so the entire vocabulary accumulated into one key that nothing ever read. Refusing it in `LearnNgram` rather than only avoiding it in the caller means a new caller cannot reintroduce it.

**Presence sets store one byte, not zero.** This was a real bug, found by three failing tests. bbolt's `Bucket.Get` returns nil both for a missing key and for a key stored with an empty value, so `Get(k) == nil` could not tell them apart: every presence check reported absent, and both the distinct-author count and the Kneser-Ney predecessor count silently counted occurrences instead of distinct things. 500 repetitions by one person reported 500 distinct authors.

**Snowflakes are stored as fixed-width big-endian.** Discord IDs are 64-bit integers whose high bits are a timestamp, so numeric order is chronological order. Stored as decimal *strings* they were not: a 17-digit ID sorted before an 18-digit one, so history eviction removed entries essentially at random. Fixed-width bytes make a cursor's `First()` genuinely the oldest.

**Trims use counters, never `Bucket.Stats()`.** `Stats()` walks every page, and the old trims called it in the *loop condition*, making eviction quadratic in pages. Counters live in the `meta` bucket and are updated in the same transaction as the insert, so they cannot drift.

**`schema_version` refuses a corpus it does not understand.** There is no migration from the pre-M6 layout and there will not be one: converting means rewriting the whole corpus, and the corpus is re-derivable from Discord history. `Open` distinguishes a new file from an old one by looking for data, and refuses the latter with an explanation.

`internal/dbtest` gives tests a real corpus in `t.TempDir()`. There is **no skip path** and there is not meant to be one: unlike merlin's Postgres harness this one cannot be unavailable, so "these tests silently skipped" is not a failure mode it can have.

### Lifecycle: what starts, in what order, and what stops first

`cmd/bot` owns the process: config, logger, corpus, session, dispatcher, registry, shutdown. `internal/core` owns the mechanisms. Nothing in `core` imports a feature package and nothing may: services import `core`, and registration happens only in `cmd/bot`.

**Startup order is not arbitrary.** Open the corpus, build the session, `registry.InitAll()`, arm `WatchReady`, `session.Open()`, wait for READY, start the dispatcher, `registry.StartAll()`. Two of those placements are load-bearing:

- `WatchReady` **must be armed before `Open`**, because discordgo starts dispatching inside `Open` and a handler registered afterwards can lose the race with READY, which would fail startup on a healthy connection.
- Gateway handlers are registered in `Init`, not `Start`, because `Open` happens between them. Registering in `Start` drops every message that arrived in that window.

`Open` returning nil means the identify was *sent*, never that Discord accepted it. A rejected identify (close code 4014, a privileged intent the app was not granted) leaves discordgo reconnecting in a loop forever while startup logs success, so the process looks healthy, holds no connection, and learns nothing. `core.WatchReady` turns that into a startup error naming the Developer Portal checkbox.

**Shutdown order is reverse-of-dependency and each step is there because the old code got it wrong:**

```
1. session.Close()        first, to stop the inflow
2. dispatcher.Shutdown()  drain work already accepted
3. registry.ShutdownAll() reverse start order; loops stop and are waited for
4. store.Close()          via defer in cmd/bot, strictly last
```

One 8-second budget covers steps 2 and 3 together, deliberately under Docker's 10 seconds between SIGTERM and SIGKILL, because giving each step its own timeout would let them add past it.

**Nothing outside `core` spawns a goroutine per event.** `core.Dispatcher` is a bounded pool reading a buffered channel. `Submit` never blocks: discordgo dispatches every event on its own goroutine, so blocking would grow goroutines without bound and turn a slow corpus into unbounded memory. A full queue drops and counts, and the count is reported. Every `wg.Add` in the dispatcher happens in `Start`, which is what makes the old crash impossible rather than unlikely: the previous handler called `wg.Add` on the WaitGroup the shutdown path waited on, and an `Add` racing a `Wait` at zero panics.

**Background work goes through `core.RunLoop`, not a hand-rolled ticker.** There were nine near-identical copies, each selecting on a shared `stopSignal`, and a panic in any of them killed the process. Each iteration is now panic-isolated, because every one of those loops is an optional behavior.

`stopSignal` and the package-level `wg` are **gone**. Cancellation is a `context.Context` parameter; do not reintroduce a package-level stop channel.

**There is no shared `*rand.Rand`.** There was one, seeded in `main` and called from every message goroutine, which is a data race on the hot path. `math/rand/v2`'s top-level functions are goroutine-safe and auto-seeded, so the fix is to have no shared generator rather than to lock one. Use `rand.Float64()` and `rand.IntN()`; do not add a `*rand.Rand` back for seeding convenience.

### Configuration is environment variables, validated once, all at once

`internal/config` turns the environment into one `*Config` that `cmd/bot` hands to `legacy.Run`. There is no `config.yaml` and there should not be: merlin needs one because its settings are per guild and edited live from Discord, whereas peregrine's are per process and change on a deploy, which is what an env var already models.

Four rules, each from a specific failure mode:

- **Every field in `Config` is read by code that exists.** A knob wired to nothing is worse than no knob: an operator tunes it during an incident, nothing happens, and the bot gets blamed for ignoring configuration. This is why `ContextWindow` and `CoherencyBalance` were deleted rather than promoted, and why `Creativity` stayed a constant (see the trap below). Variables that later milestones will read live in `.env.example` and in the `deferredVars` map, not in the struct, and startup **warns** naming each one you have set plus its milestone. `TestDeferredVarsAreNotAlsoLive` fails if an entry outlives its milestone.
- **`Load` reports every problem, not the first.** A container that fails on one bad variable per restart makes a six-variable mistake take six deploys. `cmd/bot` unpacks the `errors.Join` into one log record each, because slog quotes a multi-line value and a joined error otherwise arrives as one line full of literal `\n`.
- **A value that does not parse is a startup error, never a fallback to the default.** Especially booleans: `PEREGRINE_ENABLE_X=ture` reading as "off" is indistinguishable from the feature being broken, and that exact shape is how autonomous posting stayed dark. Accepted forms are `1/true/yes/on` and `0/false/no/off`, case-insensitive.
- **The token is not required by `Load`.** `-clean-db` operates on the corpus and never touches Discord, so requiring a credential to clean a poisoned corpus would be backwards. `cfg.RequireToken()` is the bot path's check.

`PEREGRINE_BOOTSTRAP_ADMIN_USER_ID` replaces a user ID that was hardcoded in the source as the only authorization check in the codebase. It **fails closed**: empty refuses everyone, and getting that direction wrong on an empty string is how a missing variable turns an operator command into a public one.

### bbolt, and why the Dockerfile can be merlin's verbatim

The corpus is [bbolt](https://github.com/etcd-io/bbolt): an embedded, single-file, pure-Go B-tree key/value store. Pure Go is the load-bearing property. It means `CGO_ENABLED=0` produces a fully static binary, which means the image can be `gcr.io/distroless/static-debian12:nonroot` with no shell, no package manager and no libc, exactly like merlin's, **while still owning a database**. Merlin needs a whole Postgres service and a connect-retry loop; peregrine needs a volume. Do not introduce a cgo dependency without understanding that it costs the entire deployment story.

The consequences of bbolt that bite in practice:

- **One writer at a time, process-wide.** Every write transaction serializes against every other. A slow loop inside a `db.Update` blocks all ingestion, which is why the O(n^2) co-occurrence loops in `learnMessage` are a correctness-adjacent problem and not just slow.
- **An exclusive `flock` on the file.** A second process opening it read-write blocks. `bbolt.Open` is called with a 5-second `Timeout` for this reason: running `-clean-db` against a live bot used to hang forever with no output, and now it fails and says why.
- **Nested transactions deadlock.** bbolt holds `mmaplock.RLock` for a read transaction's entire life and takes the write lock to grow the mmap. Go's `RWMutex` queues new readers behind a waiting writer, so an outer read transaction plus a writer waiting to remap plus an inner `db.View` is an unrecoverable hang, and it gets likelier as the file grows. The generation path does exactly this today (`SPEC.md` §8, finding 1). M6 introduces a `Reader`/`Writer` seam bound to a `*bbolt.Tx` specifically so that nothing outside `internal/storage` can reach a `*bbolt.DB`, making the bug **unwritable** rather than fixed once.
- **The file never shrinks.** Deleting keys frees pages for reuse but does not return them to the filesystem. That is why a `-compact` mode is planned rather than optional.

### Paths must never be relative

Every runtime path used to be resolved against the working directory, so starting the bot from anywhere but the repo root silently created a fresh empty corpus and looked like it was working. `PEREGRINE_DB_PATH` now overrides it and is mandatory in a container: the image runs with `read_only: true`, so a relative path resolves against the distroless working directory and `bbolt.Open` fails outright. Production points it at `/data/markov.db` inside the mounted volume. The word list took the same lesson further and is now embedded with `go:embed`, so there is no path to be wrong at all.

### Failures in one feature must not take down the bot

The dictionary load used to be `log.Fatalf`, so a missing 64 KB word list killed learning, generation, replies and everything else along with word games. It now logs a warning and sets `wordGamesAvailable = false`. Treat this as the general rule: peregrine is a bag of loosely related engagement behaviors, and exactly one of them failing should disable that one. `log.Fatal` is for "the token is missing" and "the corpus will not open", nothing else.

### Discord calls are logged, never discarded

`sendMessage`, `editMessage` and `deleteMessage` in `internal/legacy/legacy.go` wrap the `discordgo` calls. Every one of these used to discard its error, so a send Discord refused (missing permission, rate limit, channel deleted mid-flight) was indistinguishable from one that succeeded: the bot appeared to ignore people at random with nothing in the log. They are deliberately thin because M10 replaces them with `internal/discordguard`, which owns the same logging plus mention suppression and the outbound safety gate at a single chokepoint. Add new sends through these helpers, not through `s.ChannelMessage*` directly.

### Nothing the bot posts may ping

Not yet enforced, and it is finding 8 in `SPEC.md`. `discordgo.ChannelMessageSendReply` sets no `AllowedMentions`, so Discord's default applies and the replied-to author is pinged on every single interaction. This matters more here than in merlin: peregrine's output is Markov text assembled from arbitrary user messages, so **every send is untrusted-input-shaped by construction** and a user mention that got learned will ping that person forever. M10 ports merlin's guard, which overwrites `AllowedMentions` with a non-nil empty `Parse` on every send. Use a non-nil empty slice, not the zero value: a nil `Parse` marshals as `"parse":null` and only `"parse":[]` is Discord's documented "allow nothing".

## The generation pipeline

Read `SPEC.md` §5 for the full specification. The parts most worth knowing before touching it:

**It is currently less random than it looks.** Candidate scores start as raw n-gram counts and are multiplied by an unbounded topic-gravity term and roughly a dozen more ad-hoc factors, with no normalization anywhere, then raised to a power. Scores therefore span orders of magnitude and one candidate almost always dominates: the sampler is effectively argmax with noise. For a chaos bot that is the worst available failure mode, and it hides well because the output still varies a little. M7 normalizes to a real probability, moves every heuristic to an **additive logit in log space**, and puts temperature and top-k/top-p on top so that chaos is a dial that moves.

**`Creativity` is a trap, and it is deliberately still a constant.** It is applied as an exponent of `1/(Creativity+0.01)`, so at its 0.75 default the exponent is 1.316, which *sharpens* the distribution. Raising it toward 1 can only ever approach an exponent of 1.0 and never pass it, so the knob cannot reach the interesting half of its own range. M2 promoted every other tuning constant to an environment variable and left this one alone on purpose: exposing a dial whose arithmetic contradicts its name invites tuning something broken. It becomes `PEREGRINE_TEMPERATURE` in M7, in the same change that normalizes the scoring so the dial actually moves. Do not add a `PEREGRINE_CREATIVITY`.

**Backoff carries no weight.** The prefix-shrink loop tries the longest prefix, then shorter ones, and takes the first non-empty result, so a 4-gram continuation and a bigram continuation are scored on the same scale. M7 replaces this with **interpolated Kneser-Ney**, which is the right model for a corpus this sparse: a server's chat is on the order of 10^5 to 10^6 tokens, so at `MaxNGram=5` nearly every 4-gram has count 1.

**Kneser-Ney is deliberately not applied in its textbook form, and this is the single most counter-intuitive decision in the codebase.** KN estimates lower orders from *continuation counts*, the number of distinct contexts a token follows, which correctly demotes a token like "Francisco" that is frequent but nearly always preceded by "San". The problem is that a meme, a copypasta and an inside joke are statistically indistinguishable from "Francisco": high frequency, few distinct contexts. Pure KN would therefore systematically suppress exactly the register this server runs on. `PEREGRINE_KN_RAW_MIX` interpolates the lower-order estimate back toward raw counts (`0.0` is textbook KN, `1.0` is raw counts, default `0.25`). If someone "fixes" this to 0 because a paper says so, output quality by the only metric that matters here gets worse.

**Length is tuned short on purpose.** The old bound was `30 + rand(15)` words, which is a paragraph. Short and punchy reads as a joke; long reads as a malfunction.

**Custom emote output had never worked, and M3 fixed the cause.** The `:shortcode:` resolver walks `s.State.Guilds`, and the session never requested `IntentsGuilds`, so that slice was always empty and the resolver had never once succeeded. In a meme server the server's own emotes are most of the register, so this was an engagement bug before it was a performance bug. `core.NewSession` now requests the intent. The state cache it populates is what M10 uses to replace the per-message REST `s.Channel` call for the NSFW check; that call site is still REST today. Emote-bearing output is worth a golden-sample check in M7 now that it is possible at all.

## Safety model

`SPEC.md` §4 is the full threat model. Assume users actively try to make the bot say gross or borderline illegal things, and that **the operator carries the consequences of whatever it posts**. Four rules follow, and each exists because of a specific hole.

**Filtering the corpus cannot bound the output, even in principle.** A Markov chain composes novel sequences from n-grams learned separately, so fragments that were individually innocuous can join into something the operator has to answer for. Input filtering lowers the rate; only an output gate bounds the result. This is why there are two gates and not one, and why removing the output gate as redundant would be wrong.

**Gates go at chokepoints, never at call sites.** `gate.CheckLearn` is called **inside `learnMessage`**, not at any of its four callers. That placement is the entire fix for the worst finding in the review: the live handler filtered, but the historical backfill, self-learning and voice transcripts passed content through raw, and since the backfill re-read the trailing 24 hours every ten minutes, a message the live path blocked was learned anyway, unfiltered, minutes later. A check at one of four call sites is not a check. A fifth caller is now covered without anyone remembering to cover it.

Do not hoist it to the call sites for performance. `internal/legacy/learngate_test.go` parses the package and fails if the call leaves `learnMessage`'s body, because that regression passes every behavioural test.

**Reject, never launder, on the learning path.** `Verdict` deliberately has no field for rewritten text, which makes laundering unexpressible here rather than merely discouraged. A rewritten message is still learned, with its structure intact and a harmless token sitting in the offending word's grammatical position: the bot has been taught the sentence. The `m.Content = filterSlurs(m.Content)` that used to sit in the live handler is gone, and removing it was load-bearing in a second way: with the gate in place, laundering *before* the gate would hand it pre-cleaned text and defeat it.

**Match on a normalized form, never raw text.** `safety.Normalize` case-folds, applies NFKD, strips combining marks and format characters, folds Cyrillic and Greek confusables plus leet substitutions to ASCII, collapses whitespace, joins runs of three-or-more spaced single letters, and caps repeated characters at two. Blocklist patterns are written against that form and **must not** re-enumerate evasions it already removes; enumerating them by hand is a game the defender loses.

The output is **matching-only**. It is lossy by design (it folds digits to letters, so `2026` becomes `zo2g`), and must never be stored in the corpus, emitted, or shown to a user. `internal/filter` still matches raw text, which is why its tests assert its own evadability, and why `internal/safety`'s tests assert the same inputs are caught.

Two thresholds in the normalizer are the difference between a defence and a false-positive generator, and both are pinned by tests: joining requires **three** spaced single letters (two is ordinary text, so `is a b test` survives), and a separator adjacent to a real multi-character word ends a run (so `well-known` and `e-mail` survive).

**The blocklist is data, not source.** It loads from `PEREGRINE_BLOCKLIST_PATH`; the real file is gitignored and only `blocklist.example.txt` is committed. Committing the real list would make this repo a searchable copy of one and turn every addition into a rebuild and a public diff instead of an edit made mid-incident.

Once set it fails **closed**: a missing file, an unreadable file, a malformed line, an uncompilable pattern and an *empty* file are all startup errors, reported one per line with line numbers. Leaving it unset is allowed and warns loudly, because a developer on a scratch corpus should not need to invent a slur list first and the built-in baseline still applies. Categories are `slur` (drop on learn, block on emit), `illegal` (same, plus an operator alert, so keep it narrow) and `spam` (drop on learn only, since the bot generating something that resembles advertising is not an incident).

**`PEREGRINE_PAUSE_ALL_WRITES` refuses every send, and deliberately does not stop learning.** During an incident the output is the problem, not the corpus, and stopping ingestion would also stop the bot noticing what is being said to it. It logs at startup, so a silent bot is never a mystery. `Gate.SetPaused` exists so a Discord-reachable kill switch can be added later without inventing a second lever.

**Poisoning is cheap unless generation requires author diversity.** n-gram weight is raw frequency, so repeating a phrase is a direct write to the model, and one determined user can teach the bot to say anything. `PEREGRINE_MIN_DISTINCT_AUTHORS` requires a continuation to have come from `k` distinct authors before the bot will *generate* it, independent of how often it was seen. That turns the attack from persistence into collusion. The bot's own output is excluded from those counts, so self-learning cannot bootstrap a phrase into eligibility.

## Deployment

Merlin's pipeline, adapted. `.github/workflows/ci.yml` runs `go vet`, `golangci-lint`, `go test -race -cover`, `govulncheck`, a `gitleaks` secret scan, the prose check and a Docker build on every push and PR. On push to `main` only it also builds and pushes a multi-arch (amd64/arm64) image to GHCR and deploys to the VPS over SSH using `docker-compose.prod.yml`.

The prod image tag is pinned to the commit SHA in `deployed-tag.env` on the host, with the previous value kept in `previous-tag.env`, so a rollback is editing one line rather than working out from GHCR what used to be running. The image prune is scoped `--filter "until=168h"` deliberately: a bare `docker image prune -f` on a host that just retagged can delete the previous release, which is the one thing a rollback needs to still exist.

**There is no backup sidecar, and the omission is deliberate.** Merlin's works because `pg_dump` is a client asking the server for a consistent snapshot. bbolt has no equivalent, and `cp markov.db` is *not* a backup: the file is a single mmap updated by copy-on-write pages plus a meta-page flip at commit, so an external byte copy can capture a state between the page write and the flip, or mid-remap, and the result usually *appears* to work, which is the worst property a backup can have. A sidecar cannot snapshot it either, because of the exclusive flock. The correct mechanism is in-process: a read transaction calling `tx.CopyFile`, consistent by construction and non-blocking for writers. That lands in M13.

`markov.db`, the whisper model and the ffmpeg binary are all gitignored. `voicenotes/models/ggml-small.bin` is 465 MiB, over GitHub's hard 100 MiB per-file limit, so a commit containing it cannot be pushed at all and the only remedy is rewriting history. `.dockerignore` exists for the same weight: the working tree is around 692 MB and `COPY . .` would ship all of it into the builder on every build.

## Transcription is Windows-local only

Voice-note transcription shells out to `ffmpeg` and `whisper-cli` and needs a 465 MiB model. None of that exists in a distroless image, which has no shell at all, so `PEREGRINE_ENABLE_TRANSCRIPTION` defaults to **false** and that default deliberately differs from the old in-code constant. The binaries and model are gitignored and fetched by hand. Only `voicenotes/bin/windows/` exists; on Linux the path lookup fails and every voice note produces a failure reply, which is another reason the flag defaults off.

## Conventions

- Plain punctuation only: no em dashes, ellipsis characters or curly quotes. CI enforces it. See the tokenizer exception above.
- Comments explain *why*, and name the failure mode that motivated the code. A comment restating what the line does is noise; a comment saying "this used to hang forever with no output" is what stops someone reverting it.
- `main` is protected: PR review plus all CI checks, no direct or force pushes.
- Run `go vet ./...`, `go test ./... -cover`, `golangci-lint run` and the prose grep before opening a PR.
- One milestone (or a slice of one) per PR, per `SPEC.md` §9. The tree must build, vet, lint and test green at the end of every one.

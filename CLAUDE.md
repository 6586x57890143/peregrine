# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`peregrine` is a Markov-chain Discord engagement bot (Go 1.25, `discordgo` v0.29, `bbolt` v1.5). It learns from channel messages and replies in the server's own voice. Full design doc: [`SPEC.md`](./SPEC.md). Read it before any non-trivial change, especially §4 (safety and threat model) and §5 (the generation pipeline). The bot lives in a meme-heavy server and exists to cause engagement, fun and chaos, so **output that lands matters more than output that is grammatical**, and several things that look like bugs are deliberate register. The things that are actually bugs are catalogued in §8.

**The restructure is complete as of M13.** The repository went from a single 3,200-line `package main` to merlin's `cmd/` plus `internal/` layout, one subsystem per milestone (`SPEC.md` §9), and `internal/legacy` is **deleted**. If you find a reference to it in a comment, that comment is stale and worth fixing.

`cmd/bot` is the entrypoint: `main.go` decides which mode to run and translates a signal into a cancelled context, and `services.go` is the only place the features are named together. `internal/core` owns the lifecycle mechanisms and imports no feature package, which is what makes that the only possible place for registration.

## Commands

```sh
go build ./...                                # compile everything
go vet ./...
golangci-lint run                             # matches CI's pinned v2.12.2
go test ./... -cover                          # full suite
go test ./... -race -cover -covermode=atomic  # matches CI exactly (needs CGO_ENABLED=1 and a C toolchain)
go test ./wordgames/ -run TestTruncateRunes -v # single package, single test
go test ./internal/markov/ -run TestGenerateGolden -v  # print golden samples and READ them
govulncheck ./...
```

The `-race` line needs a C toolchain, which a stock Windows checkout does not have. Without gcc it fails with `cgo: C compiler "gcc" not found`, which is a missing toolchain and not a broken test. CI runs on `ubuntu-latest` where cgo works, so the race detector is effectively CI-only unless you install one locally.

There is a prose check in CI that fails the build on em dashes, ellipsis characters and curly quotes anywhere in the repo. Run it before pushing, because it is easy to trip in a comment:

```sh
em=$'\342\200\224' ell=$'\342\200\246' ldq=$'\342\200\234' rdq=$'\342\200\235'
grep -rnI --exclude-dir=.git -e "$em" -e "$ell" -e "$ldq" -e "$rdq" .
```

**One exception, and it is load-bearing:** `tokenRegex` in `internal/text/text.go` contains a literal right single quote inside its character class so the tokenizer can handle curly-apostrophe contractions. The CI check deliberately does not scan for that character. Do not "clean it up": removing it silently changes what the bot learns.

Local run:
```sh
cp .env.example .env      # DISCORD_BOT_TOKEN is the only variable required today
go run ./cmd/bot
docker compose up --build # or in a container
```

Maintenance modes, which operate on the corpus, never touch Discord, and need no token. One at a time: they each take the whole corpus, and the order they should run in is the operator's decision rather than a default:
```sh
go run ./cmd/bot -clean-db                  # remove spammy and blocklisted n-grams
go run ./cmd/bot -compact ./markov.new      # reclaim free pages; bbolt never shrinks its file
go run ./cmd/bot -purge-author 123456789    # undo one author's contribution to diversity counts
```

## Architecture

### The entrypoint, and why `internal/legacy` existed

`cmd/bot/main.go` is thin on purpose and mirrors merlin's: `main` builds the logger and loads `.env`, `runGuarded` turns a panic into a logged fatal instead of a bare stderr trace, and `run` parses flags, creates the signal context, and calls `legacy.Run(ctx)`. Nothing else belongs there. Build it as `./cmd/bot`, not `.`; the root is no longer a main package.

`internal/legacy` held the old `main.go`, `filter.go` and `cleanup.go`, and **M13 deleted it**. It existed because **two `main` packages cannot share code**: making `cmd/bot` the entrypoint required the 3,200 lines it called to live somewhere importable, and moving them verbatim was the only sequencing that kept `go build ./...` green at every commit while ending at merlin's layout.

The part worth keeping is how it shrank. Each milestone took one subsystem out and then let the linter report what was left unreachable, which is how the leftover wrappers in M5, M6b and M13 were each confirmed dead rather than assumed to be. A holding pen only works if something keeps emptying it.

As of M6b it is one file. `cleanup.go` was replaced by `internal/maintenance`, and `filter.go` went with it because its last two wrappers existed only for that pass; the linter reporting them unused is how that was confirmed rather than assumed.

**Only `internal/storage` may import bbolt, and a test enforces it.** `TestOnlyThisPackageReachesBbolt` lives in `internal/storage` and walks the whole module, because the import is the exact invariant: the bbolt API is unreachable without it, so if no file outside that package imports it then no function outside it can name a bucket, hold a handle, or start a transaction. It began life in `internal/legacy` scoped to that one package, and M11c widened it when there were eight packages above the seam rather than one.

Two behaviors changed in that move, and both are about `log.Fatal`. `main()` became `Run(ctx) error`, so its six `log.Fatal` calls are returned errors, and `performDatabaseCleanup` became `CleanDatabase() error` for the same reason. `log.Fatal` calls `os.Exit`, which **skips every deferred function**, so any startup failure after `bbolt.Open` left the exclusive flock on `markov.db` held by a dying process, and the operator's natural next attempt then failed on the five-second `Open` timeout instead of on the original problem. There is no `os.Exit` anywhere outside `main` itself.

Logging is still bridged, and the bridge is still load-bearing. `cmd/bot` calls `slog.SetDefault`, which routes the stdlib `log` package through the slog handler, and the plugins that came out of legacy kept their `log.Printf` calls rather than converting ~200 call sites inside milestones whose value was being reviewable as moves. `slog.SetLogLoggerLevel` must be called **before** `SetDefault` or every bridged record arrives at Info regardless of the handler's level. The newest services (health, backup, ingest, voicenote) use `slog` directly, so both forms appear in the output; converting the rest is tidying rather than a fix.

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

### Clustering is deleted, and the reasoning matters

**M8 is dropped and the package is gone as of M7b.** `SPEC.md` §8 finding 29 has the full case; the short version is four independent reasons, and the second is the one that turns "not worth it" into "actively wrong":

- **It derives from one index and adds nothing.** `PerformClusteringOptimized` reads exactly one bucket, `name_topics`. A cluster is a stale precomputation of what `Reader.NameTopicsFor` already answers directly, inside the read transaction generation already holds.
- **Fixing the codec would have shipped a regression.** `MergeThreshold` is 0.005 against a cosine over weights normalized to sum to 1. Two name-seeds sharing a *single* topic word score about 0.09, eighteen times the threshold, and the stop-word list is plain English function words only, so "lol", "bird" and "ratio" are the top associations of every name in a meme server. Every seed merges with every other one into a single blob, and that blob fed the highest-weight seed tier, at 50.0 against a next tier at 25.0, applied to *every member*. Seeding would have become approximately uniform over the name-adjacent vocabulary.
- **No corpus scale gives useful structure.** A few hundred sparse vectors either collapse at this threshold or stay singletons at a higher one.
- **Its cost lands on bbolt's single writer**, competing with ingestion, for a structure that was the only derived, stale, order-dependent thing in the layout.

The one idea worth keeping is *transitive* association, which co-occurrence gives only the first hop of. It is a bounded query-time two-hop tier in `markov.Seed` now: never stale, no bucket, no codec, no persistence, and a weight golden samples can judge. `PEREGRINE_ENABLE_CLUSTERING` and `PEREGRINE_CLUSTERING_TICK` are **deleted rather than deferred**, and that direction matters: a deferred variable promises a milestone, so leaving them pointing at M8 would promise one that is never coming. Setting either now does nothing and warns about nothing, exactly like any other unrecognized variable.

The general lesson is the part to keep: **a derived index inherits every assumption of its source and adds a staleness window and a tuning constant of its own.** Before rebuilding one, ask whether the question can be put to the source directly, and whether its tuning constant was ever validated against real data. This one's never had been.

### How clustering got there (kept because the failure modes recur)

`clustering` persisted cluster members **string-keyed**; the generation path unmarshalled them into `map[int]float32`. Go parses integer map keys with `strconv`, so every cluster failed to unmarshal, and both consumers guarded with `if err := json.Unmarshal(...); err == nil` and no `else`. The failure was completely silent. So the pass ran a full similarity walk over the corpus every 24 hours, inside a write transaction against bbolt's single writer, ending in a destructive `DeleteBucket` plus `CreateBucket`, and produced data that **has never once been read**. `SPEC.md` §8 finding 27 has the detail.

M6b removed the loop and both dead consumers, and there are three independent reasons rather than one, which is worth knowing before anyone tries to restore them:

- The codec has never worked, as above.
- `PerformClusteringOptimized` takes a `*bbolt.DB`, and nothing outside `internal/storage` can hold one now. It is not callable from `legacy` even in principle.
- It read the name-topic index as a JSON map per name, which the composite-key layout does not store, so it would find nothing with the codec fixed.

The concept-cluster consumers were also **unfixable as written**, not merely broken: cluster members are `text.Interner` ids, and ids depend on insertion order, so an id written to disk means a different word to the next process. That was the fourth reason M8 became a deletion rather than a rebuild.

The general lesson is worth keeping: an unmarshal guarded by `err == nil` with no `else` is indistinguishable from one that works, and only round-tripping the two real types tells you which you have.

### Two indexes written from the same loop are one index

M6b found that `TopicClusterBucket` recorded nothing `TopicWordBucket` did not. Both were written from the same place in `learnMessage`, over the same message, under the same guard and the same stop-word exclusion; the second was the first with the direction and the position discarded, keyed `min|max` with a literal pipe. It is gone, and its two seed-selection tiers read the surviving indexes at their old weights (`SPEC.md` §8, finding 28).

Recorded as a shape rather than a one-off: two indexes written from the same loop, from the same data, differing only in what one of them throws away, are a duplicate however different their readers look. The pipe separator is the other lesson on its way out, and it is why the new layout uses NUL: a word containing `|` produced a key that split into three parts and was silently skipped by every reader.

### The storage seam, and why it is a seam rather than a fix

`internal/storage` is the only package that knows a bucket exists. Everything above it gets a `*storage.Reader` or `*storage.Writer`, handed to a callback and bound to one transaction.

That shape exists to make the worst bug in the review **unwritable**. Generation used to run inside a `db.View` and call helpers that each opened their own `db.View`; bbolt holds `mmaplock.RLock` for a read transaction's whole life and takes the write lock to grow the mmap, and Go's `RWMutex` queues new readers behind a waiting writer, so outer-read plus waiting-writer plus inner-read is a deadlock with no timeout and no recovery. **`Reader` has no method that starts a transaction**, so a consumer cannot nest one even by accident. A `Writer` embeds `Reader`, which is why nesting is never necessary: a write path can read its own writes.

**The key layout is composite, not map-valued.** `<prefix> 0x00 <next>` to a fixed 12 bytes (count `uint64`, distinct authors `uint32`). The old layout put a JSON `map[string]int` of every successor in the *value* of each prefix key, so learning one occurrence of "the cat" rewrote every successor "the" had ever had. `0x00` is asserted, not assumed: it sorts below space, so `Seek("the\x00")` cannot wander into `"the cat\x00"` keys, and a NUL inside a token would produce a key that stores and retrieves fine under a prefix the caller did not mean.

**An empty prefix is refused by the writer.** The old ingestion loop descended to order 1, where the prefix is empty, so the entire vocabulary accumulated into one key that nothing ever read. Refusing it in `LearnNgram` rather than only avoiding it in the caller means a new caller cannot reintroduce it.

**Presence sets store one byte, not zero.** This was a real bug, found by three failing tests. bbolt's `Bucket.Get` returns nil both for a missing key and for a key stored with an empty value, so `Get(k) == nil` could not tell them apart: every presence check reported absent, and both the distinct-author count and the Kneser-Ney predecessor count silently counted occurrences instead of distinct things. 500 repetitions by one person reported 500 distinct authors.

**Snowflakes are stored as fixed-width big-endian.** Discord IDs are 64-bit integers whose high bits are a timestamp, so numeric order is chronological order. Stored as decimal *strings* they were not: a 17-digit ID sorted before an 18-digit one, so history eviction removed entries essentially at random. Fixed-width bytes make a cursor's `First()` genuinely the oldest. A message ID that is not a snowflake is now an *error* rather than a key, which is the right answer for a caller that invented one.

**There is a cheap-answer method for every question the hot path asks, and you should use it.** `HasSuccessors`, `CorpusEmpty`, `IsName` and `TopicCount` exist next to `Successors`, `NgramCount` and `Name` because the scorer calls some of these once per candidate per step per generated word, and the decoding version would put a JSON unmarshal or a `Bucket.Stats()` page walk in that innermost loop. `HasSuccessors` in particular is not a byte-prefix match: keys are `<prefix> 0x00 <next>`, so it seeks the prefix's range and bounds it, which is what stops a query for `"the"` being satisfied by `"the cat"` keys.

**`total_messages_learned` is a meta counter, not a key in the stats bucket.** It used to be the latter, so every reader of a bucket whose every other key is a Discord user ID holding a JSON `WeeklyStat` had to recognise and skip it; the leaderboard did that with a `strconv.ParseInt` on the key, and anything that forgot would decode an integer as a stat and count a phantom user.

**`PurgeAuthor` removes diversity, not frequency, and that asymmetry is deliberate.** The counts do not record who produced them, and storing that would mean a value on every entry of `ngram_auth`, already the fastest-growing structure in the database. So a purge reduces the author diversity of everything that author touched, which is what generation eligibility reads. In practice that is the effective half: a phrase only one person ever said drops to zero distinct authors.

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

- **Every field in `Config` is read by code that exists.** A knob wired to nothing is worse than no knob: an operator tunes it during an incident, nothing happens, and the bot gets blamed for ignoring configuration. This is why `ContextWindow` and `CoherencyBalance` were deleted rather than promoted, and why `Creativity` stayed a constant until the change that gave it working arithmetic (see the generation section). Variables that later milestones will read live in `.env.example` and in the `deferredVars` map, not in the struct, and startup **warns** naming each one you have set plus its milestone. `TestDeferredVarsAreNotAlsoLive` fails if an entry outlives its milestone.

  The same rule covers a variable whose *units* change rather than its name. `PEREGRINE_PROMPT_RELEVANCE_BOOST` was an addend on a raw n-gram count with a default of 15.0; as a logit, 15.0 multiplies a candidate's odds by three million. It kept its name and narrowed its range to 0 to 5, so a stale value is a **startup error naming the range**. Renaming it would have let the old value silently stop being read, which is the failure this rule exists to prevent, so prefer rescale-and-refuse over rename.
- **`Load` reports every problem, not the first.** A container that fails on one bad variable per restart makes a six-variable mistake take six deploys. `cmd/bot` unpacks the `errors.Join` into one log record each, because slog quotes a multi-line value and a joined error otherwise arrives as one line full of literal `\n`.
- **A value that does not parse is a startup error, never a fallback to the default.** Especially booleans: `PEREGRINE_ENABLE_X=ture` reading as "off" is indistinguishable from the feature being broken, and that exact shape is how autonomous posting stayed dark. Accepted forms are `1/true/yes/on` and `0/false/no/off`, case-insensitive.
- **The token is not required by `Load`.** `-clean-db` operates on the corpus and never touches Discord, so requiring a credential to clean a poisoned corpus would be backwards. `cfg.RequireToken()` is the bot path's check.

`PEREGRINE_BOOTSTRAP_ADMIN_USER_ID` replaces a user ID that was hardcoded in the source as the only authorization check in the codebase. It **fails closed**: empty refuses everyone, and getting that direction wrong on an empty string is how a missing variable turns an operator command into a public one.

### bbolt, and why the Dockerfile can be merlin's verbatim

The corpus is [bbolt](https://github.com/etcd-io/bbolt): an embedded, single-file, pure-Go B-tree key/value store. Pure Go is the load-bearing property. It means `CGO_ENABLED=0` produces a fully static binary, which means the image can be `gcr.io/distroless/static-debian12:nonroot` with no shell, no package manager and no libc, exactly like merlin's, **while still owning a database**. Merlin needs a whole Postgres service and a connect-retry loop; peregrine needs a volume. Do not introduce a cgo dependency without understanding that it costs the entire deployment story.

The consequences of bbolt that bite in practice:

- **One writer at a time, process-wide.** Every write transaction serializes against every other. A slow loop inside a `db.Update` blocks all ingestion, which is why the O(n^2) co-occurrence loops in `learnMessage` are a correctness-adjacent problem and not just slow.
- **An exclusive `flock` on the file.** A second process opening it read-write blocks. `bbolt.Open` is called with a 5-second `Timeout` for this reason: running `-clean-db` against a live bot used to hang forever with no output, and now it fails and says why.
- **Nested transactions deadlock.** bbolt holds `mmaplock.RLock` for a read transaction's entire life and takes the write lock to grow the mmap. Go's `RWMutex` queues new readers behind a waiting writer, so an outer read transaction plus a writer waiting to remap plus an inner `db.View` is an unrecoverable hang, and it gets likelier as the file grows. The generation path did exactly this until M6b (`SPEC.md` §8, finding 1): `isRecognizedName` opened a transaction per prompt word and `getNextMap` opened one per candidate per backoff step per generated word, all inside the read transaction wrapping generation. Both take the `*storage.Reader` they are already inside now, and the version that could nest **does not compile**, because a `Reader` has no method that opens a transaction.
- **The file never shrinks.** Deleting keys frees pages for reuse but does not return them to the filesystem. That is why a `-compact` mode is planned rather than optional.

### Paths must never be relative

Every runtime path used to be resolved against the working directory, so starting the bot from anywhere but the repo root silently created a fresh empty corpus and looked like it was working. `PEREGRINE_DB_PATH` now overrides it and is mandatory in a container: the image runs with `read_only: true`, so a relative path resolves against the distroless working directory and `bbolt.Open` fails outright. Production points it at `/data/markov.db` inside the mounted volume. The word list took the same lesson further and is now embedded with `go:embed`, so there is no path to be wrong at all.

### Failures in one feature must not take down the bot

The dictionary load used to be `log.Fatalf`, so a missing 64 KB word list killed learning, generation, replies and everything else along with word games. It now logs a warning and the `Manager` is left with a nil dictionary, so `Available()` reports false and every entry point declines. Treat this as the general rule: peregrine is a bag of loosely related engagement behaviors, and exactly one of them failing should disable that one. `log.Fatal` is for "the token is missing" and "the corpus will not open", nothing else.

### Word games: `internal/wordgame` owns the game, legacy does the talking

**The Manager takes no session and performs no I/O.** It returns what should be said or deleted and the caller sends it through the guard, so a word-game announcement cannot skip mention suppression or the emit gate. That is also what makes the whole game testable without a gateway connection.

**One sweep, not a goroutine per game.** Every started game used to spawn up to three: one to expire it after 60 seconds and one per announcement to delete it 30 seconds later. None took a context, so after shutdown they woke against a closed session, and the count was bounded only by how often people played. `Manager.Expired` and `Manager.DueDeletions` are swept by one `core.RunLoop`, which makes the loop panic-isolated and context-bound for free. The cost is that a game can outlive its deadline by up to one tick, which is invisible.

**The scramble cannot recurse to death, and there are two independent reasons.** It recursed whenever a shuffle reproduced the original word, with no depth limit, so a word whose letters are all identical recursed until the stack died. `LoadDictionary` now rejects words with fewer than two distinct letters, and `scramble` bounds its attempts and falls back to a rotation. Both, because the first depends on the operator's word list and the second does not. It was unreachable with the embedded dictionary and reachable with a custom one, which is the worst combination: it could only ever have fired in production.

**The leaderboard's mutex is unexported and its marshalling happens under it.** The old struct exported the mutex on a type that gets JSON-marshalled to be persisted, and the marshalling ran outside the lock while `AddWin` held it: concurrent map read and write, which in Go is a **fatal** runtime error, not a recoverable panic. `MarshalJSON` and `UnmarshalJSON` are the only way in.

**The weekly reset compares week boundaries and catches up.** It asked `pool.ntp.org` what day it was, hourly, and reset only during one hour on Monday, so a failed query or downtime across Monday morning skipped a whole week (finding 17). A bot that was off all Monday now resets on its first tick back. The display derives the next reset from the same boundary, where it used to compute it from the host clock while the reset consulted NTP.

**The pre-M11 leaderboard format is still read**, and that asymmetry with the corpus is deliberate: `storage.Open` refuses an old corpus outright because a corpus is re-derivable from Discord history, whereas a week of wins is not re-derivable from anything.

**The Manager does not count messages any more; it asks a `Counter` it declares itself.** M11a gave it a per-channel activity map, and M11b took it away, because two other features needed the same number and one of them keeping a private copy is the finding-28 shape. `Note` became `MaybeStart`. `Start` no longer clears the channel's count, since the count belongs to a shared observer now and zeroing it would lie to aggro and autonomous posting about how busy the channel is; a per-channel cooldown of one `ActivityWindow` reproduces the old behaviour exactly, because clearing meant a full window of fresh traffic before the threshold could be met again. A nil `Counter` disables the trigger, which is the quiet direction: no way to tell whether a channel is busy is a reason to start no games.

**`MaybeStart` asks the counter outside its own lock**, because the counter is another package's mutex and holding one lock while taking another is how a lock-ordering deadlock gets built. Nothing needs the count and the game map to be consistent with each other; the worst case is a game started on a count that was true a microsecond ago.

### `internal/activity` is the one place that knows where people are talking

Three features need to know how busy a channel is or who is around: word games deciding a channel has earned a puzzle, autonomous posting and interval-mode word games choosing somewhere to speak, and aggro choosing a target. Before M11b, two of them **paged Discord's REST API** for it and the third kept its own copy.

**The two that asked Discord were the expensive mistake.** `getActiveChannels` paged every text channel in every guild fifty messages at a time with a 50ms sleep between pages; `findRandomActiveUser` then called it per guild and fetched another hundred messages per active channel. Hundreds of REST calls per aggro tick, on an hourly ticker, for information the gateway had already delivered and the bot had thrown away. Both functions are deleted, not refactored.

**It is the state cache plus a tally, not the state cache alone**, because they answer different halves. `s.State` knows a channel exists, its name, its type and its NSFW flag, and `LastMessageID` gives recency. It does not know volume. So `busiestChannel` reads the tracker for how busy and `State` for what and where, and neither costs a request.

**The tracker keeps timestamps, not a counter, so each consumer brings its own window.** Word games want "is it busy right now" (5 minutes); aggro wants "who is around" (6 hours). One counter with one window would have forced them to agree, and that is what made the word-game manager keep a second copy in the first place.

**Both maps are bounded and this is not optional.** They are keyed by channel and by user, so they grow with every guild the bot joins and every person it meets. That leak has already happened twice here: the conversation memory before M7b and the word-game activity map in M11a.

**The per-channel history is a fixed ring, so a count saturates**, and that has a config consequence worth knowing: `PEREGRINE_WORDGAME_ACTIVITY_THRESHOLD` is capped below the ring size, because a threshold above it could never be met and the knob would silently do nothing. `activity.PerChannelHistory` is exported so a test asserts the relationship instead of two files agreeing by comment.

**`Note` is called after the learn gate, deliberately.** A spam flood would otherwise advertise a channel as busy and pull the bot toward exactly the place it should be ignoring. Counting only what the corpus was willing to see is the same decision, made once.

Two behaviour changes to know before debugging them as outages: **aggro has no target for the first minutes after a restart**, because the candidates are people the bot has seen, and **`busiestChannel` returns nothing on a cold start** rather than falling back to `LastMessageID` recency, because recency without volume would let a channel whose last message was 59 minutes ago decide where the bot speaks unprompted.

### Image reposting is the bot republishing under its own name

`SPEC.md` §4 A7 is the finding. Cached user-posted image and Tenor URLs get reposted later, by the bot, in a channel of its choosing, so a hostile user seeding the cache is the attack and the operator wears the result.

**The mitigations were specified in M5 and unbuildable until M11b, because the problem was the key layout rather than a missing check.** The cache was keyed by the URL alone, so nothing recorded where an entry came from: there was nothing to attribute, nothing to cap per person, and no way to ask which entries a deleted message had contributed. `Writer.DeleteImageURL` sat in `internal/storage` for a milestone with a comment about deleted messages and no caller, which is what an unimplementable design looks like from the inside. The key is `<message snowflake> NUL <url>` with the author snowflake as the value now, and three of the four mitigations fell out of that.

**The caps are enforced inside `AddImageURL`, not at the caller**, for exactly the reason `CheckLearn` lives inside `learnMessage`. At the per-author cap the author's **own oldest** entry is evicted rather than the new URL dropped, so a prolific poster stays current without exceeding their share. A cap at or above `PEREGRINE_IMAGE_CACHE_SIZE` is a startup error naming both variables: a cap one person can saturate is not a cap, and an operator who believes the protection is on is worse off than one who knows it is off.

**Deletion is a signal about republishing, not about the corpus.** The `MESSAGE_DELETE` handlers revoke cached URLs; they do not unlearn n-grams. Removing learned text on delete is a different and much harder question and is deliberately out of scope.

**Putting the snowflake first also made eviction correct and cheap by construction**, which is finding 10's lesson one bucket over. Byte order is numeric order is chronological order, so a cursor's `First()` is the oldest. The previous trim searched the whole bucket for the minimum timestamp *inside* the eviction loop, so trimming k entries cost O(n*k), which is finding 11's shape.

**Do not hold `imageURLMutex` across a `store.Update`.** It used to wrap the whole capture, so one goroutine's write transaction (already serialized against every write in the process) also blocked every other capture from reading the slice. The lock guards the slice; the store guards itself.

**`schema_version` is 2, and it has a migration where version 1 has a refusal.** That asymmetry is the rule: `upgradeToV2` empties the image cache, because a hundred URLs refill themselves from live traffic within minutes, whereas converting the corpus would mean rewriting every key and the corpus is re-derivable from Discord history anyway. Migrate what is cheap to lose, refuse what is not.

### Authorization is one function and only one

`games.Service.Authorized` is the only authorization check in the codebase and the only place that reads the admin user ID. It moved there with the commands in M11c; the AST test that pinned it in `internal/legacy` went with them.

It took two milestones because the value and the shape are different problems. M2 replaced a hardcoded Discord ID with a fail-closed environment variable, which fixed the value; it stayed an inline comparison in one command body, which left the shape. The second command to need authorization would reimplement it, and the way an inline reimplementation goes wrong is the empty case, which fails **open**: a missing variable becomes a public operator command. A behavioural test cannot cover that, because it would have to know about the command nobody has written yet.

Note the trap this exposed. The old inline form was `cfg.AdminUserID == "" || m.Author.ID != cfg.AdminUserID`, which short-circuited and therefore never evaluated `m.Author` when no admin was configured. Turning it into a function call made the argument always evaluate, which found a nil `Author` in a test fixture. Production always has one; the fixture did not.

### Ingestion asks "what is new", not "what have I forgotten"

`internal/ingest` walks guilds and channels, `internal/learn` learns, and `internal/plugins/ingest` is the three adapters between them plus the loop. That split is deliberate: reading the wrong messages wastes API budget and corrupts counts, whereas learning them wrongly is a safety question with a gate in front of it.

**The old loop re-read the trailing `PEREGRINE_INGEST_LOOKBACK` on every tick**, which at the shipped defaults is roughly 144 passes over every message, and relied on the history bucket for dedup. That bucket is capped at `PEREGRINE_MAX_HISTORY`, so on a busy guild the older half of each window had already been evicted and was **learned again, counting its n-grams twice** (finding 13). Raising the cap would only move the corpus size at which it starts.

**`PEREGRINE_INGEST_LOOKBACK` is now a bootstrap bound, not a re-read window.** It applies to the first pass over a channel and never again, because later passes resume from a stored cursor. If you change what it means, you are reintroducing finding 13.

**`ingest.SnowflakeAt` synthesizes an `afterID` from a timestamp**, which is how the bootstrap gets an anchor for an instant that has no message. It clamps below the Discord epoch rather than wrapping, and that guard is not decorative: an unclamped subtraction produces a *future* snowflake, so the pass would ask for messages after the year 4000 and silently ingest nothing, forever.

**Cursors are monotonic in the writer, not just in the caller.** `Writer.SetCursor` refuses to move backwards. Two things can hand it an older ID: a batch processed out of order, and two passes overlapping on one channel. Either would rewind the mark and re-learn everything between, which is finding 13 by a different route.

**The cursor advances past bot messages too.** A page of nothing but bot messages is still progress; leaving the mark behind means re-requesting them every pass forever.

**The count-then-fetch double scan was deleted, not replaced.** Its job was deciding which channels were worth reading, and a cursor answers that as a side effect: an idle channel returns one empty page. A test measures requests per channel and asserts exactly one.

**A channel worker swallows its error on purpose.** `errgroup` cancels its context on the first error returned, so a guild the bot cannot read would abandon the ones it can. Failing to list guilds *is* fatal, because there is nothing to walk and a cheerful log line would hide a revoked token.

### The plugin layout, and what each package may reach

As of M11c the features are packages. Five of them are shared and hold no feature state:

- **`internal/names`** answers who a message is about. Its two phases are split by transaction duration rather than tidiness: `Resolve` makes REST calls and touches no corpus, `FromContent` reads the corpus and makes no network call, and it takes the `*storage.Reader` it is already inside so the version that could nest does not compile.
- **`internal/learn`** is the only way anything enters the corpus, and `CheckLearn` lives inside `Learner.Message`. The AST test that pins that moved with it.
- **`internal/generate`** is the glue from a prompt to a sentence, plus the per-channel conversation memory. There is **no emit gate here**: it moved to the guard in M10a and leaving a copy would invite the belief that a producer gates itself, which is how three paths reached Discord ungated.
- **`internal/channels`** answers what a channel is and where the bot may speak unprompted, from the state cache, never REST. Four features needed one or both.
- **`internal/activity`** counts traffic, described above.

Then `internal/plugins/{chat,aggro,images,games,autopost}`, each a `core.Service` owning its own state. **`chat` is the reactor** and it names the others through interfaces it declares itself, which is what keeps the step table testable without five real plugins behind it.

**Registration happens only in `cmd/bot`**, in `registerServices`, and the order there is behaviour: `Init` runs in registration order and `Shutdown` in reverse, so `chat` is registered last (its `Init` arms the gateway handler and its `Start` resolves the bot's identity, so everything it calls must have loaded first) and therefore stops first.

**Two constants exist in one place because they had three.** `corpus.StartOfWeekUTC` had three implementations before M11c, and one of the disagreements between them is finding 17. `text.IsStopWord` had two, and the list being plain English function words only is half of why clustering collapsed (finding 29).

**Interface type identity forces a few imports that look wrong and are not.** Go requires exact type identity on an interface method's signature, so `channels.Counter` names `activity.Channel`, `chat.Images` names `images.Attachment`, and every plugin's `Guard` names `*discordgo.Message`. Each of those could be avoided with an adapter whose only job is renaming fields, which is more code and one more place for a mistake.

### Health and the counters nobody was reading

`internal/plugins/health` is one service with two loops, and it exists because two things were reporting into the void.

**Four counters existed specifically so that a persistent problem would be visible, and were read by no code at all**: `Dispatcher.Dropped`, `Dispatcher.Queued`, `Gate.LearnRejected` and `Gate.EmitRejected`. Peregrine drops work and refuses output by design (the dispatcher drops rather than blocking, because discordgo dispatches every event on its own goroutine; the gate refuses in both directions), every one of those decisions is correct, and every one is invisible. A queue that is persistently full and a blocklist that is firing constantly look exactly like a quiet server unless something says otherwise.

**The report carries deltas as well as totals.** A lifetime count of 40,000 rejections says nothing about whether it is happening now, which is the only question worth asking of a counter on a ticker. A nonzero delta additionally gets its own record at a level that carries, naming what to do about it; the routine line gets skimmed.

**The latency probe reads `Session.HeartbeatLatency()`.** It used to make a `User("@me")` REST call every two minutes purely to time it, which is asking the network for something the library already measures: finding 17's shape in a different feature. It also measured the wrong thing, since a slow REST endpoint and a struggling gateway connection are different problems.

**Shutdown reports once more, before the wait.** That final line is where an operator reading a container's last output finds out whether the queue had been full, and putting it before the wait means a stuck loop cannot cost it.

### The reactor, and the consume contract

`chat.handle` runs a table of named steps in `internal/plugins/chat/chat.go`, each returning whether it **consumed** the message. The runner stops at the first one that does.

**Consumed means "this was addressed to me", not "I did something".** The aggro step reacts and the reply step posts, and neither consumes, because a message that earns a reply is still conversation and must still be learned from. Only a command consumes. `TestOnlyCommandsConsume` parses the file and fails if any other step returns `true`, because a step that starts consuming silently stops learning for whatever it matches and no behavioural test would notice.

That contract is what closed finding 9. A `return` statement would have fixed the one branch and left the shape available to the next command somebody adds: `!leaderboard` answered and then fell through into the reply generator and the learn step, so the bot replied to its own command as if it were conversation and taught itself the string `!leaderboard`.

**Two orderings are load-bearing.** The learn gate is first, so a message the bot will not learn from is also not replied to or reacted to. Commands come before reply and learn, which is the entire point.

**Commands match the whole trimmed message, not a prefix.** A prefix match makes it impossible to talk *about* a command, so "you should try !leaderboard sometime" would be swallowed and answered.

**`!wordgame` is not a command when word games are off**, because consuming it would mean silently ignoring a message for a feature that is not running. `!leaderboard` is deliberately *not* gated on the feature flag: its chat half reads the stats bucket, which is populated regardless.

**Self-learning is keyed by the reply's own message ID.** Both it and the learn step used the *user's* ID, and `learnMessage` dedupes on that ID, so whichever transaction committed first won and the other became a no-op: one message was silently discarded per interaction, and which one depended on a goroutine race (finding 6). This was data loss, not inefficiency.

**`reaction.names()` memoizes `extractNamesFromMessage`.** It was called three times per message, and each call makes a `GuildMember` REST request per mention plus a read transaction. Use the cached list; if you append to it, copy first, because the self-learn step reads the same slice.

### Every outbound Discord call goes through `internal/discordguard`

**Nothing peregrine posts may ping, and this is where that is enforced.** Peregrine's output is Markov text assembled from arbitrary user messages, so every send is untrusted-input-shaped *by construction*: a user mention that got learned is a corpus token like any other, and the generator emits it again whenever the chain walks through it. Nobody chose that and nobody can predict when it fires.

discordgo will not stop it. Its send helpers build a request with a nil `AllowedMentions`, the field carries `omitempty`, so it is dropped from the JSON and Discord reads a missing field as "parse every mention". `ChannelMessageSendReply` additionally sets nothing, so the replied-to author was pinged on **every single interaction** (finding 8).

**Set the slices explicitly, do not rely on the zero value.** `&discordgo.MessageAllowedMentions{}` marshals as `"parse":null`, not `"parse":[]`. discordgo's own comment says a zero value allows no mentions, and that is true of the field being *present* but not of its value being the documented empty array; whether Discord treats a null parse like an empty one is a fair reading that is not written down. `allowedMentions()` sets `Parse`, `Roles` and `Users` to empty slices and `RepliedUser` false. The test asserts the **marshalled JSON**, because that is the only place the difference exists.

**The guard is a chokepoint for the same reason `CheckLearn` lives inside `learnMessage`.** Fourteen call sites send something; a rule applied at thirteen of them is not a rule. `TestNothingBypassesTheGuard` parses the package and fails if any function outside the three named adapters calls `ChannelMessageSend*`, `ChannelMessageEdit*`, `ChannelMessageDelete` or `MessageReactionAdd` on a session. A behavioural test cannot cover this: suppression only matters once a request is marshalled, so a handler test proves nothing about a site it does not reach.

**`CheckEmit` moved here from the generation exit, and that closed real holes.** It used to cover the reply path only, so the autonomous poster, the word-game announcements and the transcription results all reached Discord ungated. Generation is not the only thing that produces text: the worst case was the transcript path, where someone could have had the bot say anything by saying it out loud.

**Deletes are not content-gated and reactions are.** A delete says nothing and cannot ping, so gating it would stop the bot cleaning up during exactly the incident it needs to. A reaction is the bot visibly participating, so `PAUSE_ALL_WRITES` has to stop it. `Unreact` is the mirror image and is *not* pause-gated: taking a reaction back is withdrawing, and refusing it would leave the bot's mark on someone's message with no way to remove it until the pause lifts.

**A forbidden-call list is only as good as its enumeration.** The M10b split dropped a `MessageReactionRemove` call and `TestNothingBypassesTheGuard` did not notice, because that method was missing from the list. When the guard grows a method, the list grows with it.

**`PEREGRINE_IGNORE_CHANNELS` is enforced in the guard, not the reply path.** An operator setting it means "not in there", not "not in reply to a message in there", so the autonomous poster and word games have to respect it. It does not stop *learning* from those channels, which is the same asymmetry `PAUSE_ALL_WRITES` has.

### Discord calls are logged, never discarded

Every one of them used to discard its error, so a send Discord refused (missing permission, rate limit, channel deleted mid-flight) was indistinguishable from one that succeeded: the bot appeared to ignore people at random with nothing in the log. The guard logs all four operations, at deliberately different levels: a failed send or edit is an error, whereas a failed delete or reaction is Info, because failing to delete is routinely benign (somebody removed the message first, or the bot has no Manage Messages there) and alarming about it trains an operator to ignore the log.

`sendMessage`, `editMessage` and `deleteMessage` were three-line adapters onto the guard in the old holding pen, and M11c deleted them: they existed so the call sites would not change shape twice, once for the chokepoint and again for the plugin move, and this was the second time. **`TestNothingBypassesTheGuard` now permits no adapters at all** and scans the whole module rather than one package, so outside `internal/discordguard` no function may name a session method that speaks.

### What peregrine deliberately did NOT port from merlin's guard

Merlin's `discordguard` carries a per-guild pause, a dry-run mode, a write governor and an audit journal, because its dangerous operations are irreversible from Discord's side: a deleted archive channel, a member stripped of every role. Peregrine deletes nothing and edits no permissions. Its dangerous operation is **speaking**, and the controls that fit that are one process-wide pause and one content gate, both already in `internal/safety`. Porting the rest would be structure with no failure mode behind it.

## The generation pipeline

The engine is `internal/markov` as of M7a. Read `SPEC.md` §5 for the full specification. The parts most worth knowing before touching it:

**`internal/markov` imports neither bbolt nor discordgo, and must not start.** It declares a `Corpus` interface and `*storage.Reader` satisfies it structurally, which is what makes a thousand lines of scoring testable against Go maps. The compile-time assertion that the real reader satisfies it lives in an **external** test package (`markov_test`), so the `storage` import needed to make that claim cannot leak into the production package's import list.

**A Generator must not outlive its transaction, and that is why there is no package-level one.** A `Generator` holds a `Corpus`, which in production is a `*storage.Reader` bound to one transaction. `legacy.generateSentenceAttempt` constructs one inside each `store.View`, costing a struct allocation per reply. Do not cache it in a package variable: that reintroduces exactly the class of bug the `Reader` type exists to make unwritable.

**Scoring is `log P` plus additive logits, and the additivity is the whole point.** Addition in log space multiplies a candidate's probability by a *bounded* factor: a weight of 0.7 roughly doubles its odds, whatever the base probability was. The old form multiplied an unnormalized score by an unbounded topic term and a dozen ad-hoc factors, so scores spanned orders of magnitude and one candidate almost always dominated. The sampler was argmax with noise, which for a chaos bot is the worst available failure mode, and it hid well because the output still varied a little. **Every term that sums over evidence is squashed with `tanh` so it cannot exceed its own weight.** If you add a heuristic, bound it, and add it to `Weights` rather than writing a bare number into `heuristics`.

**The thirteen logit weights are constants, not environment variables, and that is deliberate.** They are only meaningful relative to each other and an operator has no instrument to judge them. `Params` comes from the environment because `SPEC.md` §5.3 says those specific dials are the operator's. Tune `DefaultWeights` by reading golden samples.

**`Creativity` is gone and there is no `PEREGRINE_CREATIVITY`.** It was applied as an exponent of `1/(Creativity+0.01)`, so at its 0.75 default the exponent was 1.316, which *sharpened* the distribution, and raising it toward 1 could only approach 1.0 and never pass it: the knob could not reach the interesting half of its own range. M2 deliberately declined to promote it for that reason, and `PEREGRINE_TEMPERATURE` replaced it in M7a in the same change that normalized the scoring so the dial actually moves. Do not bring it back.

**Backoff is interpolated Kneser-Ney, and lambda is not a free parameter.** The old prefix-shrink loop took the first non-empty result from the longest prefix, so a 4-gram and a bigram continuation were scored on the same scale. KN is the right model for a corpus this sparse: a server's chat is 10^5 to 10^6 tokens, so at `MaxNGram=5` nearly every 4-gram has count 1. `lambda(c) = D * N1+(c .) / c(c .)` is *exactly* the mass the discount removed, which is why the distribution sums to one with no normalization step. `TestInterpolatedKNSumsToOne` is the test that catches a wrong lambda and the only one that would: any other value still generates words, and only the balance between orders is wrong, which nothing in the output reveals.

**`Reader.PrefixTotal` is a cursor scan and that inverts this repo's usual rule on purpose.** The KN normalizer `c(prefix, .)` is needed at every order including the single-word context. Keeping it as a counter would mean an unconditional read and write of `kn_succ` on every n-gram of every message, about twenty percent more steady-state write work inside bbolt's single writer, which serializes all ingestion. The scan is zero-allocation, sequential, sums only eight-byte values, and is memoized per sentence. Trading a bounded read-path scan for write-path contention is the right direction here: the reader is a human waiting for a reply.

**Candidate enumeration is bounded, and the `minCandidates` floor does real work.** At order 5 the longest context usually has exactly one continuation, so without backing off to widen the set the step is deterministic no matter what the sampler does. That was much of why the old engine felt canned. The per-order and total caps select by count, which biases enumeration toward frequency; that is stated in the code rather than hidden, and it is acceptable because top-k truncates at 40 anyway.

**Kneser-Ney is deliberately not applied in its textbook form, and this is the single most counter-intuitive decision in the codebase.** KN estimates lower orders from *continuation counts*, the number of distinct contexts a token follows, which correctly demotes a token like "Francisco" that is frequent but nearly always preceded by "San". The problem is that a meme, a copypasta and an inside joke are statistically indistinguishable from "Francisco": high frequency, few distinct contexts. Pure KN would therefore systematically suppress exactly the register this server runs on. `PEREGRINE_KN_RAW_MIX` interpolates the lower-order estimate back toward raw counts (`0.0` is textbook KN, `1.0` is raw counts, default `0.25`). If someone "fixes" this to 0 because a paper says so, output quality by the only metric that matters here gets worse.

**Candidate order is now deterministic, and one heuristic had to go because of it.** Candidates used to arrive in Go's randomized map iteration order, because the successors of a prefix were a `map[string]int`. `Reader.Successors` is a cursor scan, so they arrive in sorted key order. That is fine everywhere except one line: a 1.0-to-1.05 bonus applied to `cands[0]`, which was meaningless-but-unbiased when the index was random and would have become a permanent 5% advantage for whichever continuation sorts first alphabetically, at every step of every sentence. It was already slated for deletion in M7 (`SPEC.md` §8, G4) and was deleted in M6b instead, because a known-bad heuristic becoming systematic rather than random is a change worth not shipping. **When you change how candidates are enumerated, check what reads their index.**

**Prefix lookups are lowercased at the point of lookup, and that is defence rather than a fixed bug.** Learning lowercases the prefix before storing it; the generation path did not lowercase before looking it up. M6b added it, and the honest position is that it changed no behavior: every producer of the generation prefix is already lowercase, because the seed's candidates are all interned from lowercased sources, every later word is a stored successor token, and the one branch that could have introduced a raw prompt word is unreachable with a non-empty corpus. The reason to normalize anyway is that the alternative is an invariant held by five separate producers agreeing, none of which states it, against a lookup whose failure mode is silently returning no candidates. M7 normalizes once at the storage boundary. `TestGenerateWithAMixedCasePromptFindsTheCorpus` says in its own comment that it does not distinguish the two implementations, because a test that looks like a regression pin and is not one is worse than no test.

**The author-diversity gate applies to all THREE producers of words, not just the sampler.** This was a hole until M11c and it is worth reading before touching any of them. Generation puts a word into a sentence from three places: `eligible()` filters the sampler's candidates, `Jump` picks a token when the chain dead-ends, and `Seed` picks the first word. The last two read the co-occurrence indexes, which store a count and a position sum and no author attribution whatsoever, so a phrase one person repeated forty times was refused by the sampler at every step and then handed straight back by the jump. That is A6 defeated by a check at one of three producers, which is A1's shape one milestone after the principle naming it was written down (`SPEC.md` §8, finding 31).

`Generator.attested` is the check, and two of its asymmetries are deliberate. The end sentinel is exempt in `eligible()` and **not** in `attested`, because "the only thing following this word is the end of a message somebody once sent" is not evidence that several people use it. And a seed derived from the **prompt** is exempt, because echoing somebody's own words back is not poisoning: refusing to would make the bot least responsive exactly when it was addressed directly.

**The author-diversity gate is a filter, not a logit, and it does not relax.** A penalty is something a determined poisoner can out-repeat. When the filter empties the candidate set the step is a dead end and the sentence ends early; relaxing the threshold when it bites would make it not a control. The end sentinel is exempt, because gating it would mean a sentence cannot end until several people ended a message the same way. **Know the operational consequence before you debug it as an outage:** at the default of 2, a young corpus has almost no continuation with two authors, so the bot is close to mute until ingestion catches up.

**One length model, and do not add a second.** A target is sampled per sentence between `MIN_WORDS` and `MAX_WORDS`, skewed short; the end token is penalized below the floor, eased to neutral at the target, and rewarded past it with a cap. That single mechanism replaced three that could disagree, so a new length rule anywhere else recreates the problem. The floor penalty is finite rather than `-Inf` on purpose: a context whose only continuation is the sentinel must still be able to end. Short and punchy reads as a joke; long reads as a malfunction.

**Below two words the bot posts nothing, and there is a bounded re-seed above it.** A seed drawn from a non-prompt tier can dead-end on its first step, because the length floor is a logit penalty and a penalty does nothing when no candidate is eligible at all. `generateSentenceWithContext` re-seeds up to three times, keeps the longest attempt, and stays silent under two words. **This is a re-seed, not the discard-and-retry M7b deleted:** that one threw away an end token and continued from the same prefix, fighting the length decision. This one abandons the attempt and draws a different seed. It exists because the golden samples printed one-word replies like "roof", which is what the harness is for.

**Seed selection is seven weighted tiers and the two-hop tier is the interesting one.** It replaced the concept-cluster tiers: the co-occurrence indexes answer "what appears near X", and only a second hop answers "what appears near something that appears near X". Bounded, weight-decayed, requiring at least two recorded co-occurrences per hop, and every candidate must pass `HasSuccessors`, because a seed the chain cannot continue from produces the one-word reply above. `Generator.Jump` is the same machinery at a different moment rather than a second implementation, which is how legacy ended up with a cluster pivot that had never fired sitting next to a working one.

**The persona is one mechanism.** `Persona` drives both the in-sampler lexicon bias and the post-pass filler, so the roast decision is made once instead of by two independent coin flips. The post-pass picks its insertion point from a triangular draw concentrated mid-sentence; the old flat draw over the interior put filler at the edges, where an interjection reads as a typo. The lexicon matches whole tokens and real chat inflects, so both "cope" and "coping" are listed: extend it by enumerating forms, not by stemming, because in this register the inflected form is often the joke.

**Conversation memory is per channel and bounded.** One shared memory meant a reply here was steered by an unrelated conversation elsewhere, which is wrong context rather than chaos. The 200-channel bound is not optional: the map grows with every guild the bot joins, and a test that uses one channel would never reveal it.

**Co-occurrence is windowed on the learn path.** `PEREGRINE_COOCCURRENCE_WINDOW`, default 5, 0 for the old all-pairs behavior. Both halves matter: the loop runs inside the single write transaction that serializes all ingestion, and "co-occurs anywhere in the same message" is a claim that gets weaker the longer the message is. Both directions of each pair are still written, because the index stores the *associate's* position and the positional heuristics read it.

**There is no `*rand.Rand` anywhere, and `markov.Source` is how the golden harness stays reproducible without one.** `DefaultSource` is a stateless adapter over `math/rand/v2`'s goroutine-safe top-level functions, so production has nothing to race on; tests pass `rand.New(rand.NewPCG(...))`. Reproducibility is load-bearing rather than tidy: if the same seed produced different text twice, no printed difference could be attributed to a weight.

**Custom emote output had never worked, and M3 fixed the cause.** The `:shortcode:` resolver walks `s.State.Guilds`, and the session never requested `IntentsGuilds`, so that slice was always empty and the resolver had never once succeeded. In a meme server the server's own emotes are most of the register, so this was an engagement bug before it was a performance bug. `core.NewSession` now requests the intent. The state cache it populates is what M10 uses to replace the per-message REST `s.Channel` call for the NSFW check; that call site is still REST today. Emote-bearing output has a golden-sample check as of M7a, now that it is possible at all.

## Safety model

`SPEC.md` §4 is the full threat model. Assume users actively try to make the bot say gross or borderline illegal things, and that **the operator carries the consequences of whatever it posts**. Four rules follow, and each exists because of a specific hole.

**Filtering the corpus cannot bound the output, even in principle.** A Markov chain composes novel sequences from n-grams learned separately, so fragments that were individually innocuous can join into something the operator has to answer for. Input filtering lowers the rate; only an output gate bounds the result. This is why there are two gates and not one, and why removing the output gate as redundant would be wrong.

**Gates go at chokepoints, never at call sites.** `gate.CheckLearn` is called **inside `learn.Learner.Message`**, not at any of its callers. That placement is the entire fix for the worst finding in the review: the live handler filtered, but the historical backfill, self-learning and voice transcripts passed content through raw, and since the backfill re-read the trailing 24 hours every ten minutes, a message the live path blocked was learned anyway, unfiltered, minutes later. A check at one of four call sites is not a check. A fifth caller is now covered without anyone remembering to cover it.

Do not hoist it to the call sites for performance. `internal/learn/learn_test.go` parses the package and fails if the call leaves `Learner.Message`'s body, because that regression passes every behavioural test.

**Reject, never launder, on the learning path.** `Verdict` deliberately has no field for rewritten text, which makes laundering unexpressible here rather than merely discouraged. A rewritten message is still learned, with its structure intact and a harmless token sitting in the offending word's grammatical position: the bot has been taught the sentence. The `m.Content = filterSlurs(m.Content)` that used to sit in the live handler is gone, and removing it was load-bearing in a second way: with the gate in place, laundering *before* the gate would hand it pre-cleaned text and defeat it. A denylist test on `safety.Verdict`'s field names keeps a rewritten-text field from appearing later; it is a denylist rather than an allowlist so that innocuous additions do not fail it and train somebody to update the list without reading why.

**Match on a normalized form, never raw text.** `safety.Normalize` case-folds, applies NFKD, strips combining marks and format characters, folds Cyrillic and Greek confusables plus leet substitutions to ASCII, collapses whitespace, joins runs of three-or-more spaced single letters, and caps repeated characters at two. Blocklist patterns are written against that form and **must not** re-enumerate evasions it already removes; enumerating them by hand is a game the defender loses.

The output is **matching-only**. It is lossy by design (it folds digits to letters, so `2026` becomes `zo2g`), and must never be stored in the corpus, emitted, or shown to a user. `internal/filter` still matches raw text, which is why its tests assert its own evadability, and why `internal/safety`'s tests assert the same inputs are caught.

Two thresholds in the normalizer are the difference between a defence and a false-positive generator, and both are pinned by tests: joining requires **three** spaced single letters (two is ordinary text, so `is a b test` survives), and a separator adjacent to a real multi-character word ends a run (so `well-known` and `e-mail` survive).

**The blocklist is data, not source.** It loads from `PEREGRINE_BLOCKLIST_PATH`; the real file is gitignored and only `blocklist.example.txt` is committed. Committing the real list would make this repo a searchable copy of one and turn every addition into a rebuild and a public diff instead of an edit made mid-incident.

Once set it fails **closed**: a missing file, an unreadable file, a malformed line, an uncompilable pattern and an *empty* file are all startup errors, reported one per line with line numbers. Leaving it unset is allowed and warns loudly, because a developer on a scratch corpus should not need to invent a slur list first and the built-in baseline still applies. Categories are `slur` (drop on learn, block on emit), `illegal` (same, plus an operator alert, so keep it narrow) and `spam` (drop on learn only, since the bot generating something that resembles advertising is not an incident).

**`PEREGRINE_PAUSE_ALL_WRITES` refuses every send, and deliberately does not stop learning.** During an incident the output is the problem, not the corpus, and stopping ingestion would also stop the bot noticing what is being said to it. It logs at startup, so a silent bot is never a mystery. `Gate.SetPaused` exists so a Discord-reachable kill switch can be added later without inventing a second lever.

**Poisoning is cheap unless generation requires author diversity.** n-gram weight is raw frequency, so repeating a phrase is a direct write to the model, and one determined user can teach the bot to say anything. `PEREGRINE_MIN_DISTINCT_AUTHORS` requires a continuation to have come from `k` distinct authors before the bot will *generate* it, independent of how often it was seen. That turns the attack from persistence into collusion. The counts have been maintained since M6b and **the gate reads them as of M7a**, defaulting to 2, so the safe direction is what an operator gets by doing nothing.

It is a **filter, not a penalty**, and it does not relax when it empties the candidate set: the step becomes a dead end and the sentence ends early. A safety control that yields the moment it has an effect is not a control. The end sentinel is the one exemption, because gating a sentence's ability to end on how other people ended theirs is a length bug wearing a safety hat. The consequence worth knowing in advance: on a corpus in its first hours almost nothing has two distinct authors, so the bot is nearly mute. That is the gate working, and the fix is more people in the corpus rather than a lower threshold.

The bot's own output is excluded from those counts, and the exclusion is a comparison in `learnMessage`: if the author's user ID equals `botID`, an empty author is passed to `LearnNgram`, which does not count an empty author. That matters because self-learning feeds the bot's replies back into the corpus under the bot's own identity, so without the comparison anything it said once would carry a diversity count of one from the moment it said it, bootstrapped by the bot rather than by people. `TestLearnMessageExcludesTheBotFromAuthorDiversity` pins it.

## Deployment

Merlin's pipeline, adapted. `.github/workflows/ci.yml` runs `go vet`, `golangci-lint`, `go test -race -cover`, `govulncheck`, a `gitleaks` secret scan, the prose check and a Docker build on every push and PR. On push to `main` only it also builds and pushes a multi-arch (amd64/arm64) image to GHCR and deploys to the VPS over SSH using `docker-compose.prod.yml`.

The prod image tag is pinned to the commit SHA in `deployed-tag.env` on the host, with the previous value kept in `previous-tag.env`, so a rollback is editing one line rather than working out from GHCR what used to be running. The image prune is scoped `--filter "until=168h"` deliberately: a bare `docker image prune -f` on a host that just retagged can delete the previous release, which is the one thing a rollback needs to still exist.

**There is no backup sidecar, and the omission is deliberate.** Merlin's works because `pg_dump` is a client asking the server for a consistent snapshot. bbolt has no equivalent, and `cp markov.db` is *not* a backup: the file is a single mmap updated by copy-on-write pages plus a meta-page flip at commit, so an external byte copy can capture a state between the page write and the flip, or mid-remap, and the result usually *appears* to work, which is the worst property a backup can have. A sidecar cannot snapshot it either, because of the exclusive flock.

Backups are in-process, in `internal/plugins/backup`: `Store.Backup` takes a read transaction and calls `tx.WriteTo`, which is consistent by construction and does not block writers. **Three retention rules, and each is a way to lose everything:**

- **Write a temp name, then rename.** A half-written file with a real name is indistinguishable from a snapshot, and the one moment anybody looks in that directory is the moment they need a file that is definitely whole.
- **Prune only files this service named**, matching both the prefix and the suffix. A retention pass loose in a directory is a delete loop pointed at whatever else is in there, and on a mounted volume that could be the corpus itself.
- **Never prune after a failed snapshot.** Pruning on a schedule while backups quietly fail removes every good copy, one tick at a time. Same reasoning as scoping the deploy's image prune to `until=168h` so a rollback still has an image to roll back to.

**The loop is deliberately not `Immediate`,** which is the one thing here that looks like an oversight. A snapshot at startup means every restart writes one, so a crash loop churns through the retention window and discards every older copy in minutes, which is exactly when the older copies matter most. Shutdown takes no final snapshot either: it is a read transaction against a corpus about to close, under a budget shared with every other service.

**`PEREGRINE_BACKUP_DIR` must be a writable mount in a container.** The image runs `read_only: true`, so any path outside a volume or bind fails on the first write, and a relative path resolves against the distroless working directory. `docker-compose.prod.yml` binds `./backups` to `/backups` for this, and it is deliberately **outside** the corpus volume so that losing or removing that volume does not take the snapshots with it. Each snapshot is a full copy, so the disk cost is `KEEP` times the corpus size.

`markov.db`, the whisper model and the ffmpeg binary are all gitignored. `voicenotes/models/ggml-small.bin` is 465 MiB, over GitHub's hard 100 MiB per-file limit, so a commit containing it cannot be pushed at all and the only remedy is rewriting history. `.dockerignore` exists for the same weight: the working tree is around 692 MB and `COPY . .` would ship all of it into the builder on every build.

## Transcription is a seam with no engine behind it

`internal/plugins/voicenote` is a complete plugin over an `Engine` interface, and **the only implementation that ships reports itself unavailable**. That is the state as of M12, deliberately: the implementation it replaced shelled out to `ffmpeg` and `whisper-cli` and needed a 465 MiB model, none of which exists in a distroless image that has no shell at all, and all three assets are gitignored because the model alone is over GitHub's hard 100 MiB per-file limit. The feature had never run anywhere but one Windows machine.

`PEREGRINE_ENABLE_TRANSCRIPTION` still defaults to **false**, and the flag being on with no engine is a startup **warning** naming the seam rather than silence: a feature that is enabled and does nothing is the exact shape of findings 30 and G3.

**The plugin's own half is real and tested**, because that is the part this repository owns: a bounded queue that drops and says so, a context-bound worker the shutdown path waits for, a placeholder posted before a job is queued (and no job at all when the guard refuses the placeholder, because a transcription with nowhere to report is a Whisper run spent on nothing), and every message through the guard.

**Transcripts are not learned, and that is a decision rather than an omission.** The old path fed them into the corpus, which was one of A1's four unfiltered callers and the worst of them: a Whisper transcript of arbitrary audio is the least controlled text the bot handles, and somebody could have taught it anything by saying it out loud. If a future engine wants transcripts learned it goes through `learn.Learner.Message` like every other path, and that decision belongs in a milestone rather than in a side effect.

**What a real engine must not reproduce** is listed in the package comment: a bare `http.Get` with no timeout, status check or size cap; `exec.Command` with no context; paths resolved against the working directory; and a scratch filename built from the URL by hand. Read it before writing one.

## Conventions

- Plain punctuation only: no em dashes, ellipsis characters or curly quotes. CI enforces it. See the tokenizer exception above.
- Comments explain *why*, and name the failure mode that motivated the code. A comment restating what the line does is noise; a comment saying "this used to hang forever with no output" is what stops someone reverting it.
- `main` is protected: PR review plus all CI checks, no direct or force pushes.
- Run `go vet ./...`, `go test ./... -cover`, `golangci-lint run` and the prose grep before opening a PR.
- One milestone (or a slice of one) per PR, per `SPEC.md` §9. The tree must build, vet, lint and test green at the end of every one.

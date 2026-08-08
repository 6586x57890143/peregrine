# Peregrine Bot: Project Specification

CODENAME "peregrine", branded as "peregrine" (bird species), sibling to "merlin".

A Markov-chain Discord engagement bot in Go, built on `discordgo` v0.29 and `bbolt` v1.4. It learns from channel messages and replies in the server's own voice. It runs in a meme-heavy, high-traffic community and its purpose is engagement, fun and chaos, which makes its quality criterion unusual and is the single thing most likely to be misread by someone reading the code cold. See §1 and §5.

This document is the design target. The repository is partway to it; §9 tracks how far.

---

## 1. Design Principles

1. **Quality means "lands", not "is grammatical".** The bot exists to be funny and to provoke replies. Punchy, surprising, on-topic-enough output beats coherent output. A 40-word grammatical Markov ramble is a worse result than a 6-word non-sequitur that gets a reaction. Several deliberate choices in §5 look like defects against a conventional language-model metric and are not.
2. **The operator carries what the bot says.** Users are assumed to be actively hostile and to be trying to make the bot emit gross or borderline illegal content. Protecting the operator is a first-class requirement, not polish, and it is architectural rather than a matter of adding patterns. See §4.
3. **Gates go at chokepoints, never at call sites.** Any check that must not be bypassable belongs at the single funnel every path traverses. A check at one of four call sites is not a check. This principle exists because the moderation in this bot was defeated exactly that way for its whole life (§4, A1).
4. **Fail safe, and never fail silent.** A missing blocklist is a startup error, not an empty ruleset. A refused send is logged, not discarded. One feature failing disables that feature, never the process. `log.Fatal` is reserved for a missing token and an unopenable corpus.
5. **Every feature is part of the product.** Aggro, image reposts, word games, the roast persona and autonomous posting are the engagement surface, not cruft. They get fixed, configured and turned on deliberately. The only thing deleted outright is configuration that was never wired to anything.
6. **Prefer making a bug unwritable over fixing it once.** Where a structural change can make a class of defect impossible to express, take it: the `Reader`/`Writer` seam in §3, the reactor `handled` contract in §6, the two gates in §4.

---

## 2. Repository Layout

Target, per merlin's conventions:

```
cmd/bot/                 wiring only: flags, logger, config, signal handling
internal/config/         Config, Load, validation. Env only, no YAML
internal/corpus/         value types ONLY, zero internal imports
internal/storage/        the only package that knows a bucket exists
internal/dbtest/         temp-file bbolt harness
internal/text/           tokenizer, similarity, sentence cleanup, interner
internal/filter/         spam and character-class filters, precompiled
internal/safety/         normalizer, blocklist, CheckLearn and CheckEmit
internal/markov/         learn and generate. Imports neither bbolt nor discordgo
internal/clustering/     pure algorithm, no I/O
internal/ingest/         guild and channel walk, per-channel cursors
internal/core/           Service registry, Dispatcher, RunLoop, WatchReady
internal/discordguard/   the send chokepoint
internal/maintenance/    -clean-db and -compact against an already-open store
internal/plugins/        reply, learn, aggro, images, wordgame, voicenote, health
```

Seams follow merlin's dominant pattern: **each consumer declares the minimal interface it needs, and the concrete type satisfies it structurally.** `internal/markov` declares `Corpus` (read) and `Sink` (write); `*storage.Reader` and `*storage.Writer` satisfy them with no glue. This exists so that (a) packages cannot form import cycles, and (b) the 1,000 lines of generation heuristics can be tested against Go maps with no database at all, which is currently impossible.

Import rules, stated because each has already nearly been violated:

- `corpus` and `text` stay leaves. `corpus` holds type declarations and nothing else.
- `clustering` currently points the wrong way: `main.go` aliases *its* bucket-name constants, so the algorithm package owns storage-layer names. All bucket names move into `storage` unexported, and `clustering` needs none, because it stops touching a database.
- `markov` must never import `storage`. Every method on `Corpus`/`Sink` returns a stdlib or `corpus.` type, or the cycle returns.
- `storage` never imports `config`. `Open` takes a path plus options.
- `core` never imports `plugins`. Plugins import `core`; registration happens only in `cmd/bot`.
- `dbtest` is not a `_test.go` file (so any package's tests can import it) and is imported by nothing outside `_test.go` files (so it never enters the binary).

**Sequencing.** Everything was `package main`, and two `main` packages cannot share code, so M1 moved the root files verbatim into `internal/legacy` with `main()` renamed to `Run(ctx)`. Every later milestone moves one subsystem *out* of `legacy`, which shrinks monotonically and is deleted in M13. This is the only ordering that keeps `go build ./...` green at every commit while still ending at the target layout.

As shipped in M1:

```
cmd/bot/main.go          main, runGuarded, run. Flags, slog, godotenv, signal context
internal/legacy/         legacy.go (was main.go), filter.go, cleanup.go. Unchanged
clustering/              still at the root; moves under internal/ in M8
wordgames/               still at the root; moves under internal/ in M11
voicenotes/              still at the root; moves under internal/ in M12
```

The three root subpackages keep their paths deliberately. Relocating them is a one-line import change with no risk, which is exactly why it should ride along with the milestone that actually rewrites each one, rather than producing a diff that touches them twice.

`legacy.Run` takes only a `ctx` and no logger, which differs from what this section said before M1 landed. Every log call in the package is the stdlib `log` package, and `cmd/bot` routes those into the slog handler with `slog.SetDefault`, so the output is structured without editing 200 call sites inside a milestone whose entire value was being reviewable as a rename. `slog.SetLogLoggerLevel` must be called before `SetDefault`, or bridged records ignore the handler's level. The signature grows a `*config.Config` in M2.

---

## 3. Storage

bbolt: embedded, single-file, pure Go. Pure Go is load-bearing, because it is what allows `CGO_ENABLED=0`, which is what allows a distroless static image identical to merlin's while still owning a database.

Properties that shape the design rather than merely inform it:

- **One writer, process-wide.** Any slow loop inside `db.Update` blocks all ingestion.
- **Exclusive flock.** `bbolt.Open` always passes a `Timeout` (5s); without it, `-clean-db` against a live bot hangs forever with no output.
- **Nested transactions deadlock.** A read txn holds `mmaplock.RLock` for its whole life; a writer needing to grow the mmap takes the write lock; Go's `RWMutex` queues new readers behind a waiting writer. Outer read plus waiting writer plus inner read never completes, and it gets likelier as the file grows.
- **The file never shrinks.** Deletes free pages for reuse only, hence `-compact`.

### 3.1 The key layout, and why it changes

The current layout stores a JSON `map[string]int` as the *value* of each prefix key. So the key `"the"` holds a map of every word that has ever followed "the", read, decoded, mutated, re-encoded and rewritten in full on **every occurrence of the word "the"**. That is the root performance defect, and it has a pathological special case: the ingestion loop runs the n-gram order down to 1, and at order 1 the prefix slice is empty, so the key is the empty string and **the entire vocabulary accumulates into a single database key**. Nothing reads it: every reader constructs a prefix of at least one word. It is pure write amplification and the dominant reason the corpus reached 128 MiB.

Target:

```
bucket "markov"
  key   = <prefix bytes> 0x00 <next token bytes>
  value = 8-byte big-endian uint64 count
```

`0x00` is a safe separator because the tokenizer can only emit URLs, mentions, emotes, shortcodes and runs of letters, digits, symbols and apostrophes, none of which contain a NUL. The codec asserts this and returns an error rather than writing an ambiguous key. `0x00` also sorts below space, so a seek on `"the\x00"` cannot wander into `"the cat\x00..."`. `IncSuccessor` becomes one 8-byte get, an add and one 8-byte put: constant work, no decode, no whole-map rewrite. The ingestion loop starts at order 2, so the empty prefix cannot be produced, and `PEREGRINE_MAX_NGRAM` validates a minimum of 2. Unigram frequency already lives correctly in the `topics` bucket, one key per word.

`TopicWordBucket` and `NameTopicBucket` get the same treatment (`word 0x00 assoc` to a 16-byte `{count, posSum}`), because they share the shape and are the real cost behind finding 12.

### 3.2 Kneser-Ney indexes

```
bucket "kn_succ"   key = <prefix>                       value = N1+(prefix .)  distinct successors
bucket "kn_pre"    key = <token> 0x00 <preceding ctx>   value = presence
bucket "kn_pre_n"  key = <token>                        value = N1+(. token)   distinct predecessors
```

`kn_succ` is the interpolation normalizer. It is derivable with a cursor count, but that is O(successors) on every lookup, so it is stored and maintained incrementally. `kn_pre` is a presence set whose only job is to make `kn_pre_n` correct, since a distinct-predecessor count cannot be maintained without knowing whether a given predecessor was already seen. This is the honest cost of KN: roughly one extra read and two extra writes per n-gram per message, against a presence value of zero bytes. Worth paying, because §3.1 removes far more write volume than this adds.

`kn_pre` grows with distinct (token, context) pairs and is the fastest-growing structure in the database. It will need a pruning policy. Deliberately not in M6: get it correct and measure first, because a wrong prune corrupts the continuation counts that KN's entire benefit rests on.

### 3.3 Author diversity

Each markov key carries a distinct-author count alongside its frequency, maintained the same way as `kn_pre_n`. §4 explains why this is the load-bearing anti-poisoning control rather than a nicety.

### 3.4 Migration

There is none, and that is a decision rather than an omission. Every markov key changes shape, so the existing corpus is not readable under the new layout. Production starts empty on a named volume; the corpus is re-derivable from Discord history. Two guards keep the abandonment explicit rather than silent: a `schema_version` key in `ConfigBucket`, and `Open` deleting any stray empty-prefix markov key while logging the bytes reclaimed, which rescues a local development database and turns "the mystery 128 MiB" into a log line.

Starting fresh is also a **safety** win, not only a schema convenience: per §4 A1 the unfiltered path has been the main ingestion route all along, so the existing corpus is poisoned to an unknown degree.

---

## 4. Safety and Threat Model

Assume users actively try to make the bot say gross or borderline illegal things, and that the operator wears the consequences. The existing filters are the right instinct; the architecture around them does not hold.

### 4.1 Findings

**A1. The backfill path bypasses moderation entirely, which defeats the live path.** The live handler runs the spam, illegal-content and slur filters. But `learnMessage` has four callers and the other three pass content through raw: the historical rescan, self-learning, and voice transcripts. Only `learnMessage`'s own internal spam check applies to them. Since the backfill re-reads the trailing 24 hours every 10 minutes, a message the live path blocked is learned anyway, unfiltered, minutes later. **This is the highest-value fix in the document.**

**A2. There is essentially no output-side filter.** The only check on generated text tests length, character repetition and character-class ratio. No slur check, no illegal-content check. Anything in the corpus can come back out verbatim.

**A3. Filtering the corpus cannot bound the output, even in principle.** A Markov chain composes novel sequences from n-grams learned separately, so individually innocuous fragments can join into something the operator answers for. Input filtering lowers the rate; only an output gate bounds the result. This is why two gates are not redundant.

**A4. The illegal-content filter is a placeholder.** Two toy regexes and a comment saying the real patterns must be added. It blocks essentially nothing.

**A5. The slur filter launders instead of rejecting, and is easy to evade.** It *replaces* matches, so a slur-bearing message is still learned with its structure intact and the replacement token injected into the corpus. Matching is on raw text with `\b` boundaries and only `[i1]`, `[a@]`, `[o0]` leet classes, so intra-word spacing, combining marks, zero-width characters, Cyrillic homoglyphs and any unenumerated variant pass through.

**A6. Poisoning is cheap, permanent and self-amplifying.** Nothing rate-limits how much one author teaches the bot, and n-gram weight is raw frequency, so repeating a phrase is a direct write to the model. Self-learning feeds the bot's own output back in, reinforcing anything that got through once. The existence of a `-clean-db` mode is evidence this already happened.

**A7. Image reposting is an unattributed republishing channel.** Cached user-posted image and Tenor URLs are reposted later, by the bot, in a channel of its choosing. A hostile user can seed the cache and have the bot publish it under its own name. The only current guard is an NSFW flag check on the source channel.

### 4.2 The defense architecture

1. **One `internal/safety` gate, two directions.** `CheckLearn(text)` and `CheckEmit(text)`, sharing one normalizer and one ruleset. `CheckLearn` is called inside `learnMessage`, which closes A1 for all four callers at once and for any fifth. `CheckEmit` is called inside `internal/discordguard`, covering every send site structurally. On rejection the bot **stays silent** rather than substituting a fallback: silence is always safe, and a fallback is a new output to reason about.
2. **Reject, never launder, on the learning path.** A message tripping the blocklist is dropped whole. Replacement remains available for display paths, never for the corpus.
3. **Normalize before matching.** NFKC, strip zero-width characters and combining marks, fold confusable homoglyphs to ASCII, collapse repeated characters and intra-word separators. Blocklist patterns are written against that form and must not re-enumerate evasions the normalizer already removes.
4. **Author diversity gates generation, not just storage.** A continuation must have come from `k` distinct authors (`PEREGRINE_MIN_DISTINCT_AUTHORS`, default 2) before the bot will generate it, independent of frequency. This is the strongest single anti-poisoning control: it converts the attack from persistence into collusion. The bot's own output is excluded from those counts so self-learning cannot bootstrap a phrase into eligibility. A per-author learning budget per window is a second layer.
5. **The blocklist is data, not source.** Loaded from `PEREGRINE_BLOCKLIST_PATH`; the real file is gitignored and only `blocklist.example.txt` is committed. Committing it would make this repo a searchable copy of a slur list and turn every addition into a rebuild and a public diff instead of an edit the operator makes mid-incident. It fails **closed**: missing or unreadable is a startup error, because an empty ruleset is indistinguishable from a working one until the worst possible moment. The `illegal` category additionally alerts the operator, so keep it narrow: every entry pages a human.
6. **Operator controls.** `PEREGRINE_PAUSE_ALL_WRITES` is a process-wide mute for when the bot is actively saying something awful and a deploy is too slow. Every emit rejection is logged so attempted poisoning is visible rather than inferred. `-clean-db` is the retroactive remedy, plus a per-author purge so one bad actor's contributions can be removed without discarding the corpus.

For image reposting: cap cached URLs per author, never repost from a message that was subsequently deleted (deletion is a strong signal), and apply the NSFW and blocklist checks to the destination as well as the origin.

### 4.3 A policy decision worth arguing with

The character-class filter permits only `unicode.Latin`, so any predominantly Cyrillic, CJK or Arabic message is dropped as spam at the 0.80 ratio threshold. That is recorded here rather than left buried in a helper because it is a real policy with a real cost: it excludes non-Latin-script participants from the corpus entirely. It also, accidentally, blocks whole-message homoglyph attacks. Keep it, change it, or narrow it, but do so deliberately.

---

## 5. The Generation Pipeline

### 5.1 What is wrong today

**The engine is less random than it looks.** Candidate scores begin as raw n-gram counts and are multiplied by an unbounded topic-gravity term and roughly a dozen further ad-hoc factors, with no normalization, then raised to a power. Scores span orders of magnitude and one candidate almost always dominates: the sampler is effectively argmax with noise. For a bot whose purpose is chaos this is the worst available failure, and it hides well because output still varies slightly.

**`Creativity` inverts its own name.** Applied as an exponent of `1/(Creativity+0.01)`, so at the 0.75 default the exponent is 1.316, which sharpens rather than flattens. Raising it toward 1 approaches an exponent of 1.0 and never passes it, so the dial cannot reach the interesting half of its range.

**Backoff carries no weight.** The prefix-shrink loop takes the first non-empty result from the longest prefix, so a 4-gram and a bigram continuation are scored on the same scale.

**Dead and incoherent scoring.** A first-candidate bonus of 1.0 to 1.05 applied to whichever candidate Go's randomized map iteration happened to yield first. A branch comparing against `"<END>"` when the only sentinel produced is `"<end>"`, guarded by a character-length check on a word-joined string. Three hollow "removed as punctuation is no longer desired" comment stubs. A 14-entry roast-vocabulary map rebuilt, with 14 lowercase conversions, per candidate per step.

**Length is tuned for prose, not chat.** `30 + rand(15)` words is a paragraph, governed by three competing mechanisms: an end-token multiplier, a discard-and-retry, and the loop bound.

**Context bleeds across channels.** One 50-entry decayed conversation memory is shared by every channel in every guild, so a reply is steered by an unrelated conversation elsewhere. That is not chaos, it is wrong context, and it makes replies read as non-sequiturs to the thread they are in.

**Repetition control is triplicated** across a used-word map, a generated-n-gram set and an immediate-repetition scan.

**Topic gravity ignores short tokens.** Topic counts skip words under 3 characters, so "ok", "no" and "wtf" always score zero.

### 5.2 The target model

`log P(next | prefix)` from **interpolated Kneser-Ney**, plus additive logits for every heuristic, then temperature and top-k/top-p sampling.

KN is the right model here because the corpus is small and sparse: a server's chat is on the order of 10^5 to 10^6 tokens, so at order 5 nearly every 4-gram has count 1, and stupid backoff over counts that are almost all 1 is close to arbitrary. Absolute discounting is built for exactly this.

**The deliberate deviation from textbook KN.** KN estimates lower orders from *continuation counts*: the number of distinct contexts a token follows. That correctly demotes a token like "Francisco", frequent but nearly always preceded by "San". The problem is that a meme, a copypasta and an inside joke are statistically indistinguishable from "Francisco": high frequency, few distinct contexts. Pure KN would systematically suppress exactly the register this server runs on. So:

```
P_low = (1 - mu) * P_kn_continuation + mu * P_raw
```

`PEREGRINE_KN_RAW_MIX` is `mu`. `0.0` is textbook KN and maximizes conventional quality; `1.0` is raw counts; `0.25` leans to KN while keeping the memes. **Pure KN optimizes perplexity; peregrine optimizes for landing a joke.** Anyone setting this to 0 on the authority of a paper is making output worse by the only metric that matters here.

Single-discount interpolated KN first. Modified KN (separate discounts for counts 1, 2 and 3+) is a further gain on sparse data and is a later option, not a first attempt.

KN replaces the **probability model only**. Persona bias, topic gravity and the repetition penalty remain, as additive logits with documented ranges, which is what normalization makes possible.

### 5.3 Knobs

| Variable | Meaning | Default |
|---|---|---|
| `PEREGRINE_TEMPERATURE` | `logit/T`. Higher is more chaotic, and unlike `Creativity` it can actually exceed 1 | 1.0 |
| `PEREGRINE_TOP_K` | Truncate the candidate tail before sampling. 0 disables | 40 |
| `PEREGRINE_TOP_P` | Nucleus threshold | 0.95 |
| `PEREGRINE_KN_DISCOUNT` | KN absolute discount `D` | 0.75 |
| `PEREGRINE_KN_RAW_MIX` | `mu`, per §5.2 | 0.25 |
| `PEREGRINE_MAX_NGRAM` | Highest order. Minimum 2, per §3.1 | 5 |
| `PEREGRINE_MIN_WORDS` | Length floor | 4 |
| `PEREGRINE_MAX_WORDS` | Length cap. Short lands, long reads as malfunction | 18 |
| `PEREGRINE_COOCCURRENCE_WINDOW` | Replaces the all-pairs loops. 0 means unbounded and warns | 5 |
| `PEREGRINE_MIN_DISTINCT_AUTHORS` | Generation eligibility, per §4.2 | 2 |

Top-k and top-p are what keep a high temperature *surprising* rather than word salad: truncate the tail, then sample the surviving head hot.

**Repetition is deliberately not penalized hard.** Memetic repetition (copypasta cadence, a doubled emote, "ratio ratio ratio") is the desired register. The penalty should suppress stuttering artifacts without flattening intentional-looking repetition. Settle this against golden samples, not in the abstract.

**Two variables are deleted, not ported.** `ContextWindow` and `CoherencyBalance` were declared and never read. They get no env var, because a documented knob wired to nothing is worse than no knob: an operator turns it during an incident and concludes the bot is broken. Recorded here so nobody "restores" them. When a real coherence-versus-novelty dial exists, it gets a variable in the same change as the code that reads it.

**A third is deliberately still a constant.** M2 promoted every other tuning constant to an environment variable but left `Creativity` in the code, and this is not an oversight. Its arithmetic contradicts its name: applied as an exponent of `1/(Creativity+0.01)`, raising it toward 1 can only approach an exponent of 1.0 and never pass it, so the knob cannot reach the half of its own range that would add chaos. Exposing it would invite an operator to tune something broken and conclude the bot ignores configuration, which is the same failure the two deletions above avoid. `PEREGRINE_TEMPERATURE` replaces it in M7, in the same change that normalizes the scoring so the dial actually moves. There is deliberately no `PEREGRINE_CREATIVITY` in the meantime.

**Every variable that later milestones will read is documented in `.env.example` and warned about at startup.** Setting one today does nothing, and silence about that is indistinguishable from the bot ignoring its own documentation, so `config.DeferredSet` logs one warning naming each such variable and the milestone that starts reading it. The list is a map in `internal/config`, and `TestDeferredVarsAreNotAlsoLive` fails if an entry is left behind after its milestone lands, which is the mistake the mechanism invites.

### 5.4 Verification

Generation quality cannot be settled by assertions alone. A `TestGenerateGolden` harness loads a small fixed corpus and generates with a seeded rand across a sweep of temperature and top-k values, printing output so before-and-after is directly comparable and the chaos dials are tuned by reading rather than guessing. Assertions pin the mechanics: no empty-prefix key is ever written; generation is deterministic under a seeded rand; backoff prefers the higher order when both have mass; length skews short; emote-bearing output appears when the corpus has shortcodes and a resolver is present.

---

## 6. Engagement Features

These are the product. None are stripped. Each becomes a plugin with its bugs fixed and its magic numbers promoted to config. The common theme is that most are currently either dark or subtly broken, so the engagement surface the bot is supposed to have is not actually running.

- **Custom emotes.** The `:shortcode:` resolver walks `s.State.Guilds`, but the session never requests `IntentsGuilds`, so that slice is always empty and **the resolver has never once succeeded**. The bot has never spoken in the server's own emotes, which in a meme server is most of the register. Probably the largest single register improvement available. The same missing intent forces the NSFW check into a REST call on every message.
- **Word games.** Disabled by a compile-time constant, and the dictionary load was `log.Fatalf`, so a missing 64 KB word list killed the whole bot. Now embedded via `go:embed` with a non-fatal load. Remaining: three orphan `go func` plus `Sleep` timers that fire against a closed session after shutdown, unbounded `scramble` recursion on an all-identical-letters word, and a 2-minute interval default that is plainly a leftover test value. The chat leaderboard reads the stats bucket and stays available independently of the scramble game.
- **Bird aggro.** Works, but has no dials. Chance, duration, tick and emote become config.
- **Image reposting.** The cheapest chaos in the bot, throttled to 1 to 1.5% with hardcoded rates. Rates and cache size become config; per-author caps and the deleted-message rule come from §4.2.
- **Autonomous posting.** Dead twice over: the constant is false **and** the channel allowlist is empty. Most obviously wants to be on. Enabling it with an empty allowlist becomes a **startup error naming both variables**, because the previous arrangement meant flipping either one alone produced nothing and no explanation.
- **The roast persona** lives in two places at two layers: an in-sampler vocabulary boost and a post-hoc filler injector, overlapping in intent, neither testable. Consolidated into one persona layer with a configurable logit bias plus a post-pass whose insertion points are position-weighted rather than a raw random index. Same chaos, deliberate instead of accidental.

---

## 7. Lifecycle

**Shipped in M3.** What follows is the shape as built; the bugs it replaced are recorded because the shape is only defensible against them.

The per-message handler called `wg.Add(1)` on the same WaitGroup the shutdown path waited on, which panics when an `Add` races a `Wait` at zero, and let handlers keep using the database after it closed. `main()` launched nine background goroutines, six of them hand-rolled tickers, each selecting on a package-level `stopSignal` channel, and a panic in any of them killed the process. One shared `*rand.Rand` was called from all of them and from every message goroutine.

`core.Dispatcher` owns a bounded worker pool reading a buffered channel. The gateway handler rejects bots and enqueues non-blocking. Non-blocking is deliberate: discordgo dispatches each event on its own goroutine, so blocking would grow goroutines without bound, and dropping with a counter is the honest semantics for best-effort chat. The drop count is reported on shutdown and surfaces in the status line.

**The queue is never closed**, because closing it would let an in-flight enqueue panic on send. Shutdown sets a flag, drains until empty or a deadline, cancels the workers' own context, then waits on a WaitGroup that is only ever `Add`ed during `Start`, so the race cannot recur by construction. The dispatcher owns that cancel rather than relying on the context passed to `Start`: a `Shutdown` that could only wait for someone else to cancel would block for its entire deadline every time, which is exactly what it did in the first draft and what its tests caught.

`core.RunLoop` replaces the tickers, which become a table of `core.Loop` values that reads as a list of behaviors. Each iteration is panic-isolated, because every one of those loops is an optional behavior and this bot's rule is that one feature failing disables that feature and nothing else. `Immediate` preserves the real distinction the copies made by hand and inconsistently: the status line, the backfill and the clustering pass are wanted at startup, whereas a leaderboard reset check on a fresh process is pure noise. A non-positive interval is refused with a log line naming the loop rather than panicking inside `time.NewTicker`.

There is no shared `*rand.Rand`. `math/rand/v2`'s top-level functions are goroutine-safe and auto-seeded, so the fix was to have no shared generator rather than to put a mutex around one.

Shutdown order, which differed from the old code in four ways:

```
1. session.Close()        first, not last: stop the inflow before draining
2. dispatcher.Shutdown()  bounded drain
3. registry.ShutdownAll() reverse start order; loops stop and are waited for
4. store.Close()          explicit and last, via cmd/bot, not a bare defer
                          racing the per-message goroutines
```

The final leaderboard save is the legacy service's `Shutdown`, so it lands inside step 3 and therefore strictly before the store closes. One 8-second budget covers steps 2 and 3 together, deliberately under Docker's 10 seconds between SIGTERM and SIGKILL: separate timeouts per step could add past it, and the corpus being closed by SIGKILL rather than by us is the outcome worth engineering against.

Startup order has two placements that are load-bearing rather than stylistic. `WatchReady` is armed **before** `Open`, because discordgo starts dispatching inside `Open` and a handler registered after it can lose the race with READY, failing startup on a healthy connection. Gateway handlers are registered in `Init` rather than `Start`, because `Open` happens between them and registering in `Start` drops every message from that window.

Target: `core.Dispatcher` owns a bounded worker pool reading a buffered channel. The gateway handler does nothing but reject bots, snapshot the event into an immutable value (which also stops it mutating discordgo's struct) and enqueue non-blocking. Non-blocking is deliberate: discordgo dispatches each event on its own goroutine, so blocking would grow goroutines without bound, and dropping with a counter is the honest semantics for best-effort chat. The drop count surfaces in the status line so a persistently full queue is visible rather than inferred.

**The queue is never closed**, because closing it would let an in-flight enqueue panic on send. Shutdown sets a flag, drains until empty or a deadline, then cancels the workers' context and waits on a WaitGroup that is only ever `Add`ed during startup, so the race cannot recur by construction.

Shutdown order, which differs from today in four ways:

```
1. session.Close()        first, not last: stop the inflow before draining
2. dispatcher.Shutdown()  bounded 10s drain
3. registry.ShutdownAll() reverse start order
4. final leaderboard save
5. store.Close()          explicit and last, not a bare defer
```

Nine hand-rolled ticker goroutines become `core.RunLoop` calls owned by the relevant service. The orphan timers become context-bound handles the owning service stops.

---

## 8. Refactor Targets

Verified against the source. Ranked by consequence. Numbers are referenced from code comments and from §9.

**Crashes and hangs**

1. Nested bbolt transactions in the generation path can deadlock the process unrecoverably, and the odds rise as the file grows. §3.
2. The vocabulary interner writes a global map and appends a global slice from every per-message goroutine. A concurrent map write is a Go `fatal error`: no recover, no unwind, process gone. Also an unbounded leak, never pruned.
3. ~~One `*rand.Rand` shared across every message worker, the aggro ticker, autonomous posting and image reposting. Not goroutine-safe.~~ **Fixed in M3:** there is no shared generator at all now, `math/rand/v2` top-level functions being goroutine-safe and auto-seeded.
4. ~~The shutdown WaitGroup race in §7.~~ **Fixed in M3.**

**Correctness**

5. The empty-prefix unigram key. §3.1.
6. Self-learning stores the bot's reply under the *user's* message ID, and the user's message under the same ID, both gated by the same dedup check, so whichever transaction commits first makes the other a no-op and which one wins is a race.
7. `IntentsGuilds` missing. §6. **Half fixed in M3:** the intent is requested, so custom emote output is possible for the first time. The per-message REST `s.Channel` call for the NSFW check still exists and becomes an `s.State.Channel` lookup in M10.
8. Nothing suppresses mentions, so the replied-to author is pinged on every interaction, and learned user mentions ping forever.
9. The leaderboard command has no feature guard and no `return`, so it fires while word games are off and falls through into the rest of the handler. The reactor `handled` contract in §2 makes this shape impossible rather than fixed once.
10. History eviction removes the lexicographically smallest snowflake, and snowflakes are variable-length decimal strings, so a 17-digit ID is evicted before an 18-digit one regardless of age.

**Cost**

11. `Bucket.Stats()` walks every page and was called per message to fill a log field, and sits inside the loop condition of both trim functions, making trimming quadratic in pages.
12. All-pairs co-occurrence and all-pairs topic pairs, O(n^2), inside the single write transaction.
13. The 10-minute loop rescans the whole trailing 24 hours; dedup is a 10,000-key window, so on a busy guild older messages are evicted and then **re-learned, double-counting n-grams**.
14. Channel fan-out is unbounded, one goroutine per channel per guild, none visible to the shutdown path. The active-channel scan also pages every channel to count, then the ingest pass pages it again.
15. Clustering does `DeleteBucket` plus `CreateBucket` every pass, a full destructive rebuild, with timestamp-based IDs so nothing is ever diffable.
16. 25 regexes recompiled per call in the filters, plus `MustCompile` inside `learnMessage` and inside a token loop.
17. An hourly NTP query to `pool.ntp.org` for what the host clock answers, whose failure silently skips a weekly reset. Removed by deletion rather than by relabeling the dependency.

**Hygiene and privacy**

18. The history bucket stored 10,000 users' messages **verbatim** while nothing ever read the value, only the key's existence. Store a timestamp.
19. A hardcoded Discord user ID was the only authorization check in the codebase.
20. Word-game dictionary `log.Fatalf`, and CWD-relative paths throughout. **Fixed in M0.**
21. Voice-note download is a bare `http.Get`: no timeout, no context, no size cap, no status check.
22. The slur filter iterates a map, so overlapping patterns give order-dependent, non-deterministic output.
23. Uncommittable assets: a 465 MiB whisper model (over GitHub's hard 100 MiB per-file limit), a 96 MiB ffmpeg binary, a 128 MiB corpus. **Fixed in M0**, and the ordering mattered: `.gitignore` had to exist before `git init`.
24. A stale `copilot-instructions.md` documenting a test file and a path that never existed. **Deleted in M0.**
25. Zero tests; `go mod tidy` never run. **Partly fixed in M0.**
26. Six sites with ellipsis or em-dash characters, two of them in user-visible leaderboard output. **Fixed in M0.** The tokenizer's literal right single quote is load-bearing and deliberately not covered by the check.

Plus, folded into the milestones that touch them: byte-based nickname truncation splitting multi-byte runes (**fixed in M0**); trim loops calling `Stats()` in a loop condition; a `ForEach` on a possibly-nil bucket; `stringContains` reimplementing `slices.Contains`; `s.User("@me")` called per reply when the bot ID is already known.

---

## 9. Milestones

Small, mergeable PRs. `go build ./...`, `go vet ./...`, `golangci-lint run`, `go test ./... -race`, the prose check and the Docker build are all green at the end of every row.

| # | Milestone | Deliverable | Status |
|---|---|---|---|
| 0 | Repo hygiene, docs, full CI/CD | `.gitignore` before `git init`; `.dockerignore`; module path; `go mod tidy`; the six punctuation fixes; rune-safe truncation; embedded dictionary; `PEREGRINE_DB_PATH`; `bbolt.Open` timeout; lint green from day one; Dockerfile, both compose files, `.env.example`, `blocklist.example.txt`; `ci.yml` with all six jobs; dependabot; docs; delete `copilot-instructions.md` | **done** |
| 1 | `cmd/bot` and `internal/legacy` | Verbatim move, `main()` becomes `Run(ctx)`, `performDatabaseCleanup` becomes `CleanDatabase() error`, merlin's `main`/`runGuarded`/`run` split, `slog` bridged over the stdlib `log`, `godotenv`, `signal.NotifyContext`, no `os.Exit` left in `legacy`, Dockerfile builds `./cmd/bot` | **done** |
| 2 | `internal/config` | Every constant and hardcoded value becomes an env var defaulting to today's value; the two dead knobs deleted and `Creativity` deliberately left a constant (§5.3); the hardcoded admin user ID becomes a fail-closed variable; every error reported in one pass; a bad bool or enum is a startup error rather than a silent default; autonomous-post misconfiguration names both variables; the autonomous-post ticker split off the ingest ticker; deferred-variable warnings; 92% covered | **done** |
| 3 | `internal/core` lifecycle | `Service`/`Registry`/`Deps` and `WatchReady` ported; `IntentsGuilds` added, which makes custom emote output possible for the first time; bounded `Dispatcher` with drop accounting; `RunLoop` replacing nine background goroutines; ordered shutdown under one 8s budget; `stopSignal` and the shared WaitGroup deleted in favour of `ctx`; `math/rand/v2` replacing the shared generator; `core` at 94% coverage. Closes 3, 4, and the intent half of 7 | **done** |
| 4 | `internal/text` and `internal/filter` | Pure moves, regexes hoisted, slur list becomes an ordered slice, per-call interner replaces the global. Closes 2, 16, 22 | |
| 5 | `internal/safety` | Normalizer, blocklist as data, `CheckLearn` inside `learnMessage`, `CheckEmit` at the generation exit, reject-not-launder, `PAUSE_ALL_WRITES`, rejection logging. Closes A1, A2, A4, A5. **Highest-value row** | |
| 6 | `internal/corpus` and `internal/storage` | Composite-key codecs; the three KN indexes; distinct-author counts; `schema_version`; `Reader`/`Writer` seam; timestamp-only history; counter-based chronological trims; per-author purge; `dbtest`; `maintenance`. Closes 1, 5, 10, 11, 18 | |
| 7 | `internal/markov`: the engine | Interpolated KN with `mu`; log-space additive scoring; temperature and top-k/top-p; dead scoring deleted; author-diversity gate; consolidated persona; short length model; per-channel memory; co-occurrence window; `math/rand/v2`. Closes 12, A6, and §5.1 | |
| 8 | `internal/clustering` | Made pure, content-hashed IDs, diff-based persistence, nil-bucket guard. Closes 15 | |
| 9 | `internal/ingest` | Per-channel high-water cursors with `afterID` paging; `errgroup.SetLimit` at both levels. Closes 13, 14 | |
| 10 | `discordguard` and the reactor split | Guard with non-nil empty `Parse` and `RepliedUser: false`, owning `CheckEmit`; handler split into plugins; `handled` short-circuiting; state-cache channel lookup. Closes 6, 8, 9, A3 | |
| 11 | Engagement features | Word games un-darked; aggro, image reposting and autonomous posting given real config and turned on deliberately; per-author image caps and the deleted-message rule; emote output verified. Closes 19, A7, §6 | |
| 12 | voicenotes | Context, client timeout, status and host checks, size cap, `exec.CommandContext`, temp dir, `Available()` probe, transcripts through `CheckLearn`. Closes 21 | |
| 13 | health, backups, delete `legacy` | Status and latency as a service with drop and rejection counters; in-process `tx.CopyFile` backup ticker with retention and never-prune-after-failure; `internal/legacy` deleted | |

---

## 10. Open Decisions

- **`mu` and `D` need real values.** 0.25 and 0.75 are starting guesses. The right numbers are whatever makes golden samples read like the server.
- **`PEREGRINE_MIN_DISTINCT_AUTHORS` needs a real value.** Too high and a quiet server generates nothing; too low and two accounts defeat it. Start at 2, tune against the real active-user count.
- **`kn_pre` pruning policy.** Deferred until there is data, per §3.2.
- **A kill switch reachable from Discord.** `PAUSE_ALL_WRITES` needs SSH and a restart, which is slow during exactly the incident it exists for. Merlin solved this with a slash command, but peregrine registers none today, so it would be new surface. Decide before M11.
- **Repository visibility.** Given the blocklist and this threat model, private is the better default. The blocklist stays out of the repo either way.

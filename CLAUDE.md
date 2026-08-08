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

**`Creativity` is a trap.** It is applied as an exponent of `1/(Creativity+0.01)`, so at its 0.75 default the exponent is 1.316, which *sharpens* the distribution. Raising it toward 1 can only ever approach an exponent of 1.0 and never pass it, so the knob cannot reach the interesting half of its own range. It becomes `PEREGRINE_TEMPERATURE` with the arithmetic corrected.

**Backoff carries no weight.** The prefix-shrink loop tries the longest prefix, then shorter ones, and takes the first non-empty result, so a 4-gram continuation and a bigram continuation are scored on the same scale. M7 replaces this with **interpolated Kneser-Ney**, which is the right model for a corpus this sparse: a server's chat is on the order of 10^5 to 10^6 tokens, so at `MaxNGram=5` nearly every 4-gram has count 1.

**Kneser-Ney is deliberately not applied in its textbook form, and this is the single most counter-intuitive decision in the codebase.** KN estimates lower orders from *continuation counts*, the number of distinct contexts a token follows, which correctly demotes a token like "Francisco" that is frequent but nearly always preceded by "San". The problem is that a meme, a copypasta and an inside joke are statistically indistinguishable from "Francisco": high frequency, few distinct contexts. Pure KN would therefore systematically suppress exactly the register this server runs on. `PEREGRINE_KN_RAW_MIX` interpolates the lower-order estimate back toward raw counts (`0.0` is textbook KN, `1.0` is raw counts, default `0.25`). If someone "fixes" this to 0 because a paper says so, output quality by the only metric that matters here gets worse.

**Length is tuned short on purpose.** The old bound was `30 + rand(15)` words, which is a paragraph. Short and punchy reads as a joke; long reads as a malfunction.

**Custom emote output has never worked.** The `:shortcode:` resolver walks `s.State.Guilds`, but the session never requests `IntentsGuilds`, so that slice is always empty and the resolver has never once succeeded. In a meme server the server's own emotes are most of the register, so this is an engagement bug before it is a performance bug. The same missing intent forces the NSFW check into a REST call on every single message.

## Safety model

`SPEC.md` §4 is the full threat model. Assume users actively try to make the bot say gross or borderline illegal things, and that **the operator carries the consequences of whatever it posts**. Four rules follow, and each exists because of a specific hole.

**Filtering the corpus cannot bound the output, even in principle.** A Markov chain composes novel sequences from n-grams learned separately, so fragments that were individually innocuous can join into something the operator has to answer for. Input filtering lowers the rate; only an output gate bounds the result. This is why there are two gates and not one, and why removing the output gate as redundant would be wrong.

**Gates go at chokepoints, never at call sites.** The live handler filters properly today, but `learnMessage` has four callers and the other three pass content through raw: the historical backfill, self-learning, and voice transcripts. Since the backfill re-reads the trailing 24 hours every 10 minutes, **a message the live path blocked is learned anyway, unfiltered, minutes later**, which defeats the live filter entirely. M5 moves the check inside `learnMessage`, the one funnel all four callers pass through. If you add a fifth caller, it is covered automatically, and that is the entire point.

**Reject, never launder, on the learning path.** The slur filter *replaces* matches, so a slur-bearing message is still learned with its structure intact and the replacement token injected into the corpus. For learning, the verdict must be to drop the message whole.

**Match on a normalized form, never raw text.** The current patterns use `\b` boundaries and only `[i1]`, `[a@]`, `[o0]` leet classes, so intra-word spacing, combining marks, zero-width characters and Cyrillic homoglyphs walk straight through. M5 normalizes (NFKC, strip zero-width and combining marks, fold confusables, collapse runs and separators) and matches against that. Blocklist patterns are written against the normalized form and must not re-enumerate evasions the normalizer already removed.

**The blocklist is data, not source.** It loads from `PEREGRINE_BLOCKLIST_PATH` and the real file is gitignored; only `blocklist.example.txt` is committed. Committing the real list would make this repo a searchable copy of one and turn every addition into a rebuild and a public diff instead of an edit the operator can make mid-incident. It fails **closed**: a missing or unreadable file is a startup error, because an empty ruleset is indistinguishable from a working one right up until the worst possible moment.

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

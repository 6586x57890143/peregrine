# peregrine

A Markov-chain Discord engagement bot in Go. It learns from the channels it can see and replies in the server's own voice, badly and on purpose.

Sibling project to [merlin](../merlin), whose deployment conventions this repo follows. Design doc: [`SPEC.md`](./SPEC.md). Contributor guide for agents: [`CLAUDE.md`](./CLAUDE.md).

## Status

Mid-restructure. The bot works and runs, but it is being taken from a single 3,200-line `main.go` to a proper package layout one subsystem at a time, with a catalogue of known defects being closed along the way. See `SPEC.md` §8 for the defect list and §9 for the milestone order.

**Milestones 0 through 7b and all of 10 are complete:** repo hygiene and CI/CD, the `cmd/bot` entrypoint, `internal/config`, the `internal/core` lifecycle, `internal/text` plus `internal/filter`, `internal/safety`, the storage layer (`internal/corpus`, `internal/storage`, `internal/dbtest`, `internal/maintenance`), the generation engine `internal/markov`, the send chokepoint `internal/discordguard`, and the message reactor. M8 is dropped; M9 and M11 through M13 are outstanding.

M6b is what put the bot on the seam, and it is the commit that closes the worst bug in the review rather than designing around it. `internal/legacy` no longer imports bbolt at all, so nothing in it can hold a database handle, name a bucket, or start a transaction: generation used to run inside a read transaction and call two helpers that each opened their own, which is a hang with no timeout and no recovery that got likelier as the corpus grew. A test pins the import, because the import is the whole invariant.

**M7a replaced the engine.** The old scorer started with a raw n-gram count, multiplied it by an unbounded topic term and a dozen more ad-hoc factors with no normalization, then raised the result to a power: scores spanned orders of magnitude, one candidate almost always dominated, and the sampler was argmax with noise. For a bot whose whole purpose is chaos that is the worst available failure, and it hid well because the output still varied a little. It is now interpolated Kneser-Ney for the probability, additive and individually bounded logits for every heuristic, and temperature plus top-k/top-p on top, so chaos is a dial that actually moves. Kneser-Ney is deliberately *not* textbook here: pure KN suppresses high-frequency low-context-diversity tokens, which is precisely what a meme is, so `PEREGRINE_KN_RAW_MIX` pulls the base case back toward raw counts.

M7a also turned on the anti-poisoning gate: a continuation must have come from `PEREGRINE_MIN_DISTINCT_AUTHORS` different people before the bot will generate it, regardless of how often it was seen. Note the consequence on a **fresh corpus**, which looks like a fault and is not: until several people have said similar things, almost nothing clears a threshold of 2 and the bot is close to mute.

**M7b finished the pipeline around it.** Seed selection moved into the engine as seven weighted tiers; sentence length is one model instead of three that disagreed, with a target sampled short per sentence; the persona is one mechanism driving both the vocabulary bias and the post-pass instead of two independent coin flips; conversation memory is per channel, so a reply is no longer steered by an unrelated conversation in another channel; and word co-occurrence is windowed on the learn path, which cuts a 200-word message from nearly 40,000 database writes to about 2,000 inside the transaction that serializes all ingestion.

Reading the golden samples caught something reasoning had not: a seed drawn from an association tier can dead-end immediately, producing one-word replies. The bot now re-seeds up to three times and stays silent below two words, because a one-word reply reads as a malfunction rather than as a short joke.

**M8 is dropped rather than built.** Concept clustering is deleted, not rebuilt: it derived from a single index that generation can query directly, its merge threshold collapses every cluster into one blob, and fixing its codec would have handed that blob the highest-weight seed tier. `SPEC.md` §8 finding 29 has the arithmetic. Its one useful idea, transitive association, is now a bounded query-time two-hop tier in seed selection.

All three crash bugs are now closed: the shutdown WaitGroup race that could panic on exit (M3), the `*rand.Rand` shared across every message goroutine (M3), and the global vocabulary map written concurrently, which was a Go runtime *fatal error* rather than a recoverable panic (M4).

M3 also added the `GUILDS` intent, so the bot can use the server's own custom emotes for the first time; that resolver had never once succeeded. M4 found that the same is true of concept clustering, for a different reason, and turned it off by default; M7's scoping review found three further reasons and dropped it altogether (`SPEC.md` §8, findings 27 and 29).

Two things worth knowing before running it anywhere real:

- **The safety gate is in as of M5**, and it is the reason the bot is closer to deployable than it was. `CheckLearn` sits inside `learnMessage`, so the backfill path that used to re-learn blocked messages minutes later is covered by construction; `CheckEmit` sits at the generation exit. Matching happens against a normalized form, so spacing, leet, homoglyphs, combining marks and zero-width characters no longer walk through. **Set `PEREGRINE_BLOCKLIST_PATH` before pointing it at a hostile channel:** without it the bot runs on the built-in baseline only and warns loudly at startup, and the operator list is where the threat and illegal-content patterns live.
- **The corpus format changed in M6 and there is no migration.** `storage.Open` refuses a corpus written before it, with an error saying what to do, rather than opening it and silently ignoring every key in it. Deploying this needs `docker volume rm peregrine_corpus` once; see "Start the corpus over" below and `SPEC.md` §3.4.

**M10a closed the last safety blocker.** `internal/discordguard` is the single chokepoint every outbound Discord call goes through, and it suppresses mentions on all of them. That matters here more than in most bots: peregrine's output is Markov text built from what users typed, so a mention that got learned is a corpus token like any other and the generator will emit it again whenever the chain walks through it. Replies were pinging their author on every single interaction, because discordgo's reply helper sets no allowed-mentions field at all and Discord reads the absent field as "parse everything".

`CheckEmit` moved into the guard at the same time, which closed real holes rather than tidying: it had been sitting at the generation exit, so the autonomous poster, the word-game announcements and the transcription results all reached Discord ungated. A structural test parses the package and fails if any new send site skips the guard, because a rule applied at thirteen of fourteen call sites is not a rule.

Nothing is outstanding on the safety list now. What remains before a deploy is the operator's call, not a missing control.

**M10b turned the message handler into a reactor and found a data-loss bug on the way.** Handling one message is now a list of named steps, each reporting whether it consumed the message, and the run stops at the first one that does. That is what stops a command falling through: `!leaderboard` used to be answered and then *also* replied to as if it were conversation, after which the bot taught itself the string `!leaderboard`.

The other half is worse and had been happening on every single reply. Self-learning stored the bot's reply under the *user's* message ID, and the learn step stored the user's message under the same ID, and the corpus dedupes on that ID: whichever transaction committed first won, so one of the two messages was silently thrown away, and which one depended on a goroutine race. Replies are keyed by their own ID now.

## Quick start

```sh
cp .env.example .env          # set DISCORD_BOT_TOKEN
go run ./cmd/bot
```

In the Discord Developer Portal the application needs the **Message Content** privileged intent ticked, under Bot. Without it Discord refuses the connection and the bot cannot read the messages it exists to learn from. Since M3 that refusal is a startup error naming the checkbox, rather than a process that connects to nothing and looks healthy while doing so.

The other two intents, `GUILDS` and `GUILD_MESSAGES`, are not privileged and need no portal toggle.

Invite scopes: `bot`. Permissions: View Channels, Read Message History, Send Messages, Send Messages in Threads, Add Reactions, and Manage Messages (used to clean up its own word-game messages).

### With Docker

```sh
cp .env.example .env
docker compose up --build
```

`PEREGRINE_DB_PATH` must point inside the mounted volume, which `.env.example` already sets to `/data/markov.db`. The container runs with a read-only root filesystem, so a relative path resolves against the distroless working directory and the database fails to open. This is the most common way to get a bot that starts, looks healthy, and has silently learned nothing.

## Configuration

Everything is environment variables; there is no config file. `.env.example` documents every one with its default and, where it matters, why the default is what it is. Variables tagged `LATER (Mn)` are read by no code yet, and the tag names the milestone that starts reading each one; everything else is live.

Configuration is validated once at startup and a bad value is a **startup error, not a fallback to the default**. That includes booleans, so `PEREGRINE_ENABLE_X=ture` fails loudly rather than reading as "off". Every problem is reported in one pass, one log line each, so a multi-variable mistake takes one restart to diagnose rather than one restart per variable. Setting a `LATER` variable is not an error but does produce a warning listing it, because a documented knob that is silently ignored is indistinguishable from a broken one.

Maintenance modes do not need `DISCORD_BOT_TOKEN`: cleaning a poisoned corpus should not require a live credential.

## Maintenance

```sh
go run ./cmd/bot -clean-db                    # remove spammy and blocklisted n-grams
go run ./cmd/bot -compact /data/markov.new    # reclaim free pages into a fresh file
go run ./cmd/bot -purge-author 1234567890     # undo one user's contribution to diversity
```

All three run against the corpus and never touch Discord, and none needs `DISCORD_BOT_TOKEN`: cleaning a poisoned corpus should not require a live credential. They respect `PEREGRINE_DB_PATH`. Running one while the bot is live fails within five seconds with a clear message rather than hanging, because bbolt holds an exclusive lock on the file. Pass one at a time; the order they should run in is your decision, not a default.

`-clean-db` loads `PEREGRINE_BLOCKLIST_PATH`, so it removes what your list covers and not only the built-in baseline. That matters because adding a pattern to the blocklist does not retroact: this is the only thing that applies it to what is already in the corpus.

`-compact` exists because **bbolt's file never shrinks.** Deleting keys frees pages for reuse but does not return them to the filesystem, so a corpus that grew large stays large after `-clean-db` removes most of it. It writes a new file rather than replacing the original, so you move it into place yourself and the rollback stays trivial. Compacting onto the live path fails rather than corrupting anything, because bbolt would have to open a file it already holds.

`-purge-author` is the surgical alternative to discarding a corpus one bad actor has poisoned. It removes that user's contribution to the **author-diversity** counts, which is what generation eligibility reads, and leaves occurrence counts alone: the counts do not record who produced them, and storing that would mean a count on every entry of the fastest-growing index in the database. In practice it is the effective half, since a phrase only one person ever said drops to zero distinct authors.

In a container:

```sh
docker compose run --rm bot -clean-db
```

## Voice-note transcription

Off by default and Windows-only. It shells out to `ffmpeg` and `whisper-cli` and needs a 465 MiB whisper model, none of which exist in the distroless production image (which has no shell at all). The binaries and the model are gitignored and fetched by hand:

- `voicenotes/bin/windows/` needs `ffmpeg.exe`, `whisper-cli.exe` and the accompanying `ggml*.dll` and `whisper.dll`.
- `voicenotes/models/ggml-small.bin` from whisper.cpp's model releases.

Then set `PEREGRINE_ENABLE_TRANSCRIPTION=true`. Only the Windows binary directory exists; on Linux the lookup fails and every voice note produces a failure reply, which is the other reason the flag defaults off.

## Deployment

Push to `main` runs the full pipeline in `.github/workflows/ci.yml`: vet, lint, race tests, `govulncheck`, secret scan, prose check and a Docker build; then a multi-arch image to GHCR; then an SSH deploy to the VPS using `docker-compose.prod.yml`.

Required repository secrets, which do **not** carry over from merlin and must be added per repo:

| Secret | Purpose |
|---|---|
| `VPS_HOST` | Deploy target hostname |
| `VPS_SSH_KEY` | Private key for the `deploy` user |

And one repository **variable**:

| Variable | Purpose |
|---|---|
| `DEPLOY_ENABLED` | Must be exactly `true` for the deploy steps to run. Anything else, including unset, skips them |

**`DEPLOY_ENABLED` is currently unset, on purpose, so merges to `main` build and push an image but do not start the bot.** The deploy runs `docker compose up -d`, so without this gate every merge would start it, silently reversing the decision to keep a bot with no output filter out of a live channel until the M5 safety gate lands. A `docker compose stop` done by hand does not survive the next merge; the variable does. It defaults to holding rather than deploying because an unset variable should not be the thing that puts an unmoderated bot into a real server. Set it to `true` when M5 is in.

A skipped deploy reports as **green with a notice**, not red, since a deployment that was deliberately held has not gone wrong.

On the VPS, `/home/deploy/peregrine` needs `.env` (mode `0600`, since it holds a token equivalent to full control of the bot) and a `backups/` directory. The compose file and the tag files are written by CI.

### Runbook

**What is running right now**

```sh
cd /home/deploy/peregrine
cat deployed-tag.env                                    # the commit SHA currently deployed
docker compose -f docker-compose.prod.yml ps
docker compose -f docker-compose.prod.yml logs -f --tail=100 bot
```

**Roll back to the previous release**

```sh
cd /home/deploy/peregrine
cat previous-tag.env                                    # the SHA it replaced
cp previous-tag.env deployed-tag.env
set -a && . ./deployed-tag.env && set +a
docker compose -f docker-compose.prod.yml up -d
```

This works because the image is pinned to an exact commit SHA rather than a mutable `:latest`, and because the image prune is scoped `--filter "until=168h"` so the previous release still exists locally. A bare `docker image prune -f` would have deleted it.

**Stop it saying something immediately**

Set `PEREGRINE_PAUSE_ALL_WRITES=1` in `.env` and restart the container. That refuses every outbound message process-wide while leaving reads and learning alone. It requires SSH and a restart, which is a known weakness during exactly the incident it exists for; a Discord-reachable equivalent is an open decision in `SPEC.md` §10.

**Back up and restore the corpus**

Backups are taken in-process into `./backups` once the M13 ticker lands. Do **not** back up the corpus by copying `markov.db` from the host while the bot is running: bbolt is a single mmap updated by copy-on-write pages plus a meta-page flip, so an external byte copy can capture a torn state, and it will usually appear to work. See the long comment in `docker-compose.prod.yml`.

To restore, stop the bot, replace the file inside the `corpus` volume, and start it again.

**Start the corpus over**

```sh
docker compose -f docker-compose.prod.yml down
docker volume rm peregrine_corpus
docker compose -f docker-compose.prod.yml up -d
```

Required once when deploying M6b, since the key layout changed and old data is not migrated. Skipping it does not corrupt anything: `storage.Open` refuses the old file and the container fails to start with an error saying to do this.

## Development

```sh
go build ./...
go vet ./...
golangci-lint run                             # CI pins v2.12.2
go test ./... -cover
go test ./... -race -cover -covermode=atomic  # matches CI; needs CGO_ENABLED=1 and a C toolchain
```

CI also fails the build on em dashes, ellipsis characters and curly quotes. `CLAUDE.md` has the one-liner to check locally, and explains the single deliberate exception in the tokenizer.

See [`CONTRIBUTING.md`](./CONTRIBUTING.md). Security issues: [`SECURITY.md`](./SECURITY.md).

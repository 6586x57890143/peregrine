# peregrine

A Markov-chain Discord engagement bot in Go. It learns from the channels it can see and replies in the server's own voice, badly and on purpose.

Sibling project to [merlin](../merlin), whose deployment conventions this repo follows. Design doc: [`SPEC.md`](./SPEC.md). Contributor guide for agents: [`CLAUDE.md`](./CLAUDE.md).

## Status

Deployed and running. The restructure from a single 3,200-line `main.go` to a proper package layout is finished, and the catalogue of defects closed along the way is in `SPEC.md` §8, with the milestone order in §9.

**The restructure is complete.** Milestones 0 through 13 are done, except M8 which was dropped for the reasons in `SPEC.md` finding 29, and M12b (a transcription engine) which is the one row left open. The layout is the one `SPEC.md` §2 describes: `cmd/bot` wires, `internal/core` owns the lifecycle, `internal/storage` is the only package that knows a bucket exists, `internal/markov` is the generation engine, and the features are nine packages under `internal/plugins`. **`internal/legacy` is deleted**, which was the point of the whole sequence: it went 3,200 lines, then one file, then 250 lines, then nothing.

**M11c moved the features into packages and found a real safety hole doing it.** The author-diversity gate, which is the bot's strongest anti-poisoning control, was applied where the sampler picks the next word and nowhere else. Generation puts a word into a sentence from three places, and the other two read co-occurrence data that records no author at all: so a phrase one person had repeated forty times was refused at every step by the sampler and then handed straight back by the dead-end jump. It is the same shape as the worst finding in the original review, one milestone after the principle that names it ("a check at one of four call sites is not a check") was written into the design document.

It survived because the test named after that control seeded its corpus in a way that produced no co-occurrence data, so the path it needed to cover did not exist in the fixture. It failed the moment the fixture became realistic, which is the argument for rewriting fixtures during a move rather than porting them.

**A7 was the last unbuilt mitigation in the safety catalogue.** What remains of `SPEC.md` §4 is not code: A4's illegal-content patterns are deliberately not in this repository and are the operator's to write, and A6 lists a per-author learning budget as an optional second layer behind the author-diversity gate that already ships on by default.

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

**M9 stopped the backfill corrupting the corpus.** The history walk re-read the whole trailing 24 hours every 10 minutes, roughly 144 passes over every message, and recognised what it had already learned by consulting a 10,000-entry dedup window. On a busy server the older half of each pass had already fallen out of that window and was learned again, so its n-grams were counted two or three or ten times. Generation samples on raw frequency, so the model was quietly biased toward whichever messages happened to land in the re-read band.

It now keeps a per-channel high-water mark and asks Discord only for what came after it, which makes the question "what is new" rather than "what have I forgotten" and no longer depends on how much the bot remembers. `PEREGRINE_INGEST_LOOKBACK` survives as a bound on the *first* pass over a channel. The channel fan-out is bounded too, where it used to be one unbounded goroutine per channel per guild, and the pre-scan that paged every channel to count messages and then threw them away is gone: an idle channel now costs exactly one request.

**M11a rewrote the word games rather than un-darkening them**, because the feature had no owner: its state was four package-level variables behind two mutexes, its lifecycle was three hand-copied goroutines that slept and then acted, and its numbers were literals in the middle of the message handler.

Three of those were real defects. Every started game spawned up to three goroutines that took no context, so after shutdown they woke against a closed session and the count was bounded only by how often people played; one sweep replaces all of them. The scrambler recursed whenever a shuffle happened to reproduce the original word, with no depth limit, so a word whose letters are all identical recursed until the stack died and took the process with it: unreachable with the shipped dictionary and reachable with a custom one, which means it could only ever have fired in production. And the leaderboard exported its own mutex on a struct that gets JSON-marshalled to be saved, with the marshalling happening outside the lock, which is a concurrent map read and write and therefore a fatal Go runtime error rather than a recoverable panic.

The weekly reset also stopped asking an NTP server what day it was. It only fired during one hour on Monday, so a failed query or downtime across Monday morning skipped the reset for a whole week; comparing week boundaries catches up instead. `beevik/ntp` is out of `go.mod`.

Word games are **on by default** now. A game nobody can play is not a conservative default, it is a feature that does not exist.

The refactor also turned up a wiring error worth naming: **autonomous posting was gated on the word-game flag.** It posts Markov sentences, always has, but it returned early unless word games were enabled and paced itself with the word-game interval. So setting `PEREGRINE_ENABLE_AUTONOMOUS_POST=true` produced nothing unless word games happened to be on too. That is the third distinct way that feature has been dead, and the one that would have survived an operator carefully setting every variable the docs named.

**M11b closed the last safety finding, and it turned out to be a key layout rather than a check.** Image reposting takes a URL somebody else posted and republishes it later, under the bot's own name, in a channel of the bot's choosing, so seeding that cache is a cheap way to make the operator publish something. The mitigations were written into the spec back in M5 and could not be built: the cache was keyed by the URL alone, so nothing recorded where an entry came from. There was nothing to attribute, no way to cap one person's share, and no way to ask which entries a deleted message had contributed. A storage method for that last case had sat unused for a milestone with a comment describing exactly the rule nobody could implement.

Keying it by source message fixed all three at once. One author can now hold at most five of a hundred entries; deleting a message revokes what it contributed, because a deletion is a strong signal that the content should not be republished; and the destination channel gets the same NSFW check the origin already had. Eviction got cheaper on the way past, since a Discord ID's byte order is its chronological order, so finding the oldest cached image stopped being a full scan of the bucket on every eviction.

**And the bot stopped asking Discord where people are talking.** Choosing a channel to speak in, and choosing an aggro target, used to page every text channel in every guild fifty messages at a time and then another hundred messages per busy channel. Hundreds of REST calls an hour, answered with rate limits, for information that had already arrived on the websocket and been discarded. `internal/activity` counts the messages the bot already sees; the two functions that paged for it are deleted, and the word-game trigger's private copy of the same tally folded into it. One consequence worth knowing: aggro now targets only people who have spoken since the bot came up, which is the point of aggro.

**M21 was the pass over the parts people actually touch.** `!leaderboard` was slow for one
specific reason and it was not the corpus: it resolved a display name for every user in the
week's stats before sorting anything, through an uncached member lookup, so a server with two
hundred weekly talkers spent two hundred sequential rate-limited requests rendering twenty rows.
Ranking is computed from the scores alone now and only the eleven rendered rows get a name, which
is also what makes the eleventh slot possible: ranks 1 to 10, then your own rank under a divider
if you are outside them. It is one embed with both boards, a fastest-solve and current-streak
line, and the bot's status line carries a rotating fact about the corpus.

The word games got the same treatment. `!wordgame` refused an unauthorized caller by returning,
with nothing in the log and nothing in the channel, and the case that actually bit was the
operator's: with `PEREGRINE_BOOTSTRAP_ADMIN_USER_ID` unset the check fails closed and refuses
everyone including the person who deployed the bot. It now says which of the two happened. Puzzles
reveal their first letter after `PEREGRINE_WORDGAME_HINT_AFTER` by editing the announcement,
swept by the loop that already expires games rather than by a timer of their own, and
`!wordgame <word>` plants a chosen word, held to the same rules the dictionary loader enforces
because one of them is what stops the scrambler recursing.

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
go run ./cmd/bot -tuning-report ./tuning      # summarize a pulled-down tuning export
```

All three run against the corpus and never touch Discord, and none needs `DISCORD_BOT_TOKEN`: cleaning a poisoned corpus should not require a live credential. They respect `PEREGRINE_DB_PATH`. Running one while the bot is live fails within five seconds with a clear message rather than hanging, because bbolt holds an exclusive lock on the file. Pass one at a time; the order they should run in is your decision, not a default.

`-clean-db` loads `PEREGRINE_BLOCKLIST_PATH`, so it removes what your list covers and not only the built-in baseline. That matters because adding a pattern to the blocklist does not retroact: this is the only thing that applies it to what is already in the corpus.

`-compact` exists because **bbolt's file never shrinks.** Deleting keys frees pages for reuse but does not return them to the filesystem, so a corpus that grew large stays large after `-clean-db` removes most of it. It writes a new file rather than replacing the original, so you move it into place yourself and the rollback stays trivial. Compacting onto the live path fails rather than corrupting anything, because bbolt would have to open a file it already holds.

`-purge-author` is the surgical alternative to discarding a corpus one bad actor has poisoned. It removes that user's contribution to the **author-diversity** counts, which is what generation eligibility reads, and leaves occurrence counts alone: the counts do not record who produced them, and storing that would mean a count on every entry of the fastest-growing index in the database. In practice it is the effective half, since a phrase only one person ever said drops to zero distinct authors.

`-tuning-report` is the odd one out and needs none of the above: it reads a directory of JSON lines rather than the corpus, so it takes no lock, needs no `.env`, and runs on a laptop that has never had this bot on it. See **The tuning loop** below.

In a container:

```sh
docker compose run --rm bot -clean-db
```

## Voice-note transcription

**There is no transcription engine in this repository.** `internal/plugins/voicenote` is a complete plugin over an `Engine` interface, and the only implementation that ships reports itself unavailable, so voice notes are ignored.

That is deliberate rather than unfinished. The implementation it replaced shelled out to `ffmpeg` and `whisper-cli` and needed a 465 MiB whisper model; none of that exists in the distroless production image, which has no shell at all, and all three assets are gitignored because the model alone is over GitHub's hard 100 MiB per-file limit. The feature had never run anywhere but one Windows machine, and every path in it was resolved against the working directory, so it silently found nothing when started from anywhere but the repository root.

What the repository can honestly own is the plugin: a bounded queue, a worker the shutdown path waits for, a placeholder posted before work is queued, and every message through the send chokepoint. What it cannot is the model.

Setting `PEREGRINE_ENABLE_TRANSCRIPTION=true` with no engine **warns at startup** and ignores voice notes. It does not fail quietly:

```
level=WARN msg="transcription is enabled but no engine is available, so voice notes will be ignored..."
```

Writing an engine means implementing two methods. The package comment lists what the old one got wrong so the next one does not: a bare `http.Get` with no timeout, status check or size cap; `exec.Command` with no context, which shutdown could not kill; and paths resolved against the working directory.

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

On the VPS, `/home/deploy/peregrine` needs `.env` (mode `0600`, since it holds a token equivalent to full control of the bot) plus `backups/` and `tuning/` directories. CI creates both bind sources on every deploy, writes the compose file and writes the tag files; the one-time uid step below is still yours.

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

**Host bind mounts need uid 65532**

The image runs `USER nonroot`, uid 65532. A **named volume** inherits the image directory's ownership when it is created empty, which is why the corpus needs no intervention: the Dockerfile chowns `/data` to 65532. A **bind mount keeps its host ownership**, so anything bound in from the host needs permissions that let 65532 at it, or the container fails on first access.

```sh
cd /home/deploy/peregrine
sudo chgrp 65532 blocklist.txt && chmod 640 blocklist.txt   # the container reads this
sudo chgrp 65532 backups       && chmod 775 backups         # the container writes here
sudo chgrp 65532 tuning        && chmod 775 tuning          # and here
```

Group rather than world, and still owned by `deploy`, so the blocklist stays editable mid-incident without sudo. That is the whole reason it is a bind rather than baked into the image.

Both halves bit on the first production deploy. The blocklist at mode 0600 gave `permission denied` in a restart loop, which at least failed loudly. `./backups` owned by `deploy` fails **quietly**, a tick after startup rather than during it, because the backup loop is deliberately not `Immediate`, so nobody watching the boot logs sees it. `./tuning` fails the same quiet way, and the export reports the failure once per write rather than at startup.

**The tuning loop**

`SPEC.md` §10 has six open decisions and five of them say the same sentence: *revisit against real ingested text*. `mu` and `D` are starting guesses, `DefaultWeights` is "a considered first guess, not a measurement", and the only instrument that can judge output is a golden harness running against a 150-line synthetic fixture. The tuning export is what closes that: the bot writes what it said, why it said it, and whether anybody reacted.

```sh
# on the host, once: PEREGRINE_TUNING_DIR=/tuning in .env, plus the chgrp above
scp -r deploy@vps:/home/deploy/peregrine/tuning ./tuning
go run ./cmd/bot -tuning-report ./tuning
```

The bot **never uploads anything**. Files land on a bind mount and you pull them, which is the whole transport.

Three record kinds, one JSON object per line. A `sample` is one generation attempt, **including the ones that produced nothing**, with the seed tier it started from, how far the backoff had to go, and how many candidates the author-diversity gate removed. An `engagement` resolves a sample by message ID once its window closes: reactions, distinct reactors, whether a human replied. A `snapshot` carries the corpus counts, the health counters, and **the full set of dials in force** including the logit weights, which are constants in the binary and therefore not reconstructable from a deployment afterwards.

Read a report before and after a change, not one in isolation. `-tuning-report` warns when an archive spans two versions or two sets of dials, because an average over both describes neither.

What is deliberately **not** in the export: anything the emit gate refused. A sample whose send was turned down carries no text at all, matching `internal/safety` never recording the offending content anywhere. Everything else in there is message text, so treat the directory like the corpus rather than like logs.

**Back up and restore the corpus**

Snapshots are taken in-process into whatever `PEREGRINE_BACKUP_DIR` points at, which the prod compose binds to `./backups`. Empty disables them and is the default. Do **not** back up the corpus by copying `markov.db` from the host while the bot is running: bbolt is a single mmap updated by copy-on-write pages plus a meta-page flip, so an external byte copy can capture a torn state, and it will usually appear to work. See the long comment in `docker-compose.prod.yml`.

To restore, stop the bot, replace the file inside the `corpus` volume, and start it again.

**Start the corpus over**

```sh
docker compose -f docker-compose.prod.yml down     # stops AND REMOVES the container
docker volume rm peregrine_corpus
set -a && . ./deployed-tag.env && set +a           # pin the tag, or you fall through to :latest
docker compose -f docker-compose.prod.yml up -d
```

Three things here are each a step somebody skips.

`down` rather than `stop`, because **a stopped container still holds its volumes** and `volume rm` is refused while it exists. `down` does not remove named volumes itself, which is why the explicit `volume rm` sits between the two.

Sourcing `deployed-tag.env` is not optional even though the rollback recipe above is the only other place it appears. Without it the compose file falls through to its `:-latest` default, and although CI does push `:latest`, the deploy script ends with `docker logout ghcr.io`, so a hand-run pull of a private package fails with a bare `unauthorized`. The pinned SHA is already in the local image cache from the last deploy, so pinning it means no registry access at all. This omission was a real papercut: the recipe was followed on the first production deploy and failed at exactly this step.

Skipping the whole thing does not corrupt anything. `storage.Open` refuses a corpus it does not recognise and the container fails to start with an error naming this command, which is what happened on the first deploy to a host that had run a pre-M6 build by hand.

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

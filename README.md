# peregrine

A Markov-chain Discord engagement bot in Go. It learns from the channels it can see and replies in the server's own voice, badly and on purpose.

Sibling project to [merlin](../merlin), whose deployment conventions this repo follows. Design doc: [`SPEC.md`](./SPEC.md). Contributor guide for agents: [`CLAUDE.md`](./CLAUDE.md).

## Status

Mid-restructure. The bot works and runs, but it is being taken from a single 3,200-line `main.go` to a proper package layout one subsystem at a time, with a catalogue of known defects being closed along the way. See `SPEC.md` §8 for the defect list and §9 for the milestone order.

**Milestones 0 (repo hygiene, docs, CI/CD) and 1 (the `cmd/bot` entrypoint) are complete.** The entrypoint is real; the code behind it is not yet. Everything except `main`'s own startup and shutdown still sits unchanged in `internal/legacy`, which each later milestone empties one subsystem at a time.

Two things worth knowing before running it anywhere real:

- **The safety gate is not in yet** (M5). Until it lands, moderation is bypassable: the historical backfill path learns messages the live filter blocked, unfiltered, minutes later. See `SPEC.md` §4 A1. Do not point this at a hostile channel yet.
- **The corpus format changes in M6.** Anything learned before then is discarded rather than migrated, deliberately. See `SPEC.md` §3.4.

## Quick start

```sh
cp .env.example .env          # set DISCORD_BOT_TOKEN
go run ./cmd/bot
```

In the Discord Developer Portal the application needs the **Message Content** privileged intent ticked, under Bot. Without it Discord refuses the connection and the bot cannot read the messages it exists to learn from.

Invite scopes: `bot`. Permissions: View Channels, Read Message History, Send Messages, Send Messages in Threads, Add Reactions, and Manage Messages (used to clean up its own word-game messages).

### With Docker

```sh
cp .env.example .env
docker compose up --build
```

`PEREGRINE_DB_PATH` must point inside the mounted volume, which `.env.example` already sets to `/data/markov.db`. The container runs with a read-only root filesystem, so a relative path resolves against the distroless working directory and the database fails to open. This is the most common way to get a bot that starts, looks healthy, and has silently learned nothing.

## Configuration

Everything is environment variables; there is no config file. `.env.example` documents every one with its default and, where it matters, why the default is what it is. Only the variables marked ACTIVE are read today: the rest are the constants that M2 promotes to configuration, written down now so that milestone is mechanical.

## Maintenance

```sh
go run ./cmd/bot -clean-db     # strip spammy and slur-bearing keys from the corpus
```

Runs against the corpus and never touches Discord. It respects `PEREGRINE_DB_PATH`. Running it while the bot is live fails within five seconds with a clear message rather than hanging, because bbolt holds an exclusive lock on the file.

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

Required once after M6, since the key layout changes and old data is not migrated.

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

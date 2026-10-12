# Run

```sh
cp .env.example .env    # DISCORD_BOT_TOKEN is the only required variable
go run ./cmd/bot        # ./cmd/bot, not . (the root is not a main package)
# or
docker compose up --build
```

In a container, `PEREGRINE_DB_PATH` is a **directory** of per-server corpora (`<guild id>.db`),
and must point inside the mounted volume, as `.env.example` already does (`/data/corpora`). The
root filesystem is read-only, so a relative path can't be opened. A path that names an existing
file is refused at startup.

## Configuration

Everything is an environment variable; there is no config file. `.env.example` documents each
one with its default. Variables tagged `LATER (Mn)` aren't read yet, and setting one logs a
warning.

Configuration is checked once at startup, and a bad value is a startup error, not a quiet
fallback to the default. That includes booleans (`1/true/yes/on`, `0/false/no/off`). Every
problem is reported in one pass, so a multi-variable mistake takes one restart to find.

Set `PEREGRINE_BLOCKLIST_PATH` before pointing the bot at a hostile channel. Without it the bot
runs on the built-in baseline only and warns at startup. `blocklist.example.txt` shows the format.

## Voice notes

There is no transcription engine in this repository. `internal/plugins/voicenote` is the plugin,
over an `Engine` interface whose only implementation reports itself unavailable. Setting
`PEREGRINE_ENABLE_TRANSCRIPTION=true` without an engine logs a warning and ignores voice notes.
The package comment says what an engine has to get right.

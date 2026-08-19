# Handoff: M31 (per-guild corpora), and what M32 is

Written 2026-08-19, at the end of M31. Read this with `CLAUDE.md` and `SPEC.md` section 9.

## Where the branches are

| Branch | State |
|---|---|
| `main` | Has M29. |
| `m30-wordgame-settings` | **PR #50, open.** `/wordgame-config`: the word-game channel list, mode and interval move from the environment into a `BlobConfig` blob. CI is red for a reason unrelated to the code: GitHub Actions is blocked on account billing ("recent account payments have failed"), so all four jobs fail in 2-3 seconds without starting. Locally the same checks are green. |
| `m31-per-guild-corpora` | **This work. Branched off M30, not off `main`, because it edits M30's `settings.go`.** Uncommitted at the time of writing unless the commit below has landed. |

Merge order is M30 then M31. If M30 is rejected, M31 does not apply cleanly.

## What M31 did

One bbolt corpus per guild, in a directory, instead of one shared corpus for every guild.

The old arrangement let the bot generate one server's text into another. `SPEC.md`'s M31 row has
the full reasoning; the short version is that separate files make the isolation structural, in
the same way `storage.Reader` makes a nested transaction fail to compile rather than fail at
runtime.

**The rule the whole change follows: the guild is chosen where the TRANSACTION opens.** Nothing
below `Store.View`/`Store.Update` knows what a guild is, which is why `learn.Learner.Message`
kept its signature and its AST-pinned gate untouched.

New API, all in `internal/storage/set.go`:

- `storage.Set` - the corpora, opened lazily, bounded by `PEREGRINE_MAX_GUILD_CORPORA`, closed
  together. `For("")` returns `ErrNoGuild`; a non-numeric guild ID is refused because it becomes
  a path component; `For` after `Close` returns `ErrSetClosed`.
- `storage.Corpora` - the one-method interface every consumer takes.
- `storage.Single(store)` - one store for every guild. Tests only; using it in production would
  undo the milestone.
- `dbtest.Set(t)` and `dbtest.Guild(t, set, id)` alongside the existing `dbtest.Store(t)`.

Services that legitimately fan out over guilds (`games`, `aggro`, `health`, `repair`, `backup`)
take the concrete `*storage.Set`. Everything else takes the interface.

## What is per guild now, and what is not

**Per guild:** the corpus itself, the weekly leaderboard, M30's word-game settings, aggro's
target, the image repost pool, ingest and repair cursors, repair state and boundaries, backup
families, and the activity tracker's author map.

**Still process-wide, deliberately:** the dispatcher, the safety gate and its blocklist, the
guard's ignore list and pause switch, `activity`'s per-CHANNEL traffic ring (a channel belongs to
one guild anyway), conversation memory (per channel), the word-game `Manager` (per channel), the
presence line (there is one, so it quotes one guild picked at random), and the health status log
(summed, with a `guilds` count).

## Operational changes an operator must know

1. **`PEREGRINE_DB_PATH` is a DIRECTORY**, `/data/corpora` in production. It kept its name so a
   stale `/data/markov.db` is a startup ERROR naming the new shape, rather than silently becoming
   a directory of that name. There is no migration: the old blended corpus is not read, and
   per-guild corpora rebuild from Discord history through the ingest backfill. That was the
   operator's decision, recorded here so nobody "fixes" it later.
2. **Backups multiply.** One snapshot per guild per tick, `markov-<guild>-<ts>.db`, and `Keep` is
   per guild, so the disk cost is `KEEP x guilds x corpus size`.
3. **Maintenance modes take `-guild <id>`.** `-compact` requires it (one destination file);
   `-clean-db`, `-purge-author` and `-corpus-report` iterate every corpus when it is omitted, and
   `-corpus-report` prints a `=== path ===` heading per corpus.
4. **The bot is quiet in a guild it has no corpus for until ingest runs**, which is the same
   young-corpus behaviour `CLAUDE.md` describes for the author-diversity gate, now per server.

## Two bugs the conversion found

Both were pre-existing and are worth knowing because neither was found by reading:

- **A closed `Set` would reopen a corpus.** A goroutine still in flight at shutdown (self-learning
  is the clearest case) called `For` after `Close`, which created a file and took a flock nothing
  alive would ever release. A test failed to delete its own temp directory, which is how it
  surfaced. `ErrSetClosed` is the fix.
- **`chat.requester` dereferenced a nil state cache.** It was masked by every test message being
  guildless, so the DM early-return covered it. Fixing the tests exposed it.

## What is NOT done, in priority order

1. **M32: the paginated local/global leaderboard.** The whole design is in the approved plan at
   `C:\Users\kon\.claude\plans\agile-singing-hopcroft.md`. Summary: a `/leaderboard` slash command
   with `scope: local|global` and prev/next BUTTONS; `wordgame.Rank` gains a page and `Board` gains
   `Page`/`Pages`; global merges `AllUserStats` and the per-guild board across `Set.Guilds()`.
   Do the guard work FIRST, the way M26 did: `onInteraction` currently discards
   `InteractionMessageComponent`, and a component response needs a gated `Guard` method
   (`InteractionResponseUpdateMessage`) running the same `CheckEmit`-over-every-field walk
   `SendEmbed` uses. Button state rides in the `custom_id` (`lb:<scope>:<page>:<guild>`), so there
   is no map to leak and a restart still answers a press.
2. **A live two-guild smoke test.** Nothing here has run against Discord: the verification section
   of the plan file lists the five things to check, the important one being that guild B's replies
   never contain guild A's distinctive words.
3. **`docker-compose.prod.yml` mentions `/data` generally and needs no edit, but the deploy still
   has to be told the volume now holds a directory of corpora** rather than one file, and the old
   `markov.db` inside the volume can be deleted once the operator is satisfied.
4. **`-tuning-report` has no guild dimension.** `plugins/tuning/map.go` writes one record per tick
   with no corpus identity in it, so an archive from a multi-guild bot averages servers together.
   Adding a `GuildID` field to the wire type is a deliberate decision about the format, which is
   why it was not done quietly here.

## The checks this repo runs

```sh
go build ./... && go vet ./... && golangci-lint run
go test ./... -cover
em=$'\342\200\224' ell=$'\342\200\246' ldq=$'\342\200\234' rdq=$'\342\200\235'
grep -rnI --exclude-dir=.git -e "$em" -e "$ell" -e "$ldq" -e "$rdq" .
```

All four are green on `m31-per-guild-corpora` as of this writing. `-race` needs a C toolchain
this checkout does not have and is CI-only.

The single most valuable test in the milestone is
`TestOneGuildsWordsNeverReachAnother` in `internal/storage/set_test.go`. If a future change makes
it fail, the leak is back.

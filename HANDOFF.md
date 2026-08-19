# Handoff: M32 (the paginated local and global leaderboard), and what comes next

Written 2026-08-19, at the end of M32. Read this with `CLAUDE.md` and `SPEC.md` section 9.

## Where the branches are

| Branch | State |
|---|---|
| `main` | Has M31. PR #50 (M30) and PR #51 (M31, including the M31b allowlist fix) are both merged. |
| `m32-leaderboard-paging` | **This work.** One commit, branched off `main`. |

Note for the next context reset: the previous handoff recorded M30 as an open PR and M31 as
unmerged, and both had landed by the time M32 was written. **Check `gh pr list --state all`
rather than trusting this table**, which is a snapshot of a moment.

CI may still be red for a reason unrelated to the code: GitHub Actions was blocked on account
billing ("recent account payments have failed"), so all four jobs failed in 2-3 seconds without
starting. All four checks are green locally.

## What M32 did

`/leaderboard`, with a `scope` of local or global and **prev/next buttons** under the board.
`!leaderboard` still works and posts page one of the local board with the same buttons.

The pieces, and the reasons worth keeping:

- **The button state rides in the `custom_id`**, `lb:<scope>:<page>`. No pending map, no TTL,
  nothing to leak, and a restart still answers a press on a board posted last week.
  **The guild is deliberately NOT in it**, against the approved plan's own sketch. A component
  interaction already carries the guild it was pressed in, so a second copy creates a pair that
  can disagree, and the only way to resolve a disagreement is to render one server's board from
  a press in another. That is the exact leak M31 exists to make unwritable.
- **`wordgame.Rank` gained a page** and `Board` gained `Page` and `Pages`. The eleventh slot now
  means "not on THIS page": paging away from your own row must not hide it, and paging onto it
  must not print it twice. Ranks are computed over the whole field before the slice, or page two
  would renumber everybody from one. An out-of-range page is CLAMPED, because a press arrives
  from a button on a message that may be older than the board.
- **The guard work came first, the way M26 did it.** `onInteraction` used to discard
  `InteractionMessageComponent`. `Guard.UpdateEmbed` runs the same `CheckEmit`-over-every-field
  walk, pause switch, ignore list and explicit `AllowedMentions` as `SendEmbed`, because "the
  reader already had this message open" exempts nothing about what is going INTO it.
  `SendEmbed` and `RespondEmbed` took a variadic `components` argument, so every existing call
  site is unchanged.
- **Global merges `AllUserStats` and the per-guild board across `Set.Guilds()`.** What crosses a
  guild boundary is a user ID and an integer, never a word anybody typed, which is why this does
  not undo M31. An unreachable corpus is skipped for a global board and fatal for a local one.
- **`/leaderboard` is registered whether or not word games are on**, and `definitions(bool)`
  filters out the two commands that ARE the feature. `!leaderboard` has never been gated on the
  flag, so a slash form that was would be one command refusing what its twin answers.

## Two things the work found rather than planned

- **`discordgo.MessageComponentData` type-ASSERTS**, so an interaction whose type and data
  disagree panics the goroutine reading it. Those are two independent fields off the wire.
  `games.componentID` is the comma-ok version. A test built the mismatched payload by hand and
  found it in one run.
- **The display-name resolver moved out of `chat`.** It was `chat.displayName`, handed to
  `games.Command` as a closure, so the reactor carried a member cache for one call it made on
  somebody else's behalf. It is `names.Display` now, `chat.Deps.Members` is gone, and
  `games.New` takes the `names.Session`. A global board is what forced it: it resolves people
  who are not in the caller's guild at all, which makes the `User` fallback ordinary rather than
  rare.

## Operational changes an operator must know

1. **The command set changes on the next start.** Registration is a bulk overwrite, so
   `/leaderboard` appears and, with `PEREGRINE_ENABLE_WORD_GAMES` off, `/wordgame` and
   `/wordgame-config` disappear. Global commands can take up to an hour to propagate.
2. **No new environment variables.** Nothing in `.env.example` changed.
3. Everything in M31's operational list still applies: `PEREGRINE_DB_PATH` is a DIRECTORY,
   backups multiply per guild, maintenance modes take `-guild <id>`, and the bot is quiet in a
   guild it has no corpus for until ingest runs.

## What is NOT done, in priority order

1. **A live smoke test, for M31 and M32 together.** Nothing on this branch has run against
   Discord. The five checks are in the plan file
   (`C:\Users\kon\.claude\plans\agile-singing-hopcroft.md`); the important ones are that guild
   B's replies never contain guild A's distinctive words, and that a button press on a board
   posted BEFORE a restart still answers.
2. **`docker-compose.prod.yml` needs no edit, but the deploy still has to be told the volume now
   holds a directory of corpora** rather than one file, and the old `markov.db` inside the
   volume can be deleted once the operator is satisfied.
3. **`-tuning-report` has no guild dimension.** `plugins/tuning/map.go` writes one record per
   tick with no corpus identity in it, so an archive from a multi-guild bot averages servers
   together. Adding a `GuildID` field to the wire type is a deliberate decision about the
   format, which is why it was not done quietly.
4. **The remaining global dials are scalars, not lists.** Aggro's chance and duration, the
   autopost interval and skip chance, the roast chance and the word-game trigger chances are all
   process-wide. If one wants a per-guild answer, the pattern is `/wordgame-config`: a blob in
   that guild's corpus, seeded from the environment, with a command that owns it afterwards.
5. **A kill switch reachable from Discord** is still SPEC.md section 10's open decision. M26
   answered the surface question and M32 added the component half, so what is left is a decision
   about scope rather than machinery.

## The checks this repo runs

```sh
go build ./... && go vet ./... && golangci-lint run
go test ./... -cover
em=$'\342\200\224' ell=$'\342\200\246' ldq=$'\342\200\234' rdq=$'\342\200\235'
grep -rnI --exclude-dir=.git -e "$em" -e "$ell" -e "$ldq" -e "$rdq" .
```

All four are green as of this writing. `-race` needs a C toolchain this checkout does not have
and is CI-only.

The single most valuable test on this branch is still `TestOneGuildsWordsNeverReachAnother` in
`internal/storage/set_test.go`. M32's equivalent is
`TestALocalBoardCountsOnlyThisServer` in `internal/plugins/games/board_test.go`: if a future
change makes it fail, the board is blended again.

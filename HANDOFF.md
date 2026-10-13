# Handoff

Read with `CLAUDE.md` and `SPEC.md`. Update at the end of a milestone; keep it to the current
state, not a history (that is git log).

## Branches

`main` is the only long-lived branch. Check `gh pr list` for anything in flight.

## Last change

M38 made generation fast and more coherent on real text (`tools/replay`, findings 59 to 61). After
it: the invite and the Message Content intent are synced from code at boot (`core.SyncApplication`,
`peregrine.skua.lol`), and the README, SPEC and this file were cut to current state.

Measured on the main guild's snapshot, five seeds of 400 prompts, as the baseline for the next
engine change:

| replies with no unattested trigram | attested trigrams | 4-grams attested | prompt reuse | mean words | p90 latency, local |
|---|---|---|---|---|---|
| 32.2% | 76.7% | 21.9% | 73.1% | 7.0 | 32 ms |

Measured and rejected, so nobody repeats them: `PEREGRINE_KN_DISCOUNT` 0.5 or 0.3, `minCandidates`
3 or 2, a cubic length skew, a per-`Seed` memo of association reads.

## Operator notes

- The container's `mem_limit` (1536m) must stay above the combined corpus size plus a few hundred
  MB. Below it the bot looks slow, not crashed.
- A new permission goes in `core.Permissions`; it reaches the invite on the deploy that ships it.

## Not done, in priority order

1. **Confirm production latency fell** after M38: the tuning export's `took_ms` median was 2.5 s.
   Split the archive by version first. If it has not fallen, check `pgmajfault` in the container's
   `memory.stat` before the code.
2. **Measure engagement on the current version** once a week of samples exists. Short replies and
   prompt reuse are the two leads.
3. **`tools/replay` covers one guild.** The second guild's corpus has not been replayed.
4. **Unverified live:** a full wheel match with two accounts, a stale press refused privately, a
   modal repainting the card, a restart mid-match, `/leaderboard`'s gold column and `/wallet`.
5. **Open:** no lifetime leaderboard; `-tuning-report` has no guild dimension; a Discord-reachable
   kill switch (`SPEC.md` §10).

## Checks

```sh
go build ./... && go vet ./... && golangci-lint run
go test ./... -cover
em=$'\342\200\224' ell=$'\342\200\246' ldq=$'\342\200\234' rdq=$'\342\200\235'
grep -rnI --exclude-dir=.git -e "$em" -e "$ell" -e "$ldq" -e "$rdq" .
```

`-race` needs a C toolchain and is CI-only on stock Windows. For any engine change, also run
`tools/replay` over a few seeds and compare against the table above.

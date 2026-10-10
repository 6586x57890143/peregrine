# Handoff: M38 (generation measured on real text), and what comes next

Written 2026-10-10, at the end of M38. Read this with `CLAUDE.md` and `SPEC.md` section 9.

## Where the branches are

| Branch | PR | State |
|---|---|---|
| `main` | | Has M37 (`/game`), the lifetime gold board, and manual-dispatch deploys. |
| `m38-coherence-speed` | | This milestone. Branched off `main`. |

**Check `gh pr list --state all` rather than trusting this table**, which is a snapshot of a
moment.

## What M38 did

The brief was coherence, then speed, then engagement. Every change was measured with the new
`tools/replay` against the production prompts in the tuning archive and a snapshot of the main
guild's corpus, averaged over five seeds of 400 prompts each.

| | `main` | M38 |
|---|---|---|
| replies with no unattested trigram | 22.5% | 32.2% |
| attested trigrams | 69.8% | 76.7% |
| 4-grams attested (recitation check) | 16.9% | 21.9% |
| replies reusing a word of their prompt | 74.6% | 73.1% |
| mean words | 7.6 | 7.0 |
| generation p90, local | 774 ms | 32 ms |

The prompt-reuse drop is about one and a half standard errors at 2,000 replies: within noise,
but it is the number to watch, because it is also the strongest engagement correlate below.

- **Speed (finding 59).** The scorer decoded every association map it might need on every
  step, up to a million entries, to read one per candidate. It uses point lookups now, and the
  name hop reads its 24 strongest topics. The two-hop seed tier memoizes and keeps a bounded
  top-k. Output was byte-identical across that change.
- **The production latency had a second cause:** `mem_limit: 512m` against 900 MB of corpora.
  The mmap's page cache counts against the cgroup, so the bot thrashed (38k major faults in half
  an hour). Now 1536m.
- **Coherence (findings 60 and 61).** Persona filler goes only at the edges, and a `Continuity`
  logit of 1.0 per extra word of context keeps walks off stranded bigram runs.
- **Engagement was measured but not tuned.** In the archive, replies of five words or fewer drew
  engagement 78.5% of the time against about 71% for longer ones, and replies reusing a prompt
  word 74.8% against 67.6%. Both effects are moderate, and time of day swings more than either.
  M38 shortened replies and held prompt reuse; it did not move a seed weight, because the
  per-tier split was too noisy to act on (name-topic seeds draw human replies at the same rate
  as prompt seeds and differ only in reactions).

Measured and rejected, so nobody repeats them: lowering `PEREGRINE_KN_DISCOUNT` to 0.5 or 0.3
(no effect), `minCandidates` 3 or 2 (one to three trigram points, not worth a constant), a
cubic length skew (length is decided by the chain choosing to end, 92% of the time), and a
per-`Seed` memo of association reads (5 to 8% for 35 lines).

## Operational changes an operator must know

1. **The container's memory limit is 1536m.** Keep it above the combined size of
   `/data/corpora` plus a few hundred MB. A limit below the corpus looks like a slow bot, not a
   crash.
2. **Replies are a little shorter and change subject less.** No variable changed.
3. **Nothing to migrate.** No schema or config change.

## What is NOT done, in priority order

1. **Confirm the production latency fell.** After the deploy, the tuning export's `took_ms`
   should drop from a median of 2.5 s. `-tuning-report` warns when an archive spans versions:
   split it by version before comparing. If it has not fallen, look at `pgmajfault` in the
   container's `memory.stat` before looking at code.
2. **Measure engagement on the new version** the same way, once a week of samples exists.
   Short replies and prompt reuse are the two leads worth testing deliberately.
3. **`tools/replay` covers one guild.** It takes one snapshot; the second guild's corpus is a
   third of the size and has not been replayed.
4. **Carried from M35, status unverified since:** a live smoke test of the wheel (a full match
   with two accounts, a stale press refused privately, a modal submit repainting the card, a
   restart mid-match showing the ended card, `/leaderboard`'s gold column and `/wallet`); the
   card's look on real clients; three inline columns on a narrow desktop board; no lifetime
   leaderboard; `-tuning-report` has no guild dimension; a Discord-reachable kill switch is
   still SPEC section 10's open decision.

## The checks this repo runs

```sh
go build ./... && go vet ./... && golangci-lint run
go test ./... -cover
em=$'\342\200\224' ell=$'\342\200\246' ldq=$'\342\200\234' rdq=$'\342\200\235'
grep -rnI --exclude-dir=.git -e "$em" -e "$ell" -e "$ldq" -e "$rdq" .
```

`-race` needs a C toolchain this checkout does not have and is CI-only. For any engine change,
also run `tools/replay` over a few seeds and compare against the table above.

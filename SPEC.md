# peregrine: design

A Markov-chain Discord bot in Go, built on `discordgo` v0.29 and `bbolt` v1.5. It learns from
channel messages and replies in the server's own voice. It runs in a meme-heavy, high-traffic
community and its purpose is engagement, fun and chaos, which makes its quality criterion unusual
and is the thing most likely to be misread by someone reading the code cold. See §1 and §5.

Section numbers are stable because code comments cite them. Comments also cite "finding N" and
milestones ("M24"): those are in this file's history, `git show 4aaf753:SPEC.md`, sections 8 and 9.

---

## 1. Design principles

1. **Quality means "lands", not "is grammatical".** The bot exists to be funny and to provoke
   replies. A 6-word non-sequitur that gets a reaction beats a 40-word grammatical ramble. Several
   choices in §5 look like defects against a conventional language-model metric and are not.
2. **The operator carries what the bot says.** Users are assumed to be hostile and to be trying to
   make the bot emit gross or borderline illegal content. Protecting the operator is architectural,
   not a matter of adding patterns. See §4.
3. **Gates go at chokepoints, never at call sites.** A check that must not be bypassed belongs at
   the single funnel every path traverses. A check at one of four call sites is not a check.
4. **Fail safe, and never fail silent.** A missing blocklist is a startup error, not an empty
   ruleset. A refused send is logged. One feature failing disables that feature, never the
   process. `log.Fatal` is reserved for a missing token and an unopenable corpus.
5. **Every feature is part of the product.** Aggro, image reposts, word games, the roast persona
   and autonomous posting are the engagement surface. Configuration wired to nothing is deleted.
6. **Prefer making a bug unwritable over fixing it once.** The `Reader`/`Writer` seam (§3), the
   reactor's consume contract (§6) and the two gates (§4) each make a class of defect impossible
   to express.
7. **The output bar is written down and settled by reading samples, not by argument.** A reply is
   a short-to-medium memey sentence that grasps the corpus, involves the people around it, and is
   funny by **recombining** concepts that were never adjacent in one source message. A bot that
   recites is not funny even when every word is in register, so a change that improves coherence
   by removing recombination makes the product worse. §5.5 is the rubric.

---

## 2. Repository layout

```
cmd/bot/                 main.go (mode, signals) and services.go (the only place features meet)
internal/config/         Config, Load, validation. Environment only
internal/corpus/         value types only, no internal imports
internal/storage/        the only package that imports bbolt; one corpus per guild
internal/dbtest/         a real corpus in t.TempDir()
internal/text/           tokenizer, sentence cleanup, interner
internal/filter/         spam and character-class filters
internal/safety/         normalizer, blocklist, CheckLearn and CheckEmit
internal/markov/         the engine. Imports neither bbolt nor discordgo
internal/core/           Service registry, Dispatcher, RunLoop, WatchReady, install sync
internal/discordguard/   the outbound chokepoint
internal/maintenance/    -clean-db, -compact, -purge-author
internal/ingest/         guild and channel history walk, per-channel cursors
internal/learn/          the only way anything enters the corpus
internal/generate/       prompt to sentence, per-channel memory
internal/names/          who a message is about, and what the corpus calls them
internal/channels/       what a channel is, from the state cache
internal/activity/       where people are talking, from gateway traffic
internal/wordgame/       scramble games and the leaderboard
internal/wheel/          the wheel game engine; wheelart draws its strips
internal/tuning/         the tuning export's wire types (stdlib only)
internal/plugins/        chat, aggro, images, games, autopost, health, backup, ingest,
                         repair, tuning, voicenote
web/                     the peregrine.skua.lol invite redirect
tools/replay/            replays real prompts against a corpus snapshot
```

Each consumer declares the minimal interface it needs and the concrete type satisfies it
structurally. `internal/markov` declares `Corpus`; `*storage.Reader` satisfies it with no glue, and
the assertion lives in an external test package. That keeps import cycles impossible and lets the
generation heuristics be tested against Go maps.

Import rules:

- `corpus` and `text` stay leaves. `corpus` holds type declarations and nothing else.
- `markov` never imports `storage`; every `Corpus` method returns a stdlib or `corpus` type.
- `storage` never imports `config`. `Open` takes a path plus options.
- `core` never imports a plugin. Registration happens only in `cmd/bot`.
- `dbtest` is imported only from `_test.go` files, so it never enters the binary.

---

## 3. Storage

bbolt: embedded, single-file, pure Go. Pure Go is what allows `CGO_ENABLED=0` and a distroless
static image.

Properties that shape the design:

- **One writer, process-wide.** A slow loop inside `db.Update` blocks all ingestion.
- **Exclusive flock.** `Open` always passes a 5 s timeout, or a maintenance mode against a live bot
  hangs with no output.
- **Nested transactions deadlock.** A read transaction holds the mmap read lock; a writer growing
  the mmap waits for the write lock; new readers queue behind it. `Reader` has no method that opens
  a transaction, so a nested one cannot be written.
- **The file never shrinks.** Deletes free pages for reuse only, hence `-compact`.
- **Long read transactions grow the file**, because copy-on-write pages cannot be reused while an
  old reader can see them. Counts are counters in `meta`, never `Bucket.Stats()`.

### 3.1 Key layout

```
bucket "ngram"       key = <prefix> 0x00 <next>              value = count u64 | authors u32
bucket "ngram_auth"  key = <prefix> 0x00 <next> 0x00 <uid>   value = presence
bucket "cursor"      key = <channel id>                      value = snowflake be64
bucket "image"       key = <msgID be64> 0x00 <url>           value = author be64
```

The count and the distinct-author count share one 12-byte value because generation reads both for
every candidate. `0x00` is a safe separator: the tokenizer cannot emit a NUL, and the codec refuses
to write an ambiguous key. It also sorts below space, so a seek on `"the\x00"` cannot wander into
`"the cat\x00"`. Learning one n-gram is a point get, an add and a put. The ingestion loop starts at
order 2, so an empty prefix cannot be produced; `PEREGRINE_MAX_NGRAM` has a minimum of 2.

The association indexes use the same shape (`word 0x00 assoc` to a 16-byte `{count, posSum}`).
Snowflakes are fixed-width big-endian, so key order is chronological: the oldest cached image is a
cursor's `First()`, and the writer refuses to move an ingest cursor backwards with one
`bytes.Compare`.

`schema_version` is 2. A version 1 corpus is refused, not migrated, because the corpus is expensive
to convert and re-derivable from Discord history; `upgradeToV2` only empties the image cache, which
refills itself in minutes. Migrate what is cheap to lose, refuse what is not. A corpus with data and
no version is refused too, rather than opened and silently ignored.

### 3.2 Kneser-Ney indexes

```
bucket "kn_succ"   key = <prefix>                       value = N1+(prefix .)  distinct successors
bucket "kn_pre"    key = <token> 0x00 <preceding ctx>   value = presence
bucket "kn_pre_n"  key = <token>                        value = N1+(. token)   distinct predecessors
```

`kn_succ` is the interpolation normalizer, stored because deriving it is a scan per lookup.
`kn_pre` exists only to keep `kn_pre_n` correct, since a distinct count needs to know whether a
predecessor was already seen. **Presence values are one byte, not zero**: bbolt's `Get` returns nil
for both a missing key and an empty value.

`kn_pre` grows with distinct (token, context) pairs and is the fastest-growing structure. It will
want a pruning policy once there is data to design one against (§10).

### 3.3 Author diversity

Each n-gram carries a distinct-author count beside its frequency, maintained like `kn_pre_n`. §4
explains why it is the load-bearing anti-poisoning control.

---

## 4. Safety and threat model

Assume users actively try to make the bot say gross or borderline illegal things, and that the
operator wears the consequences.

### 4.1 Threats

**A1. A learning path that skips moderation defeats the one that has it.** `learnMessage` has
several callers (live messages, history backfill, repair). `gate.CheckLearn` is called inside
`learn.Learner.Message`, so every caller, and any new one, is covered. An AST test fails if the
call leaves that body.

**A2. Generated text needs its own filter.** `gate.CheckEmit` applies the built-in slur and illegal
lists plus the operator blocklist to everything the bot sends. On rejection the bot stays silent
rather than substituting a fallback.

**A3. Filtering the corpus cannot bound the output.** A Markov chain composes novel sequences from
fragments learned separately, so innocuous pieces can join into something the operator answers for.
Input filtering lowers the rate; only an output gate bounds the result. That is why two gates are
not redundant, and why `CheckEmit` lives in `internal/discordguard` and covers every send, not only
generation.

**A4. Illegal content.** The `illegal` category loads from `PEREGRINE_BLOCKLIST_PATH`, is dropped on
learn, blocked on emit, and alerts. The patterns are deliberately not in this repository; startup
warns when the loaded list has none.

**A5. Slurs: laundering and evasion.** Replacing a slur and learning the rest teaches the bot the
sentence, so the learning path rejects whole and `Verdict` has no rewritten-text field. Matching
runs against `safety.Normalize`, which removes spacing, leet, homoglyphs, combining marks and
zero-width characters. `internal/filter` keeps `TestSlurRulesAreEvadable` so nobody mistakes raw
matching for sufficient.

**A6. Poisoning is cheap and self-amplifying.** Repeating a phrase is a direct write to the model.
The generation gate requires a continuation to have come from `PEREGRINE_MIN_DISTINCT_AUTHORS`
people (default 2), which turns the attack from persistence into collusion. The bot's own output
carries no author, so self-learning cannot bootstrap a phrase. A narrow allowance
(`PEREGRINE_SOLO_REPEAT_LIMIT`) admits a one-author edge at low count and short context, because a
strict gate leaves a chat corpus nearly mute; long one-author contexts are someone's sentence and
stay refused. `-clean-db` and `-purge-author` are the retroactive remedies.

**A7. Image reposting republishes other people's links under the bot's name.** The cache is keyed
by source message, so it can attribute, cap and revoke: at most `PEREGRINE_IMAGE_MAX_PER_AUTHOR`
entries per author (enforced inside `AddImageURL`), deleting a message revokes what it contributed,
and the destination channel gets the same NSFW check as the origin. Every repost passes `CheckEmit`,
which catches words in a Tenor slug.

### 4.2 The defense architecture

1. **One gate, two directions.** `CheckLearn` inside the learner, `CheckEmit` inside the guard,
   sharing one normalizer and one ruleset. On rejection, silence.
2. **Reject, never launder, on the learning path.** Replacement is for display only.
3. **Normalize before matching.** NFKC, strip zero-width characters and combining marks, fold
   homoglyphs, collapse repeats and intra-word separators. Patterns must not re-enumerate what the
   normalizer removes.
4. **Author diversity gates generation, as a filter rather than a penalty.** A penalty can be
   out-repeated; a filter cannot. It never relaxes when it empties the candidate set.
5. **The blocklist is data, not source.** `PEREGRINE_BLOCKLIST_PATH`; the real file is gitignored.
   Committing it would make the repo a searchable slur list and turn every addition into a public
   diff. It fails closed: missing, unreadable, malformed or empty is a startup error. Keep `illegal`
   narrow, because every entry pages a human.
6. **Operator controls.** `PEREGRINE_PAUSE_ALL_WRITES` mutes every send process-wide. Every
   rejection is logged. `-clean-db` and `-purge-author` repair a corpus after the fact.
7. **No mentions, ever.** The guard sends an explicit empty allowed-mentions object on every call,
   because a learned mention is a corpus token the chain will emit again.

### 4.3 A policy decision worth arguing with

The character-class filter permits only `unicode.Latin`, so a predominantly Cyrillic, CJK or Arabic
message is dropped as spam at the 0.80 threshold. That excludes non-Latin-script participants from
the corpus entirely, and incidentally blocks whole-message homoglyph attacks. Keep it, change it or
narrow it, but deliberately.

---

## 5. Generation

`internal/markov` owns the probability model, the heuristics, the sampler, seed selection, the
length model and the persona.

### 5.1 Why this model

`log P(next | prefix)` from **interpolated Kneser-Ney**, plus bounded additive logits for every
heuristic, then temperature and top-k/top-p sampling.

KN fits because the corpus is small and sparse: at order 5 nearly every 4-gram has count 1, and
backoff over counts that are almost all 1 is close to arbitrary. Absolute discounting is built for
that.

**The deliberate deviation from textbook KN.** KN estimates lower orders from continuation counts,
which correctly demotes "Francisco" (frequent, nearly always after "San"). A meme, a copypasta and an
inside joke look statistically identical to "Francisco", so pure KN would suppress exactly this
server's register:

```
P_low = (1 - mu) * P_kn_continuation + mu * P_raw
```

`PEREGRINE_KN_RAW_MIX` is `mu`: 0 is textbook KN, 1 is raw counts, 0.25 leans KN and keeps the
memes. Pure KN optimizes perplexity; peregrine optimizes for landing a joke.

### 5.2 The model

**Additive logits.** Addition in log space multiplies a candidate's odds by a bounded factor
whatever its base probability. Every heuristic that sums evidence is `tanh`-squashed to its own
weight, and a test bounds the total by the sum of the positive weights.

**Bounded enumeration.** Candidates come from the longest context that has any, extended one order
down while the set is below a floor, with per-order and total caps. The floor matters: at high
orders nearly every context has one continuation, and without it the step is deterministic.

**`c(prefix, .)` is a scan, not a counter.** `Reader.PrefixTotal` is a memoized zero-allocation
cursor scan. A stored counter would add work inside the single write transaction that serializes
ingestion; the reader is a human waiting, the writer is background work.

**The author gate is a filter** (§4.2). The end sentinel is exempt, or a sentence could not end
until several people ended a message the same way.

**Continuity.** A logit rewards candidates that continue more of the matched context, which keeps
walks off stranded bigram runs.

**Seeds are seven weighted tiers**: prompt n-grams, names, direct associations, and a bounded
two-hop association tier for transitive reach ("near something near X"), weighted below every direct
tier. Every candidate must have continuations, and a lone function word is never a seed.

**One length model.** A target is sampled per sentence between `MIN_WORDS` and `MAX_WORDS`, skewed
short. Below the floor the end token takes a finite penalty easing to zero at the target; past it, a
capped bonus. Under two words the bot re-seeds up to three times and otherwise stays silent: a
one-word reply looks broken.

**Jumps** recover a dead end at most once per sentence, never past `Length.Min` and never after a
function word.

**One persona** drives both the in-sampler lexicon bias and the post-pass filler, which goes only at
the start or end of a reply: in the middle it reads as a typo.

**Context.** Only the prompt may seed; the reply chain, channel topics and recalled names may only
steer. Conversation memory is per channel and bounded at 200 channels. Co-occurrence is windowed on
the learn path (default 5), because proximity is what association means and all-pairs on a long
message is tens of thousands of writes in the single writer.

### 5.3 Knobs

| Variable | Meaning | Default |
|---|---|---|
| `PEREGRINE_TEMPERATURE` | `logit/T`. Range 0 to 10; 0 is argmax | 1.0 |
| `PEREGRINE_TOP_K` | Truncate the tail before sampling. 0 disables | 40 |
| `PEREGRINE_TOP_P` | Nucleus threshold, after top-k | 0.95 |
| `PEREGRINE_KN_DISCOUNT` | KN discount `D`; must stay below 1 or count-1 continuations vanish | 0.75 |
| `PEREGRINE_KN_RAW_MIX` | `mu`, per §5.1 | 0.25 |
| `PEREGRINE_MIN_DISTINCT_AUTHORS` | Generation eligibility, per §4. 0 disables | 2 |
| `PEREGRINE_SOLO_REPEAT_LIMIT` | One-author allowance, per §4 A6. 0 is the strict gate | 2 |
| `PEREGRINE_PROMPT_RELEVANCE_BOOST` | Logit for a candidate in the prompt. Range 0 to 5 | 0.6 |
| `PEREGRINE_MAX_NGRAM` | Highest order. Minimum 2 | 5 |
| `PEREGRINE_MIN_WORDS` | Length floor | 4 |
| `PEREGRINE_MAX_WORDS` | Length cap. Short lands, long reads as malfunction | 18 |
| `PEREGRINE_COOCCURRENCE_WINDOW` | Co-occurrence window on learn. 0 is unbounded | 5 |
| `PEREGRINE_ROAST_CHANCE` | Probability a direct mention gets the roast persona | 0.10 |

**The logit weights are deliberately not variables.** They are only meaningful relative to each
other and an operator has no instrument to judge them. They live in `markov.DefaultWeights`, tuned
by reading golden samples and `tools/replay`.

**When a variable's units change, the name stays and the range narrows**, so a stale value is a
startup error naming the range rather than silently reinterpreted.

**Repetition is deliberately not penalized hard.** Copypasta cadence and "ratio ratio ratio" are
the register. The penalty suppresses stutter and reaches a cap.

### 5.4 Verification

`TestGenerateGolden` generates from a fixed corpus with a seeded source across a sweep of
temperature and top-k, printing output so before and after are directly comparable:

```sh
go test ./internal/markov/ -run TestGenerateGolden -v
```

`markov.Source` is a parameter so the same seed always gives the same text. Assertions pin the
mechanics: KN sums to one; backoff prefers the higher order; temperature raises entropy
monotonically; top-k and top-p always leave a survivor; the author gate refuses a phrase one author
repeated 500 times; heuristics stay under their weights; length skews short; no sentinel or
separator leaks. The fake corpus is Go maps, and an external test package re-checks the same
properties against real bbolt.

On real text, `tools/replay` replays production prompts against a corpus snapshot and reports
attested n-gram rates, recitation and prompt reuse. Average several seeds before believing a small
difference.

### 5.5 The output bar

§1 principle 7 is the standard. Checked against the golden samples:

1. **Length.** A median of 4 to 12 words.
2. **In register.** Reads as somebody in this server typing.
3. **Grasps the data.** No dangling function word, jump seam, misplaced filler or leaked sentinel.
4. **Involves people.** When the prompt names somebody, about one reply in three engages them
   (`TestSeedDrawsTheNameOftenEnoughToNotice`).
5. **Recombines rather than recites.** No reply reproduces a five-word span of one fixture message
   (`TestGoldenSamplesAreNotRecitation`). Four-word matches are logged, not failed: short idioms
   exist verbatim because several people typed them.
6. **Spans sources.** A reply's bigrams are not all traceable to one message
   (`TestGoldenSamplesSpanSources`).
7. **Responds to its prompt.** A reply is more about the person its prompt named than anybody else,
   and at least a third of replies to a named prompt engage that person
   (`TestGoldenSamplesRespondToTheirPrompt`).
8. **Does not collapse to one phrase** (`TestGoldenSamplesDoNotCollapseToOnePhrase`).

Criteria 7 and 8 are a pair: every context signal pulls toward the conversation, and the limit of
that is a bot that says the same thing every time.

The fixture is part of the instrument: **shared idioms inside different sentences, never shared
sentences.** The author gate needs genuine agreement, and agreement built from near-identical
messages leaves recitation as the only admissible path.

---

## 6. Engagement features

These are the product.

- **Replies.** The `chat` reactor is a table of steps; only commands consume a message. A command
  matches the whole trimmed message, so a reply that happens to start with one is still a reply.
- **Custom emotes.** `:shortcode:` resolves against the guilds in the state cache.
- **Games.** Word scrambles with a hint ladder and gauntlets, and the wheel, all under `/game`.
  `/leaderboard` and `/wallet` stand alone. Engines do no I/O; the service
  sends through the guard.
- **Bird aggro.** Targets someone the bot has seen speak recently, from the activity tracker.
- **Image reposting.** Cheap chaos at a low rate, under §4 A7's controls.
- **Autonomous posting.** Off by default, and `true` with no channel list is a startup error:
  where the bot may speak unprompted is the operator's sentence to write.
- **The roast persona.** One persona layer, §5.2.

---

## 7. Lifecycle

`core.Dispatcher` owns a bounded worker pool. The gateway handler rejects bots and enqueues without
blocking: discordgo dispatches each event on its own goroutine, so blocking would grow goroutines
without bound, and dropping with a counter is the honest semantics for best-effort chat. Drops
surface in the health report and the status line. **The queue is never closed**, so an in-flight
enqueue cannot panic; shutdown sets a flag, drains to a deadline, cancels the workers and waits.

`core.RunLoop` runs background work: panic-isolated, context-bound, with `Immediate` for the loops
wanted at startup. There is no shared `*rand.Rand`; `math/rand/v2`'s top-level functions are safe.

Startup: open the corpus, build the session, sync the application (`core.SyncApplication`: install
permissions and the Message Content intent), `InitAll`, arm `WatchReady`, `Open`, wait for READY,
start the dispatcher, `StartAll`. `WatchReady` is armed before `Open` because discordgo dispatches
inside `Open`, and gateway handlers register in `Init` for the same reason. `Open` returning nil
only means identify was sent; waiting for READY turns a rejected identify (4014) into a startup
error.

Shutdown:

```
1. session.Close()        stop the inflow before draining
2. dispatcher.Shutdown()  bounded drain
3. registry.ShutdownAll() reverse registration order
4. store.Close()          last
```

One 8 s budget covers steps 2 and 3, under Docker's 10 s between SIGTERM and SIGKILL. Services are
registered only in `cmd/bot`'s `registerServices`, and the order is behaviour: `Init` in order,
`Shutdown` in reverse, `chat` last.

---

## 8. Findings

The numbered findings catalogue (1 to 61) that code comments cite is in history:
`git show 4aaf753:SPEC.md`.

## 9. Milestones

One milestone or slice per PR, green at every commit: `go vet`, `golangci-lint run`, `go test`,
the prose check and the Docker build. The milestone table (M0 to M38) is in history:
`git show 4aaf753:SPEC.md`.

---

## 10. Open decisions

- **`mu`, `D` and the logit weights need values measured on real text.** The defaults are
  considered guesses; `tools/replay` is the instrument.
- **`PEREGRINE_TEMPERATURE` may want to be higher than 1.0.** 1.6 read with more variety on the
  golden fixture and 2.5 degraded. Not a changed default until a sweep on real text agrees.
- **Low-order joins.** Backing off to a one-word context produces joins like "what are you know
  what". It is the price of capping the one-author allowance at short contexts, which is what keeps
  recitation down. Revisit only with a measurement on both sides.
- **The persona lexicon matches whole tokens**, so "cope" misses "coping". Extend it with inflected
  forms rather than stemming.
- **`kn_pre` pruning.** Deferred until there is data (§3.2).
- **A kill switch reachable from Discord.** `PAUSE_ALL_WRITES` needs SSH and a restart. The pieces
  exist (`games.Service.Authorized`, `Gate.SetPaused`, slash commands through the guard, ephemeral
  replies); what is undecided is its scope.

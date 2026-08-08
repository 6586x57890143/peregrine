# Contributing

## Before you open a PR

```sh
go build ./...
go vet ./...
golangci-lint run                 # CI pins v2.12.2, and the repo is at zero issues
go test ./... -cover
```

Plus the prose check, which is easy to trip in a comment:

```sh
em=$'\342\200\224' ell=$'\342\200\246' ldq=$'\342\200\234' rdq=$'\342\200\235'
grep -rnI --exclude-dir=.git -e "$em" -e "$ell" -e "$ldq" -e "$rdq" .
```

`go test -race` needs `CGO_ENABLED=1` and a C toolchain. If you do not have gcc it fails with `cgo: C compiler "gcc" not found`, which is a missing toolchain rather than a failing test. CI runs it on Linux where cgo works.

## Ground rules

**One milestone, or a slice of one, per PR**, per `SPEC.md` §9. The tree must build, vet, lint and test green at the end of every PR. `main` is protected: PR review plus all CI checks, no direct or force pushes.

**Lint is at zero issues and stays there.** It was brought to zero deliberately so that no one has to distinguish their own new problems from pre-existing noise. Do not add a `//nolint` without a comment explaining why the linter is wrong.

**Comments say why, not what.** A comment restating the line is noise. A comment recording the failure that motivated the code is what stops the next person reverting it. Most of the non-obvious code in this repo exists because of a specific bug; when you fix one, write down what it was. This matters more than usual here because several correct-looking behaviors are deliberate and several deliberate-looking ones are bugs.

**Plain punctuation.** No em dashes, ellipsis characters or curly quotes anywhere, including comments and docs. The one exception is a literal right single quote inside the tokenizer's character class, which is load-bearing for tokenizing curly-apostrophe contractions and which the CI check deliberately does not scan for. Do not clean it up.

**Never commit large or sensitive files.** `markov.db`, `voicenotes/models/`, `voicenotes/bin/`, `.env` and `blocklist.txt` are gitignored for real reasons. The whisper model is 465 MiB, over GitHub's hard 100 MiB per-file limit, so a commit containing it cannot be pushed and the only remedy is rewriting history. The corpus holds learned user content. The blocklist is a slur list. If you find yourself adding an exception, stop and ask.

## Things that will get a PR sent back

**Adding new code to `internal/legacy`.** That package is a holding pen for the pre-restructure `main.go` and it only shrinks. New code goes in the package the milestone table assigns it to; if no package exists for it yet, that is a sign the milestone it belongs to has not landed. The one thing that legitimately changes in `legacy` is code being *deleted* as it moves out.

**Adding a check at a call site instead of a chokepoint.** See `SPEC.md` §1.3. The moderation in this bot was defeated for its entire life because the filters lived in one of four callers of `learnMessage` instead of inside it. Any check that must not be bypassable goes at the funnel.

**Reaching past a guard.** Once `internal/discordguard` exists, sends go through it, not through `s.ChannelMessage*`. It owns mention suppression and the outbound safety gate, and a direct call silently opts out of both. Today's `sendMessage`/`editMessage`/`deleteMessage` helpers in `internal/legacy/legacy.go` are its stand-in; use those.

**Opening a nested bbolt transaction.** A `db.View` inside another transaction can deadlock the process unrecoverably. After M6 the `Reader`/`Writer` seam makes this unwritable; before then, check by hand.

**Silently discarding an error from a Discord call or a bbolt write.** A refused send used to be indistinguishable from a successful one, so the bot appeared to ignore people at random with nothing in the log. Log it or handle it.

**"Fixing" `PEREGRINE_KN_RAW_MIX` to 0.** Textbook Kneser-Ney suppresses high-frequency low-context tokens, which is exactly what a meme is. `SPEC.md` §5.2 explains at length. Pure KN optimizes perplexity; this bot optimizes for landing a joke.

**Restoring `ContextWindow` or `CoherencyBalance`.** They were deleted because they were never read. `SPEC.md` §5.3.

**Making one feature's failure fatal.** `log.Fatal` is for a missing token and an unopenable corpus. Everything else degrades to that feature being off, with a warning.

## Tests

New pure functions get table tests. The tokenizer, the filters, the normalizer, the samplers and the codecs are all cheap to test and all sit on the hot path of every message.

For the safety normalizer specifically, write **adversarial** tests, not example tests: intra-word spacing, zero-width joiners, combining marks, homoglyphs, unenumerated leet. The old filter passed every example test anyone would have written for it and was trivially evadable.

After M6, `internal/dbtest` gives you a real bbolt file in `t.TempDir()`. There is no skip path and there is not meant to be one: unlike merlin's Postgres harness, this one cannot be unavailable, so database tests run identically on your laptop and in CI.

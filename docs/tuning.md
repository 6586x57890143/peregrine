# Tune

With `PEREGRINE_TUNING_DIR` set, the bot writes what it said, why, and whether anybody reacted,
as JSON lines on a bind mount. It never uploads anything; you pull the files.

```sh
scp -r deploy@<host>:/home/deploy/peregrine/tuning ./tuning
go run ./cmd/bot -tuning-report ./tuning    # reads JSON lines only: no .env, token or corpus
```

Three record kinds:

- **`sample`**: one generation attempt, including the ones that produced nothing, with its seed
  tier, how far the backoff went, and how many candidates the author-diversity gate removed.
- **`engagement`**: reactions, distinct reactors and whether a human replied, once a sample's
  window closes.
- **`snapshot`**: corpus counts, health counters, and every dial in force, including the logit
  weights compiled into the binary.

Compare reports from before and after a change. `-tuning-report` warns when an archive spans two
versions or two sets of dials.

Anything the emit gate refused is recorded without its text. Everything else is message text, so
treat the directory like the corpus, not like logs.

For engine changes, also run `tools/replay` against a corpus snapshot; `CLAUDE.md` has the
commands.

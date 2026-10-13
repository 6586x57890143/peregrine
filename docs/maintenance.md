# Maintain the corpus

```sh
go run ./cmd/bot -clean-db                                # remove spammy and blocklisted n-grams
go run ./cmd/bot -compact /data/new.db -guild 123456789   # reclaim free pages, one server
go run ./cmd/bot -purge-author 1234567890                 # undo one user's share of author diversity
```

These run against the corpus and never touch Discord, so none needs `DISCORD_BOT_TOKEN`. They
respect `PEREGRINE_DB_PATH`. Run one at a time; you choose the order. Running one while the bot
is live fails within five seconds, because bbolt holds an exclusive lock on the file.

In a container: `docker compose run --rm bot -clean-db`.

- **`-clean-db`** loads `PEREGRINE_BLOCKLIST_PATH`, so it removes what your list covers as well
  as the built-in baseline. Adding a pattern to the blocklist doesn't apply it to what's already
  learned; this does.
- **`-compact`** exists because bbolt's file never shrinks. It writes a new file and leaves the
  original alone, so you move it into place yourself and rollback stays trivial.
- **`-purge-author`** removes a user from the author-diversity counts, which decide whether a
  phrase can be generated. Occurrence counts don't record who produced them, so they stay. A
  phrase only that person ever said drops to zero authors and can no longer be generated.

Read-only, against a snapshot rather than a live corpus (it takes a shared lock):

```sh
PEREGRINE_DB_PATH=./snapshot.db go run ./cmd/bot -corpus-report
```

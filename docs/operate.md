# Operate

All of this runs on the host, in `/home/deploy/peregrine`.

## What is running

```sh
cat deployed-tag.env                                    # the commit SHA deployed
docker compose -f docker-compose.prod.yml ps
docker compose -f docker-compose.prod.yml logs -f --tail=100 bot
```

## Roll back

```sh
cp previous-tag.env deployed-tag.env
set -a && . ./deployed-tag.env && set +a
docker compose -f docker-compose.prod.yml up -d
```

This works because images are pinned by SHA and the image prune keeps a week (`until=168h`).

## Stop it talking

Set `PEREGRINE_PAUSE_ALL_WRITES=1` in `.env` and restart. Every outbound message is refused;
reading and learning carry on.

## Back up and restore

Snapshots are taken in-process into `PEREGRINE_BACKUP_DIR` (`./backups` in production; empty
disables them). Don't copy a corpus file while the bot is running: bbolt can be caught mid-write
and the copy will usually look fine.

To restore, stop the bot, replace the file in the `corpus` volume, and start it again.

## Start the corpus over

```sh
docker compose -f docker-compose.prod.yml down     # down, not stop: a stopped container holds its volumes
docker volume rm peregrine_corpus
set -a && . ./deployed-tag.env && set +a           # without this, compose falls back to :latest and can't pull it
docker compose -f docker-compose.prod.yml up -d
```

A corpus in a format the bot doesn't recognise is refused at startup with an error naming this
procedure.

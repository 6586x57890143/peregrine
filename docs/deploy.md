# Deploy

Merging to `main` runs `.github/workflows/ci.yml`: vet, lint, race tests, `govulncheck`, secret
scan, prose check and a Docker build. Then it pushes a multi-arch image to GHCR and deploys it to
the host over SSH with `docker-compose.prod.yml`. A manual deploy of `main` is
`gh workflow run ci.yml --ref main`.

| Repository setting | Purpose |
|---|---|
| `VPS_HOST` (secret) | Deploy target hostname |
| `VPS_SSH_KEY` (secret) | Private key for the `deploy` user |
| `DEPLOY_ENABLED` (variable) | Must be exactly `true` for the deploy steps to run |

A held deploy (`DEPLOY_ENABLED` not `true`) reports green with a notice.

On the host, `/home/deploy/peregrine` holds `.env` (mode `0600`: it holds the token), the
blocklist, and the `backups/` and `tuning/` directories. CI creates the directories and writes
the compose file and the tag files on every deploy. The image is pinned to a commit SHA in
`deployed-tag.env`, with the one it replaced in `previous-tag.env`.

The image is distroless and runs as uid 65532 with a read-only root filesystem. Anything bind
mounted from the host needs permissions for that uid, once:

```sh
cd /home/deploy/peregrine
sudo chgrp 65532 blocklist.txt && chmod 640 blocklist.txt   # read
sudo chgrp 65532 backups       && chmod 775 backups         # written
sudo chgrp 65532 tuning        && chmod 775 tuning          # written
```

A wrong blocklist mode is a restart loop. A wrong `backups/` or `tuning/` mode fails quietly, a
while after startup.

`mem_limit` must stay above the combined corpus size: below it the bot doesn't crash, it gets
slow.

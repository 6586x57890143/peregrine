# Security Policy

## Reporting a vulnerability

Report privately. Do not open a public issue.

Use GitHub's private vulnerability reporting (Security tab, "Report a vulnerability") on this repository. Expect an acknowledgement within a few days.

Please include what you did, what happened, and what you expected. A proof of concept helps. If the report involves content the bot emitted, include the message link or ID rather than pasting the content.

## What is in scope

This bot's threat model assumes users are actively hostile and are trying to make it emit gross or borderline illegal content, because the operator carries the consequences of whatever it posts. `SPEC.md` §4 is the full model. Reports in these areas are especially wanted:

- **Corpus poisoning:** any way to make the bot reliably say something chosen by an attacker. Bypassing the distinct-author eligibility gate is the highest-value finding here, since it is the main control.
- **Normalizer evasion:** input that reaches the learning path or leaves the emit path while carrying blocklisted content. Homoglyphs, combining marks, zero-width characters, spacing and unenumerated leetspeak all belong in this category, and a working evasion is a real finding rather than a curiosity.
- **Gate bypass:** any path that learns or emits without passing `CheckLearn` or `CheckEmit`. A new caller that skips the funnel is the recurring shape of this bug.
- **Unintended mentions or pings**, especially via learned content, since the bot's output is assembled from arbitrary user messages.
- **Republishing:** getting the bot to post attacker-supplied media under its own name.
- Token or credential exposure, container escape, and anything that lets a non-operator run maintenance modes or read the corpus.

## Known and accepted, as of milestone 0

The repository is mid-restructure and some of the model above is specified but not yet implemented. These are tracked, not secret, and are not useful as reports:

- **The safety gate is not in yet (M5).** Moderation is bypassable today: the historical backfill path learns messages the live filter blocked, unfiltered, minutes later (`SPEC.md` §4 A1). The illegal-content pattern list is also still a placeholder (A4).
- **There is no outbound content gate yet (A2).** Anything in the corpus can be emitted verbatim.
- **Nothing suppresses mentions yet (finding 8).** Replies ping the replied-to author.
- **Author-diversity eligibility is not in yet (M7).** Repetition alone still teaches the bot.

Do not deploy this to a hostile channel before M5. `README.md` says the same thing.

## Operator controls

If the bot is actively saying something harmful:

1. Set `PEREGRINE_PAUSE_ALL_WRITES=1` in `.env` on the host and restart the container. This refuses every outbound message process-wide while leaving reads alone.
2. Add the offending pattern to `blocklist.txt` and restart. The blocklist is a mounted file precisely so this needs no rebuild and no deploy.
3. Use `-clean-db` to remove the learned content, and a per-author purge (M6) to remove one actor's contributions without discarding the corpus.

The pause lever currently requires SSH and a restart, which is slow during exactly the incident it exists for. A Discord-reachable equivalent is an open decision (`SPEC.md` §10).

## Secrets

`DISCORD_BOT_TOKEN` grants full control of the bot and is a single string. It lives only in `.env`, which is gitignored, and is injected as an environment variable; it is never baked into an image layer, since a layer is a permanent distributable copy. CI runs `gitleaks` on every push and PR to catch an accidental commit before it reaches a public diff.

If a token is exposed, regenerate it in the Discord Developer Portal immediately. Regenerating invalidates the old one, which is the only reliable remediation; removing the commit is not sufficient.

`blocklist.txt` is gitignored so the repository does not become a searchable copy of a slur list, and so the operator can edit it during an incident without a rebuild and a public diff.

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

## Known and accepted, as of milestone 5

The repository is mid-restructure and some of the model above is specified but not yet implemented. These are tracked, not secret, and are not useful as reports.

**Closed in M5:**

- The backfill bypass (`SPEC.md` §4 A1). `CheckLearn` is inside `learnMessage`, so all four callers and any fifth are covered by construction, and a test parses the package to fail if that call ever moves to the call sites.
- The absent outbound gate (A2). `CheckEmit` runs at the generation exit; a rejection produces silence, not a fallback.
- Laundering on the learning path (A5). The verdict type has no field for rewritten text, so it cannot be expressed.
- Normalizer evasion (A5). Matching happens on a case-folded, NFKD-decomposed form with combining marks and format characters stripped, confusables and leet folded, whitespace collapsed, spaced single-letter runs joined and repeats capped.

**Still open, and worth reporting only if you find a bypass rather than restating these:**

- **`CheckEmit` covers the generation exit, not all thirteen send sites.** M10 moves it into `internal/discordguard` so coverage is structural.
- **Nothing suppresses mentions (finding 8).** Replies ping the replied-to author, and a learned user mention pings that person forever.
- **Author-diversity eligibility is not in (A6, M7).** Repetition alone still teaches the bot, so poisoning is still cheap.
- **The illegal-content pattern content is the operator's (A4).** The mechanism and the alerting path exist; the patterns are deliberately not in this repository, and a deployment with no `illegal` rules is warned about at startup rather than silently accepted.
- **Image reposting is still an unattributed republishing channel (A7, M11).** No per-author cap and no deleted-message rule yet.

Do not deploy this to a hostile channel yet. This is enforced rather than merely advised: the CI deploy steps are gated on a `DEPLOY_ENABLED` repository variable that is deliberately unset, so merges to `main` build and push an image but do not start the bot. The remaining blockers for flipping it are mention suppression (M10) and author diversity (M7).

## Operator controls

If the bot is actively saying something harmful:

1. Set `PEREGRINE_PAUSE_ALL_WRITES=1` in `.env` on the host and restart the container. This refuses every outbound message process-wide while leaving reading and learning alone: during an incident the output is the problem, and stopping ingestion would also stop the bot noticing what is being said to it. It logs the fact at startup, so a silent bot is never a mystery.
2. Add the offending pattern to `blocklist.txt` and restart. The blocklist is a mounted file precisely so this needs no rebuild and no deploy. Write the pattern in its **plain** form: matching happens against a normalized version of the text, so you do not need to enumerate leet spellings, spacing tricks or homoglyphs, and the loader reports every bad line with its line number rather than failing on the first.
3. Use `-clean-db` to remove the learned content, and a per-author purge (M6) to remove one actor's contributions without discarding the corpus.

A pattern added in step 2 takes effect on **both** directions: it stops the phrase being learned again and stops it being emitted. Adding it does not remove what is already in the corpus, which is what step 3 is for.

The pause lever currently requires SSH and a restart, which is slow during exactly the incident it exists for. A Discord-reachable equivalent is an open decision (`SPEC.md` §10).

## Secrets

`DISCORD_BOT_TOKEN` grants full control of the bot and is a single string. It lives only in `.env`, which is gitignored, and is injected as an environment variable; it is never baked into an image layer, since a layer is a permanent distributable copy. CI runs `gitleaks` on every push and PR to catch an accidental commit before it reaches a public diff.

If a token is exposed, regenerate it in the Discord Developer Portal immediately. Regenerating invalidates the old one, which is the only reliable remediation; removing the commit is not sufficient.

`blocklist.txt` is gitignored so the repository does not become a searchable copy of a slur list, and so the operator can edit it during an incident without a rebuild and a public diff.

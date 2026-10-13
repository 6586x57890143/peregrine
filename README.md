# peregrine

A Markov-chain Discord bot in Go. It learns from the channels it can see and replies in
the server's own voice, badly and on purpose. Built on
[discordgo](https://github.com/bwmarrin/discordgo) and [bbolt](https://github.com/etcd-io/bbolt).

- Learns one corpus per server, and won't say a phrase until at least two different
  people have said it.
- Two safety gates: one on what it learns, and one on everything it sends.
- Nothing it sends can ping anyone.
- Word games, a wheel and a weekly leaderboard, all as slash commands.
- Writes its own invite permissions and Message Content intent to Discord at boot.

Add it to a server from **https://peregrine.skua.lol**.

[`SPEC.md`](SPEC.md) is the design.

## Docs

- [Run](docs/run.md) it locally, and configure it
- [Invite](docs/invite.md): the link, the intents and the permissions
- [Maintain the corpus](docs/maintenance.md): clean, compact and purge
- [Deploy](docs/deploy.md): what a merge to `main` does
- [Operate](docs/operate.md): rollback, pause, backups and starting over
- [Tune](docs/tuning.md) generation from what it actually said

[Contributing](CONTRIBUTING.md) · [Security](SECURITY.md)

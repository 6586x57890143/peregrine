# Invite

Add peregrine to a server from **https://peregrine.skua.lol**. That is `web/`, a Cloudflare
Worker that redirects to Discord's install link for application `1436415016965509181`. Deploy it
with `cd web && wrangler deploy`, only when `web/` changes.

What that link asks for is written by peregrine itself at every boot, from `core.Permissions` in
`internal/core/install.go`. A new permission reaches the link on the deploy that ships it.

The same step switches on the **Message Content** intent, using the `_LIMITED` flag Discord lets
an app in fewer than 100 servers set for itself. Without that intent Discord refuses the
connection. A verified app can't grant it to itself: tick "Message Content Intent" under Bot in
the Developer Portal. If it's missing, the startup error names that checkbox. The other intents,
`GUILDS`, `GUILD_MESSAGES` and `GUILD_MESSAGE_REACTIONS`, aren't privileged.

Scopes: `bot` and `applications.commands`. Permissions: View Channels, Read Message History,
Send Messages, Send Messages in Threads, Embed Links, Add Reactions, and Manage Messages (to clean
up its own word-game messages).

The long-form link, for when the Worker is down:

```
https://discord.com/oauth2/authorize?client_id=1436415016965509181&scope=bot+applications.commands&permissions=274878000192
```

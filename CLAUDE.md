# CLAUDE.md — operating instructions for kubetunnel

Guidance for an AI assistant working in this repo. Read this before installing,
updating, or restarting kubetunnel.

## When the user asks to "update" / "install" / "reinstall" kubetunnel

Act immediately. Do not over-explain first. Pick the scenario and run the single
command. Verify with `tunnelctl status` afterward.

### Scenario A — code changed (no new/renamed tunnel hostname). The common case.

Run, from the repo root:

```
./scripts/update.sh
```

That rebuilds both binaries, installs them to `/usr/local/bin`, and restarts the
daemon with `launchctl kickstart -k system/dev.kubetunnel` (in-place restart; the
daemon re-reads config and re-issues certs on boot). This is the canonical update
path.

### Scenario B — config change that adds or renames a tunnel hostname.

`update.sh` does NOT rewrite `/etc/hosts`. After it, also run:

```
sudo tunnelctl install
```

This rewrites the `/etc/hosts` block from the current config and issues the cert
for the new hostname.

## Making it fully automatable (one-time, do this if not done)

By default the `sudo` steps prompt for a password, and an AI session has no TTY to
type it, so the command fails. Install the sudoers snippet ONCE:

```
./scripts/install-sudoers.sh
```

It writes a `visudo`-validated `/etc/sudoers.d/kubetunnel-dev` whitelisting exactly
the commands `update.sh` needs (the two binary `install`s plus
`launchctl kickstart/bootout/bootstrap` for `system/dev.kubetunnel`). After this,
`./scripts/update.sh` runs end-to-end with no password, so the AI can run it
directly with no human in the loop.

Check whether it is already installed: `ls /etc/sudoers.d/kubetunnel-dev`.

## If you are blocked by sudo (no sudoers snippet, no TTY)

Do not retry sudo in a loop. Hand the user exactly ONE command to paste at the
prompt (the `!` prefix runs it in-session so the output lands in the conversation):

```
! ./scripts/update.sh
```

Then continue once it returns.

## Critical gotcha — do NOT use `tunnelctl install` for code updates

For code/logic changes use `update.sh` (which uses `kickstart -k`). Do NOT use
`tunnelctl install` to apply a code change: `install` does `bootout` + `bootstrap`,
and if the CURRENTLY INSTALLED `tunnelctl` predates the bootstrap-retry fix
(`63e6b10`), the async bootout race leaves the daemon dead (no socket, no process,
`Could not find service "dev.kubetunnel"`). Recovery is:

```
sudo launchctl bootstrap system /Library/LaunchDaemons/dev.kubetunnel.plist
```

`tunnelctl install` is only for first install or hostname changes (Scenario B), and
only after the installed binary has the retry fix.

## Knowing the installed "version" (git commit)

There is no explicit version (no `--version` flag, no `version` subcommand, no
ldflags). But Go embeds VCS info at build time, so the installed commit is
recoverable. The two binaries — `kubetunneld` (daemon) and `tunnelctl` (CLI
client, talks to the daemon over `/var/run/kubetunnel.sock`) — are built together
by `update.sh` from the same module, so they should always share a commit; nothing
enforces it, and they can drift if installed separately (that is what broke things
once: new daemon, old `tunnelctl`).

Read each binary's commit:

```
go version -m /usr/local/bin/kubetunneld | awk '/vcs.revision/{print $2}'
go version -m /usr/local/bin/tunnelctl   | awk '/vcs.revision/{print $2}'
```

Also useful from `go version -m`: `vcs.time` (commit date) and `vcs.modified`
(`true` means a dirty build with uncommitted changes). Compare both binaries and
the working tree (`git rev-parse HEAD`) to confirm everything is in sync.

## Verify after any of the above

```
tunnelctl status
```

All tunnels should be `Running` / `OK`. If a tunnel is `FAIL`/`Starting` in a loop,
read its logs: `tunnelctl logs --tail 200 | grep <tunnel-name>`. A common cause is
missing `pods/portforward` RBAC/IAM permission in the target namespace — check with
`kubectl auth can-i create pods/portforward -n <namespace>`.

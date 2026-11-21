---
title: Deployment
nav_order: 10
---

# Deployment
{: .no_toc }

Running the Forgejo service on Linux with systemd, using the script and units
in `examples/systemd/`.

1. TOC
{:toc}

## Layout

| | |
|---|---|
| Binary | `/usr/local/bin/murasame` |
| User | `murasame`, home `/var/lib/murasame` (0700): clones, worktrees, logs, DB |
| Config | `/etc/murasame/murasame.toml` (`root:murasame`, 0640) |
| Token | `/etc/murasame/env` (`root:murasame`, 0640) |
| Units | `murasame.service`, plus `murasame-proxy.socket`/`.service` if the status page is exposed |
| Logs | `journalctl -u murasame` |

## Prerequisites

On the host: Linux with systemd, `git`, `curl`, HTTP access to Forgejo, and the
tools your gates need installed system-wide. The unit sets `ProtectHome=yes`,
so toolchains under `/home` are invisible to gates.

Build the binary anywhere:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o murasame ./cmd/murasame
scp murasame examples/systemd/install.sh you@host:/tmp/
```

In Forgejo, as an admin on the Forgejo host (running as Forgejo's system user):

```sh
forgejo admin user create --username murasame --email murasame@localhost \
  --random-password --must-change-password=false
forgejo admin user generate-access-token --username murasame --token-name murasame --raw \
  --scopes write:repository,write:issue,read:user
```

Without `--must-change-password=false`, every API call from the bot returns
403. For an existing user, run
`forgejo admin user must-change-password --unset murasame`. Save the token;
it's shown once.

Then give `murasame` write access to each repository, and make sure the target
branch exists.

## Install

```sh
sudo FORGEJO_URL=http://forge.example:3000 /tmp/install.sh /tmp/murasame
```

The script creates the user with a git identity, installs the binary, and on
the first run asks for the repository, the approvers and the token. It writes
the config and opens it in `$EDITOR` so you can set the gate:

```toml
[repos.gates.check]
command = ["sh", "-c", "go vet ./... && go build ./... || exit 1"]
```

It then starts the units and checks `/healthz`. Rerunning it with a new binary
upgrades and keeps the config and token.

Afterwards, protect the target branch in each repository: restrict push and
merge to `murasame` only, and leave required approvals at 0 (approval is the
`approve` comment). Test with `@murasame ping` on a PR.

## Status page

The status page has no login and only answers same-host requests for a
loopback name or the `external_url` host. To view it remotely, tunnel:

```sh
ssh -N -L 8737:127.0.0.1:8737 you@host     # then open http://localhost:8737
```

Or have the script add a `systemd-socket-proxyd` proxy:

```sh
sudo FORGEJO_URL=http://forge.example:3000 \
  STATUS_ADDR=192.0.2.10:8737 STATUS_URL=http://forge.example:8737 \
  /tmp/install.sh /tmp/murasame
```

`STATUS_ADDR` is where the proxy listens. Use a private (LAN or VPN) address,
since anyone who reaches it can read the queue and logs. `STATUS_URL` becomes
`external_url`; open the page by that host name, as other names get 403. On an
existing install, set `listen` and `external_url` in the config yourself.

## Isolation

Without `[repo] command_prefix`, gates, hooks and conflict commands run as the
`murasame` user. The token is kept out of their environment, but they can still
read the token file. `check-config` and `serve` warn about this.

That's acceptable for code you trust. Otherwise run commands as another user or
in a sandbox:

```toml
[repo]
command_prefix = ["sudo", "-u", "murasame-ci", "--"]
```

The prefix can use `{repo}`, `{state_dir}`, `{integ}` and `{target}`, and it's
part of the gate config that pass records are keyed on.

## Uninstall

```sh
sudo systemctl disable --now murasame murasame-proxy.socket
sudo rm /etc/systemd/system/murasame{.service,-proxy.socket,-proxy.service}
sudo rm -r /etc/murasame /usr/local/bin/murasame
sudo userdel -r murasame          # also deletes /var/lib/murasame
```

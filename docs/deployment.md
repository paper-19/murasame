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

On the host: Linux with systemd, `git`, `curl` and HTTP access to Forgejo. No
build tools: the gate runs in Forgejo Actions, on your runners. Each repository
needs a [workflow on the staging branch](gates.html#the-workflow).

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

## A Forgejo Actions runner

Tested with `tests/e2e/forgejo.sh` against Forgejo 15 and 16 with runner 9.
Forgejo 14 passes everything except cancelling a superseded run: there,
`concurrency: cancel-in-progress` did not cancel it.

The gate needs Actions enabled on the Forgejo instance (`[actions] ENABLED =
true` in `app.ini`; on by default in recent versions) and at least one runner
that picks up the workflow's `runs-on` label. Without one, every batch stops
with "no CI status appeared".

A runner in Docker, registered with a token from the Forgejo host:

```sh
# On the Forgejo host, as Forgejo's user:
forgejo actions generate-runner-token

# On the runner host:
forgejo-runner register --no-interactive --instance https://forge.example.com \
  --token <token> --name runner-1 --labels docker:docker://node:20-bookworm
forgejo-runner daemon
```

`docker:docker://node:20-bookworm` runs each job in a fresh container; a label
like `host:host` runs jobs directly on the runner host instead. Install the
tools the workflow needs in the image or with setup actions
(`actions/setup-go` and the like). The runner can be on another machine: it
only needs to reach Forgejo over HTTP. Runner 9 does not enforce `timeout-minutes`: limit
long steps yourself (see [Verdicts](gates.html#verdicts)).

## Install

```sh
sudo FORGEJO_URL=http://forge.example:3000 /tmp/install.sh /tmp/murasame
```

The script creates the user with a git identity, installs the binary, and on
the first run asks for the repository, the approvers and the token. It writes
the config and opens it in `$EDITOR` so you can set the CI statuses that must
pass:

```toml
[repos.ci]
contexts = ["ci / check (push)"]
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

murasame runs no repository code: no gates, hooks or conflict commands. It
holds the bot's token and talks to git and the forge API. The code runs on
your Actions runners, which isolate jobs as you configured them.

A push to the staging branch runs the workflow with the repository's Actions
secrets. Only approved PRs reach it, so a reviewer's approval is what lets a
PR's code run with them.

## Uninstall

```sh
sudo systemctl disable --now murasame murasame-proxy.socket
sudo rm /etc/systemd/system/murasame{.service,-proxy.socket,-proxy.service}
sudo rm -r /etc/murasame /usr/local/bin/murasame
sudo userdel -r murasame          # also deletes /var/lib/murasame
```

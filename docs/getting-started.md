---
title: Getting started
nav_order: 2
---

# Getting started

## Install

Building needs Go 1.24 or later. Running needs `git`.

```sh
git clone https://github.com/paper-19/murasame
cd murasame
go build -o murasame ./cmd/murasame
```

The SQLite driver is pure Go, so cross-compiling works without cgo:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o murasame ./cmd/murasame
```

In a checkout you can also use `bin/murasame`. It rebuilds into
`.murasame/bin/` on every call and loads `.env` if present (copy
`.env.example`; the file is sourced as bash).

## Add a workflow

murasame pushes each merged result to a staging branch and waits for the
commit statuses Forgejo Actions reports on it. Add a workflow that runs on
pushes to that branch, `.forgejo/workflows/ci.yml`:

{% raw %}
```yaml
name: ci
on:
  push:
    branches: [murasame/staging]
  pull_request:
concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true
jobs:
  check:
    runs-on: docker
    steps:
      - uses: actions/checkout@v4
      - run: timeout 30m make check
```
{% endraw %}

Its job reports the status `ci / check (push)`: `<workflow> / <job> (<event>)`.

## Write a config

Pass the config with `--config FILE` or `MURASAME_CONFIG`:

```toml
[repo]
target = ["main"]

[ci]
contexts = ["ci / check (push)"]   # the statuses that must pass

[records]
enabled = true                     # skip trees that already passed

[forgejo]
url = "https://forge.example.com"
owner = "team"
repo = "app"
git_remote_url = "https://forge.example.com/team/app.git"
reviewers_from_permission = "write"
```

Check how murasame reads it:

```console
$ export MURASAME_CONFIG=murasame.toml
$ murasame check-config
config murasame.toml: ok
  target: main; integ: {state_dir}/integ; state: .murasame; state DB: murasame.sqlite3
  CI: pushes to murasame/staging, then waits for ci / check (push) (timeout 7200 s; no status after 900 s: infrastructure failure; checked every 15 s)
  pair check: on
  records: a tree that passed CI is not sent to CI again for 172800 s
  forgejo: https://forge.example.com team/app as murasame
  approvals: `@murasame approve <full sha of the head>` comments by anyone with write permission
```

With the token set, `murasame check-config --forge` also checks the
repository: Actions on, the bot's permission, and the workflow.

## Run it

```sh
export MURASAME_FORGEJO_TOKEN=...   # the bot user's token
murasame serve --repo /var/lib/murasame/app.git
```

`--repo` is the service's own bare clone; it is created on the first cycle.
Then comment `@murasame approve <sha>` on a pull request. See
[Forgejo service](forgejo.html) for the commands and
[Deployment](deployment.html) for running it under systemd.

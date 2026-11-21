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

## Write a config

Pass the config with `--config FILE` or `MURASAME_CONFIG`. The only required
table is a gate:

```toml
[repo]
target = ["main"]
create_integ = true     # create the integration worktree on first run
batch = true

[gates.check]
command = ["sh", "-c", "make check || exit 1"]

[records]
enabled = true          # skip trees that already passed
```

Commands are argument lists. For a shell line, use `["sh", "-c", "..."]`.

{: .warning }
The gate must exit 1 when tests fail. Any other non-zero code counts as an
infrastructure failure: murasame retries it, then stops without blaming a
branch. `make` exits 2 on failure, hence `|| exit 1`. See
[exit codes](gates.html#exit-codes).

Check how murasame reads it:

```console
$ export MURASAME_CONFIG=murasame.toml
$ murasame check-config
config murasame.toml: ok
  target: main; integ: {state_dir}/integ; state: .murasame; state DB: murasame.sqlite3
  gates: check (timeout 7200 s, retries 1 30 s apart); default check
  pair check: on
  project commands run as this user (no command_prefix: they can read this process's secrets)
  records: a tree that passed a gate is not gated again for 172800 s
```

State goes in `.murasame/` by default. Keep it out of git:

```sh
echo .murasame >> .git/info/exclude
```

## Run it

```sh
murasame run --dry-run feature-a          # merge in a throwaway worktree, report only
murasame run feature-a feature-b          # batch or not, per [repo].batch
murasame run --one-by-one feature-a feature-b
murasame history
```

Branches merge in the order given. `run` exits 0 if everything landed and 1 if
anything was set aside or the run stopped.

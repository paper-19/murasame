# murasame

A merge queue: batches, bisection, one gate.

murasame lands branches on a target branch only after the merged result passes
a **gate** (any command you choose, such as `make check`). It can test many
branches together in one batch and, when a batch fails, bisect it to find the
branch at fault. It runs either as a local CLI or as a bot for
[Forgejo](https://forgejo.org) (and Gitea-compatible) repositories, driven by
pull-request comments.

## Features

- **Batching and bisection.** Gate all queued branches at once; on a failure,
  split the batch to isolate the culprit. Or go `--one-by-one`.
- **Configurable gates.** Several named gates per repository, each with its own
  command, timeout, retries and environment.
- **Pass records.** A tree that already passed a gate isn't gated again (for a
  configurable time).
- **Automatic conflict resolution** for selected paths: `keep-both`, `ours`,
  `theirs`, or regenerate the file with a `command`.
- **Hooks.** `post_merge` hooks run on each merge in the integration branch;
  `post_land` hooks run after the target moves.
- **Forgejo service.** Webhooks plus polling, commit statuses, comment commands,
  priorities, rollup control, and an HTML/JSON status page.
- **Multi-repo instances** with a shared SQLite state database and a limit on
  concurrent gates.

## Install

Requires Go 1.24+.

```sh
go build -o murasame ./cmd/murasame
```

Or use the wrapper `bin/murasame`, which rebuilds into `.murasame/bin/` and runs
the result on every call.

To run the Forgejo service on a server, see
[`examples/systemd/`](examples/systemd/README.md): prerequisites, an install
script, and the systemd units.

## Usage

```
murasame [-h] [--version] {run,serve,tick,check-config,history} ...
```

| Command | What it does |
|---|---|
| `run BRANCH...` | Merge the branches, in order, through the gate |
| `serve` | Run the Forgejo service (`--once` for one cycle) |
| `tick` | Run one Forgejo service cycle |
| `check-config` | Load the config and print what it means |
| `history` | Recent merges and batches (read-only) |

Every command takes `--config FILE` (default `$MURASAME_CONFIG`) and
`--repo DIR` (any checkout or worktree; default `.`). Run
`murasame COMMAND -h` for the full option list.

### Local runs

```sh
export MURASAME_CONFIG=examples/generic.toml

murasame check-config
murasame run feature-a feature-b            # batch or not, per [repo].batch
murasame run --one-by-one feature-a feature-b
murasame run --dry-run feature-a            # merge in a throwaway worktree; report only
murasame run --gate build --regate feature-a
murasame run --skip-hook toc feature-a
```

`--batch` gates all the branches together and bisects on a failure.
`--one-by-one` gates each branch separately and stops at the first problem.

### History

```sh
murasame history                    # last 20 runs
murasame history --branch feature-a
murasame history --pr 42            # also the PR's state changes
murasame history --log              # merges.log lines instead of runs
murasame history --json --limit 100
murasame history --repos            # repositories in the state DB
```

## Configuration

The config is TOML. See [`examples/generic.toml`](examples/generic.toml) for a
single repository and [`examples/multi.toml`](examples/multi.toml) for a
multi-repo instance.

```toml
[repo]
target = ["main", "master"]       # the target branch: main or master
state_dir = "{repo}/.murasame"
integ = "{state_dir}/integ"       # the integration branch
create_integ = true
batch = true

[merge]
no_ff = true

[gates.check]
command = ["sh", "-c", "${CHECK_COMMAND:-make check}"]
timeout = 3600
retries = 1
retry_wait = 30

[[conflicts]]
label = "docs"
paths = ["CHANGELOG.md", "docs/*.md"]
strategy = "keep-both"            # keep-both | ours | theirs | command

[[hooks.post_merge]]
name = "toc"
run = ["make", "toc"]
timeout = 300

[records]
enabled = true
```

**Substitution.** `${VAR}` and `${VAR:-default}` read the environment when the
config is loaded. `{name}` placeholders are filled in at run time in some
fields. Which ones are available depends on the field:

| Where | Placeholders |
|---|---|
| everywhere | `{repo}`, `{state_dir}`, `{integ}`, `{target}` |
| gate commands | plus `{gate}`, `{retried}` |
| post-merge hooks | plus `{branch}`, `{hook}` |
| conflict commands | plus `{branch}` |
| post-land hooks | plus `{hook}`, `{target_sha}`, `{target_short}` |

Write `{{` and `}}` for literal braces.

**Isolation.** Gate, hook and conflict commands run as the murasame user unless
you set `[repo].command_prefix` (for example, a `sudo -u` or sandbox wrapper).
Without a prefix they can read the process's secrets; `check-config` warns you
about this.

## Forgejo service

Add a `[forgejo]` table (single repo) or an `[instance]` with `[forges.*]` and
`[[repos]]` (multi-repo), then run `murasame serve`.

```toml
[forgejo]
url = "https://forge.example.com"
owner = "team"
repo = "app"
bot_user = "murasame"
reviewers_from_permission = "write"   # or list them in reviewers = [...]
batch_max = 8
poll_interval = 60
```

Environment:

| Variable | Purpose |
|---|---|
| `MURASAME_CONFIG` | Default config path |
| `MURASAME_FORGEJO_TOKEN` | API token for the bot user (`token_env` to rename) |
| `MURASAME_WEBHOOK_SECRET` | Webhook HMAC secret (`webhook_secret_env` to rename) |

### Comment commands

Reviewers write `@murasame <command>` on a pull request:

| Command | Effect |
|---|---|
| `approve [SHA]` | Queue the PR (optionally only at this head) |
| `unapprove` | Take it out of the queue |
| `retry` | Queue it again after a failure |
| `cancel` | Stop a running test |
| `priority N` | Set queue priority (−9999…9999) |
| `rollup` / `rollup never` | Allow, or forbid, batching it with others |
| `status` | Report the PR's state |
| `ping` | Check that the bot is listening |

All of these except `status` and `ping` need reviewer permission. The bot
suggests a fix for a misspelled command.

PR states: `open` → `queued` → `testing` → `merged`, with `set_aside`, `held`
and `closed` along the way.

### HTTP endpoints

The server listens on `127.0.0.1:8737` by default (`listen`).

| Route | Purpose |
|---|---|
| `POST /webhook` | Forgejo/Gitea webhook (signature checked when a secret is set) |
| `GET /` | Status page (HTML) |
| `GET /api/status` | Status (JSON) |
| `GET /repos/NAME/logs/LOG` | A gate log |
| `GET /healthz` | Liveness |

The status page only answers on a loopback host name or the host of
`external_url`.

## Project layout

```
cmd/murasame/        entry point
internal/cli/        argument parsing, run/history commands
internal/engine/     merge, batch and bisection logic
internal/gate/       running gate commands and their logs
internal/conflicts/  automatic conflict resolution
internal/hooks/      post-merge and post-land hooks
internal/gitx/       git plumbing
internal/states/     state machines (PRs, batches, slots, backoff)
internal/service/    Forgejo queue service
internal/server/     HTTP server, webhooks, status page
internal/instance/   multi-repo instance config
internal/forgejo/    Forgejo API client
internal/commands/   comment command parsing
internal/config/     TOML config loading and validation
internal/db/         SQLite state database
internal/store/      locks, records, on-disk state
```

## License

MIT. See [LICENSE](LICENSE).

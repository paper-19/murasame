# murasame

A merge queue for Forgejo: batches, bisection, Forgejo Actions as the gate.

murasame lands pull requests on a target branch only after the merged result
passes CI. It merges the approved PRs onto the target, pushes the result to a
staging branch, and waits for the commit statuses your Forgejo Actions workflow
reports on it. If CI passes, it pushes that exact commit to the target. If CI
fails, it bisects the batch to find the PR at fault. It runs as a bot for
[Forgejo](https://forgejo.org) (and Gitea-compatible) repositories, driven by
pull-request comments.

murasame only queues: it runs no project commands. Your runners do the
building and testing, with the toolchains, caches and secrets they already
have.

## Features

- **Batching and bisection.** Gate all queued PRs at once; on a failure, split
  the batch to isolate the culprit.
- **Forgejo Actions as the gate.** Each merged result is pushed to a staging
  branch; the required commit statuses decide.
- **Pass records.** A tree that already passed CI isn't sent to CI again (for a
  configurable time).
- **Automatic conflict resolution** for selected paths: `keep-both`, `ours` or
  `theirs`.
- **Forgejo service.** Webhooks plus polling, commit statuses, comment commands,
  priorities, rollup control, and an HTML/JSON status page.
- **Multi-repo instances** with a shared SQLite state database.

## Install

Requires Go 1.24+.

```sh
go build -o murasame ./cmd/murasame
```

Or use the wrapper `bin/murasame`, which rebuilds into `.murasame/bin/` and runs
the result on every call. It also loads `.env` from the checkout if present:
copy [`.env.example`](.env.example) to `.env` for local settings and secrets.

To run the service on a server, see
[`examples/systemd/`](examples/systemd/README.md): prerequisites, an install
script, and the systemd units.

## Usage

```
murasame [-h] [--version] {serve,tick,check-config,history} ...
```

| Command | What it does |
|---|---|
| `serve` | Run the Forgejo service (`--once` for one cycle) |
| `tick` | Run one Forgejo service cycle |
| `check-config` | Load the config and print what it means (`--forge`: also check the repository, token and workflow on the forge) |
| `history` | Recent merges and batches (read-only) |

Every command takes `--config FILE` (default `$MURASAME_CONFIG`) and
`--repo DIR` (the service's bare clone; default `.`). Run
`murasame COMMAND -h` for the full option list.

```sh
murasame history                    # last 20 runs
murasame history --pr 42            # also the PR's state changes
murasame history --log              # merges.log lines instead of runs
murasame history --json --limit 100
```

## Setting up CI

1. Add a workflow that runs on pushes to the staging branch. See
   [`examples/forgejo-workflow.yml`](examples/forgejo-workflow.yml):

   ```yaml
   on:
     push:
       branches: [murasame/staging]
   ```

2. List the jobs that must pass in `[ci] contexts`. Forgejo names each job's
   status `<workflow> / <job> (push)`, for example `ci / check (push)`. With
   no contexts, every job of the run must finish without failing.
3. Let the bot user push to the staging branch (it needs write access to the
   repository anyway), and keep the staging branch unprotected.

A push to the staging branch runs with the repository's Actions secrets. Only
approved PRs get there, so a reviewer's approval is what lets code run with
those secrets.

## Configuration

The config is TOML. See [`examples/generic.toml`](examples/generic.toml) for a
single repository and [`examples/multi.toml`](examples/multi.toml) for a
multi-repo instance.

```toml
[repo]
target = ["main", "master"]       # the target branch: main or master
state_dir = "{repo}/.murasame"
integ = "{state_dir}/integ"       # the integration worktree

[merge]
no_ff = true

[ci]
staging = "murasame/staging"      # the branch murasame pushes merged results to
contexts = ["ci / check (push)"]  # the statuses that must pass ([]: all of them)
timeout = 3600                    # no result after this: infrastructure failure
start_timeout = 900               # no status at all after this: infrastructure failure
poll_interval = 15

[[conflicts]]
label = "docs"
paths = ["CHANGELOG.md", "docs/*.md"]
strategy = "keep-both"            # keep-both | ours | theirs | stop

[records]
enabled = true

[forgejo]
url = "https://forge.example.com"
owner = "team"
repo = "app"
git_remote_url = "https://forge.example.com/team/app.git"
bot_user = "murasame"
reviewers_from_permission = "write"   # or list them in reviewers = [...]
batch_max = 8
poll_interval = 60
```

Only statuses of a run started by murasame's push count. A `failure` or
`error` fails the gate and starts bisection; a required job that was skipped
never passes it. A cancelled job, a run that never reports, statuses that
can't be read or a refused push are infrastructure failures: the batch goes
back in the queue and the queue pauses for `infra_backoff` seconds.
[`docs/gates.md`](docs/gates.md) has every case.

Environment:

| Variable | Purpose |
|---|---|
| `MURASAME_CONFIG` | Default config path |
| `MURASAME_FORGEJO_TOKEN` | API token for the bot user (`token_env` to rename) |
| `MURASAME_WEBHOOK_SECRET` | Webhook HMAC secret (`webhook_secret_env` to rename) |

## Comment commands

Reviewers write `@murasame <command>` on a pull request:

| Command | Effect |
|---|---|
| `approve SHA` | Queue the PR at this head (the full 40-character SHA) |
| `unapprove` | Take it out of the queue |
| `retry` | Queue it again after a failure |
| `cancel` | Stop a running test |
| `priority N` | Set queue priority (−9999…9999) |
| `rollup` / `rollup never` | Allow, or forbid, batching it with others |
| `status` | Report the PR's state (with a link to the CI run while it runs) |
| `ping` | Check that the bot is listening |

All of these except `status` and `ping` need reviewer permission.

PR states: `open` → `queued` → `testing` → `merged`, with `set_aside`, `held`
and `closed` along the way.

## HTTP endpoints

The server listens on `127.0.0.1:8737` by default (`listen`).

| Route | Purpose |
|---|---|
| `POST /webhook` | Forgejo/Gitea webhook (signature checked when a secret is set) |
| `GET /` | Status page (HTML) |
| `GET /api/status` | Status (JSON) |
| `GET /repos/NAME/logs/LOG` | A gate log (the CI statuses murasame saw, with links to the runs) |
| `GET /healthz` | Liveness |

## Project layout

```
cmd/murasame/        entry point
internal/cli/        argument parsing, check-config and history
internal/engine/     merge, batch and bisection logic
internal/gate/       pushing to the staging branch and reading CI statuses
internal/conflicts/  automatic conflict resolution
internal/gitx/       git plumbing
internal/states/     state machines (PRs, batches, backoff)
internal/service/    Forgejo queue service
internal/server/     HTTP server, webhooks, status page
internal/instance/   multi-repo instance config
internal/forgejo/    Forgejo API client
internal/commands/   comment command parsing
internal/config/     TOML config loading and validation
internal/db/         SQLite state database
internal/store/      records, on-disk state
tests/               tests: a fake forge and fake CI around a real git repository
```

Run the tests with `go test ./tests/`. `tests/e2e/forgejo.sh` runs murasame
against a real Forgejo 15 and Actions runner in Docker (about two minutes).

## License

MIT. See [LICENSE](LICENSE).

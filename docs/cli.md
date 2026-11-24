---
title: Command line
nav_order: 4
---

# Command line
{: .no_toc }

```text
murasame [-h] [--version] {serve,tick,check-config,history} ...
```

1. TOC
{:toc}

## Common options

| Option | Meaning |
|---|---|
| `--config FILE` | The config. Default `$MURASAME_CONFIG`; one of them is required. |
| `--repo DIR` | The service's bare clone. Default `.`. Ignored with a multi-repo config. |
| `-h` | Help. |

| Exit code | Meaning |
|---|---|
| 0 | Success. |
| 1 | A service cycle failed, `tick` or `serve --once` was stopped by a signal, `serve` stopped at once on a second signal, or `serve` could not listen. |
| 2 | Usage or config error, a service that could not be set up, or a state DB that `history` could not read; nothing ran. |

## serve and tick

`serve` runs the [Forgejo service](forgejo.html). `serve --once` and `tick` both
run a single cycle and exit, which suits cron or testing a config.

The first SIGINT or SIGTERM stops a batch that is waiting on CI and puts its
PRs back in the queue. A second one exits at once; the CI run on the staging
branch may still finish, but nothing lands.

## check-config

Validates the config and prints what it means: target, CI, conflict rules,
records, Forgejo settings and who can approve. Errors name the key.

`check-config --forge` also asks the forge, with the bot's token, and exits 1
on a problem that would stop the queue:

```text
  forge ok   the bot's token reads team/app
  forge ok   @murasame has write permission
  forge ok   .forgejo/workflows/ci.yml mention(s) murasame/staging
  forge ok   the jobs on murasame/staging (5242465e0715) are not stuck waiting for a runner
  forge skip the branch protection rules need admin access to read: whether one covers murasame/staging is not checked
```

It checks that Actions are enabled, that the bot can write, that a workflow on
the target branch mentions the staging branch, and that each context in
`ci.contexts` names an existing workflow. It warns when the jobs on the staging
branch have waited more than 10 minutes to start, which usually means no
runner with the workflow's `runs-on` label is online.

## history

Read-only view of past runs from the state database.

| Option | Meaning |
|---|---|
| `--branch NAME` | Only this branch (for a PR, its branch or `#N`). |
| `--pr N` | Only this pull request, plus its state changes. |
| `--limit N` | Default 20. |
| `--log` | Show `merges.log` lines instead of runs. |
| `--json` | JSON output. |
| `--repo-key KEY` | Which repository in a shared DB (`all` for every one). |
| `--repos` | List the repositories in the DB. |

## Environment

| Variable | Purpose |
|---|---|
| `MURASAME_CONFIG` | Default config path. |
| `MURASAME_FORGEJO_TOKEN` | Bot API token (rename with `token_env`). |
| `MURASAME_WEBHOOK_SECRET` | Webhook HMAC secret (rename with `webhook_secret_env`). |

Any variable can be used in the config as `${VAR}`. See
[Substitution](configuration.html#substitution).

---
title: Command line
nav_order: 4
---

# Command line
{: .no_toc }

```text
murasame [-h] [--version] {run,serve,tick,check-config,history} ...
```

1. TOC
{:toc}

## Common options

| Option | Meaning |
|---|---|
| `--config FILE` | The config. Default `$MURASAME_CONFIG`; one of them is required. |
| `--repo DIR` | Any checkout or worktree of the repository (the bare clone, for the service). Default `.`. Ignored with a multi-repo config. |
| `-h` | Help. With a config, `run -h` lists the gate names. |

Options accept unambiguous prefixes (`--one` for `--one-by-one`) and both
`--gate=build` and `--gate build`.

| Exit code | Meaning |
|---|---|
| 0 | Success. For `run`: everything landed or was already in the target. |
| 1 | `run` set something aside, stopped, timed out on the lock, or hit an error. |
| 2 | Usage or config error; nothing ran. |

## run

```text
murasame run [--batch | --one-by-one] [--dry-run | --no-land] [--regate]
             [--gate NAME] [--skip-hook NAME]... BRANCH [BRANCH ...]
```

| Option | Meaning |
|---|---|
| `BRANCH` | Branches in merge order: names or full refs (`refs/remotes/origin/x`). |
| `--batch` | One gate for all branches, bisected on failure. |
| `--one-by-one` | One gate per branch; the first problem stops the run. |
| `--dry-run` | Merge in a throwaway worktree and report. No gate, no landing. |
| `--no-land` | Gate one branch and record the pass without landing. See [Gating ahead](gates.html#gating-ahead---no-land). |
| `--regate` | Gate even if a pass record exists. |
| `--gate NAME` | Use this gate instead of the default. |
| `--skip-hook NAME` | Skip this post-land hook. Repeatable. |

Without `--batch` or `--one-by-one`, `[repo] batch` decides. Duplicate branches
merge once. `run` needs a project config; for one repository of a multi-repo
instance, use the file its `[[repos]]` entry includes.

The first Ctrl-C (or SIGTERM) stops the running gate, resets `integ` and exits.
A second one exits at once and may leave the gate running.

## check-config

Validates the config and prints what it means: target, gates, conflict rules,
hooks, records, Forgejo settings and who can approve. Errors name the key:

```text
murasame: gates.check.command: a list of strings like ["make", "check"], not the string "make check" (for a shell line write ["sh", "-c", "make check"])
```

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

```console
$ murasame history --limit 1
run 1 (cli, batch) 2026-11-21 09:14:45 .. 09:14:47: main c3d110f28b7b -> 9f8da37c26fb, 3 gate run(s), exit 1
  merged       feat-a
  merged       feat-b
  set_aside    feat-bad: the gate failed (check; log: .murasame/20261121-091446-feat-bad-alone.gate.log)
```

## serve and tick

`serve` runs the [Forgejo service](forgejo.html). `serve --once` and `tick` both
run a single cycle and exit, which suits cron or testing a config.

## Environment

| Variable | Purpose |
|---|---|
| `MURASAME_CONFIG` | Default config path. |
| `MURASAME_FORGEJO_TOKEN` | Bot API token (rename with `token_env`). |
| `MURASAME_WEBHOOK_SECRET` | Webhook HMAC secret (rename with `webhook_secret_env`). |

Any variable can be used in the config as `${VAR}`. See
[Substitution](configuration.html#substitution).

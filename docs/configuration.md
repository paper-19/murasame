---
title: Configuration
nav_order: 6
---

# Configuration reference
{: .no_toc }

A project config describes one repository. It's TOML with these tables, all
optional except at least one `[gates.<name>]`:

```text
[repo]  [merge]  [gate]  [gates.<name>]  [[conflicts]]
[[hooks.post_merge]]  [[hooks.post_land]]  [records]  [forgejo]
```

A config with `[instance]`, `[forges]` or `[[repos]]` is a
[multi-repo config](multi-repo.html).

Unknown keys, wrong types and unknown placeholders are errors.

1. TOC
{:toc}

## Value types

- Durations are whole seconds.
- Commands are string lists, run without a shell: `["make", "check"]`. For a
  shell line, use `["sh", "-c", "..."]`.
- Path patterns (`paths`, `exclude`, `full_when`) are globs from the repo root.
  `*` stays within one path segment; `**` crosses segments.
- Relative paths in `[repo]` are relative to the main checkout.

## Substitution

Environment variables are expanded when the config loads, in any string:
`${VAR}` (empty if unset) or `${VAR:-default}` (default if unset or empty).

Placeholders like `{repo}` are filled in at run time, only in the fields below.
Write {% raw %}`{{`{% endraw %} and {% raw %}`}}`{% endraw %} for literal braces.
Braces inside an expanded environment value are kept literally.

| Field | Placeholders |
|---|---|
| `repo.state_dir`, `repo.key` | `{repo}` |
| `repo.integ`, `repo.db` | `{repo}` `{state_dir}` |
| `repo.command_prefix` | `{repo}` `{state_dir}` `{integ}` `{target}` |
| gate `command`, `affected_command`, `env`; `gate.env` | those four, plus `{gate}` `{retried}` `{base}` |
| `hooks.post_merge` `run`, `env` | those four, plus `{branch}` `{hook}` |
| `conflicts` `run`, `env` | those four, plus `{branch}` |
| `hooks.post_land` `run`, `env`, `dir` | those four, plus `{hook}` `{target_sha}` `{target_short}` |

## [repo]

| Key | Default | Meaning |
|---|---|---|
| `target` | `["main"]` | Target branch: the first name that exists. |
| `state_dir` | `".murasame"` | Logs, lock, `merges.log`, state DB, and by default `integ`. |
| `integ` | `"{state_dir}/integ"` | The integration worktree. |
| `create_integ` | `false` | Create `integ` if missing. |
| `db` | `"murasame.sqlite3"` | State DB, relative to `state_dir`. Not allowed in multi-repo configs. |
| `key` | derived | The repository's key in the state DB. |
| `batch` | `false` | Use batch mode by default. |
| `pair_check` | `true` | Gate a failing branch alone for diagnosis. See [the pair check](how-it-works.html#the-pair-check). |
| `lock_wait` | `0` | Seconds to wait for the queue lock; 0 waits indefinitely. |
| `command_prefix` | `[]` | Wrapper for every gate, hook and conflict command, for example `["sudo", "-u", "ci", "--"]`. See [Isolation](deployment.html#isolation). |

## [merge]

| Key | Default | Meaning |
|---|---|---|
| `no_ff` | `false` | Always create a merge commit. |

## [gate]

| Key | Default | Meaning |
|---|---|---|
| `default` | the only gate | Required with more than one gate. |
| `timeout` | `7200` | Default timeout for all gates. |
| `env` | `{}` | Env for all gates. |

## [gates.&lt;name&gt;] {#gates}

Names start with a letter or digit, then letters, digits, `.`, `-`, `_`.

| Key | Default | Meaning |
|---|---|---|
| `command` | required | Exit 0 passes, 1 fails, anything else is an [infrastructure failure](gates.html#exit-codes). |
| `timeout` | `[gate] timeout` | Seconds before the gate is stopped. |
| `retries` | `1` | Retries after an infrastructure failure. |
| `retry_wait` | `30` | Seconds between retries. |
| `env` | `{}` | Added on top of `[gate] env`. |
| `affected_command` | none | See [Affected tests](gates.html#affected-tests). |
| `full_when` | `[]` | With `affected_command`: patterns that force a full run. |
| `full_every` | `0` | With `affected_command`: full run after this many affected landings. Needs records. |

## [[conflicts]]

See [conflict rules](conflicts-and-hooks.html#conflict-rules).

| Key | Default | Meaning |
|---|---|---|
| `label` | required | Name used in messages. |
| `paths` | required | Files the rule covers. |
| `strategy` | required | `keep-both`, `ours`, `theirs`, `command` or `stop`. |
| `exclude` | `[]` | Files to leave out. |
| `run` | | `command` only: regenerates the files; conflicted paths are appended. |
| `take` | `"ours"` | `command` only: side checked out before `run`. |
| `env`, `timeout` | `{}`, `3600` | `command` only. |

## [[hooks.post_merge]] and [[hooks.post_land]]

See [hooks](conflicts-and-hooks.html#post-merge-hooks).

| Key | Default | Meaning |
|---|---|---|
| `name` | required | Unique name. |
| `run` | required | The command. |
| `env` | `{}` | |
| `timeout` | `3600` | |
| `dir` | the repo | Post-land only. Must exist. |
| `checkout` | `false` | Post-land only. Check `dir` out at the landed commit first. |

## [records]

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Record passing trees and skip re-gating them. Required by `--no-land` and `full_every`. |
| `max_age` | `172800` | Seconds a record stays valid. |

## [forgejo]

Used by the [Forgejo service](forgejo.html). In a multi-repo config, the
connection keys move to `[instance]` and `[forges.*]`.

| Key | Default | Meaning |
|---|---|---|
| `url` | | Forge base URL. |
| `owner`, `repo` | | The repository. |
| `git_remote_url` | | URL to fetch from and push to. |
| `token_env` | `"MURASAME_FORGEJO_TOKEN"` | Env var holding the API token. |
| `bot_user` | `"murasame"` | Bot user; comments address `@<bot_user>`. |
| `reviewers` | `[]` | Users allowed to run privileged commands. |
| `reviewers_from_permission` | `""` | Also allow anyone with this permission or higher: `read`, `write`, `admin`, `owner`. |
| `status_context` | `"murasame"` | Commit status context. |
| `batch_max` | `8` | Maximum PRs per batch. |
| `poll_interval` | `60` | Seconds between polls. |
| `poll_during_gate` | `30` | Seconds between checks for cancels and pushes during a gate. |
| `listen` | `"127.0.0.1:8737"` | HTTP address. |
| `webhook_secret_env` | `"MURASAME_WEBHOOK_SECRET"` | Env var holding the webhook secret. |
| `external_url` | `""` | Public status page URL; its host is allowed along with loopback. |
| `comment_on_start` | `false` | Comment when a PR's test starts. |
| `log_excerpt_lines` | `30` | Gate log lines quoted in failure comments. |
| `mark_merged` | `true` | Mark the PR merged on the forge after landing. |
| `infra_backoff` | `300` | Seconds to pause the queue after an infrastructure failure. |
| `reply_cooldown` | `60` | Minimum seconds between replies to the same non-reviewer. |

## Full example

```toml
[repo]
target = ["main", "master"]
create_integ = true
batch = true

[merge]
no_ff = true

[gates.check]
command = ["sh", "-c", "${CHECK_COMMAND:-make check} || exit 1"]
timeout = 3600

[[conflicts]]
label = "docs"
paths = ["CHANGELOG.md", "docs/*.md"]
strategy = "keep-both"

[[hooks.post_merge]]
name = "toc"
run = ["make", "toc"]
timeout = 300

[records]
enabled = true

[forgejo]
url = "${MURASAME_FORGEJO_URL:-}"
owner = "${MURASAME_FORGEJO_OWNER:-}"
repo = "${MURASAME_FORGEJO_REPO:-}"
git_remote_url = "${MURASAME_GIT_URL:-}"
reviewers_from_permission = "write"
```

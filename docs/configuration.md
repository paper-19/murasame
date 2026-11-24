---
title: Configuration
nav_order: 6
---

# Configuration reference
{: .no_toc }

A project config describes one repository. It's TOML with these tables, all
optional (the service needs the connection keys of `[forgejo]`):

```text
[repo]  [merge]  [ci]  [[conflicts]]  [records]  [forgejo]
```

A config with `[instance]`, `[forges]` or `[[repos]]` is a
[multi-repo config](multi-repo.html).

Unknown keys, wrong types and unknown placeholders are errors.

1. TOC
{:toc}

## Value types

- Durations are whole seconds.
- Path patterns (`paths`, `exclude`) are globs from the repo root.
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

## [repo]

| Key | Default | Meaning |
|---|---|---|
| `target` | `["main"]` | Target branch: the first name that exists. |
| `state_dir` | `".murasame"` | Logs, lock, `merges.log`, state DB, and by default `integ`. |
| `integ` | `"{state_dir}/integ"` | The integration worktree; created if missing. |
| `db` | `"murasame.sqlite3"` | State DB, relative to `state_dir`. Not allowed in multi-repo configs. |
| `key` | derived | The repository's key in the state DB. |
| `pair_check` | `true` | Gate a failing PR alone for diagnosis. See [the pair check](how-it-works.html#the-pair-check). |

## [merge]

| Key | Default | Meaning |
|---|---|---|
| `no_ff` | `false` | Always create a merge commit. |

## [ci]

See [Gates](gates.html).

| Key | Default | Meaning |
|---|---|---|
| `staging` | `"murasame/staging"` | The branch murasame force-pushes merged results to. Not a target branch. |
| `contexts` | `[]` | Commit status contexts that must pass, such as `"ci / check (push)"`. Empty: every status on the commit except murasame's own. |
| `timeout` | `7200` | Seconds to wait for a result; then an infrastructure failure. |
| `start_timeout` | `900` | Seconds to wait for the first status; then an infrastructure failure. At most `timeout`; 0 waits for `timeout`. |
| `poll_interval` | `15` | Seconds between reads of the statuses. |

## [[conflicts]]

See [Conflicts](conflicts.html).

| Key | Default | Meaning |
|---|---|---|
| `label` | required | Name used in messages. |
| `paths` | required | Files the rule covers. |
| `strategy` | required | `keep-both`, `ours`, `theirs` or `stop`. |
| `exclude` | `[]` | Files to leave out. |

## [records]

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Record trees that passed CI and don't send them to CI again. |
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
| `poll_during_gate` | `30` | Seconds between reads of the batch's comments (cancel, unapprove) while waiting on CI. |
| `listen` | `"127.0.0.1:8737"` | HTTP address. |
| `webhook_secret_env` | `"MURASAME_WEBHOOK_SECRET"` | Env var holding the webhook secret. |
| `external_url` | `""` | Public status page URL; its host is allowed along with loopback. |
| `comment_on_start` | `false` | Comment when a PR's test starts. |
| `log_excerpt_lines` | `30` | Gate log lines (the CI statuses) quoted in failure comments. |
| `mark_merged` | `true` | Mark the PR merged on the forge after landing. Needs the repository's "manually merged" setting. |
| `infra_backoff` | `300` | Seconds to pause the queue after an infrastructure failure. |
| `reply_cooldown` | `60` | Minimum seconds between replies to the same non-reviewer. |

## Full example

```toml
[repo]
target = ["main", "master"]

[merge]
no_ff = true

[ci]
contexts = ["ci / check (push)"]
timeout = 3600

[[conflicts]]
label = "docs"
paths = ["CHANGELOG.md", "docs/*.md"]
strategy = "keep-both"

[records]
enabled = true

[forgejo]
url = "${MURASAME_FORGEJO_URL:-}"
owner = "${MURASAME_FORGEJO_OWNER:-}"
repo = "${MURASAME_FORGEJO_REPO:-}"
git_remote_url = "${MURASAME_GIT_URL:-}"
reviewers_from_permission = "write"
```

---
title: Multi-repo instances
nav_order: 9
---

# Multi-repo instances
{: .no_toc }

One `murasame serve` can run queues for many repositories on one or more forges.
They share a SQLite database. Each repository's gate is its own Forgejo
Actions workflow, so the runners decide how many run at once.

1. TOC
{:toc}

## Example

```toml
[instance]
state_dir = "${MURASAME_STATE:-/var/lib/murasame}"

[forges.main]
url = "${MURASAME_FORGEJO_URL:-http://127.0.0.1:3000}"

# Settings from a project config file, with overrides:
[[repos]]
repository = "team/app"
include = "generic.toml"

[repos.forgejo]
reviewers = ["alice", "bob"]
batch_max = 4

# Settings inline:
[[repos]]
repository = "team/handbook"
name = "docs"

[repos.repo]
pair_check = false

[repos.ci]
contexts = ["docs / build (push)"]
```

Multi-repo configs work with `serve`, `tick`, `check-config` and `history`.

## [instance]

| Key | Default | Meaning |
|---|---|---|
| `state_dir` | required | Shared DB and default clone location. Relative to the config file. |
| `db` | `"murasame.sqlite3"` | Shared DB, relative to `state_dir`. |
| `bot_user` | `"murasame"` | Bot user on every forge. |
| `listen` | `"127.0.0.1:8737"` | HTTP address. |
| `webhook_secret_env` | `"MURASAME_WEBHOOK_SECRET"` | Env var for the webhook secret. |
| `poll_interval` | `60` | Seconds between polls. |
| `external_url` | `""` | Public status page URL. |

## [forges.&lt;name&gt;]

| Key | Default | Meaning |
|---|---|---|
| `url` | required | Forge base URL. |
| `token_env` | `"MURASAME_FORGEJO_TOKEN"` | Env var for this forge's token. |
| `git_url` | `url` | Base URL for git operations. |

## [[repos]]

| Key | Default | Meaning |
|---|---|---|
| `repository` | required | `"owner/name"`. |
| `name` | the repo name | Short name for files and URLs. Unique, ignoring case. |
| `forge` | the only forge | Required with several forges. |
| `clone` | `"{state_dir}/{name}.git"` | Bare clone; created if missing. |
| `git_url` | `<forge git_url>/<owner>/<name>.git` | Fetch and push URL. |
| `include` | none | Project config to start from, relative to this file. |

Project tables (`repo`, `ci`, `conflicts`, `records`, `forgejo` and `merge`) can also go inline as `[repos.<table>]`. They are merged over the
included file key by key; an inline array such as `[[repos.conflicts]]`
replaces the included one.

These keys come from the instance, forge or entry and are ignored in an
included `forgejo` table (and rejected inline): `url`, `token_env`,
`bot_user`, `listen`, `webhook_secret_env`, `poll_interval`, `external_url`,
`owner`, `repo`, `git_remote_url`. `repo.db` is also not allowed.

## Validation

murasame rejects a config in which two repositories are the same forge
repository, share a name, clone, state dir, integration tree or DB key, or
have any of those nested inside another's.

## History

```sh
murasame history --repos                     # repositories and their keys
murasame history --repo-key <key>
murasame history --repo-key all --limit 50
```

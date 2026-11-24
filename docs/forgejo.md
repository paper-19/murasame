---
title: Forgejo service
nav_order: 8
---

# Forgejo service
{: .no_toc }

`murasame serve` runs the queue as a bot for Forgejo or Gitea. Reviewers queue
pull requests with comments; murasame sends the merged result to Forgejo
Actions and pushes what passes.

1. TOC
{:toc}

## Setup

Add `[forgejo]` to the project config (or use a [multi-repo config](multi-repo.html)):

```toml
[forgejo]
url = "https://forge.example.com"
owner = "team"
repo = "app"
git_remote_url = "https://forge.example.com/team/app.git"
reviewers_from_permission = "write"
```

```sh
export MURASAME_FORGEJO_TOKEN=...
murasame check-config                  # shows who can approve
murasame serve --repo /var/lib/murasame/app.git
```

At startup, `serve` runs the checks of [`check-config --forge`](cli.html#check-config)
and logs any problem it finds (a missing workflow, Actions off, no runner); it
starts anyway.

murasame polls every `poll_interval` seconds, so a webhook is optional. Pointing
one at `POST /webhook` makes it react to comments and pushes immediately. Set
`MURASAME_WEBHOOK_SECRET` to verify signatures. Forgejo 15 sends no webhook
when a commit status changes, so CI results are read every `ci.poll_interval`
seconds; any webhook that arrives during a batch makes murasame read them at
once.

The bot user needs write access to the repository and a token with
`write:repository`, `write:issue` and `read:user`. Protect the target branch so
only the bot can push or merge. [Deployment](deployment.html) covers a full
server install.

## Permissions

`approve`, `unapprove`, `retry`, `cancel`, `priority` and `rollup` need a
reviewer: a user in `reviewers`, or anyone with at least
`reviewers_from_permission` on the repository. With neither set, nobody can
approve.

Anyone can use `status` and `ping`. Non-reviewers get at most one reply per
`reply_cooldown` seconds.

Commands in an edited comment are ignored, since anyone with write access can
edit comments. Post a new comment instead.

## Commands

Comment `@murasame <command>` on a pull request:

| Command | Effect |
|---|---|
| `approve SHA` | Queue the PR at this head. The full 40- or 64-character SHA is required. |
| `unapprove` | Remove the approval and dequeue. |
| `retry` | Requeue after a set-aside or cancel. |
| `cancel` | Stop its test or leave the line, keeping the approval (`held`). |
| `priority N` | -9999 to 9999; higher goes first. |
| `rollup` / `rollup never` | Allow batching (default) / always gate alone. |
| `status` | Report the PR's state (while it is in CI, with a link to the run). |
| `ping` | Check the bot is listening. |

The approval names a commit so it can't apply to code pushed later. If the head
has moved, the bot refuses and quotes the current head. Any push to a queued PR
sends it back to `open`.

For unknown commands, the bot suggests the closest match. It recognises common
variants such as `lgtm` or `r+` (approve), `abort` (cancel), `rerun` (retry)
and `p=5` (priority 5).

## PR states

```mermaid
stateDiagram-v2
  direction LR
  [*] --> open
  open --> queued: approve
  queued --> testing: start
  queued --> held: cancel
  testing --> merged: land
  testing --> set_aside: fail
  testing --> held: cancel
  testing --> queued: requeue
  set_aside --> queued: retry / approve
  held --> queued: retry
  open --> closed: close
  closed --> open: reopen
```

| State | Meaning |
|---|---|
| `open` | Not approved. |
| `queued` | Waiting in line. |
| `testing` | In the running batch. |
| `set_aside` | Failed or conflicted. |
| `held` | Cancelled, approval kept. |
| `merged` | Landed, by murasame or otherwise. |
| `closed` | PR closed. |

`unapprove` or a push returns any active state to `open`.
`murasame history --pr N` shows a PR's transitions.

## Batches

The line is sorted by priority (highest first), then approval time, then PR
number. Each cycle takes up to `batch_max` PRs from the front, stopping at a
`rollup never` PR, and gates them as a [batch](how-it-works.html#batches-and-bisection).

While waiting on CI, murasame reads the batch's comments every `poll_during_gate`
seconds, so a cancel or unapprove stops it quickly. A push to a PR in the batch
doesn't: the approval names a commit, so the approved commit is what CI tests
and what lands, and the PR then goes back to `open` for its new commits. After an infrastructure
failure the queue pauses for `infra_backoff` seconds; `status` shows why.

## Feedback

- Commit statuses under `status_context`: pending, then success or failure.
- Comments when a PR is queued, lands, is set aside (with the last
  `log_excerpt_lines` of the gate log: the failing CI status and its link) or is
  cancelled. `comment_on_start` adds
  one when testing starts.
- With `mark_merged` (default on), the PR is marked merged after landing. This
  needs the repository setting "Enable marking pull requests as manually
  merged" (Settings → Repository → Pull requests; `allow_manual_merge` in the
  API). Without it, Forgejo answers 405, murasame logs it and tries again each
  cycle, and the PR stays open.

## HTTP endpoints

The server listens on `listen` (default `127.0.0.1:8737`).

| Route | Purpose |
|---|---|
| `POST /webhook` | Webhook receiver. |
| `GET /` | Status page (HTML). |
| `GET /api/status` | Status (JSON). |
| `GET /repos/NAME/logs/LOG` | A gate log: the CI statuses murasame saw, with links. |
| `GET /healthz` | Liveness. |

The status page has no login. It only answers requests addressed to a loopback
host name or to `external_url`'s host; anything else gets 403. See
[Status page](deployment.html#status-page).

The token is redacted from logs. murasame runs no repository code; see
[Isolation](deployment.html#isolation).

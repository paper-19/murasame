---
title: Conflicts and hooks
nav_order: 7
---

# Conflicts and hooks
{: .no_toc }

1. TOC
{:toc}

## Conflict rules

Some files conflict all the time without anyone being at fault: changelogs,
generated indexes, lock files. A `[[conflicts]]` rule lets murasame resolve
them so the branch can still land.

```toml
[[conflicts]]
label = "changelog"
paths = ["CHANGELOG.md", "docs/*.md"]
strategy = "keep-both"

[[conflicts]]
label = "lock files"
paths = ["**/package-lock.json"]
exclude = ["vendor/**"]
strategy = "command"
take = "theirs"
run = ["npm", "install", "--package-lock-only"]
```

Each conflicted path goes to the first rule whose `paths` match and whose
`exclude` doesn't.

| Strategy | Action |
|---|---|
| `keep-both` | Keep both sides' lines in each conflicting hunk. Text files changed on both sides only. |
| `ours` | Take the version already in `integ`. |
| `theirs` | Take the incoming branch's version. |
| `command` | Check out the `take` side, then run `run` with the conflicted paths as arguments. |
| `stop` | Never resolve these paths, even if a later rule matches. |

The merge is resolved only if every conflicted path is. Otherwise a person has
to fix it: the branch is set aside in batch mode, or the run stops in one-by-one
mode.

`command` rules run in `integ`, can use `{branch}`, and have their own `env` and
`timeout` (default 3600 s). The resolved files are part of the gated tree, so a
bad resolution fails the gate.

## Post-merge hooks

A post-merge hook runs in `integ` after each branch is merged, before the gate.
Use it to regenerate files such as a table of contents or generated code.

```toml
[[hooks.post_merge]]
name = "toc"
run = ["make", "toc"]
timeout = 300
```

Hooks run in the order listed. If a hook changes files, murasame commits them
separately (`murasame: toc after merging feature-a`), and that commit lands with
the branch. A hook may add commits but must not drop them or switch branches.

Hooks and conflict commands treat any non-zero exit as the branch's fault: it's
set aside, or the run stops in one-by-one mode. Timeouts, signals, start
failures and leftover processes are infrastructure failures and stop the run.

## Post-land hooks

A post-land hook runs once per run, after the target moves, at the landed
commit. Use it to deploy, publish or notify.

```toml
[[hooks.post_land]]
name = "publish-docs"
dir = "/srv/docs-checkout"
checkout = true                 # check dir out at the landed commit first
run = ["make", "publish"]
env = { REV = "{target_short}" }
timeout = 600
```

`dir` defaults to the repository and must exist. `checkout` needs `dir`.
Placeholders: `{hook}`, `{target_sha}`, `{target_short}`.

A post-land hook can't undo a landing. If it fails, murasame logs a warning and
continues. Skip one for a run with `--skip-hook NAME`.

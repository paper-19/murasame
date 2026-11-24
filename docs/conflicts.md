---
title: Conflicts
nav_order: 7
---

# Conflicts

Some files conflict all the time without anyone being at fault: changelogs,
generated indexes, lock files. A `[[conflicts]]` rule lets murasame resolve
them so the PR can still land.

```toml
[[conflicts]]
label = "changelog"
paths = ["CHANGELOG.md", "docs/*.md"]
strategy = "keep-both"

[[conflicts]]
label = "lock files"
paths = ["**/package-lock.json"]
exclude = ["vendor/**"]
strategy = "theirs"
```

Each conflicted path goes to the first rule whose `paths` match and whose
`exclude` doesn't.

| Strategy | Action |
|---|---|
| `keep-both` | Keep both sides' lines in each conflicting hunk. Text files changed on both sides only. |
| `ours` | Take the version already in `integ`. |
| `theirs` | Take the incoming PR's version. |
| `stop` | Never resolve these paths, even if a later rule matches. |

The merge is resolved only if every conflicted path is. Otherwise a person has
to fix it, and the PR is set aside without a CI run.

The resolved files are part of the commit CI tests, so a bad resolution fails
the gate. To regenerate a file after a resolution (a lock file, an index), do
it in CI and fail the job if the file is out of date.

murasame runs no project code. Post-merge checks go in the CI workflow,
post-land steps in a workflow on pushes to the target branch.

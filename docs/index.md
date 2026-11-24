---
title: Overview
layout: home
nav_order: 1
---

# murasame

murasame is a merge queue for Forgejo. It merges approved pull requests onto a
target branch, pushes the result to a staging branch for Forgejo Actions to
test, and moves the target only if CI passes.

[Get started](getting-started.html){: .btn .btn-primary .mr-2 }
[Configuration](configuration.html){: .btn }

It can gate many PRs at once. If that batch fails, murasame bisects it to find
the bad PR, sets it aside, and lands the others.

murasame only queues. It runs no project commands: your runners build and
test, with the toolchains, caches and secrets they already have. Reviewers
queue pull requests with comments like `@murasame approve <sha>`.

Other features: pass records that skip re-testing a tree, automatic conflict
resolution for chosen files, and multi-repo instances.

## Example

```console
== batch on main c3d110f: #12 #13 #14 (3 branch(es), one gate)
-- gate: pushing 5242465e0715 to murasame/staging for CI
   ci / check (push): failure (Failing after 2m) https://forge.example.com/team/app/actions/runs/88
-- the batch failed the gate: bisecting: (#12 #13) first, then (#14)

== batch on main c3d110f: #12 #13 (2 branch(es), one gate)
-- gate: pushing f303ce1b40c9 to murasame/staging for CI
   ci / check (push): success (Successful in 2m) https://forge.example.com/team/app/actions/runs/89
-- 2026-11-21 09:14:46 merged branch=#12 commit=09ea4a9 target=main ...
-- 2026-11-21 09:14:46 merged branch=#13 commit=645f81a target=main ...

== batch on main 9f8da37: #14 (1 branch(es), one gate)
-- SET ASIDE #14: the gate failed (ci; log: ...): CI failed: ci / check (push): failure ...
merged: #12 #13
set aside: #14
```

[How it works](how-it-works.html) explains each step.

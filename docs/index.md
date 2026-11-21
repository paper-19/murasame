---
title: Overview
layout: home
nav_order: 1
---

# murasame

murasame is a merge queue. It merges branches onto a target branch, runs a
command you choose (the gate) on the merged result, and moves the target only if
the gate passes.

[Get started](getting-started.html){: .btn .btn-primary .mr-2 }
[Configuration](configuration.html){: .btn }

It can gate many branches at once. If that batch fails, murasame bisects it to
find the bad branch, sets that branch aside, and lands the others.

It runs as a local CLI (`murasame run`) or as a bot for Forgejo and Gitea
(`murasame serve`), where reviewers queue pull requests with comments like
`@murasame approve <sha>`.

Other features: several named gates per repository, affected-tests gates, pass
records that skip re-gating a tree, automatic conflict resolution for chosen
files, post-merge and post-land hooks, and multi-repo instances.

## Example

```console
$ murasame run feat-a feat-b feat-bad

== batch on main c3d110f: feat-a feat-b feat-bad (3 branch(es), one gate)
-- the batch failed the gate: bisecting: (feat-a feat-b) first, then (feat-bad)

== batch on main c3d110f: feat-a feat-b (2 branch(es), one gate)
-- 2026-11-21 09:14:46 merged branch=feat-a commit=09ea4a9 target=main ...
-- 2026-11-21 09:14:46 merged branch=feat-b commit=645f81a target=main ...

== batch on main 9f8da37: feat-bad (1 branch(es), one gate)
-- SET ASIDE feat-bad: the gate failed (check; log: .murasame/...-feat-bad-alone.gate.log)
merged: feat-a feat-b
set aside: feat-bad
```

[How it works](how-it-works.html) explains each step.

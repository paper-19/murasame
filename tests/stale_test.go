package tests

import (
	"strings"
	"testing"

	"murasame/internal/forgejo"
	"murasame/internal/states"
)

// A lone PR on an unchanged target fast-forwards, so the queue gates the PR's own
// head commit, which may already carry statuses from earlier runs.

func TestSkippedRequiredJobAfterAFailureSetsThePRAside(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Mode = ciNeedsFailed
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	before := f.Main()
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	q.Tick()

	if f.Main() != before {
		t.Fatalf("main moved although the required job was skipped\n%s", q.Out)
	}
	q.WantState(1, states.SetAside)
	if r := q.PR(1).Reason; !strings.Contains(r, "skipped after ci / build (push): failure") {
		t.Fatalf("reason: %q", r)
	}
}

func TestSkippedRequiredJobWithoutAFailureIsTheWorkflows(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Mode = ciSkipped
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	before := f.Main()
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	q.Tick()

	if f.Main() != before {
		t.Fatalf("main moved although the required job was skipped\n%s", q.Out)
	}
	q.WantState(1, states.Queued)
	if !strings.Contains(q.Out.String(), "skipped although no job failed") {
		t.Fatalf("want an infrastructure stop naming the workflow\n%s", q.Out)
	}
}

func TestRetryAfterAFlakyFailureRunsCIAgain(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Mode = ciFlakyFirstRun
	sha := f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	q.Tick()
	q.WantState(1, states.SetAside)

	f.Say(1, "alice", "@murasame retry")
	q.Tick()

	q.WantState(1, states.Merged)
	if runs := f.CIRuns(); len(runs) != 2 || runs[0] != sha || runs[1] != sha {
		t.Fatalf("CI runs %v: want two runs of the PR's head %s\n%s", runs, sha, q.Out)
	}
}

func TestOldFailureOnTheCommitIsIgnored(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	sha := f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	f.Preset(sha, forgejo.CommitStatus{Context: ciContext, State: "failure", Description: "an old run"})
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	q.Tick()

	q.WantState(1, states.Merged)
}

func TestOtherWorkflowsOnThePRHeadDoNotDecide(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	sha := f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	f.Preset(sha, forgejo.CommitStatus{Context: "ci / lint (pull_request)", State: "failure"})
	cfg := strings.Replace(baseConfig(f, ""), "contexts = [\""+ciContext+"\"]", "contexts = []", 1)
	q := NewQueue(t, f, cfg)
	f.Approve(1)
	q.Tick()

	q.WantState(1, states.Merged)
}

func TestOldStatusesDoNotHideAMissingWorkflow(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Mode = ciSilent
	sha := f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	f.Preset(sha, forgejo.CommitStatus{Context: ciContext, State: "pending", Description: "an old run that never ended"})
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	q.Tick()

	q.WantState(1, states.Queued)
	if !strings.Contains(q.Out.String(), "no CI status appeared") {
		t.Fatalf("want the start timeout, not the full timeout\n%s", q.Out)
	}
}

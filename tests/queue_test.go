package tests

import (
	"os"
	"strings"
	"testing"
	"time"

	"murasame/internal/states"
)

func TestApprovedPRIsGatedByCIAndLands(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	sha := f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	q.Tick()

	q.WantState(1, states.Merged)
	main := f.Main()
	if !f.Contains(main, "a.txt") {
		t.Fatalf("main %s does not have the PR's file\n%s", main, q.Out)
	}
	if got := f.StagingTip(); got != main {
		t.Fatalf("main is %s, but CI ran on the staging branch at %s: the landed commit must be the gated one", main, got)
	}
	if runs := f.CIRuns(); len(runs) != 1 || runs[0] != main {
		t.Fatalf("CI runs: %v, want exactly one, on %s", runs, main)
	}
	if f.Marked(1) != main {
		t.Fatalf("PR 1 marked merged at %q, want %s", f.Marked(1), main)
	}
	var last string
	for _, st := range f.OwnStatuses(sha) {
		last = st.State
	}
	if last != "success" {
		t.Fatalf("murasame's last status on the PR head: %q, want success: %v", last, f.OwnStatuses(sha))
	}
}

func TestFailingPRIsSetAsideAndTargetStays(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.OpenPR(1, map[string]string{"BROKEN": "x\n"})
	before := f.Main()
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	q.Tick()

	q.WantState(1, states.SetAside)
	if f.Main() != before {
		t.Fatalf("main moved to %s after a CI failure", f.Main())
	}
	reason := q.PR(1).Reason
	if !strings.Contains(reason, ciContext) || !strings.Contains(reason, "failure") {
		t.Fatalf("the set-aside reason should name the failing CI context: %q", reason)
	}
	found := false
	for _, c := range f.CommentsBy(1, "murasame") {
		if strings.Contains(c, "actions/runs/") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no comment linking the CI run: %v", f.CommentsBy(1, "murasame"))
	}
}

func TestBatchBisectsToTheBrokenPR(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.OpenPR(1, map[string]string{"one.txt": "1\n"})
	f.OpenPR(2, map[string]string{"BROKEN": "2\n"})
	f.OpenPR(3, map[string]string{"three.txt": "3\n"})
	f.OpenPR(4, map[string]string{"four.txt": "4\n"})
	q := NewQueue(t, f, baseConfig(f, ""))
	for n := 1; n <= 4; n++ {
		f.Approve(n)
	}
	q.Tick()

	q.WantState(1, states.Merged)
	q.WantState(2, states.SetAside)
	q.WantState(3, states.Merged)
	q.WantState(4, states.Merged)
	main := f.Main()
	for _, p := range []string{"one.txt", "three.txt", "four.txt"} {
		if !f.Contains(main, p) {
			t.Fatalf("main lacks %s\n%s", p, q.Out)
		}
	}
	if f.Contains(main, "BROKEN") {
		t.Fatalf("the broken PR landed")
	}
	// [1 2 3 4] fails, [1 2] fails, [1] passes, [2] fails, [3 4] passes.
	if n := len(f.CIRuns()); n != 5 {
		t.Fatalf("%d CI runs, want 5 for one bad PR among 4\n%s", n, q.Out)
	}
}

func TestNoCIStatusIsAnInfrastructureFailure(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Mode = ciSilent
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	before := f.Main()
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	q.Tick()

	q.WantState(1, states.Queued)
	if f.Main() != before {
		t.Fatal("main moved without CI")
	}
	if !strings.Contains(q.Out.String(), "no CI status appeared") {
		t.Fatalf("the log should say that no CI status appeared:\n%s", q.Out)
	}
	if f.StagingTip() == "" {
		t.Fatal("nothing was pushed to the staging branch")
	}
}

func TestUnreadableStatusesStopTheBatch(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Mode = ciFlakyAPI
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	q.Tick()

	q.WantState(1, states.Queued)
	if !strings.Contains(q.Out.String(), "could not be read") {
		t.Fatalf("expected the read failures to stop the batch:\n%s", q.Out)
	}
}

func TestPendingCIIsWaitedFor(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Pending = 3
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	q.Tick()

	q.WantState(1, states.Merged)
}

func TestPassRecordSkipsCIForTheSameTree(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	sha := f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	start := f.Main()
	q := NewQueue(t, f, baseConfig(f, "\n[records]\nenabled = true\n"))
	f.Approve(1)
	q.Tick()
	q.WantState(1, states.Merged)
	if n := len(f.CIRuns()); n != 1 {
		t.Fatalf("%d CI runs, want 1", n)
	}

	// Someone resets main, and the same commit comes back as PR 2: the queue builds the
	// same tree again and reuses the pass instead of sending it to CI.
	git(t, f.Work, "push", "-q", "-f", f.Bare, start+":refs/heads/main")
	f.ReopenAs(2, sha)
	f.Approve(2)
	q.Tick()

	q.WantState(2, states.Merged)
	if n := len(f.CIRuns()); n != 1 {
		t.Fatalf("%d CI runs, want still 1: the tree had a pass record\n%s", n, q.Out)
	}
	if !strings.Contains(q.Out.String(), "gate skipped: tree") {
		t.Fatalf("expected a skip message\n%s", q.Out)
	}
}

func TestCancelDuringCIHoldsThePR(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Pending = 1000
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	before := f.Main()
	q := NewQueue(t, f, baseConfig(f, "poll_during_gate = 1\n"))
	f.Approve(1)
	f.OnPoll = func(sha string, n int) {
		if n == 2 {
			f.Say(1, "alice", "@murasame cancel")
		}
	}
	q.Tick()

	q.WantState(1, states.Held)
	if f.Main() != before {
		t.Fatal("main moved after a cancel")
	}
	if !strings.Contains(q.Out.String(), "the gate was stopped") {
		t.Fatalf("expected the CI wait to stop\n%s", q.Out)
	}
}

func TestConflictingPRIsSetAsideWithoutCI(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.OpenPR(1, map[string]string{"README.md": "one\n"})
	f.OpenPR(2, map[string]string{"README.md": "two\n"})
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	f.Approve(2)
	q.Tick()

	q.WantState(1, states.Merged)
	q.WantState(2, states.SetAside)
	if r := q.PR(2).Reason; !strings.Contains(r, "merge conflict that needs a person") {
		t.Fatalf("reason: %q", r)
	}
	if n := len(f.CIRuns()); n != 1 {
		t.Fatalf("%d CI runs, want 1: a conflicting PR is never sent to CI\n%s", n, q.Out)
	}
}

func TestKeepBothResolvesAConflictBeforeCI(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.OpenPR(1, map[string]string{"CHANGELOG.md": "hello\none\n"})
	f.OpenPR(2, map[string]string{"CHANGELOG.md": "hello\ntwo\n"})
	cfg := baseConfig(f, "") + "\n[[conflicts]]\nlabel = \"changelog\"\npaths = [\"CHANGELOG.md\"]\nstrategy = \"keep-both\"\n"
	q := NewQueue(t, f, cfg)
	f.Approve(1)
	f.Approve(2)
	q.Tick()

	q.WantState(1, states.Merged)
	q.WantState(2, states.Merged)
	got := git(t, f.Bare, "show", "main:CHANGELOG.md")
	if !strings.Contains(got, "one") || !strings.Contains(got, "two") {
		t.Fatalf("CHANGELOG.md on main:\n%s", got)
	}
}

func TestPendingStatusLinksToTheCIRun(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Pending = 2
	sha := f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	q.Tick()

	q.WantState(1, states.Merged)
	for _, st := range f.OwnStatuses(sha) {
		if st.State == "pending" && strings.Contains(st.TargetURL, "/actions/runs/") {
			return
		}
	}
	t.Fatalf("no pending status linking the Actions run: %+v", f.OwnStatuses(sha))
}

func TestPushToThePRDuringCILandsTheApprovedCommitOnly(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Pending = 3
	tested := f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	var later string
	f.OnPoll = func(sha string, n int) {
		if n == 1 {
			later = f.OpenPR(1, map[string]string{"a.txt": "changed\n"})
		}
	}
	q.Tick()

	// The approval names a commit: that commit was tested and lands. The newer one needs
	// its own approval, so the PR goes back to open.
	q.WantState(1, states.Open)
	main := f.Main()
	if !f.Contains(main, "a.txt") || git(t, f.Bare, "merge-base", "--is-ancestor", tested, main) != "" {
		t.Fatalf("the approved commit %s did not land on %s", tested, main)
	}
	if git(t, f.Bare, "show", main+":a.txt") != "a" {
		t.Fatalf("the commit pushed during CI (%s) landed", later)
	}
}

func TestTargetMovedDuringCIIsNotOverwritten(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Pending = 2
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	var outside string
	f.OnPoll = func(sha string, n int) {
		if n == 1 {
			git(t, f.Work, "checkout", "-q", "main")
			f.write("outside.txt", "pushed by hand\n")
			git(t, f.Work, "add", "-A")
			git(t, f.Work, "commit", "-q", "-m", "outside")
			git(t, f.Work, "push", "-q", f.Bare, "main:refs/heads/main")
			outside = git(t, f.Work, "rev-parse", "HEAD")
		}
	}
	q.Tick()

	if f.Main() != outside {
		t.Fatalf("main is %s, want the outside commit %s kept\n%s", f.Main(), outside, q.Out)
	}
	if st := q.PR(1).State; st == states.Merged {
		t.Fatalf("PR 1 merged over a moved target\n%s", q.Out)
	}
}

func TestRefusedStagingPushIsAnInfrastructureFailure(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	hook := "#!/bin/sh\nwhile read old new ref; do\n  case $ref in refs/heads/murasame/*) echo 'staging is protected' >&2; exit 1;; esac\ndone\n"
	path := f.Bare + "/hooks/pre-receive"
	if err := os.WriteFile(path, []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	before := f.Main()
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	q.Tick()

	q.WantState(1, states.Queued)
	if f.Main() != before {
		t.Fatal("main moved without CI")
	}
	if !strings.Contains(q.Out.String(), "could not be pushed to the staging branch") {
		t.Fatalf("expected a refused-push message\n%s", q.Out)
	}
	if len(f.CIRuns()) != 0 {
		t.Fatal("CI ran without a staging push")
	}
}

func TestApprovalRevokedDuringCIDoesNotLand(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Pending = 2
	f.perms["bob"] = "write"
	sha := f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	before := f.Main()
	q := NewQueue(t, f, baseConfig(f, "reviewers_from_permission = \"write\"\n"))
	f.Say(1, "bob", "@murasame approve "+sha)
	f.OnPoll = func(sha string, n int) {
		if n == 1 {
			f.mu.Lock()
			f.perms["bob"] = "read"
			f.mu.Unlock()
		}
	}
	q.Tick()

	if f.Main() != before {
		t.Fatalf("main moved although the approver lost write permission during CI\n%s", q.Out)
	}
	q.WantState(1, states.Open)
}

func TestWebhookCutsTheCIWaitShort(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Pending = 1
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	cfg := strings.Replace(baseConfig(f, ""), "poll_interval = 1\n", "poll_interval = 200000\n", 1)
	cfg = strings.Replace(cfg, "timeout = 60\n", "timeout = 400000\n", 1)
	q := NewQueue(t, f, cfg)
	f.Approve(1)
	done := make(chan error, 1)
	go func() { done <- q.Svc.Tick() }()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				q.Svc.Nudge()
			}
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tick: %v\n%s", err, q.Out)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the CI wait did not end after a webhook nudge")
	}
	q.WantState(1, states.Merged)
}

func TestSetAsideStatusLinksToTheFailingRun(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	sha := f.OpenPR(1, map[string]string{"BROKEN": "x\n"})
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	q.Tick()

	q.WantState(1, states.SetAside)
	sts := f.OwnStatuses(sha)
	last := sts[len(sts)-1]
	if last.State != "failure" || !strings.Contains(last.TargetURL, "/actions/runs/") {
		t.Fatalf("last status %+v: want a failure linking the CI run", last)
	}
}

func TestStoppingDuringCIRequeuesAndARestartFinishes(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Pending = 1000000
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	before := f.Main()
	text := baseConfig(f, "")
	q := NewQueue(t, f, text)
	f.Approve(1)
	f.OnPoll = func(sha string, n int) {
		if n == 3 {
			go q.Svc.Stop("the service is stopping")
		}
	}
	done := make(chan error, 1)
	go func() { done <- q.Svc.Tick() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the CI wait did not stop")
	}
	q.WantState(1, states.Queued)
	if f.Main() != before {
		t.Fatal("main moved during a stop")
	}
	q.Svc.Close()

	f.mu.Lock()
	f.Pending, f.OnPoll = 0, nil
	f.mu.Unlock()
	again := NewQueueAt(t, f, text, q.Svc.RepoDir)
	again.Tick()
	again.WantState(1, states.Merged)
}

func TestStatusCommandLinksTheRunningCI(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Pending = 4
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	q := NewQueue(t, f, baseConfig(f, "poll_during_gate = 1\n"))
	f.Approve(1)
	f.OnPoll = func(sha string, n int) {
		if n == 1 {
			f.Say(1, "bob", "@murasame status")
			q.Svc.Nudge()
		}
	}
	q.Tick()

	q.WantState(1, states.Merged)
	for _, c := range f.CommentsBy(1, "murasame") {
		if strings.Contains(c, "is being tested now") && strings.Contains(c, "(CI: http://forge.test/actions/runs/") {
			return
		}
	}
	t.Fatalf("no status reply linking the CI run: %q\n%s", f.CommentsBy(1, "murasame"), q.Out)
}

func TestNoRunnerTimesOutWithAHint(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Mode = ciNoRunner
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	cfg := strings.Replace(baseConfig(f, ""), "timeout = 60\n", "timeout = 8\n", 1)
	cfg = strings.Replace(cfg, "start_timeout = 2\n", "start_timeout = 5\n", 1)
	q := NewQueue(t, f, cfg)
	f.Approve(1)
	q.Tick()

	q.WantState(1, states.Queued)
	if !strings.Contains(q.Out.String(), "never left \"Waiting to run\": is a runner") {
		t.Fatalf("the timeout should point at the runner\n%s", q.Out)
	}
}

func TestATypoInContextsIsNamed(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	cfg := strings.Replace(baseConfig(f, ""), "contexts = [\""+ciContext+"\"]", `contexts = ["ci / tset (push)"]`, 1)
	cfg = strings.Replace(cfg, "start_timeout = 2\n", "start_timeout = 5\n", 1)
	q := NewQueue(t, f, cfg)
	f.Approve(1)
	q.Tick()

	q.WantState(1, states.Queued)
	out := q.Out.String()
	if !strings.Contains(out, "ci / tset (push)") || !strings.Contains(out, "reported only "+ciContext) ||
		!strings.Contains(out, "a typo in ci.contexts?") {
		t.Fatalf("want the missing and the reported contexts named\n%s", out)
	}
}

func TestAMissingSecondContextIsNamed(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	cfg := strings.Replace(baseConfig(f, ""), "contexts = [\""+ciContext+"\"]",
		`contexts = ["`+ciContext+`", "ci / lnit (push)"]`, 1)
	q := NewQueue(t, f, cfg)
	f.Approve(1)
	q.Tick()

	q.WantState(1, states.Queued)
	want := "the run finished without ci / lnit (push); it reported only " + ciContext
	if !strings.Contains(q.Out.String(), want) {
		t.Fatalf("want %q\n%s", want, q.Out)
	}
}

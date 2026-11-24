package tests

import (
	"strings"
	"testing"

	"murasame/internal/forgejo"
	"murasame/internal/gate"
)

func st(id int64, context, state string) forgejo.CommitStatus {
	return forgejo.CommitStatus{ID: id, Context: context, State: state}
}

func TestEvaluate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		statuses []forgejo.CommitStatus
		required []string
		want     string
		why      string
	}{
		{"nothing yet", nil, nil, gate.Pending, "no CI status yet"},
		{"all passed", []forgejo.CommitStatus{st(1, "build", "success"), st(2, "test", "success")}, nil, gate.Pass,
			"passed: build, test"},
		{"one still running", []forgejo.CommitStatus{st(1, "build", "success"), st(2, "test", "pending")}, nil,
			gate.Pending, "running: test"},
		{"a failure ends it early", []forgejo.CommitStatus{st(1, "build", "failure"), st(2, "test", "pending")}, nil,
			gate.Fail, "build: failure"},
		{"error counts as failure", []forgejo.CommitStatus{st(1, "build", "error")}, nil, gate.Fail, "build: error"},
		{"warning counts as passed", []forgejo.CommitStatus{st(1, "lint", "warning")}, nil, gate.Pass, "passed: lint"},
		{"a skipped job does not block", []forgejo.CommitStatus{st(1, "build", "success"), st(2, "deploy", "skipped")},
			nil, gate.Pass, "passed: build, deploy"},
		{"every job skipped tests nothing", []forgejo.CommitStatus{st(1, "deploy", "skipped")}, nil, gate.Infra,
			"nothing was tested"},
		{"a required job skipped after a failure fails",
			[]forgejo.CommitStatus{st(1, "build", "failure"), st(2, "test", "skipped")}, []string{"test"}, gate.Fail,
			"skipped after build: failure"},
		{"Forgejo 15 reports a skipped job as a success",
			[]forgejo.CommitStatus{st(1, "build", "failure"), {ID: 2, Context: "test", State: "success",
				Description: "Has been skipped"}}, []string{"test"}, gate.Fail, "skipped after build: failure"},
		{"a skipped job reported as success tests nothing",
			[]forgejo.CommitStatus{{ID: 1, Context: "test", State: "success", Description: "Has been skipped"}}, nil,
			gate.Infra, "nothing was tested"},
		{"a cancelled job is not a test result", []forgejo.CommitStatus{{ID: 1, Context: "test", State: "failure",
			Description: "Has been cancelled"}}, nil, gate.Infra, "the job was cancelled"},
		{"a real failure wins over a cancelled job", []forgejo.CommitStatus{{ID: 1, Context: "a", State: "failure",
			Description: "Has been cancelled"}, st(2, "b", "failure")}, nil, gate.Fail, "b: failure"},
		{"a required job skipped with no failure is the workflow's", []forgejo.CommitStatus{st(1, "test", "skipped")},
			[]string{"test"}, gate.Infra, "skipped although no job failed"},
		{"the newest status of a context wins", []forgejo.CommitStatus{st(1, "test", "failure"), st(2, "test", "success")},
			nil, gate.Pass, "passed: test"},
		{"newest wins whatever the order", []forgejo.CommitStatus{st(9, "test", "pending"), st(3, "test", "failure")},
			nil, gate.Pending, "running: test"},
		{"required contexts not reported yet", []forgejo.CommitStatus{st(1, "build", "success")},
			[]string{"build", "test"}, gate.Pending, "waiting for test"},
		{"other contexts are ignored when some are required",
			[]forgejo.CommitStatus{st(1, "build", "success"), st(2, "docs", "failure")}, []string{"build"}, gate.Pass,
			"passed: build"},
		{"a required failure fails", []forgejo.CommitStatus{st(1, "build", "failure")}, []string{"build", "test"},
			gate.Fail, "build: failure"},
		{"murasame's own status is ignored", []forgejo.CommitStatus{st(1, "murasame", "pending"), st(2, "ci", "success")},
			nil, gate.Pass, "passed: ci"},
		{"only murasame's own status", []forgejo.CommitStatus{st(1, "murasame", "success")}, nil, gate.Pending,
			"no CI status yet"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := gate.Evaluate(c.statuses, c.required, "murasame")
			if ev.Verdict != c.want || !strings.Contains(ev.Why, c.why) {
				t.Fatalf("got %s (%q), want %s (%q)", ev.Verdict, ev.Why, c.want, c.why)
			}
		})
	}
}

func TestLatestKeepsOneStatusPerContext(t *testing.T) {
	t.Parallel()
	got := gate.Latest([]forgejo.CommitStatus{st(5, "b", "success"), st(4, "a", "pending"), st(6, "a", "failure"),
		st(1, "b", "pending"), st(7, "", "success")}, "")
	if len(got) != 2 || got[0].Context != "a" || got[0].State != "failure" || got[1].Context != "b" ||
		got[1].State != "success" {
		t.Fatalf("got %+v", got)
	}
}

func TestSinceDropsEarlierRuns(t *testing.T) {
	t.Parallel()
	run := func(id int64, url, state string) forgejo.CommitStatus {
		return forgejo.CommitStatus{ID: id, Context: "ci / test (push)", State: state, TargetURL: url}
	}
	old := []forgejo.CommitStatus{run(3, "http://f/runs/1/jobs/0", "pending")}
	now := []forgejo.CommitStatus{run(6, "http://f/runs/1/jobs/0", "failure"), run(5, "http://f/runs/2/jobs/0", "pending"),
		run(3, "http://f/runs/1/jobs/0", "pending")}
	got := gate.Since(now, old)
	if len(got) != 1 || got[0].ID != 5 {
		t.Fatalf("got %+v: want only the new run's status", got)
	}
	if ev := gate.Evaluate(got, []string{"ci / test (push)"}, ""); ev.Verdict != gate.Pending {
		t.Fatalf("the earlier run's late failure decided: %+v", ev)
	}
}

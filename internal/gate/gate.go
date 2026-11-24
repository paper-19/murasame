package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"murasame/internal/config"
	"murasame/internal/forgejo"
	"murasame/internal/gitx"
	"murasame/internal/reporter"
	"murasame/internal/safefile"
)

const Name = "ci"

const (
	Pass      = "pass"
	Fail      = "fail"
	Infra     = "infra"
	Cancelled = "cancelled"
	Pending   = "pending"
)

type Forge interface {
	Push(sha, branch string) error
	Statuses(sha string) ([]forgejo.CommitStatus, error)
}

type Run struct {
	Outcome  string
	Used     string
	Log      string
	InfraWhy string
	Record   map[string]string
}

type Eval struct {
	Verdict string
	Why     string
	Latest  []forgejo.CommitStatus
	Started bool
	Missing []string
}

func Latest(statuses []forgejo.CommitStatus, ignore string) []forgejo.CommitStatus {
	by := map[string]forgejo.CommitStatus{}
	for _, st := range statuses {
		if st.Context == "" || st.Context == ignore {
			continue
		}
		if had, ok := by[st.Context]; !ok || st.ID > had.ID {
			by[st.Context] = st
		}
	}
	out := make([]forgejo.CommitStatus, 0, len(by))
	for _, st := range by {
		switch {
		case Skipped(st):
			st.State = "skipped"
		case WasCancelled(st):
			st.State = "cancelled"
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Context < out[j].Context })
	return out
}

func Skipped(st forgejo.CommitStatus) bool {
	return st.State == "skipped" || st.State == "success" && strings.HasPrefix(st.Description, "Has been skipped")
}

func WasCancelled(st forgejo.CommitStatus) bool {
	return st.State == "cancelled" || st.State == "failure" && strings.HasPrefix(st.Description, "Has been cancelled")
}

func done(state string) bool {
	switch state {
	case "success", "warning", "skipped", "failure", "error", "cancelled":
		return true
	}
	return false
}

func queued(sts []forgejo.CommitStatus) bool {
	waiting := false
	for _, st := range sts {
		switch {
		case done(st.State):
		case strings.HasPrefix(st.Description, "Waiting to run") ||
			strings.HasPrefix(st.Description, "Blocked by required conditions"):
			waiting = true
		default:
			return false
		}
	}
	return waiting
}

func finished(sts []forgejo.CommitStatus) bool {
	for _, st := range sts {
		if !done(st.State) {
			return false
		}
	}
	return len(sts) > 0
}

func failed(state string) bool { return state == "failure" || state == "error" }

func describe(st forgejo.CommitStatus) string {
	x := st.Context + ": " + st.State
	if d := strings.TrimSpace(st.Description); d != "" {
		x += " (" + d + ")"
	}
	if st.TargetURL != "" {
		x += " " + st.TargetURL
	}
	return x
}

func Evaluate(statuses []forgejo.CommitStatus, required []string, ignore string) Eval {
	latest := Latest(statuses, ignore)
	ev := Eval{Verdict: Pending, Latest: latest}
	have := map[string]forgejo.CommitStatus{}
	for _, st := range latest {
		have[st.Context] = st
	}
	consider := latest
	if len(required) > 0 {
		consider = nil
		for _, c := range required {
			if st, ok := have[c]; ok {
				consider = append(consider, st)
			}
		}
	}
	ev.Started = len(consider) > 0
	for _, st := range consider {
		if failed(st.State) {
			ev.Verdict, ev.Why = Fail, describe(st)
			return ev
		}
	}
	for _, st := range consider {
		if st.State == "cancelled" {
			ev.Verdict, ev.Why = Infra, describe(st)+": the job was cancelled, which says nothing about the commit"
			return ev
		}
	}
	for _, st := range consider {
		if len(required) == 0 || st.State != "skipped" {
			continue
		}
		for _, other := range latest {
			if failed(other.State) {
				ev.Verdict, ev.Why = Fail, describe(st)+": a required job was skipped after "+describe(other)
				return ev
			}
		}
		ev.Verdict, ev.Why = Infra, describe(st)+": a required job was skipped although no job failed, so nothing "+
			"tested this commit (check the workflow's if: and path filters)"
		return ev
	}
	if len(required) > 0 {
		var missing []string
		for _, c := range required {
			if _, ok := have[c]; !ok {
				missing = append(missing, c)
			}
		}
		if len(missing) > 0 {
			ev.Why, ev.Missing = "waiting for "+strings.Join(missing, ", "), missing
			return ev
		}
	}
	if len(consider) == 0 {
		ev.Why = "no CI status yet"
		return ev
	}
	var running []string
	for _, st := range consider {
		if !done(st.State) {
			running = append(running, st.Context)
		}
	}
	if len(running) > 0 {
		ev.Why = "running: " + strings.Join(running, ", ")
		return ev
	}
	if len(required) == 0 {
		ran := false
		for _, st := range consider {
			if st.State != "skipped" {
				ran = true
			}
		}
		if !ran {
			ev.Verdict, ev.Why = Infra, "every CI job on the commit was skipped, so nothing was tested (list the "+
				"jobs that must run in ci.contexts)"
			return ev
		}
	}
	ev.Verdict = Pass
	names := make([]string, len(consider))
	for i, st := range consider {
		names[i] = st.Context
	}
	ev.Why = "passed: " + strings.Join(names, ", ")
	return ev
}

type Runner struct {
	Spec   config.CISpec
	Forge  Forge
	Rep    *reporter.Reporter
	Ignore string
	Sleep  func(float64)
	Clock  func() time.Time
	Poll   func() string
	Seen   func(url string)
	Hurry  func() bool

	cancelled string
}

func NewRunner(spec config.CISpec, forge Forge, rep *reporter.Reporter, ignore string, sleep func(float64)) *Runner {
	if sleep == nil {
		sleep = func(s float64) { time.Sleep(time.Duration(s * float64(time.Second))) }
	}
	return &Runner{Spec: spec, Forge: forge, Rep: rep, Ignore: ignore, Sleep: sleep, Clock: time.Now}
}

func (r *Runner) poll() bool {
	why := ""
	if r.Poll != nil {
		why = r.Poll()
	}
	if why != "" {
		r.cancelled = why
	}
	return why != ""
}

func (r *Runner) wait(secs float64) bool {
	left := secs
	for !r.poll() {
		if left <= 0 || r.Hurry != nil && r.Hurry() {
			return true
		}
		step := min(left, 1)
		r.Sleep(step)
		left -= step
	}
	return false
}

func (r *Runner) both(log, msg string) {
	r.Rep.Say(msg)
	if f, err := openLog(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY); err == nil {
		f.WriteString(msg + "\n")
		f.Close()
	}
}

const tailBytes = 64 << 10

func Since(statuses, old []forgejo.CommitStatus) []forgejo.CommitStatus {
	before, runs := newest(old), map[string]bool{}
	for _, st := range old {
		if st.TargetURL != "" {
			runs[st.TargetURL] = true
		}
	}
	var out []forgejo.CommitStatus
	for _, st := range statuses {
		if st.ID > before && !runs[st.TargetURL] {
			out = append(out, st)
		}
	}
	return out
}

func newest(statuses []forgejo.CommitStatus) int64 {
	var n int64
	for _, st := range statuses {
		n = max(n, st.ID)
	}
	return n
}

func RunURL(log string) string {
	b, _, err := safefile.ReadTail(log, tailBytes)
	if err != nil {
		return ""
	}
	lines := gitx.Lines(strings.ToValidUTF8(string(b), "\uFFFD"))
	for i := len(lines) - 1; i >= 0; i-- {
		f := strings.Fields(lines[i])
		for j := len(f) - 1; j >= 0; j-- {
			if strings.HasPrefix(f[j], "https://") || strings.HasPrefix(f[j], "http://") {
				return f[j]
			}
		}
	}
	return ""
}

func LogResult(log string) string {
	b, _, err := safefile.ReadTail(log, tailBytes)
	if err != nil {
		return ""
	}
	lines := gitx.Lines(strings.ToValidUTF8(string(b), "�"))
	for i := len(lines) - 1; i >= 0; i-- {
		if x := strings.TrimSpace(lines[i]); x != "" {
			return strings.TrimPrefix(x, "-- ")
		}
	}
	return ""
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func (r *Runner) Run(sha, log string) *Run {
	run := &Run{Used: Name, Log: log}
	r.cancelled = ""
	err := ensureLogDir(filepath.Dir(log))
	var f *os.File
	if err == nil {
		f, err = openLog(log, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	}
	if err != nil {
		why := fmt.Sprintf("the CI gate could not be started: cannot write its log: %s", err)
		r.Rep.Say("-- " + why)
		run.Outcome, run.InfraWhy = Infra, why
		return run
	}
	f.Close()
	branch := r.Spec.Staging
	var old []forgejo.CommitStatus
	for try := 1; ; try++ {
		var err error
		if old, err = r.Forge.Statuses(sha); err == nil {
			break
		}
		if try == 3 {
			run.Outcome = Infra
			run.InfraWhy = fmt.Sprintf("the CI statuses %s already has could not be read (%s): an infrastructure failure, "+
				"not the branch's", short(sha), err)
			r.both(log, "-- "+run.InfraWhy)
			return run
		}
		if !r.wait(1) {
			r.both(log, "-- the gate was stopped: "+r.cancelled)
			run.Outcome, run.InfraWhy = Cancelled, "cancelled: "+r.cancelled
			return run
		}
	}
	if n := len(Latest(old, r.Ignore)); n > 0 {
		r.both(log, fmt.Sprintf("-- %s already has CI statuses for %d context(s): only those of a new run count",
			short(sha), n))
	}
	r.Rep.Say(fmt.Sprintf("-- gate: pushing %s to %s for CI (log: %s)", short(sha), branch, log))
	if err := r.Forge.Push(sha, branch); err != nil {
		run.Outcome = Infra
		run.InfraWhy = fmt.Sprintf("%s could not be pushed to the staging branch %s (%s): an infrastructure failure, "+
			"not the branch's", short(sha), branch, err)
		r.both(log, "-- "+run.InfraWhy)
		return run
	}
	if f, err := openLog(log, os.O_APPEND|os.O_WRONLY); err == nil {
		fmt.Fprintf(f, "-- gate: %s pushed to %s for CI\n", sha, branch)
		f.Close()
	}
	start := r.Clock()
	every := float64(max(r.Spec.PollInterval, 1))
	last, errs, seen, linked, waiting := "", 0, false, false, false
	var missingSince time.Time
	var reported []string
	reportedDone := false
	for {
		if r.poll() {
			r.both(log, "-- the gate was stopped: "+r.cancelled)
			run.Outcome, run.InfraWhy = Cancelled, "cancelled: "+r.cancelled
			return run
		}
		statuses, err := r.Forge.Statuses(sha)
		if err != nil {
			errs++
			r.both(log, fmt.Sprintf("-- cannot read the CI statuses of %s (%s)", short(sha), err))
			if errs >= 5 {
				run.Outcome = Infra
				run.InfraWhy = fmt.Sprintf("the CI statuses of %s could not be read %d times in a row (%s): an "+
					"infrastructure failure, not the branch's", short(sha), errs, err)
				r.both(log, "-- "+run.InfraWhy)
				return run
			}
		} else {
			errs = 0
			ev := Evaluate(Since(statuses, old), r.Spec.Contexts, r.Ignore)
			waiting = queued(ev.Latest)
			reported = reported[:0]
			for _, st := range ev.Latest {
				reported = append(reported, st.Context)
			}
			reportedDone = finished(ev.Latest)
			if len(ev.Missing) == 0 || !finished(ev.Latest) {
				missingSince = time.Time{}
			} else if missingSince.IsZero() {
				missingSince = r.Clock()
			}
			grace := time.Duration(max(r.Spec.StartTimeout, 0)) * time.Second
			if !missingSince.IsZero() && grace > 0 && r.Clock().Sub(missingSince) >= grace {
				run.Outcome = Infra
				run.InfraWhy = fmt.Sprintf("the run finished without %s; it reported only %s (a typo in ci.contexts?): an "+
					"infrastructure failure, not the branch's", strings.Join(ev.Missing, ", "), strings.Join(reported, ", "))
				r.both(log, "-- "+run.InfraWhy)
				return run
			}
			var shown []string
			for _, st := range ev.Latest {
				shown = append(shown, describe(st))
			}
			if now := strings.Join(shown, "\n"); now != last {
				for _, line := range shown {
					if !slices.Contains(strings.Split(last, "\n"), line) {
						r.both(log, "   "+line)
					}
				}
				last = now
			}
			seen = seen || ev.Started
			if !linked {
				for _, st := range ev.Latest {
					if st.TargetURL != "" {
						linked = true
						if r.Seen != nil {
							r.Seen(st.TargetURL)
						}
						break
					}
				}
			}
			switch ev.Verdict {
			case Pass:
				r.both(log, "-- CI "+ev.Why)
				run.Outcome = Pass
				return run
			case Fail:
				r.both(log, "-- CI failed: "+ev.Why)
				run.Outcome = Fail
				return run
			case Infra:
				run.Outcome = Infra
				run.InfraWhy = ev.Why + ": an infrastructure failure, not the branch's"
				r.both(log, "-- "+run.InfraWhy)
				return run
			}
		}
		took := r.Clock().Sub(start)
		jobsRunning := len(reported) > 0 && !reportedDone
		if !seen && !jobsRunning && r.Spec.StartTimeout > 0 && took >= time.Duration(r.Spec.StartTimeout)*time.Second {
			run.Outcome = Infra
			if len(reported) > 0 {
				run.InfraWhy = fmt.Sprintf("none of ci.contexts (%s) appeared on %s within %d s; the run reported only %s "+
					"(a typo in ci.contexts?): an infrastructure failure, not the branch's",
					strings.Join(r.Spec.Contexts, ", "), short(sha), r.Spec.StartTimeout, strings.Join(reported, ", "))
				r.both(log, "-- "+run.InfraWhy)
				return run
			}
			run.InfraWhy = fmt.Sprintf("no CI status appeared on %s within %d s of pushing it to %s (does a workflow run "+
				"on pushes to %s?): an infrastructure failure, not the branch's", short(sha), r.Spec.StartTimeout, branch,
				branch)
			r.both(log, "-- "+run.InfraWhy)
			return run
		}
		if r.Spec.Timeout > 0 && took >= time.Duration(r.Spec.Timeout)*time.Second {
			run.Outcome = Infra
			hint := ""
			if waiting {
				hint = "; its jobs never left \"Waiting to run\": is a runner with the workflow's runs-on label online?"
			}
			run.InfraWhy = fmt.Sprintf("CI gave no result for %s within %d s (log: %s)%s: an infrastructure failure, "+
				"not the branch's", short(sha), r.Spec.Timeout, log, hint)
			r.both(log, "-- "+run.InfraWhy)
			return run
		}
		if !r.wait(every) {
			r.both(log, "-- the gate was stopped: "+r.cancelled)
			run.Outcome, run.InfraWhy = Cancelled, "cancelled: "+r.cancelled
			return run
		}
	}
}

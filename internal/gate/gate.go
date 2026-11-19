package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"murasame/internal/config"
	"murasame/internal/gitx"
	"murasame/internal/reporter"
	"murasame/internal/safefile"
	"murasame/internal/secretenv"
	"murasame/internal/states"
)

type Attempt struct {
	Gate string
	Status
}

func Groups(pgids []int) string {
	out := make([]string, len(pgids))
	for i, g := range pgids {
		out[i] = fmt.Sprint(g)
	}
	return strings.Join(out, " ")
}

type Run struct {
	Outcome  string
	Used     string
	Retried  bool
	Log      string
	InfraWhy string
	Record   map[string]string
}

type Runner struct {
	Cfg   *config.Config
	Rep   *reporter.Reporter
	Vars  config.Vars
	Asked string
	Sleep func(float64)
	Probe *Probe

	Poll func() string

	cancelled string
}

func NewRunner(cfg *config.Config, rep *reporter.Reporter, vars config.Vars, asked string, sleep func(float64)) *Runner {
	if sleep == nil {
		sleep = func(s float64) { time.Sleep(time.Duration(s * float64(time.Second))) }
	}
	return &Runner{Cfg: cfg, Rep: rep, Vars: vars, Asked: asked, Sleep: sleep}
}

func (r *Runner) Select() string {
	if r.Asked != "" {
		return r.Asked
	}
	return r.Cfg.DefaultGate
}

func (r *Runner) Once(gate, log string, retried bool) *Attempt {
	spec := r.Cfg.Gates[gate]
	rt := "0"
	if retried {
		rt = "1"
	}
	vars := r.Vars.With("retried", rt, "gate", gate)
	integ := "."
	if v, ok := r.Vars["integ"]; ok {
		integ = v
	}
	argv := config.ExpandAll(spec.Command.Args, vars)
	env := map[string]string{}
	for k, v := range r.Cfg.GateEnv {
		env[k] = config.Expand(v, vars)
	}
	for k, v := range spec.Env {
		env[k] = config.Expand(v, vars)
	}
	var poll func() bool
	if r.Poll != nil {
		poll = r.poll
	}
	st, _ := RunStreamed(argv, StreamOpts{Cwd: integ, Env: env, Log: log, Append: true,
		Echo: r.Rep.Say, Timeout: time.Duration(spec.Timeout) * time.Second, Poll: poll,
		Prefix: config.ExpandAll(r.Cfg.CommandPrefix, vars), Reap: true})
	att := &Attempt{Gate: gate, Status: st}
	switch {
	case !st.Clean():
		r.both(log, "-- the gate left "+st.Leftover())
	case st.Cancelled && r.cancelled != "":
		r.both(log, "-- the gate was stopped: "+r.cancelled)
	case st.TimedOut:
		r.both(log, fmt.Sprintf("-- the %s gate timed out after %d s", gate, spec.Timeout))
	}
	return att
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

func Classify(att *Attempt) string {
	return string(states.GateVerdict(att.State, att.Code))
}

func describe(att *Attempt) string {
	switch states.AttemptOutcome(att.State) {
	case states.AttemptTimedOut:
		return "timed out"
	case states.AttemptStartFailed:
		return "the command could not be started"
	case states.AttemptCancelled:
		return "cancelled"
	}
	switch {
	case att.Code < 0:
		return fmt.Sprintf("killed by signal %d", -att.Code)
	case att.Code == 127:
		return "exit 127: the command could not be run"
	}
	return fmt.Sprintf("exit %d", att.Code)
}

const tailBytes = 64 << 10

func (r *Runner) LogResult(log string) string {
	b, _, err := safefile.ReadTail(log, tailBytes)
	if err != nil {
		return ""
	}
	lines := gitx.Lines(strings.ToValidUTF8(string(b), "�"))
	for i := len(lines) - 1; i >= 0; i-- {
		if x := strings.TrimSpace(lines[i]); x != "" {
			return x
		}
	}
	return ""
}

func (r *Runner) wait(secs float64) bool {
	if r.Poll == nil {
		r.Sleep(secs)
		return true
	}
	left := secs
	for !r.poll() {
		if left <= 0 {
			return true
		}
		step := left
		if step > 1 {
			step = 1
		}
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

type Probe struct {
	Prefix   []string
	Dir      string
	Env      map[string]string
	StateDir string

	mu   sync.Mutex
	done bool
	why  string
}

func (p *Probe) Check() string {
	if p == nil || len(p.Prefix) == 0 {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return p.why
	}
	p.done = true
	truth, err := findProgram("true", secretenv.ChildEnv(p.Env), "")
	if err != nil {
		truth = "true"
	}
	log := reporter.FreshLog(p.StateDir, time.Now().Format("20060102-150405")+"-command-prefix.probe")
	prefix := strings.Join(p.Prefix, " ")
	for _, direct := range []bool{true, false} {
		st, lines := RunStreamed([]string{truth}, StreamOpts{Cwd: p.Dir, Env: p.Env, Log: log, Prefix: p.Prefix,
			Timeout: probeTimeout, Reap: true, Direct: direct, Append: !direct})
		if st.Verdict() == states.VerdictPass {
			continue
		}
		if !st.Clean() {
			p.why = fmt.Sprintf("the command_prefix %s left %s (log: %s): "+
				"stopping so that integ is not reused while they may still run", prefix, st.Leftover(), log)
			return p.why
		}
		tail := ""
		for i := len(lines) - 1; i >= 0; i-- {
			if x := strings.TrimSpace(lines[i]); x != "" {
				tail = ": " + x
				break
			}
		}
		att := &Attempt{Status: Status{State: st.State, Code: st.Code}}
		what := fmt.Sprintf("cannot run commands (%s %s", prefix, truth)
		if !direct {
			what = fmt.Sprintf("cannot run murasame's exec helper (%s %s %s <nonce> -- %s", prefix, execHelper, ExecArg,
				truth)
		}
		p.why = fmt.Sprintf("the command_prefix %s %s: %s; log: %s)%s: an infrastructure failure, not the branch's",
			prefix, what, describe(att), log, tail)
		return p.why
	}
	return ""
}

var probeTimeout = 60 * time.Second

func (r *Runner) Run(gate, log string) *Run {
	r.Rep.Say(fmt.Sprintf("-- gate (%s): %s", gate, log))
	r.cancelled = ""
	spec := r.Cfg.Gates[gate]
	run := &Run{Used: gate, Log: log}
	err := ensureLogDir(filepath.Dir(log))
	var f *os.File
	if err == nil {
		f, err = openLog(log, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	}
	if err != nil {
		why := fmt.Sprintf("the %s gate could not be started: cannot write its log: %s", gate, err)
		r.Rep.Say("-- " + why)
		run.Outcome, run.InfraWhy = "infra", why
		return run
	}
	f.Close()
	if why := r.Probe.Check(); why != "" {
		r.both(log, "-- "+why)
		run.Outcome, run.InfraWhy = "infra", why
		return run
	}
	for try := 0; ; try++ {
		att := r.Once(gate, log, run.Retried)
		if st := att.Status; !st.Clean() {
			run.Outcome = "infra"
			run.InfraWhy = fmt.Sprintf("the %s gate left %s (log: %s): stopping "+
				"so that integ is not reused while they may still run", gate, st.Leftover(), log)
			return run
		}
		if r.cancelled != "" {
			run.Outcome, run.InfraWhy = "cancelled", "cancelled: "+r.cancelled
			return run
		}
		run.Outcome = Classify(att)
		if run.Outcome != "infra" {
			return run
		}
		why := describe(att)
		if try >= spec.Retries {
			run.InfraWhy = fmt.Sprintf("the %s gate did not give a result in %d attempt(s) (%s; log: %s)", gate,
				try+1, why, log)
			return run
		}
		run.Retried = true
		r.both(log, fmt.Sprintf("-- infrastructure failure, not a test result (%s: %s): retry %d of %d in %d s",
			gate, why, try+1, spec.Retries, spec.RetryWait))
		if !r.wait(float64(spec.RetryWait)) {
			r.both(log, "-- the gate was stopped: "+r.cancelled)
			run.Outcome, run.InfraWhy = "cancelled", "cancelled: "+r.cancelled
			return run
		}
	}
}

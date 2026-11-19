package hooks

import (
	"fmt"
	"os"
	"strings"
	"time"

	"murasame/internal/config"
	"murasame/internal/gate"
	"murasame/internal/gitx"
	"murasame/internal/reporter"
	"murasame/internal/states"
)

type Result struct {
	State states.MergeState
	Why   string
}

func Done() Result { return Result{State: states.MergeDone} }

func Aside(why string) Result { return Result{State: states.MergeAside, Why: why} }

func Infra(why string) Result { return Result{State: states.MergeInfra, Why: why} }

func (r Result) OK() bool { return r.State == states.MergeDone }

func (r Result) Event() states.MergeEvent {
	switch r.State {
	case states.MergeDone:
		return states.MergeOnDone
	case states.MergeAside:
		return states.MergeOnFail
	case states.MergeInfra:
		return states.MergeOnInfra
	}
	panic(&states.InvalidTransition{Msg: fmt.Sprintf("hooks: a result in state %q", r.State)})
}

type Hooks struct {
	Cfg      *config.Config
	Rep      *reporter.Reporter
	Vars     config.Vars
	StateDir string

	Poll      func() string
	Probe     *gate.Probe
	cancelled string
}

func New(cfg *config.Config, rep *reporter.Reporter, vars config.Vars, stateDir string) *Hooks {
	return &Hooks{Cfg: cfg, Rep: rep, Vars: vars, StateDir: stateDir}
}

func (h *Hooks) poll() bool {
	why := ""
	if h.Poll != nil {
		why = h.Poll()
	}
	if why != "" {
		h.cancelled = why
	}
	return why != ""
}

func (h *Hooks) prefix() []string {
	return config.ExpandAll(h.Cfg.CommandPrefix, h.Vars)
}

func (h *Hooks) logPath(slug, name string) string {
	return reporter.FreshLog(h.StateDir, time.Now().Format("20060102-150405")+"-"+slug+"."+name)
}

func expandEnv(env map[string]string, vars config.Vars) map[string]string {
	out := map[string]string{}
	for k, v := range env {
		out[k] = config.Expand(v, vars)
	}
	return out
}

func tail(lines []string, n int) string {
	var out []string
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if x := strings.TrimSpace(lines[i]); x != "" {
			out = append([]string{x}, out...)
		}
	}
	return strings.Join(out, " / ")
}

type Command struct {
	What    string
	Name    string
	Slug    string
	Dir     string
	Run     config.Cmd
	Extra   []string
	Env     map[string]string
	Timeout int
	Vars    config.Vars
}

func (h *Hooks) Run(c Command) Result {
	if why := h.Probe.Check(); why != "" {
		return Infra(why)
	}
	log := h.logPath(c.Slug, c.Name)
	h.cancelled = ""
	var poll func() bool
	if h.Poll != nil {
		poll = h.poll
	}
	argv := append(config.ExpandAll(c.Run.Args, c.Vars), c.Extra...)
	st, lines := gate.RunStreamed(argv, gate.StreamOpts{Cwd: c.Dir, Env: expandEnv(c.Env, c.Vars), Log: log,
		Timeout: time.Duration(c.Timeout) * time.Second, Poll: poll, Prefix: h.prefix(), Reap: true})
	switch st.CommandVerdict() {
	case states.VerdictOK:
		return Done()
	case states.VerdictAside:
		why := fmt.Sprintf("%s failed (exit %d; log: %s)", c.What, st.Code, log)
		if t := tail(lines, 5); t != "" {
			why += ": " + t
		}
		return Aside(why)
	}
	switch {
	case !st.Clean():
		return Infra(fmt.Sprintf("%s left %s (log: %s): stopping so "+
			"that integ is not reused while they may still run", c.What, st.Leftover(), log))
	case st.StartFailed:
		why := fmt.Sprintf("%s could not be started (log: %s): an infrastructure failure", c.What, log)
		if t := tail(lines, 1); t != "" {
			why += ": " + t
		}
		return Infra(why)
	case st.Cancelled:
		why := h.cancelled
		if why == "" {
			why = c.What + " was stopped"
		}
		return Infra("cancelled: " + why)
	case st.TimedOut:
		return Infra(fmt.Sprintf("%s was stopped after %d s, its timeout (log: %s): an infrastructure failure",
			c.What, c.Timeout, log))
	case st.Code < 0:
		return Infra(fmt.Sprintf("%s was killed by signal %d (log: %s): an infrastructure failure", c.What,
			-st.Code, log))
	}
	return Infra(fmt.Sprintf("%s ended as %s (log: %s): an infrastructure failure", c.What, st.State, log))
}

func (h *Hooks) PostMerge(tree *gitx.Git, branch, slug string) Result {
	for _, hook := range h.Cfg.PostMerge {
		h.Rep.Say("-- post-merge hook " + hook.Name)
		head, on := tree.Head(), tree.SymbolicHead()
		before := tree.Snapshot()
		r := h.Run(Command{What: "the post-merge hook " + hook.Name, Name: hook.Name, Slug: slug, Dir: tree.Cwd,
			Run: hook.Run, Env: hook.Env, Timeout: hook.Timeout, Vars: h.Vars.With("branch", branch, "hook", hook.Name)})
		if !r.OK() {
			return r
		}
		if now, at := tree.Rev("HEAD"), tree.SymbolicHead(); at != on || now == "" || !tree.IsAncestor(head, now) {
			return Aside(fmt.Sprintf("the post-merge hook %s moved integ's HEAD from %s to %s%s (it may add commits, "+
				"never drop or switch them)", hook.Name, short(head), short(now), switched(on, at)))
		}
		changed := tree.ChangedSince(before)
		tree.Stage(changed)
		if tree.StagedChanges() {
			tree.Commit(fmt.Sprintf("murasame: %s after merging %s", hook.Name, branch))
			h.Rep.Say(fmt.Sprintf("-- %s changed %d file(s): committed", hook.Name, len(changed)))
		}
	}
	return Done()
}

func short(s string) string {
	if s == "" {
		return "-"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func switched(was, now string) string {
	if was == now {
		return ""
	}
	if was == "" {
		was = "a detached HEAD"
	}
	if now == "" {
		now = "a detached HEAD"
	}
	return fmt.Sprintf(" and switched from %s to %s", was, now)
}

var checkoutSafe = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

const checkoutTimeout = 10 * time.Minute

func (h *Hooks) checkout(repo *gitx.Git, name, dir, sha string) string {
	env := repo.WithoutCredentials().Env
	args := append(append([]string{}, checkoutSafe...), "checkout", "-q", sha)
	prefix := h.prefix()
	if len(prefix) == 0 {
		if r := gitx.New(dir, env).Run(args...); !r.OK() {
			return gitx.LastLine(r.Out)
		}
		return ""
	}
	log := reporter.FreshLog(h.StateDir, time.Now().Format("20060102-150405")+"."+name+".checkout")
	st, lines := gate.RunStreamed(append([]string{"git"}, args...), gate.StreamOpts{Cwd: dir, Env: env, Log: log,
		Timeout: checkoutTimeout, Prefix: prefix, Reap: true})
	if st.CommandVerdict() == states.VerdictOK && st.Clean() {
		return ""
	}
	why := fmt.Sprintf("git checkout ended as %s, exit %d (log: %s)", st.State, st.Code, log)
	if t := tail(lines, 1); t != "" {
		why += ": " + t
	}
	return why
}

func (h *Hooks) PostLand(repo *gitx.Git, landed string, skip map[string]bool) {
	sha := repo.Out("rev-parse", landed+"^{commit}")
	for _, hook := range h.Cfg.PostLand {
		if skip[hook.Name] {
			h.Rep.Say(fmt.Sprintf("-- post-land hook %s: skipped", hook.Name))
			continue
		}
		if why := h.Probe.Check(); why != "" {
			h.Rep.Say(fmt.Sprintf("-- WARNING: the post-land hook %s did not run: %s; the target branch itself is fine",
				hook.Name, why))
			continue
		}
		vars := h.Vars.With("hook", hook.Name, "target_sha", sha, "target_short", repo.Short(sha, 0))
		dir := h.Vars["repo"]
		if hook.Dir != "" {
			dir = config.Expand(hook.Dir, vars)
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				h.Rep.Say(fmt.Sprintf("-- WARNING: the post-land hook %s did not run: no directory %s", hook.Name, dir))
				continue
			}
			if hook.Checkout {
				if why := h.checkout(repo, hook.Name, dir, sha); why != "" {
					h.Rep.Say(fmt.Sprintf("-- WARNING: the post-land hook %s did not run: could not check %s out at %s: %s",
						hook.Name, dir, repo.Short(sha, 0), why))
					continue
				}
			}
		}
		h.Rep.Say(fmt.Sprintf("-- post-land hook %s at %s", hook.Name, repo.Short(sha, 0)))
		log := reporter.FreshLog(h.StateDir, time.Now().Format("20060102-150405")+"."+hook.Name)
		st, _ := gate.RunStreamed(config.ExpandAll(hook.Run.Args, vars), gate.StreamOpts{Cwd: dir, Env: expandEnv(hook.Env, vars),
			Log: log, Echo: h.Rep.Say, Timeout: time.Duration(hook.Timeout) * time.Second, Prefix: h.prefix(), Reap: true})
		if !st.Clean() {
			h.Rep.Say(fmt.Sprintf("-- WARNING: the post-land hook %s left %s (log: %s); the target branch itself is fine",
				hook.Name, st.Leftover(), log))
		}
		switch {
		case st.StartFailed:
			h.Rep.Say(fmt.Sprintf("-- WARNING: the post-land hook %s could not be started (log: %s); the target branch "+
				"itself is fine", hook.Name, log))
		case st.TimedOut:
			h.Rep.Say(fmt.Sprintf("-- WARNING: the post-land hook %s was stopped after %d s, its timeout (log: %s); "+
				"the target branch itself is fine", hook.Name, hook.Timeout, log))
		case st.Code < 0:
			h.Rep.Say(fmt.Sprintf("-- WARNING: the post-land hook %s was killed by signal %d (log: %s); the target branch "+
				"itself is fine", hook.Name, -st.Code, log))
		case st.Code != 0:
			h.Rep.Say(fmt.Sprintf("-- WARNING: the post-land hook %s failed (exit %d; log: %s); the target branch itself "+
				"is fine", hook.Name, st.Code, log))
		}
	}
}

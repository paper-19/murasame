package engine

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"murasame/internal/config"
	"murasame/internal/conflicts"
	"murasame/internal/db"
	"murasame/internal/gate"
	"murasame/internal/gitx"
	"murasame/internal/hooks"
	"murasame/internal/panics"
	"murasame/internal/reporter"
	"murasame/internal/states"
	"murasame/internal/store"
)

func Safe(name string) string { return strings.NewReplacer("/", "_", " ", "_").Replace(name) }

type Item struct {
	Name    string
	Ref     string
	Message string
	Slug    string
	Sha     string
}

func (e *Engine) tip(item Item) string {
	if item.Sha != "" {
		return item.Sha
	}
	return e.G.CommitOf(item.Ref)
}

func NewItem(name, ref, message, slug string) Item {
	if ref == "" {
		ref = name
	}
	if slug == "" {
		slug = Safe(name)
	}
	return Item{Name: name, Ref: ref, Message: message, Slug: slug}
}

type Outcome = store.Outcome

type RunResult struct {
	Code     int
	Outcomes map[string]Outcome
	Merged   []string
	SetAside []string
	Gates    int
	Stopped  string
}

type Events interface {
	Gating(items []Item, log string)
	Outcome(item Item, o Outcome)
}

type Poller interface{ Poll() string }

type Locker interface{ Locked(held bool) }

type Throwaway interface{ Throwaway(cleanup func()) }

type Slotter interface {
	GateSlot(what string) string
	GateDone()
}

func AndList(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

type Lander interface {
	Precheck() string
	Land(sha string) string
}

type LocalLander struct {
	Repo   *gitx.Git
	Target string
}

func (l *LocalLander) Precheck() string {
	if l.Repo.SymbolicHead() != l.Target {
		return "the main checkout is not on " + l.Target
	}
	return ""
}

func (l *LocalLander) Land(sha string) string {
	if why := l.Precheck(); why != "" {
		return why + " any more: refusing to fast-forward it"
	}
	if !l.Repo.OK("merge", "--ff-only", "-q", sha) {
		return l.Target + " could not fast-forward to integ (the main checkout has local changes in the way?)"
	}
	now := l.Repo.Rev("refs/heads/" + l.Target)
	if now != sha {
		shown := now
		if shown == "" {
			shown = "-"
		}
		return fmt.Sprintf("%s is at %s after the fast-forward, not at the gated %s (the main checkout changed during it?)",
			l.Target, short(shown), short(sha))
	}
	return ""
}

type PushLander struct {
	Repo   *gitx.Git
	Remote string
	Target string
	stale  string
}

func (l *PushLander) Stale() string { return l.stale }

func (l *PushLander) Precheck() string { return "" }

func (l *PushLander) Land(sha string) string {
	r := l.Repo.AuthRun("push", "-q", l.Remote, sha+":refs/heads/"+l.Target)
	if !r.OK() {
		return fmt.Sprintf("%s could not fast-forward to integ on the forge (the push was refused: %s)", l.Target,
			gitx.LastLine(r.Out))
	}
	if u := l.Repo.WithoutCredentials().Run("-c", "core.hooksPath=/dev/null", "update-ref", "refs/heads/"+l.Target,
		sha); !u.OK() {
		l.stale = fmt.Sprintf("%s was pushed to %s, but the local refs/heads/%s could not follow (%s): stopping before "+
			"anything is built on it", l.Target, short(sha), l.Target, gitx.LastLine(u.Out))
	}
	return ""
}

func colon(result string) string {
	if result == "" {
		return ""
	}
	return ": " + result
}

func paren(result string) string {
	if result == "" {
		return ""
	}
	return " (" + result + ")"
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

type stopSignal struct{ why string }

type cancelSignal struct{ why string }

type failedTree struct {
	log   string
	items []Item
	base  string
}

type built struct {
	item Item
	sha  string
	tip  string
	from string
	to   string
	note string
}

type gatedState struct {
	head  string
	tree  string
	dirty bool
}

type Options struct {
	Gate            string
	SkipHooks       map[string]bool
	Regate          bool
	Lander          Lander
	Events          Events
	Stream          io.Writer
	Sleep           func(float64)
	Sink            func(string)
	GitEnv          map[string]string
	RepoKey         string
	ResetStaleInteg bool
}

type Engine struct {
	Cfg             *config.Config
	Main            string
	G               *gitx.Git
	GitEnv          map[string]string
	Target          string
	StateDir        string
	IntegDir        string
	Integ           *gitx.Git
	Vars            config.Vars
	Rep             *reporter.Reporter
	Gate            *gate.Runner
	Hooks           *hooks.Hooks
	Merger          *conflicts.Merger
	Records         *store.GateRecords
	SkipHooks       map[string]bool
	Regate          bool
	Lander          Lander
	Events          Events
	Res             *RunResult
	RunID           int64
	repoKey         string
	resetStaleInteg bool
	repo            *db.Repo
	failedTrees     map[string]*failedTree
	gateName        string
	steps           int
	integ           *states.Tracker[states.IntegState, states.IntegEvent]
	gated           map[*gate.Run]gatedState
	landed          string
	integRef        string
	integRefKnown   bool
}

func New(cfg *config.Config, repoDir string, o Options) (e *Engine, err error) {
	defer gitx.Catch(&err)
	main := gitx.New(repoDir, o.GitEnv).MainCheckout()
	e = &Engine{Cfg: cfg, Main: main, G: gitx.New(main, o.GitEnv), GitEnv: map[string]string{}, repoKey: o.RepoKey,
		resetStaleInteg: o.ResetStaleInteg, SkipHooks: o.SkipHooks, Regate: o.Regate, Events: o.Events,
		Res: &RunResult{Outcomes: map[string]Outcome{}}, failedTrees: map[string]*failedTree{},
		integ: states.IntegMachine.Start()}
	for k, v := range o.GitEnv {
		e.GitEnv[k] = v
	}
	if e.SkipHooks == nil {
		e.SkipHooks = map[string]bool{}
	}
	e.Target = e.target()
	sd := config.Expand(cfg.StateDir, config.Vars{"repo": main})
	if !filepath.IsAbs(sd) {
		sd = filepath.Join(main, sd)
	}
	e.StateDir = sd
	integ := config.Expand(cfg.Integ, config.Vars{"repo": main, "state_dir": sd})
	if !filepath.IsAbs(integ) {
		integ = filepath.Join(main, integ)
	}
	e.IntegDir = integ
	e.Integ = gitx.New(integ, o.GitEnv)
	e.Vars = config.Vars{"repo": main, "state_dir": sd, "integ": integ, "target": e.Target}
	e.Rep = reporter.New(sd, o.Stream, o.Sink, e.historyLine)
	e.Gate = gate.NewRunner(cfg, e.Rep, e.Vars, o.Gate, o.Sleep)
	if p, ok := o.Events.(Poller); ok {
		e.Gate.Poll = p.Poll
	}
	e.Hooks = hooks.New(cfg, e.Rep, e.Vars, sd)
	e.Hooks.Poll = e.Gate.Poll
	probeEnv := map[string]string{}
	for k, v := range cfg.GateEnv {
		probeEnv[k] = config.Expand(v, e.Vars)
	}
	probe := &gate.Probe{Prefix: config.ExpandAll(cfg.CommandPrefix, e.Vars), Dir: integ, Env: probeEnv, StateDir: sd}
	e.Gate.Probe, e.Hooks.Probe = probe, probe
	e.Merger = &conflicts.Merger{Cfg: cfg, Hooks: e.Hooks}
	e.Records = store.NewGateRecords(cfg.Records, e.DB)
	e.Lander = o.Lander
	if e.Lander == nil {
		e.Lander = &LocalLander{Repo: e.G, Target: e.Target}
	}
	return e, nil
}

func (e *Engine) DB() (*db.Repo, error) {
	if e.repo == nil {
		path := db.Path(e.StateDir, config.Expand(e.Cfg.DB, e.Vars))
		key := e.repoKey
		if key == "" {
			key = config.Expand(e.Cfg.RepoKey, e.Vars)
		}
		if key == "" {
			key = db.LocalKey(e.Main)
		}
		d, err := db.Open(path, false)
		if err != nil {
			return nil, err
		}
		repo, err := db.Scope(d, key, "local", db.RepoInfo{Path: e.Main})
		if err != nil {
			d.Close()
			return nil, err
		}
		e.repo = repo
	}
	return e.repo, nil
}

func (e *Engine) history() (*store.History, error) {
	r, err := e.DB()
	if err != nil {
		return nil, err
	}
	return store.NewHistory(r), nil
}

func (e *Engine) bestEffort(what string, fn func() error) {
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				if re, ok := r.(error); ok {
					err = re
					return
				}
				panic(r)
			}
		}()
		return fn()
	}()
	if err != nil {
		e.Rep.Say(fmt.Sprintf("-- WARNING: the state DB: %s not recorded: %s", what, err))
	}
}

func (e *Engine) historyLine(kind, branches, line, at string) {
	e.bestEffort("a merges.log line", func() error {
		h, err := e.history()
		if err != nil {
			return err
		}
		return h.Line(e.RunID, kind, branches, line, at)
	})
}

func (e *Engine) historyBegin(items []Item, mode, args, source string) {
	members := make([]store.Member, len(items))
	for i, it := range items {
		members[i] = store.Member{Name: it.Name, Ref: it.Ref}
	}
	e.bestEffort("the run", func() error {
		h, err := e.history()
		if err != nil {
			return err
		}
		e.RunID, err = h.Begin(source, mode, e.Target, e.G.Rev("refs/heads/"+e.Target), args, members)
		return err
	})
}

func (e *Engine) closeAbandonedRuns() {
	e.bestEffort("closing earlier runs that never finished", func() error {
		h, err := e.history()
		if err != nil {
			return err
		}
		_, err = h.InterruptedGone("cli", "interrupted: the murasame run ended before it finished")
		return err
	})
}

func (e *Engine) historyFinish(note string) {
	rid := e.RunID
	if rid == 0 {
		return
	}
	e.bestEffort("the run's end", func() error {
		h, err := e.history()
		if err != nil {
			return err
		}
		return h.Finish(rid, e.Res.Code, e.Res.Gates, e.Res.Stopped, e.G.Rev("refs/heads/"+e.Target), note, e.Res.Outcomes)
	})
}

func (e *Engine) target() string {
	for _, t := range e.Cfg.Target {
		if e.G.Rev("refs/heads/"+t) != "" {
			return t
		}
	}
	return e.Cfg.Target[len(e.Cfg.Target)-1]
}

func (e *Engine) short(rev string) string { return e.G.Short(rev, 0) }

func (e *Engine) base() string { return e.G.Out("rev-parse", "refs/heads/"+e.Target) }

func (e *Engine) resetTo(rev string) {
	if e.integRefKnown {
		e.Integ.Reattach(e.integRef)
	}
	e.Integ.MustRun("reset", "-q", "--hard", rev)
	e.Integ.MustRun("clean", "-ffdq")
}

func (e *Engine) resetInteg() {
	e.Integ.Run("merge", "--abort")
	e.resetTo(e.base())
	e.integ.Must(states.IntegOnReset)
}

func (e *Engine) leaveInteg(failed bool) (why string) {
	st := e.integ.State()
	if st == states.IntegUnknown || (!failed && st == states.IntegAtTarget) {
		return ""
	}
	if failed && e.integ.Can(states.IntegOnDirty) {
		e.integ.Must(states.IntegOnDirty)
	}
	defer func() {
		if r := recover(); r != nil {
			why = fmt.Sprint(r)
		}
	}()
	e.resetInteg()
	return ""
}

func (e *Engine) settle(item Item, o Outcome) Outcome {
	from := states.OutcomePending
	if had, ok := e.Res.Outcomes[item.Name]; ok {
		from = had.Status
	}
	o.Status = states.OutcomeMachine.Must(from, o.Status)
	e.Res.Outcomes[item.Name] = o
	return o
}

func (e *Engine) outcome(item Item, o Outcome) {
	o = e.settle(item, o)
	if rid := e.RunID; rid != 0 {
		e.bestEffort(item.Name+"'s outcome", func() error {
			h, err := e.history()
			if err != nil {
				return err
			}
			return h.Outcome(rid, item.Name, o)
		})
	}
	if e.Events != nil {
		e.Events.Outcome(item, o)
	}
}

func (e *Engine) recordStopped(name, why string) {
	e.Rep.Record("stopped", name, "branch", name, "reason", why)
}

func names(items []Item) string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Name
	}
	return strings.Join(out, " ")
}

func (e *Engine) stop(items []Item, why string) {
	e.resetInteg()
	e.Rep.Say("")
	if len(items) > 0 {
		e.Rep.Say(fmt.Sprintf("MERGE QUEUE STOPPED at %s: %s", names(items), why))
	} else {
		e.Rep.Say("MERGE QUEUE STOPPED: " + why)
	}
	e.Rep.Say(fmt.Sprintf("  integ is reset to %s (%s); %s has every branch merged before it", e.Target,
		e.short(e.base()), e.Target))
	for _, i := range items {
		e.recordStopped(i.Name, why)
		e.outcome(i, Outcome{Status: states.OutcomeStopped, Why: why})
	}
	e.Res.Stopped = why
	panic(stopSignal{why})
}

func (e *Engine) setAside(item Item, why, log string, partners []string) {
	e.Rep.Say(fmt.Sprintf("-- SET ASIDE %s: %s", item.Name, why))
	e.recordStopped(item.Name, why)
	e.Res.SetAside = append(e.Res.SetAside, item.Name)
	e.outcome(item, Outcome{Status: states.OutcomeSetAside, Why: why, Log: log, Partners: append([]string{}, partners...)})
}

func flatten(q [][]Item) []Item {
	var out []Item
	for _, g := range q {
		out = append(out, g...)
	}
	return out
}

type pairResult struct {
	why      string
	partners []string
	log      string
}

type group = states.Tracker[states.GroupState, states.GroupEvent]

func mergeWhy(m conflicts.Merge) string {
	if m.State == states.MergeNeedsPerson {
		return "merge conflict that needs a person: " + m.Why
	}
	return m.Why
}

func (e *Engine) postMerge(stop func(string), item Item, tip, from string, m conflicts.Merge) conflicts.Merge {
	var r hooks.Result
	e.building(stop, func() {
		r = e.afterMerge(item)
		if r.OK() {
			if why := e.lost(item, tip, from); why != "" {
				r = hooks.Aside(why)
			}
		}
	})
	m.AfterHooks(r)
	return m
}

func (e *Engine) gateOutcome(g *group, gr *gate.Run) {
	if gr.Record != nil {
		g.Must(states.GroupOnRecord)
		return
	}
	g.Must(states.GroupOnGate)
	switch gr.Outcome {
	case "pass":
		g.Must(states.GroupOnPass)
	case "infra", "cancelled":
		g.Must(states.GroupOnInfra)
	default:
		g.Must(states.GroupOnFail)
	}
}

func (e *Engine) pairCheck(g *group, item Item, known *failedTree, queue [][]Item) (res *pairResult) {
	var partners []Item
	for _, i := range known.items {
		if i.Name != item.Name {
			partners = append(partners, i)
		}
	}
	if !e.Cfg.PairCheck || len(partners) == 0 {
		return nil
	}
	g.Must(states.GroupOnPair)
	e.Rep.Say(fmt.Sprintf("-- %s failed only on top of %s: gating it alone on %s %s (pair check; it does not land)",
		item.Name, names(partners), e.Target, e.short(known.base)))
	defer e.resetInteg()
	e.resetInteg()
	e.integ.Must(states.IntegOnBuild)
	e.resetTo(known.base)
	rest := append([]Item{item}, flatten(queue)...)
	stopRest := func(why string) {
		g.Must(states.GroupOnStop)
		e.stop(rest, why)
	}
	unbuilt := func(why string) *pairResult {
		g.Must(states.GroupOnAside)
		return &pairResult{fmt.Sprintf("on its own on %s %s, it does not build: %s (together with %s, the gate failed; "+
			"log: %s%s)", e.Target, e.short(known.base), why, AndList(itemNames(partners)), known.log,
			colon(e.Gate.LogResult(known.log))), nil, known.log}
	}
	tip := e.tip(item)
	var m conflicts.Merge
	e.building(stopRest, func() { m = e.merge(item, tip) })
	if m.State.Merged() {
		m = e.postMerge(stopRest, item, tip, known.base, m)
	}
	switch m.State {
	case states.MergeInfra:
		stopRest(m.Why)
	case states.MergeAside, states.MergeNeedsPerson:
		return unbuilt(mergeWhy(m))
	}
	e.integ.Must(states.IntegOnBuilt)
	what := "pair check of " + item.Name
	if solo, failed := e.failedTrees[e.Integ.TreeOf("HEAD")]; failed {
		g.Must(states.GroupOnAlone)
		return &pairResult{fmt.Sprintf("the gate failed on its own too (the same tree as an earlier gate run; log: %s)",
			solo.log) + colon(e.Gate.LogResult(solo.log)), nil, solo.log}
	}
	gr := e.gate(item.Slug+"-alone", []Item{item}, true, what)
	delete(e.gated, gr)
	if gr.Outcome == "cancelled" || gr.Outcome == "infra" {
		stopRest(gr.InfraWhy)
	}
	if gr.Outcome != "pass" {
		g.Must(states.GroupOnAlone)
		tree := e.Integ.TreeOf("HEAD")
		if _, ok := e.failedTrees[tree]; !ok {
			e.failedTrees[tree] = &failedTree{gr.Log, []Item{item}, known.base}
		}
		return &pairResult{fmt.Sprintf("the gate failed (%s; log: %s)", gr.Used, gr.Log) + colon(e.Gate.LogResult(gr.Log)), nil, gr.Log}
	}
	g.Must(states.GroupOnTogether)
	pn := itemNames(partners)
	landed := true
	for _, p := range pn {
		if !slices.Contains(e.Res.Merged, p) {
			landed = false
		}
	}
	has := "have"
	if len(pn) == 1 {
		has = "has"
	}
	then := fmt.Sprintf("Once %s %s landed,", AndList(pn), has)
	if landed {
		then = fmt.Sprintf("%s %s already landed, so", AndList(pn), has)
	}
	why := fmt.Sprintf("%s fails only when merged together with %s. On its own, it passes. %s merge `%s` into %s, "+
		"fix the clash, and queue it again. (alone: %s; together: %s%s)", item.Name, AndList(pn), then, e.Target,
		item.Name, gr.Log, known.log, colon(e.Gate.LogResult(known.log)))
	return &pairResult{why, pn, known.log}
}

func itemNames(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Name
	}
	return out
}

func (e *Engine) landedAt() string {
	if e.landed != "" {
		return e.landed
	}
	return "refs/heads/" + e.Target
}

func (e *Engine) finish(code int) *RunResult {
	if len(e.Res.Merged) > 0 {
		e.Hooks.PostLand(e.G, e.landedAt(), e.SkipHooks)
	}
	merged := strings.Join(e.Res.Merged, " ")
	if merged == "" {
		merged = "(none)"
	}
	e.Rep.Say("merged: " + merged)
	if len(e.Res.SetAside) > 0 {
		e.Rep.Say(fmt.Sprintf("set aside: %s (each with its reason in %s: \"stopped branch=<branch> reason=...\")",
			strings.Join(e.Res.SetAside, " "), e.Rep.Path()))
	}
	e.Res.Code = code
	return e.Res
}

type RunOptions struct {
	Batch    bool
	DryRun   bool
	Args     string
	LockWait *int
	RunID    int64
}

func (e *Engine) Run(items []Item, o RunOptions) (res *RunResult, err error) {
	os.MkdirAll(e.StateDir, 0o755)
	e.RunID = o.RunID
	if o.DryRun {
		defer e.closeDB()
		defer gitx.Catch(&err)
		res = e.dryRun(items, o.Batch)
		e.historyFinish("")
		return res, nil
	}
	lock := store.NewQueueLock(e.StateDir)
	wait := e.Cfg.LockWait
	if o.LockWait != nil {
		wait = *o.LockWait
	}
	if lerr := lock.Acquire(wait, o.Args, e.Rep.Say); lerr != nil {
		if _, ok := lerr.(*store.LockTimeout); !ok {
			e.closeDB()
			return e.Res, lerr
		}
		fmt.Fprintln(os.Stderr, lerr.Error())
		e.Res.Code = 1
		e.historyFinish(lerr.Error())
		e.closeDB()
		return e.Res, nil
	}
	if l, ok := e.Events.(Locker); ok {
		l.Locked(true)
		defer l.Locked(false)
	}
	note := ""
	defer func() {
		if r := recover(); r != nil {
			err = panics.AsError(r)
		}
		if err == nil {
			if why := e.leaveInteg(false); why != "" {
				err = fmt.Errorf("integ could not be reset to %s at the end of the run: %s", e.Target, why)
			}
		}
		if err != nil {
			note = fmt.Sprintf("error: %v", err)
			if e.Res.Code == 0 {
				e.Res.Code = 1
			}
			if why := e.leaveInteg(true); why != "" {
				e.Rep.Say(fmt.Sprintf("-- WARNING: integ could not be reset after the error: %s", why))
			}
			if len(e.Res.Merged) > 0 {
				func() {
					defer func() {
						if r := recover(); r != nil {
							e.Rep.Say(fmt.Sprintf("-- WARNING: the post-land hooks could not run after the error: %v", r))
						}
					}()
					e.Hooks.PostLand(e.G, e.landedAt(), e.SkipHooks)
				}()
			}
		}
		e.historyFinish(note)
		lock.Release()
		e.closeDB()
	}()
	if e.RunID == 0 {
		e.closeAbandonedRuns()
		mode := "one-by-one"
		if o.Batch {
			mode = "batch"
		}
		e.historyBegin(items, mode, o.Args, "cli")
	}
	if why := e.precheck(); why != "" {
		fmt.Fprintln(os.Stderr, why)
		note = why
		e.Res.Code = 1
		e.Res.Stopped = why
		return e.Res, nil
	}
	e.resetInteg()
	e.gateName = e.Gate.Select()
	return e.runMode(items, o.Batch), nil
}

func (e *Engine) runMode(items []Item, batch bool) (res *RunResult) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(stopSignal); ok {
				res = e.finish(1)
				return
			}
			panic(r)
		}
	}()
	if batch {
		return e.batch(items)
	}
	return e.oneByOne(items)
}

func (e *Engine) building(stop func(string), fn func()) {
	defer func() {
		if r := recover(); r != nil {
			if ge, ok := r.(*gitx.Error); ok {
				stop(fmt.Sprintf("git %s failed (exit %d: %s): an infrastructure failure, not the branch's",
					strings.Join(ge.Args, " "), ge.Code, gitx.LastLine(ge.Output)))
			}
			panic(r)
		}
	}()
	fn()
}

func (e *Engine) closeDB() {
	if e.repo != nil {
		e.repo.DB.Close()
		e.repo = nil
	}
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func samePath(a, b string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
}

func (e *Engine) precheck() string {
	t := e.Target
	if !isDir(e.IntegDir) {
		if !e.Cfg.CreateInteg {
			return fmt.Sprintf("no integration tree at %s (git worktree add %s -b integ %s)", e.IntegDir, e.IntegDir, t)
		}
		e.G.MustRun("worktree", "add", "-q", "--detach", e.IntegDir, "refs/heads/"+t)
	}
	same := false
	panics.Catch[*gitx.Error](func() {
		same = e.Integ.CommonDir() == e.G.CommonDir() && samePath(e.Integ.TopLevel(), e.IntegDir) &&
			!samePath(e.IntegDir, e.Main)
	})
	if !same {
		return fmt.Sprintf("%s is not a worktree of %s", e.IntegDir, e.Main)
	}
	on := e.Integ.HeadRef()
	if on == "refs/heads/"+t {
		return fmt.Sprintf("%s has %s checked out, so its merges would move %s before any gate: detach it "+
			"(git -C %s switch --detach) or give it a branch of its own", e.IntegDir, t, t, e.IntegDir)
	}
	e.integRef, e.integRefKnown = on, true
	if why := e.Lander.Precheck(); why != "" {
		return why
	}
	if e.resetStaleInteg && (e.Integ.DirtyTracked() || !e.Integ.IsAncestor("HEAD", "refs/heads/"+t)) {
		e.Rep.Say(fmt.Sprintf("-- %s was left half-built by an interrupted run: reset to %s", e.IntegDir, t))
		e.resetInteg()
	}
	if e.Integ.DirtyTracked() {
		return e.IntegDir + " has uncommitted changes: commit or reset them first"
	}
	if !e.resetStaleInteg {
		if extra := e.Integ.Untracked(); len(extra) > 0 {
			shown := extra
			if len(shown) > 5 {
				shown = append(append([]string{}, shown[:5]...), fmt.Sprintf("and %d more", len(extra)-5))
			}
			return fmt.Sprintf("%s has untracked files (%s): move them or remove them", e.IntegDir,
				strings.Join(shown, ", "))
		}
	}
	if !e.Integ.IsAncestor("HEAD", "refs/heads/"+t) {
		return fmt.Sprintf("%s has commits that are not on %s (a merge by hand in progress?): reset it to %s first",
			e.IntegDir, t, t)
	}
	return ""
}

func (e *Engine) merge(item Item, sha string) conflicts.Merge {
	return e.Merger.Merge(e.Integ, sha, item.Name, item.Slug, item.Message)
}

func (e *Engine) afterMerge(item Item) hooks.Result {
	return e.Hooks.PostMerge(e.Integ, item.Name, item.Slug)
}

func (e *Engine) lost(item Item, tip, from string) string {
	head := e.Integ.Rev("HEAD")
	if head != "" && e.Integ.IsAncestor(tip, head) && e.Integ.IsAncestor(from, head) {
		return ""
	}
	return fmt.Sprintf("integ's HEAD %s after merging %s does not contain %s %s or what was merged before it (%s): a "+
		"conflict command or post-merge hook moved it", short(head), item.Name, item.Name, short(tip), short(from))
}

func (e *Engine) notLanding(bs []built, sha string) string {
	for _, b := range bs {
		if !e.Integ.IsAncestor(b.sha, sha) {
			return fmt.Sprintf("%s (%s) is not in integ's %s, the commit that passed the gate: refusing to fast-forward "+
				"%s to it (an invariant: never guess)", b.item.Name, short(b.sha), short(sha), e.Target)
		}
	}
	return ""
}

func (e *Engine) gateHash(name string) string {
	return e.Cfg.Gates[name].ConfigHash(e.Cfg.GateEnv)
}

func (e *Engine) gate(label string, items []Item, skip bool, what string) *gate.Run {
	e.integ.Must(states.IntegOnGate)
	name := e.gateName
	log := reporter.FreshLog(e.StateDir, time.Now().Format("20060102-150405")+"-"+Safe(label)+".gate")
	if skip && e.Records.Enabled() && !e.Regate && !e.Integ.DirtyTracked() {
		tree := e.Integ.TreeOf("HEAD")
		found := e.Records.Find(tree, name, e.gateHash(name))
		if found.OK {
			note := fmt.Sprintf("gate skipped: tree %s passed the %s gate at %s (log: %s)", tree[:12], name, found.When,
				found.Log)
			e.Rep.Say("-- " + note)
			return &gate.Run{Outcome: "pass", Used: name, Log: found.Log, Record: map[string]string{
				"when": found.When, "log": found.Log, "tree": tree}}
		}
		if found.Why != "no record" {
			e.Rep.Say(fmt.Sprintf("-- gating: tree %s has no gate record the queue accepts: %s", tree[:12], found.Why))
		}
	}
	slotter, hasSlots := e.Events.(Slotter)
	if hasSlots {
		w := what
		if w == "" {
			w = label
		}
		if why := slotter.GateSlot(w); why != "" {
			return &gate.Run{Outcome: "cancelled", Used: name, Log: log, InfraWhy: "cancelled: " + why}
		}
	}
	defer func() {
		if hasSlots {
			slotter.GateDone()
		}
	}()
	if e.Events != nil {
		e.Events.Gating(items, log)
	}
	e.Res.Gates++
	before := gatedState{head: e.Integ.Head(), tree: e.Integ.TreeOf("HEAD"), dirty: e.Integ.DirtyTracked()}
	gr := e.Gate.Run(name, log)
	if gr != nil {
		if e.gated == nil {
			e.gated = map[*gate.Run]gatedState{}
		}
		e.gated[gr] = before
	}
	return gr
}

func (e *Engine) land(gr *gate.Run) string {
	next := e.Integ.Head()
	if gr.Record != nil {
		want := gr.Record["tree"]
		if e.Integ.TreeOf("HEAD") != want || e.Integ.DirtyTracked() {
			return fmt.Sprintf("integ's tree is not the %s the gate record is for: refusing to fast-forward %s to it",
				short(want), e.Target)
		}
	} else {
		st, ok := e.gated[gr]
		delete(e.gated, gr)
		switch {
		case !ok:
			return fmt.Sprintf("no record of what the gate checked: refusing to fast-forward %s to %s (log: %s)",
				e.Target, short(next), gr.Log)
		case st.dirty:
			return fmt.Sprintf("the gate checked %s with uncommitted changes in integ: refusing to fast-forward %s to it "+
				"(log: %s)", short(st.head), e.Target, gr.Log)
		case next != st.head || e.Integ.TreeOf("HEAD") != st.tree:
			return fmt.Sprintf("integ's HEAD moved from the gated %s to %s while the gate ran: refusing to fast-forward "+
				"%s to it (log: %s)", short(st.head), short(next), e.Target, gr.Log)
		case e.Integ.DirtyTracked():
			return fmt.Sprintf("the gate left uncommitted changes in integ's tracked files on top of %s: refusing to "+
				"fast-forward %s to it (log: %s)", short(next), e.Target, gr.Log)
		}
		if e.Records.Enabled() {
			tree := e.Integ.TreeOf("HEAD")
			e.bestEffort("the gate record", func() error {
				return e.Records.Write(tree, next, gr.Used, e.gateHash(gr.Used), gr.Log)
			})
		}
	}
	why := e.Lander.Land(next)
	if why == "" {
		e.landed = next
		e.integ.Must(states.IntegOnLand)
	}
	return why
}

func (e *Engine) gateFields(gr *gate.Run, secs int) []string {
	f := []string{"gate", gr.Used, "secs", strconv.Itoa(secs)}
	if gr.Record != nil {
		f = append(f, "reused", gr.Record["when"])
	}
	if gr.Retried {
		f = append(f, "retried", "yes")
	}
	return f
}

func (e *Engine) mergedLine(b built, gr *gate.Run, secs, n int) string {
	f := append([]string{"branch", b.item.Name, "commit", b.tip, "target", e.Target, "range", b.from + ".." + b.to},
		e.gateFields(gr, secs)...)
	if n > 1 {
		f = append(f, "batch", strconv.Itoa(n))
	}
	if b.note != "" {
		f = append(f, "resolved", b.note)
	}
	return e.Rep.Record("merged", b.item.Name, f...)
}

func unseen(items []Item, seen map[string]bool) []Item {
	var out []Item
	more := map[string]bool{}
	for _, it := range items {
		if !seen[it.Name] && !more[it.Name] {
			more[it.Name] = true
			out = append(out, it)
		}
	}
	return out
}

func (e *Engine) oneByOne(items []Item) *RunResult {
	seen := map[string]bool{}
	for idx, item := range items {
		if seen[item.Name] {
			e.Rep.Say(fmt.Sprintf("== %s: given twice: merged once", item.Name))
			continue
		}
		seen[item.Name] = true
		rest := append([]Item{item}, unseen(items[idx+1:], seen)...)
		e.Rep.Say("== " + item.Name)
		var sha string
		var in bool
		e.building(func(why string) { e.stopRest(rest, why) }, func() {
			if sha = e.G.CommitOf(item.Ref); sha != "" {
				in = e.G.IsAncestor(sha, "refs/heads/"+e.Target)
			}
		})
		if sha == "" {
			e.stopRest(rest, "no such branch")
		}
		if in {
			e.Rep.Say("-- already in " + e.Target + ": skipped")
			e.outcome(item, Outcome{Status: states.OutcomeSkipped, Why: "already in " + e.Target})
			continue
		}
		t0 := time.Now()
		from := e.base()
		old := e.short(from)
		tip := e.short(sha)
		g := states.GroupMachine.Start()
		g.Must(states.GroupOnBuild)
		e.integ.Must(states.IntegOnBuild)
		stopHere := func(why string) {
			g.Must(states.GroupOnStop)
			e.stopRest(rest, why)
		}
		var m conflicts.Merge
		e.building(stopHere, func() { m = e.merge(item, sha) })
		if !m.State.Merged() {
			stopHere(mergeWhy(m))
		}
		if m.Note != "" {
			e.Rep.Say("-- conflicts resolved: " + m.Note)
		}
		what := fmt.Sprintf("%s, %d of %d", item.Name, idx+1, len(items))
		if m = e.postMerge(stopHere, item, sha, from, m); m.State != states.MergeDone {
			stopHere(m.Why)
		}
		e.integ.Must(states.IntegOnBuilt)
		g.Must(states.GroupOnBuilt)
		gr := e.gate(item.Slug, []Item{item}, true, what)
		e.gateOutcome(g, gr)
		switch g.State() {
		case states.GroupInfra:
			stopHere(gr.InfraWhy)
		case states.GroupFailed:
			stopHere(fmt.Sprintf("the gate failed (%s; log: %s)", gr.Used, gr.Log) + colon(e.Gate.LogResult(gr.Log)))
		}
		g.Must(states.GroupOnLand)
		landed := e.Integ.Head()
		b := built{item: item, sha: sha, tip: tip, from: old, to: e.short(landed), note: m.Note}
		if why := e.notLanding([]built{b}, landed); why != "" {
			stopHere(why)
		}
		if why := e.land(gr); why != "" {
			stopHere(why)
		}
		g.Must(states.GroupOnLanded)
		stale := e.staleTarget()
		if stale == "" {
			e.resetInteg()
		}
		e.Res.Merged = append(e.Res.Merged, item.Name)
		line := e.mergedLine(b, gr, int(time.Since(t0).Seconds()), 1)
		e.Rep.Say("-- " + line)
		e.outcome(item, Outcome{Status: states.OutcomeMerged, Line: line, Log: gr.Log, Sha: landed})
		if stale != "" {
			e.notTried(rest[1:])
			e.stop(nil, stale)
		}
	}
	return e.finish(0)
}

func (e *Engine) notTried(items []Item) {
	for _, i := range items {
		e.settle(i, Outcome{Status: states.OutcomeNotTried})
	}
}

func (e *Engine) stopRest(rest []Item, why string) {
	e.notTried(rest[1:])
	e.stop(rest[:1], why)
}

func (e *Engine) batch(items []Item) *RunResult {
	var todo []Item
	seen := map[string]bool{}
	for idx, item := range items {
		if seen[item.Name] {
			e.Rep.Say(fmt.Sprintf("== %s: given twice: merged once", item.Name))
			continue
		}
		seen[item.Name] = true
		var in bool
		stopAll := func(why string) {
			e.stop(append(append(append([]Item{}, todo...), item), unseen(items[idx+1:], seen)...), why)
		}
		e.building(stopAll, func() {
			if item.Sha = e.G.CommitOf(item.Ref); item.Sha != "" {
				in = e.G.IsAncestor(item.Sha, "refs/heads/"+e.Target)
			}
		})
		switch {
		case item.Sha == "":
			e.setAside(item, "no such branch", "", nil)
		case in:
			e.Rep.Say(fmt.Sprintf("== %s: already in %s: skipped", item.Name, e.Target))
			e.outcome(item, Outcome{Status: states.OutcomeSkipped, Why: "already in " + e.Target})
		default:
			todo = append(todo, item)
		}
	}
	var queue [][]Item
	if len(todo) > 0 {
		queue = append(queue, todo)
	}
	for len(queue) > 0 {
		grp := queue[0]
		queue = queue[1:]
		queue = e.batchGroup(grp, queue)
	}
	e.resetInteg()
	e.Rep.Say(fmt.Sprintf("-- batch done: %d gate run(s) for %d branch(es)", e.Res.Gates, len(items)))
	code := 0
	if len(e.Res.SetAside) > 0 {
		code = 1
	}
	return e.finish(code)
}

func builtItems(bs []built) []Item {
	out := make([]Item, len(bs))
	for i, b := range bs {
		out[i] = b.item
	}
	return out
}

func (e *Engine) batchGroup(grp []Item, queue [][]Item) [][]Item {
	t0 := time.Now()
	old := e.short(e.base())
	e.Rep.Say("")
	e.Rep.Say(fmt.Sprintf("== batch on %s %s: %s (%d branch(es), one gate)", e.Target, old, names(grp), len(grp)))
	e.resetInteg()
	g := states.GroupMachine.Start()
	g.Must(states.GroupOnBuild)
	e.integ.Must(states.IntegOnBuild)
	stopGroup := func(items []Item, why string) {
		g.Must(states.GroupOnStop)
		e.stop(items, why)
	}
	var bs []built
	what := "batch gate"
	if e.steps != 0 {
		what = fmt.Sprintf("bisect step %d", e.steps)
	}
	for k, item := range grp {
		stopAll := func(why string) {
			all := append(builtItems(bs), grp[k:]...)
			stopGroup(append(all, flatten(queue)...), why)
		}
		var sha string
		var in bool
		e.building(stopAll, func() {
			sha = e.tip(item)
			in = e.G.IsAncestor(sha, "refs/heads/"+e.Target)
		})
		if in {
			e.Rep.Say(fmt.Sprintf("== %s: already in %s: skipped", item.Name, e.Target))
			e.outcome(item, Outcome{Status: states.OutcomeSkipped, Why: "already in " + e.Target})
			continue
		}
		e.Rep.Say("== " + item.Name)
		from := e.Integ.Short("HEAD", 0)
		fromSha := e.Integ.Head()
		var m conflicts.Merge
		e.building(stopAll, func() { m = e.merge(item, sha) })
		if m.State == states.MergeInfra {
			stopAll(m.Why)
		}
		if !m.State.Merged() {
			e.resetTo(fromSha)
			e.setAside(item, mergeWhy(m), "", nil)
			continue
		}
		if m.Note != "" {
			e.Rep.Say("-- conflicts resolved: " + m.Note)
		}
		m = e.postMerge(stopAll, item, sha, fromSha, m)
		if m.State == states.MergeInfra {
			stopAll(m.Why)
		}
		if m.State != states.MergeDone {
			e.resetTo(fromSha)
			e.setAside(item, m.Why, "", nil)
			continue
		}
		bs = append(bs, built{item: item, sha: sha, tip: e.short(sha), from: from, to: e.Integ.Short("HEAD", 0),
			note: m.Note})
	}
	if len(bs) == 0 {
		g.Must(states.GroupOnNothing)
		e.integ.Must(states.IntegOnNothing)
		return queue
	}
	e.integ.Must(states.IntegOnBuilt)
	g.Must(states.GroupOnBuilt)
	n := len(bs)
	nm := names(builtItems(bs))
	label := bs[0].item.Slug
	if n > 1 {
		label = fmt.Sprintf("batch-%s+%d", bs[0].item.Slug, n-1)
	}
	tree := e.Integ.TreeOf("HEAD")
	known := e.failedTrees[tree]
	var gr *gate.Run
	if known != nil {
		e.Rep.Say(fmt.Sprintf("-- this tree failed the gate already (log: %s): not gated again", known.log))
		gr = &gate.Run{Outcome: "fail", Used: e.gateName, Log: known.log}
		g.Must(states.GroupOnKnown)
	} else {
		e.steps++
		gr = e.gate(label, builtItems(bs), true, what)
		e.gateOutcome(g, gr)
	}
	if gr.Outcome != "pass" {
		delete(e.gated, gr)
	}
	if g.State() == states.GroupInfra {
		stopGroup(append(builtItems(bs), flatten(queue)...), gr.InfraWhy)
	}
	if st := g.State(); st == states.GroupFailed || st == states.GroupKnownFail {
		if known == nil {
			e.failedTrees[tree] = &failedTree{gr.Log, builtItems(bs), e.base()}
		}
		result := e.Gate.LogResult(gr.Log)
		if n == 1 {
			var partners []string
			log := gr.Log
			var why string
			if known != nil {
				why = fmt.Sprintf("the gate failed (the same tree as an earlier gate run; log: %s)", gr.Log) + colon(result)
				if pair := e.pairCheck(g, bs[0].item, known, queue); pair != nil {
					why, partners, log = pair.why, pair.partners, pair.log
				} else {
					g.Must(states.GroupOnAside)
				}
			} else {
				why = fmt.Sprintf("the gate failed (%s; log: %s)", gr.Used, gr.Log) + colon(result)
				g.Must(states.GroupOnAside)
			}
			e.setAside(bs[0].item, why, log, partners)
		} else {
			g.Must(states.GroupOnSplit)
			h := (n + 1) / 2
			h1, h2 := builtItems(bs[:h]), builtItems(bs[h:])
			e.Rep.Say(fmt.Sprintf("-- the batch failed the gate%s: bisecting: (%s) first, then (%s)", paren(result),
				names(h1), names(h2)))
			queue = append([][]Item{h1, h2}, queue...)
		}
		return queue
	}
	g.Must(states.GroupOnLand)
	landed := e.Integ.Head()
	if why := e.notLanding(bs, landed); why != "" {
		stopGroup(append(builtItems(bs), flatten(queue)...), why)
	}
	if why := e.land(gr); why != "" {
		stopGroup(append(builtItems(bs), flatten(queue)...), why)
	}
	g.Must(states.GroupOnLanded)
	stale := e.staleTarget()
	secs := int(time.Since(t0).Seconds())
	for _, b := range bs {
		line := e.mergedLine(b, gr, secs, n)
		e.Rep.Say("-- " + line)
		e.Res.Merged = append(e.Res.Merged, b.item.Name)
		e.outcome(b.item, Outcome{Status: states.OutcomeMerged, Line: line, Log: gr.Log, Sha: landed})
	}
	if n > 1 {
		line := e.Rep.Record("batch", nm, append([]string{"branches", nm, "count", strconv.Itoa(n), "target", e.Target,
			"range", old + ".." + e.short(landed)}, e.gateFields(gr, secs)...)...)
		e.Rep.Say("-- " + line)
	}
	if stale != "" {
		e.stop(flatten(queue), stale)
	}
	return queue
}

func (e *Engine) staleTarget() string {
	if s, ok := e.Lander.(interface{ Stale() string }); ok {
		return s.Stale()
	}
	return ""
}

func (e *Engine) dryRun(items []Item, batch bool) *RunResult {
	tmp := filepath.Join(e.StateDir, fmt.Sprintf("dry-%d", os.Getpid()))
	e.G.MustRun("worktree", "add", "-q", "--detach", tmp, "refs/heads/"+e.Target)
	t := gitx.New(tmp, e.GitEnv)
	admin := t.Out("rev-parse", "--path-format=absolute", "--git-dir")
	status := 0
	var once sync.Once
	remove := func() {
		once.Do(func() {
			if e.G.OK("worktree", "remove", "--force", tmp) {
				return
			}
			os.RemoveAll(tmp)
			if admin != "" && filepath.Dir(admin) == filepath.Join(e.G.CommonDir(), "worktrees") {
				os.RemoveAll(admin)
			}
		})
	}
	defer remove()
	if w, ok := e.Events.(Throwaway); ok {
		w.Throwaway(remove)
		defer w.Throwaway(nil)
	}
	dry := &conflicts.Merger{Cfg: e.Cfg}
	var after string
	for _, h := range e.Cfg.PostMerge {
		after += "; post-merge hook " + h.Name + " would run"
	}
	e.Rep.Say(fmt.Sprintf("== dry run on %s %s (a throwaway worktree; nothing else is touched)", e.Target, e.short(e.base())))
	seen := map[string]bool{}
	for _, item := range items {
		if seen[item.Name] {
			e.Rep.Say(fmt.Sprintf("%s: given twice: checked once", item.Name))
			continue
		}
		seen[item.Name] = true
		sha := e.G.CommitOf(item.Ref)
		if sha == "" {
			if batch {
				e.Rep.Say(item.Name + ": no such branch: it would be set aside")
			} else {
				e.Rep.Say(item.Name + ": no such branch: the queue would stop here")
			}
			status = 1
			e.outcome(item, Outcome{Status: states.OutcomeSetAside, Why: "no such branch"})
			continue
		}
		if e.G.IsAncestor(sha, "refs/heads/"+e.Target) {
			e.Rep.Say(fmt.Sprintf("%s: already in %s (skipped)", item.Name, e.Target))
			e.outcome(item, Outcome{Status: states.OutcomeSkipped, Why: "already in " + e.Target})
			continue
		}
		m := dry.Merge(t, sha, item.Name, item.Slug, item.Message)
		switch m.State {
		case states.MergeInfra:
			e.Rep.Say(fmt.Sprintf("%s: %s (not a conflict: the real run would stop here)", item.Name, m.Why))
			status = 1
			e.outcome(item, Outcome{Status: states.OutcomeStopped, Why: m.Why})
			continue
		case states.MergeAside:
			e.Rep.Say(fmt.Sprintf("%s: %s", item.Name, m.Why))
			if batch {
				e.Rep.Say("   it would be set aside; the branches after it are checked without it")
			} else {
				e.Rep.Say("   the queue would stop here; the branches after it are checked as if it were skipped")
			}
			status = 1
			e.outcome(item, Outcome{Status: states.OutcomeSetAside, Why: m.Why})
			continue
		case states.MergeNeedsPerson:
			e.Rep.Say(fmt.Sprintf("%s: CONFLICT that needs a person: %s", item.Name, m.Why))
			if batch {
				e.Rep.Say("   it would be set aside; the branches after it are checked without it")
			} else {
				e.Rep.Say("   the queue would stop here; the branches after it are checked as if it were skipped")
			}
			status = 1
			e.outcome(item, Outcome{Status: states.OutcomeSetAside, Why: mergeWhy(m)})
			continue
		}
		line := item.Name + ": merges"
		if m.Note != "" {
			line += " (resolved: " + m.Note + ")"
		}
		line += after
		e.Rep.Say(line)
		e.outcome(item, Outcome{Status: states.OutcomeMerged, Line: line})
	}
	e.Res.Code = status
	return e.Res
}

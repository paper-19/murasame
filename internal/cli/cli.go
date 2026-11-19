package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"

	"murasame/internal/config"
	"murasame/internal/engine"
	"murasame/internal/instance"
	"murasame/internal/server"
	"murasame/internal/status"
)

const Version = "0.1.0"

var newEngine = engine.New

var commandNames = []string{"run", "serve", "tick", "check-config", "history"}

func subParser(name string, cfg *config.Config) *parser {
	p := &parser{prog: "murasame " + name}
	p.add(opt{flag: "--config", dest: "config", kind: "value", help: "the project config (TOML); default $MURASAME_CONFIG"})
	p.add(opt{flag: "--repo", dest: "repo", kind: "value", help: "the repository (any checkout or worktree of it; " +
		"the service: its bare clone); default .; a multi-repo config names its own"})
	if name == "run" {
		p.add(opt{flag: "--batch", dest: "batch", kind: "bool", help: "one gate for all the branches, bisected on a failure"})
		p.add(opt{flag: "--one-by-one", dest: "batch", kind: "false", help: "one gate per branch; the first problem stops"})
		p.add(opt{flag: "--dry-run", dest: "dry_run", kind: "bool", help: "merge in a throwaway worktree only; report"})
		p.add(opt{flag: "--regate", dest: "regate", kind: "bool", help: "gate even a tree with a pass record"})
		gate := opt{flag: "--gate", dest: "gate", kind: "value", help: "the gate to use (default: the config's default gate)"}
		if cfg != nil {
			gate.choice = append([]string{}, cfg.GateOrder...)
			sort.Strings(gate.choice)
			gate.help = "the gate to use (default: " + cfg.DefaultGate + ")"
		}
		p.add(gate)
		p.add(opt{flag: "--skip-hook", dest: "skip_hook", kind: "append", meta: "NAME", help: "don't run this post-land hook"})
		p.posName, p.posHelp = "branches", "the branches, in merge order"
	}
	if name == "serve" {
		p.add(opt{flag: "--once", dest: "once", kind: "bool", help: "one cycle, then exit"})
	}
	if name == "history" {
		p.add(opt{flag: "--branch", dest: "branch", kind: "value", help: "only runs (lines) with this branch (a PR: its branch or #N)"})
		p.add(opt{flag: "--pr", dest: "pr", kind: "int", help: "only runs (lines) with this pull request; also its state moves"})
		p.add(opt{flag: "--limit", dest: "limit", kind: "int", nonneg: true,
			help: "at most this many runs, lines or PR state moves (default 20)"})
		p.add(opt{flag: "--log", dest: "log", kind: "bool", help: "the merges.log lines instead of the runs"})
		p.add(opt{flag: "--json", dest: "json", kind: "bool", help: "JSON instead of text"})
		p.add(opt{flag: "--repo-key", dest: "repo_key", kind: "value", meta: "KEY", help: "which repository of the state DB " +
			"(a key from --repos, or 'all'); default: the only one, else this config's"})
		p.add(opt{flag: "--repos", dest: "repos", kind: "bool", help: "list the repositories in the state DB"})
	}
	return p
}

const topUsage = "usage: murasame [-h] [--version] {run,serve,tick,check-config,history} ..."

func Main(argv []string, stdout, stderr io.Writer) int {
	surviveBrokenPipes()
	code, err := run(argv, stdout, stderr)
	var ue *usageError
	var hr *helpRequest
	switch {
	case errors.As(err, &hr):
		hr.p.help(stdout)
		return 0
	case errors.As(err, &ue):
		usage := topUsage
		if ue.usage != "" {
			usage = ue.usage
		} else if ue.prog != "murasame" {
			for _, n := range commandNames {
				if ue.prog == "murasame "+n {
					usage = subParser(n, nil).usage()
				}
			}
		}
		fmt.Fprintln(stderr, usage)
		fmt.Fprintf(stderr, "%s: error: %s\n", ue.prog, ue.msg)
		return 2
	case err != nil:
		fmt.Fprintf(stderr, "murasame: %s\n", err)
		return 2
	}
	return code
}

func run(argv []string, stdout, stderr io.Writer) (int, error) {
	cmdAt := -1
	for i, a := range argv {
		if a == "--version" {
			fmt.Fprintln(stdout, "murasame "+Version)
			return 0, nil
		}
		if a == "-h" || a == "--help" {
			fmt.Fprintln(stdout, topUsage+"\n\nA merge queue: batches, bisection, one gate.\n\npositional arguments:\n"+
				"  {run,serve,tick,check-config,history}\n    run                 merge branches through the gate\n"+
				"    serve               the Forgejo service\n    tick                one Forgejo service cycle\n"+
				"    check-config        load and show the config\n    history             recent merges and batches (read-only)")
			return 0, nil
		}
		if !strings.HasPrefix(a, "-") {
			cmdAt = i
			break
		}
		return 0, &usageError{prog: "murasame", msg: "unrecognized arguments: " + a}
	}
	if cmdAt < 0 {
		return 0, &usageError{prog: "murasame", msg: "the following arguments are required: command"}
	}
	name := argv[cmdAt]
	if !slices.Contains(commandNames, name) {
		return 0, &usageError{prog: "murasame", msg: fmt.Sprintf("argument command: invalid choice: '%s' (choose from 'run', "+
			"'serve', 'tick', 'check-config', 'history')", name)}
	}
	rest := argv[cmdAt+1:]
	known, err := subParser(name, nil).parse(rest, true)
	var help *helpRequest
	if err != nil && !errors.As(err, &help) {
		return 0, err
	}
	if help != nil {
		known, _ = subParser(name, nil).parse(withoutHelp(rest), true)
	}
	cfgPath := os.Getenv("MURASAME_CONFIG")
	repoDir := "."
	if known != nil {
		if v, ok := known.str["config"]; ok {
			cfgPath = v
		}
		if v, ok := known.str["repo"]; ok {
			repoDir = v
		}
	}
	if cfgPath == "" {
		if help != nil {
			return 0, help
		}
		return 0, &usageError{prog: "murasame", msg: "no config: --config FILE or MURASAME_CONFIG"}
	}
	raw, err := config.ReadTOML(cfgPath, nil)
	if err != nil {
		if help != nil {
			return 0, help
		}
		return 0, err
	}
	if config.IsMulti(raw) {
		if help != nil {
			return 0, help
		}
		if name == "run" {
			return 0, config.Errf("%s is a multi-repo service config: `run` takes a project config (e.g. the file a "+
				"[[repos]] entry includes)", cfgPath)
		}
		spec, err := instance.Load(cfgPath, nil, nil, "")
		if err != nil {
			return 0, err
		}
		a, err := subParser(name, nil).parse(rest, false)
		if err != nil {
			return 0, err
		}
		if len(a.pos) > 0 {
			return 0, &usageError{prog: "murasame " + name, msg: "unrecognized arguments: " + strings.Join(a.pos, " ")}
		}
		return instanceCommand(spec, name, a, repoDir, stdout, stderr), nil
	}
	cfg, err := config.Load(cfgPath, config.LoadOptions{})
	if err != nil {
		if help != nil {
			return 0, help
		}
		return 0, err
	}
	a, err := subParser(name, cfg).parse(rest, false)
	if err != nil {
		return 0, err
	}
	if name != "run" && len(a.pos) > 0 {
		return 0, &usageError{prog: "murasame " + name, msg: "unrecognized arguments: " + strings.Join(a.pos, " ")}
	}
	switch name {
	case "check-config":
		return checkConfig(cfg, "", true, stdout), nil
	case "history":
		return History(cfg, historyArgs(a, repoDir), "", nil, stdout, stderr), nil
	case "serve", "tick":
		spec, err := instance.Load(cfgPath, nil, nil, repoDir)
		if err != nil {
			return 0, err
		}
		return server.Serve(spec, name == "tick" || a.flag("once")), nil
	}
	if len(a.pos) == 0 {
		return 0, &usageError{prog: "murasame run", msg: "the following arguments are required: branches",
			usage: subParser("run", cfg).usage()}
	}
	skip := map[string]bool{}
	var hooks []string
	for _, h := range cfg.PostLand {
		hooks = append(hooks, h.Name)
	}
	for _, h := range a.lists["skip_hook"] {
		if !slices.Contains(hooks, h) {
			known := "none are configured"
			if len(hooks) > 0 {
				known = "choose from " + strings.Join(hooks, ", ")
			}
			return 0, &usageError{prog: "murasame run", msg: fmt.Sprintf("argument --skip-hook: no post-land hook %q (%s)", h,
				known), usage: subParser("run", cfg).usage()}
		}
		skip[h] = true
	}
	it := newInterruption(a.flag("dry_run"))
	stop := stopOnSignals(it, stderr)
	defer stop()
	eng, err := newEngine(cfg, repoDir, engine.Options{Gate: a.str["gate"], SkipHooks: skip, Regate: a.flag("regate"),
		Events: it})
	if err != nil {
		fmt.Fprintf(stderr, "murasame: %v\n", err)
		return 1, nil
	}
	batch := cfg.DefaultBatch
	if b := a.bools["batch"]; b != nil {
		batch = *b
	}
	items := make([]engine.Item, len(a.pos))
	for i, b := range a.pos {
		ref := ""
		if !strings.HasPrefix(b, "refs/") && eng.G.OK("show-ref", "--verify", "-q", "refs/heads/"+b) {
			ref = "refs/heads/" + b
		}
		items[i] = engine.NewItem(b, ref, "", "")
	}
	it.Begin(eng.Target)
	res, err := eng.Run(items, engine.RunOptions{Batch: batch, DryRun: a.flag("dry_run"), Args: strings.Join(argv, " ")})
	stop()
	if err != nil {
		fmt.Fprintf(stderr, "murasame: %v\n", err)
		return 1, nil
	}
	return res.Code, nil
}

func withoutHelp(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--" {
			return append(out, args[i:]...)
		}
		if a != "-h" && a != "--help" {
			out = append(out, a)
		}
	}
	return out
}

func historyArgs(a *parsed, repoDir string) HistoryArgs {
	h := HistoryArgs{Repo: repoDir, Branch: a.str["branch"], RepoKey: a.str["repo_key"], Log: a.flag("log"),
		JSON: a.flag("json"), Repos: a.flag("repos"), Limit: 20}
	if v, ok := a.str["pr"]; ok {
		n := atoi(v)
		h.PR = &n
	}
	if v, ok := a.str["limit"]; ok {
		h.Limit = atoi(v)
	}
	return h
}

func atoi(s string) int {
	n := 0
	fmt.Sscan(strings.TrimSpace(s), &n)
	return n
}

func instanceCommand(spec *instance.Spec, name string, a *parsed, repoDir string, stdout, stderr io.Writer) int {
	switch name {
	case "serve", "tick":
		return server.Serve(spec, name == "tick" || a.flag("once"))
	case "history":
		return History(spec.Repos[0].Cfg, historyArgs(a, repoDir), spec.DB, instanceKeys(spec), stdout, stderr)
	}
	fmt.Fprintf(stdout, "config %s: ok (multi-repo: %d repositories)\n", spec.Path, len(spec.Repos))
	fmt.Fprintf(stdout, "  instance: bot %s; listen %s; poll every %d s; at most %d gate(s) at once; state DB %s\n",
		spec.BotUser, spec.Listen, spec.PollInterval, spec.MaxConcurrentGates, spec.DB)
	for _, n := range spec.ForgeOrder {
		f := spec.Forges[n]
		fmt.Fprintf(stdout, "  forge %s: %s (token $%s)\n", f.Name, shownURL(f.URL), f.TokenEnv)
	}
	for _, e := range spec.Repos {
		fmt.Fprintf(stdout, "repo %s: %s/%s on %s, clone %s\n", e.Name, e.Cfg.Forgejo.Owner, e.Cfg.Forgejo.Repo,
			shownURL(e.Cfg.Forgejo.URL), e.Clone)
		checkConfig(e.Cfg, "  ", false, stdout)
	}
	return 0
}

func shownURL(raw string) string {
	if safe := status.SafeURL(raw); safe != "" {
		return safe
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return "<not a URL>"
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return u.String()
}

func instanceKeys(spec *instance.Spec) []string {
	var out []string
	for _, e := range spec.Repos {
		out = append(out, instance.EffectiveRepoKey(e))
	}
	return out
}

func checkConfig(cfg *config.Config, indent string, header bool, w io.Writer) int {
	if header {
		fmt.Fprintf(w, "config %s: ok\n", cfg.Path)
	}
	say := func(line string) { fmt.Fprintln(w, indent+line) }
	key := ""
	if cfg.RepoKey != "" {
		key = " (repo key " + cfg.RepoKey + ")"
	}
	say(fmt.Sprintf("  target: %s; integ: %s; state: %s; state DB: %s%s", strings.Join(cfg.Target, " or "), cfg.Integ,
		cfg.StateDir, cfg.DB, key))
	var gs []string
	for _, name := range cfg.GateOrder {
		g := cfg.Gates[name]
		x := fmt.Sprintf("%s (timeout %d s, retries %d", name, g.Timeout, g.Retries)
		if g.Retries > 0 {
			x += fmt.Sprintf(" %d s apart", g.RetryWait)
		}
		gs = append(gs, x+")")
	}
	say(fmt.Sprintf("  gates: %s; default %s", strings.Join(gs, ", "), cfg.DefaultGate))
	pair := "off"
	if cfg.PairCheck {
		pair = "on"
	}
	say(fmt.Sprintf("  pair check: %s", pair))
	if len(cfg.CommandPrefix) > 0 {
		say("  project commands run through: " + strings.Join(cfg.CommandPrefix, " "))
	} else {
		say("  project commands run as this user (no command_prefix: they can read this process's secrets)")
	}
	for _, r := range cfg.Conflicts {
		x := fmt.Sprintf("  conflicts %s: %s -> %s", r.Label, strings.Join(r.Paths, " "), r.Strategy)
		if r.Strategy == "command" {
			x += fmt.Sprintf(" (take %s, then run: %s <paths>)", r.Take, strings.Join(r.Run.Args, " "))
		}
		say(x)
	}
	for _, h := range cfg.PostMerge {
		say(fmt.Sprintf("  post-merge hook %s: %s", h.Name, strings.Join(h.Run.Args, " ")))
	}
	for _, h := range cfg.PostLand {
		x := fmt.Sprintf("  post-land hook %s: %s", h.Name, strings.Join(h.Run.Args, " "))
		if h.Dir != "" {
			x += " (in " + h.Dir + ")"
		}
		say(x)
	}
	if cfg.Records.Enabled {
		say(fmt.Sprintf("  records: a tree that passed a gate is not gated again for %d s", cfg.Records.MaxAge))
	} else {
		say("  records: off")
	}
	if cfg.Forgejo.URL != "" {
		fj := cfg.Forgejo
		say(fmt.Sprintf("  forgejo: %s %s/%s as %s", shownURL(fj.URL), fj.Owner, fj.Repo, fj.BotUser))
		who := "nobody (set reviewers or reviewers_from_permission)"
		if len(fj.Reviewers) > 0 {
			who = strings.Join(fj.Reviewers, ", ")
		}
		if fj.ReviewersFromPermission != "" {
			if len(fj.Reviewers) > 0 {
				who += " and "
			} else {
				who = ""
			}
			who += "anyone with " + fj.ReviewersFromPermission + " permission"
		}
		say(fmt.Sprintf("  approvals: `@%s approve <full sha of the head>` comments by %s", fj.BotUser, who))
	}
	return 0
}

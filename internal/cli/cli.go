package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"

	"murasame/internal/config"
	"murasame/internal/instance"
	"murasame/internal/server"
	"murasame/internal/status"
)

const Version = "0.2.0"

var commandNames = []string{"serve", "tick", "check-config", "history"}

var brokenPipes sync.Once

func surviveBrokenPipes() {
	brokenPipes.Do(func() {
		signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	})
}

func subParser(name string) *parser {
	p := &parser{prog: "murasame " + name}
	p.add(opt{flag: "--config", dest: "config", kind: "value", help: "the project config (TOML); default $MURASAME_CONFIG"})
	p.add(opt{flag: "--repo", dest: "repo", kind: "value", help: "the repository (any checkout or worktree of it; " +
		"the service: its bare clone); default .; a multi-repo config names its own"})
	if name == "check-config" {
		p.add(opt{flag: "--forge", dest: "forge", kind: "bool", help: "also check the forge with the bot's token: " +
			"Actions, the bot's permission, a workflow on the staging branch, protection rules"})
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

const topUsage = "usage: murasame [-h] [--version] {serve,tick,check-config,history} ..."

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
					usage = subParser(n).usage()
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
			fmt.Fprintln(stdout, topUsage+"\n\nA merge queue for Forgejo: batches, bisection, Forgejo Actions as the gate."+
				"\n\npositional arguments:\n  {serve,tick,check-config,history}\n"+
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
		msg := fmt.Sprintf("argument command: invalid choice: '%s' (choose from 'serve', 'tick', 'check-config', "+
			"'history')", name)
		return 0, &usageError{prog: "murasame", msg: msg}
	}
	rest := argv[cmdAt+1:]
	known, err := subParser(name).parse(rest, true)
	var help *helpRequest
	if err != nil && !errors.As(err, &help) {
		return 0, err
	}
	if help != nil {
		known, _ = subParser(name).parse(withoutHelp(rest), true)
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
		spec, err := instance.Load(cfgPath, nil, nil, "")
		if err != nil {
			return 0, err
		}
		a, err := subParser(name).parse(rest, false)
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
	a, err := subParser(name).parse(rest, false)
	if err != nil {
		return 0, err
	}
	if len(a.pos) > 0 {
		return 0, &usageError{prog: "murasame " + name, msg: "unrecognized arguments: " + strings.Join(a.pos, " ")}
	}
	switch name {
	case "check-config":
		code := checkConfig(cfg, "", true, stdout)
		if a.flag("forge") && !checkForge(cfg, "", stdout) {
			code = 1
		}
		return code, nil
	case "history":
		return History(cfg, historyArgs(a, repoDir), "", nil, stdout, stderr), nil
	case "serve", "tick":
		spec, err := instance.Load(cfgPath, nil, nil, repoDir)
		if err != nil {
			return 0, err
		}
		if name == "serve" && !a.flag("once") {
			go startupChecks(spec, stdout)
		}
		return server.Serve(spec, name == "tick" || a.flag("once")), nil
	}
	return 0, nil
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
		if name == "serve" && !a.flag("once") {
			go startupChecks(spec, stdout)
		}
		return server.Serve(spec, name == "tick" || a.flag("once"))
	case "history":
		return History(spec.Repos[0].Cfg, historyArgs(a, repoDir), spec.DB, instanceKeys(spec), stdout, stderr)
	}
	code := 0
	repos := "repositories"
	if len(spec.Repos) == 1 {
		repos = "repository"
	}
	fmt.Fprintf(stdout, "config %s: ok (multi-repo: %d %s)\n", spec.Path, len(spec.Repos), repos)
	fmt.Fprintf(stdout, "  instance: bot %s; listen %s; poll every %d s; state DB %s\n",
		spec.BotUser, spec.Listen, spec.PollInterval, spec.DB)
	for _, n := range spec.ForgeOrder {
		f := spec.Forges[n]
		fmt.Fprintf(stdout, "  forge %s: %s (token $%s)\n", f.Name, shownURL(f.URL), f.TokenEnv)
	}
	for _, e := range spec.Repos {
		fmt.Fprintf(stdout, "repo %s: %s/%s on %s, clone %s\n", e.Name, e.Cfg.Forgejo.Owner, e.Cfg.Forgejo.Repo,
			shownURL(e.Cfg.Forgejo.URL), e.Clone)
		checkConfig(e.Cfg, "  ", false, stdout)
		if a.flag("forge") && !checkForge(e.Cfg, "  ", stdout) {
			code = 1
		}
	}
	return code
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
	ci := cfg.CI
	contexts := "every job of the run"
	if len(ci.Contexts) > 0 {
		contexts = strings.Join(ci.Contexts, ", ")
	}
	start := ""
	if ci.StartTimeout > 0 {
		start = fmt.Sprintf("; no status after %d s: infrastructure failure", ci.StartTimeout)
	}
	say(fmt.Sprintf("  CI: pushes to %s, then waits for %s (timeout %d s%s; checked every %d s)", ci.Staging, contexts,
		ci.Timeout, start, ci.PollInterval))
	pair := "off"
	if cfg.PairCheck {
		pair = "on"
	}
	say(fmt.Sprintf("  pair check: %s", pair))
	for _, r := range cfg.Conflicts {
		say(fmt.Sprintf("  conflicts %s: %s -> %s", r.Label, strings.Join(r.Paths, " "), r.Strategy))
	}
	if cfg.Records.Enabled {
		say(fmt.Sprintf("  records: a tree that passed CI is not sent to CI again for %d s", cfg.Records.MaxAge))
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

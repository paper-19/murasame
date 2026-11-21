package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"murasame/internal/glob"
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type repoTable struct {
	Target        []string `mapstructure:"target"`
	Integ         string   `mapstructure:"integ"`
	StateDir      string   `mapstructure:"state_dir"`
	DB            string   `mapstructure:"db"`
	Key           string   `mapstructure:"key"`
	CreateInteg   bool     `mapstructure:"create_integ"`
	LockWait      int      `mapstructure:"lock_wait"`
	Batch         bool     `mapstructure:"batch"`
	PairCheck     bool     `mapstructure:"pair_check"`
	CommandPrefix []string `mapstructure:"command_prefix"`
}

type gateTable struct {
	Default string            `mapstructure:"default"`
	Timeout int               `mapstructure:"timeout"`
	Env     map[string]string `mapstructure:"env"`
}

type hooksTable struct {
	PostMerge []map[string]any `mapstructure:"post_merge"`
	PostLand  []map[string]any `mapstructure:"post_land"`
}

type mergeTable struct {
	NoFF bool `mapstructure:"no_ff"`
}

type projectFile struct {
	Repo      map[string]any            `mapstructure:"repo"`
	Gate      map[string]any            `mapstructure:"gate"`
	Gates     map[string]map[string]any `mapstructure:"gates"`
	Conflicts []map[string]any          `mapstructure:"conflicts"`
	Hooks     map[string]any            `mapstructure:"hooks"`
	Records   map[string]any            `mapstructure:"records"`
	Forgejo   map[string]any            `mapstructure:"forgejo"`
	Merge     map[string]any            `mapstructure:"merge"`
}

func build(d map[string]any, path string) (*Config, error) {
	var unknown []string
	for _, k := range slices.Sorted(maps.Keys(d)) {
		if !slices.Contains(ProjectTables, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return nil, Errf("unknown table(s): %s", strings.Join(unknown, ", "))
	}
	var f projectFile
	if err := Decode(d, &f, ""); err != nil {
		return nil, err
	}
	repo := repoTable{Target: []string{"main"}, Integ: "{state_dir}/integ", StateDir: ".murasame", DB: "murasame.sqlite3",
		PairCheck: true, CommandPrefix: []string{}}
	gate := gateTable{Timeout: 7200, Env: map[string]string{}}
	var hooks hooksTable
	var merge mergeTable
	records := RecordsSpec{MaxAge: 2 * 24 * 3600}
	fj := ForgejoSpec{TokenEnv: "MURASAME_FORGEJO_TOKEN", BotUser: "murasame", StatusContext: "murasame", BatchMax: 8,
		PollInterval: 60, Listen: "127.0.0.1:8737", WebhookSecretEnv: "MURASAME_WEBHOOK_SECRET", LogExcerptLines: 30,
		MarkMerged: true, InfraBackoff: 300, PollDuringGate: 30, ReplyCooldown: 60, Reviewers: []string{}}
	for _, t := range []struct {
		v    map[string]any
		out  any
		what string
	}{{f.Repo, &repo, "repo"}, {f.Gate, &gate, "gate"}, {f.Hooks, &hooks, "hooks"}, {f.Merge, &merge, "merge"},
		{f.Records, &records, "records"}, {f.Forgejo, &fj, "forgejo"}} {
		if err := Decode(t.v, t.out, t.what); err != nil {
			return nil, err
		}
	}
	if len(repo.Target) == 0 || slices.Contains(repo.Target, "") {
		return nil, Errf("repo.target: one or more branch names, not %q", repo.Target)
	}
	if fj.PollInterval <= 0 {
		return nil, Errf("forgejo.poll_interval: more than 0 s, not %d", fj.PollInterval)
	}
	if fj.BatchMax < 1 {
		return nil, Errf("forgejo.batch_max: at least 1, not %d", fj.BatchMax)
	}
	fj.ReviewersFromPermission = strings.ToLower(strings.TrimSpace(fj.ReviewersFromPermission))
	if fj.ReviewersFromPermission != "" && !slices.Contains(Permissions, fj.ReviewersFromPermission) {
		return nil, Errf("forgejo.reviewers_from_permission: one of %s (or \"\": only the reviewers), not %s",
			strings.Join(Permissions, ", "), strconv.Quote(fj.ReviewersFromPermission))
	}
	gs, err := gates(f.Gates, gate.Timeout, repo.CommandPrefix)
	if err != nil {
		return nil, err
	}
	order := slices.Sorted(maps.Keys(gs))
	defaultGate := gate.Default
	switch {
	case defaultGate == "" && len(order) == 1:
		defaultGate = order[0]
	case defaultGate == "":
		return nil, Errf("gate.default: required with more than one gate (one of %s)", strings.Join(order, ", "))
	case gs[defaultGate] == nil:
		return nil, Errf("gate.default: no gate %s", strconv.Quote(defaultGate))
	}
	cfg := &Config{
		Path: path, Target: repo.Target, Integ: repo.Integ, StateDir: repo.StateDir, DB: repo.DB, RepoKey: repo.Key,
		CreateInteg: repo.CreateInteg, LockWait: repo.LockWait, DefaultBatch: repo.Batch, PairCheck: repo.PairCheck,
		CommandPrefix: repo.CommandPrefix, Gates: gs, GateOrder: order, DefaultGate: defaultGate, GateEnv: gate.Env,
		NoFF: merge.NoFF, Records: records, Forgejo: fj,
	}
	if cfg.Conflicts, err = conflicts(f.Conflicts); err != nil {
		return nil, err
	}
	if cfg.PostMerge, err = postMerge(hooks.PostMerge); err != nil {
		return nil, err
	}
	if cfg.PostLand, err = postLand(hooks.PostLand); err != nil {
		return nil, err
	}
	if err := checkPlaceholders(cfg); err != nil {
		return nil, err
	}
	for _, name := range order {
		if gs[name].FullEvery > 0 && !records.Enabled {
			return nil, Errf("gates.%s.full_every: needs [records] enabled = true (the records count the landings)", name)
		}
	}
	return cfg, nil
}

func affected(spec *GateSpec, m map[string]any) error {
	what := "gates." + spec.Name
	if spec.AffectedCommand.Empty() {
		for _, k := range []string{"full_when", "full_every"} {
			if _, ok := m[k]; ok {
				return Errf("%s.%s: only with affected_command", what, k)
			}
		}
		return nil
	}
	for _, g := range spec.FullWhen {
		if err := glob.Check(g); err != nil {
			return Errf("%s.full_when: bad path pattern %q: %s", what, g, err)
		}
	}
	return nil
}

func gates(m map[string]map[string]any, timeout int, prefix []string) (map[string]*GateSpec, error) {
	if len(m) == 0 {
		return nil, Errf("no [gates.<name>] table: the queue needs a gate")
	}
	out := map[string]*GateSpec{}
	for _, name := range slices.Sorted(maps.Keys(m)) {
		if !nameRE.MatchString(name) {
			return nil, Errf("[gates.<name>]: a letter or digit, then letters, digits, '.', '-' and '_', not %s",
				strconv.Quote(name))
		}
		spec := &GateSpec{Name: name, Env: map[string]string{}, Timeout: timeout, Retries: 1, RetryWait: 30,
			CommandPrefix: prefix}
		if err := Decode(m[name], spec, "gates."+name, "command"); err != nil {
			return nil, err
		}
		if err := affected(spec, m[name]); err != nil {
			return nil, err
		}
		out[name] = spec
	}
	return out, nil
}

func conflicts(rules []map[string]any) ([]*ConflictRule, error) {
	var out []*ConflictRule
	for i, r := range rules {
		what := fmt.Sprintf("conflicts[%d]", i)
		rule := &ConflictRule{Take: "ours", Env: map[string]string{}, Timeout: 3600, Exclude: []string{}}
		if err := Decode(r, rule, what, "label", "paths", "strategy"); err != nil {
			return nil, err
		}
		if len(rule.Paths) == 0 {
			return nil, Errf("%s.paths: at least one pattern", what)
		}
		for _, g := range append(append([]string{}, rule.Paths...), rule.Exclude...) {
			if err := glob.Check(g); err != nil {
				return nil, Errf("%s: bad path pattern %q: %s", what, g, err)
			}
		}
		if !slices.Contains(Strategies, rule.Strategy) {
			return nil, Errf("%s.strategy: one of %s, not %s", what, strings.Join(Strategies, ", "), strconv.Quote(rule.Strategy))
		}
		if rule.Strategy == "command" {
			if rule.Run.Empty() {
				return nil, Errf("%s: strategy command needs run", what)
			}
			if rule.Take != "ours" && rule.Take != "theirs" {
				return nil, Errf("%s.take: 'ours' or 'theirs', not %s", what, strconv.Quote(rule.Take))
			}
		} else {
			for _, k := range []string{"run", "take", "env", "timeout"} {
				if _, ok := r[k]; ok {
					return nil, Errf("%s.%s: only for strategy command, not %s", what, k, strconv.Quote(rule.Strategy))
				}
			}
		}
		out = append(out, rule)
	}
	return out, nil
}

func postMerge(hooks []map[string]any) ([]*PostMergeHook, error) {
	var out []*PostMergeHook
	seen := map[string]bool{}
	for i, h := range hooks {
		what := fmt.Sprintf("hooks.post_merge[%d]", i)
		hook := &PostMergeHook{Env: map[string]string{}, Timeout: 3600}
		if err := Decode(h, hook, what, "name", "run"); err != nil {
			return nil, err
		}
		if err := checkName(hook.Name, what, seen); err != nil {
			return nil, err
		}
		out = append(out, hook)
	}
	return out, nil
}

func postLand(hooks []map[string]any) ([]*PostLandHook, error) {
	var out []*PostLandHook
	seen := map[string]bool{}
	for i, h := range hooks {
		what := fmt.Sprintf("hooks.post_land[%d]", i)
		hook := &PostLandHook{Env: map[string]string{}, Timeout: 3600}
		if err := Decode(h, hook, what, "name", "run"); err != nil {
			return nil, err
		}
		if hook.Checkout && hook.Dir == "" {
			return nil, Errf("%s.checkout: needs dir", what)
		}
		if err := checkName(hook.Name, what, seen); err != nil {
			return nil, err
		}
		out = append(out, hook)
	}
	return out, nil
}

func checkName(name, what string, seen map[string]bool) error {
	if !nameRE.MatchString(name) {
		return Errf("%s.name: a letter or digit, then letters, digits, '.', '-' and '_', not %s", what, strconv.Quote(name))
	}
	if seen[name] {
		return Errf("%s.name: %s is used twice", what, strconv.Quote(name))
	}
	seen[name] = true
	return nil
}

var (
	baseVars   = []string{"repo", "state_dir", "integ", "target"}
	gateVars   = append([]string{"gate", "retried", "base"}, baseVars...)
	mergeVars  = append([]string{"branch", "hook"}, baseVars...)
	ruleVars   = append([]string{"branch"}, baseVars...)
	landVars   = append([]string{"hook", "target_sha", "target_short"}, baseVars...)
	prefixVars = baseVars
)

func UnknownPlaceholder(s string, allowed []string) string {
	for _, m := range phRE.FindAllStringSubmatch(s, -1) {
		if m[1] != "" && !slices.Contains(allowed, m[1]) {
			return m[0]
		}
	}
	return ""
}

func checkPlaceholders(cfg *Config) error {
	type field struct {
		what    string
		values  []string
		allowed []string
	}
	envValues := func(m map[string]string) []string {
		out := make([]string, 0, len(m))
		for _, k := range slices.Sorted(maps.Keys(m)) {
			out = append(out, m[k])
		}
		return out
	}
	fields := []field{
		{"repo.state_dir", []string{cfg.StateDir}, []string{"repo"}},
		{"repo.integ", []string{cfg.Integ}, []string{"repo", "state_dir"}},
		{"repo.db", []string{cfg.DB}, []string{"repo", "state_dir"}},
		{"repo.key", []string{cfg.RepoKey}, []string{"repo"}},
		{"repo.command_prefix", cfg.CommandPrefix, prefixVars},
		{"gate.env", envValues(cfg.GateEnv), gateVars},
	}
	for _, name := range cfg.GateOrder {
		g := cfg.Gates[name]
		fields = append(fields, field{"gates." + name + ".command", g.Command.Args, gateVars},
			field{"gates." + name + ".affected_command", g.AffectedCommand.Args, gateVars},
			field{"gates." + name + ".env", envValues(g.Env), gateVars})
	}
	for i, r := range cfg.Conflicts {
		what := fmt.Sprintf("conflicts[%d]", i)
		fields = append(fields, field{what + ".run", r.Run.Args, ruleVars}, field{what + ".env", envValues(r.Env), ruleVars})
	}
	for i, h := range cfg.PostMerge {
		what := fmt.Sprintf("hooks.post_merge[%d]", i)
		fields = append(fields, field{what + ".run", h.Run.Args, mergeVars}, field{what + ".env", envValues(h.Env), mergeVars})
	}
	for i, h := range cfg.PostLand {
		what := fmt.Sprintf("hooks.post_land[%d]", i)
		fields = append(fields, field{what + ".run", h.Run.Args, landVars}, field{what + ".env", envValues(h.Env), landVars},
			field{what + ".dir", []string{h.Dir}, landVars})
	}
	for _, f := range fields {
		for _, v := range f.values {
			if bad := UnknownPlaceholder(v, f.allowed); bad != "" {
				return Errf("%s: unknown placeholder %s (here: %s)", f.what, bad, braced(f.allowed))
			}
		}
	}
	return nil
}

func braced(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "{" + n + "}"
	}
	return strings.Join(out, " ")
}

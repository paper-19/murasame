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
	Target    []string `mapstructure:"target"`
	Integ     string   `mapstructure:"integ"`
	StateDir  string   `mapstructure:"state_dir"`
	DB        string   `mapstructure:"db"`
	Key       string   `mapstructure:"key"`
	PairCheck bool     `mapstructure:"pair_check"`
}

type mergeTable struct {
	NoFF bool `mapstructure:"no_ff"`
}

type projectFile struct {
	Repo      map[string]any   `mapstructure:"repo"`
	CI        map[string]any   `mapstructure:"ci"`
	Conflicts []map[string]any `mapstructure:"conflicts"`
	Records   map[string]any   `mapstructure:"records"`
	Forgejo   map[string]any   `mapstructure:"forgejo"`
	Merge     map[string]any   `mapstructure:"merge"`
}

var refRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

func branchName(b string) bool {
	if !refRE.MatchString(b) || strings.HasPrefix(b, "refs/") || strings.HasPrefix(b, "-") || strings.Contains(b, "..") {
		return false
	}
	for _, part := range strings.Split(b, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") || strings.HasSuffix(part, ".") {
			return false
		}
	}
	return true
}

func DefaultCI() CISpec {
	return CISpec{Staging: "murasame/staging", Contexts: []string{}, Timeout: 7200, StartTimeout: 900, PollInterval: 15}
}

func checkCI(ci CISpec, targets []string) error {
	b := ci.Staging
	if !branchName(b) {
		return Errf("ci.staging: a branch name such as \"murasame/staging\", not %s", strconv.Quote(b))
	}
	for _, t := range targets {
		switch {
		case t == b:
			return Errf("ci.staging: %s is also a target branch; the staging branch must be one of its own", strconv.Quote(b))
		case strings.HasPrefix(b, t+"/") || strings.HasPrefix(t, b+"/"):
			return Errf("ci.staging: %s and the target %s can't both be branches (one is a directory of the other)",
				strconv.Quote(b), strconv.Quote(t))
		}
	}
	seen := map[string]bool{}
	for _, c := range ci.Contexts {
		if strings.TrimSpace(c) == "" {
			return Errf("ci.contexts: an empty context name")
		}
		if seen[c] {
			return Errf("ci.contexts: %s is listed twice", strconv.Quote(c))
		}
		seen[c] = true
	}
	if ci.Timeout <= 0 {
		return Errf("ci.timeout: more than 0 s, not %d", ci.Timeout)
	}
	if ci.PollInterval <= 0 {
		return Errf("ci.poll_interval: more than 0 s, not %d", ci.PollInterval)
	}
	if ci.StartTimeout > ci.Timeout {
		return Errf("ci.start_timeout: at most ci.timeout (%d s), not %d", ci.Timeout, ci.StartTimeout)
	}
	return nil
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
		PairCheck: true}
	ci := DefaultCI()
	var merge mergeTable
	records := RecordsSpec{MaxAge: 2 * 24 * 3600}
	fj := ForgejoSpec{TokenEnv: "MURASAME_FORGEJO_TOKEN", BotUser: "murasame", StatusContext: "murasame", BatchMax: 8,
		PollInterval: 60, Listen: "127.0.0.1:8737", WebhookSecretEnv: "MURASAME_WEBHOOK_SECRET", LogExcerptLines: 30,
		MarkMerged: true, InfraBackoff: 300, PollDuringGate: 30, ReplyCooldown: 60, Reviewers: []string{}}
	for _, t := range []struct {
		v    map[string]any
		out  any
		what string
	}{{f.Repo, &repo, "repo"}, {f.CI, &ci, "ci"}, {f.Merge, &merge, "merge"}, {f.Records, &records, "records"},
		{f.Forgejo, &fj, "forgejo"}} {
		if err := Decode(t.v, t.out, t.what); err != nil {
			return nil, err
		}
	}
	if len(repo.Target) == 0 || slices.Contains(repo.Target, "") {
		return nil, Errf("repo.target: one or more branch names, not %q", repo.Target)
	}
	if err := checkCI(ci, repo.Target); err != nil {
		return nil, err
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
	if fj.StatusContext != "" && slices.Contains(ci.Contexts, fj.StatusContext) {
		return nil, Errf("ci.contexts: %s is murasame's own status context (forgejo.status_context)",
			strconv.Quote(fj.StatusContext))
	}
	cfg := &Config{
		Path: path, Target: repo.Target, Integ: repo.Integ, StateDir: repo.StateDir, DB: repo.DB, RepoKey: repo.Key,
		PairCheck: repo.PairCheck, CI: ci, NoFF: merge.NoFF, Records: records, Forgejo: fj,
	}
	var err error
	if cfg.Conflicts, err = conflicts(f.Conflicts); err != nil {
		return nil, err
	}
	if err := checkPlaceholders(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func conflicts(rules []map[string]any) ([]*ConflictRule, error) {
	var out []*ConflictRule
	for i, r := range rules {
		what := fmt.Sprintf("conflicts[%d]", i)
		rule := &ConflictRule{Exclude: []string{}}
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
		out = append(out, rule)
	}
	return out, nil
}

func UnknownPlaceholder(s string, allowed []string) string {
	for _, m := range phRE.FindAllStringSubmatch(s, -1) {
		if m[1] != "" && !slices.Contains(allowed, m[1]) {
			return m[0]
		}
	}
	return ""
}

func checkPlaceholders(cfg *Config) error {
	fields := []struct {
		what, value string
		allowed     []string
	}{
		{"repo.state_dir", cfg.StateDir, []string{"repo"}},
		{"repo.integ", cfg.Integ, []string{"repo", "state_dir"}},
		{"repo.db", cfg.DB, []string{"repo", "state_dir"}},
		{"repo.key", cfg.RepoKey, []string{"repo"}},
	}
	for _, f := range fields {
		if bad := UnknownPlaceholder(f.value, f.allowed); bad != "" {
			return Errf("%s: unknown placeholder %s (here: %s)", f.what, bad, braced(f.allowed))
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

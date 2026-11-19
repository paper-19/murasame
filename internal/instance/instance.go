package instance

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"murasame/internal/config"
	"murasame/internal/db"
	"murasame/internal/secretenv"
	"murasame/internal/states"
)

var InstanceKeys = []string{"url", "token_env", "bot_user", "listen", "webhook_secret_env", "poll_interval",
	"external_url"}

var IdentityKeys = []string{"owner", "repo", "git_remote_url"}

var (
	repositoryRE = regexp.MustCompile(`^([^/\s]+)/([^/\s]+)$`)
	nameRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

type ForgeSpec struct {
	Name     string
	URL      string
	TokenEnv string
	GitURL   string
}

type RepoEntry struct {
	Name  string
	Cfg   *config.Config
	Clone string
}

type Spec struct {
	Path               string
	Multi              bool
	Repos              []RepoEntry
	BotUser            string
	Listen             string
	WebhookSecretEnv   string
	PollInterval       int
	MaxConcurrentGates int
	ExternalURL        string
	StateDir           string
	DB                 string
	Forges             map[string]*ForgeSpec
	ForgeOrder         []string
}

func merge(base, over map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		bm, bok := out[k].(map[string]any)
		om, ook := v.(map[string]any)
		if bok && ook {
			out[k] = merge(bm, om)
		} else {
			out[k] = v
		}
	}
	return out
}

type multiFile struct {
	Instance map[string]any `mapstructure:"instance"`
	Forges   map[string]any `mapstructure:"forges"`
	Repos    []any          `mapstructure:"repos"`
}

type instanceTable struct {
	BotUser            string `mapstructure:"bot_user"`
	Listen             string `mapstructure:"listen"`
	WebhookSecretEnv   string `mapstructure:"webhook_secret_env"`
	PollInterval       int    `mapstructure:"poll_interval"`
	MaxConcurrentGates int    `mapstructure:"max_concurrent_gates"`
	ExternalURL        string `mapstructure:"external_url"`
	StateDir           string `mapstructure:"state_dir"`
	DB                 string `mapstructure:"db"`
}

type forgeTable struct {
	URL      string `mapstructure:"url"`
	TokenEnv string `mapstructure:"token_env"`
	GitURL   string `mapstructure:"git_url"`
}

type repoIdentity struct {
	Repository string         `mapstructure:"repository"`
	Name       string         `mapstructure:"name"`
	Tables     map[string]any `mapstructure:",remain"`
}

type repoEntry struct {
	Forge   string         `mapstructure:"forge"`
	Clone   string         `mapstructure:"clone"`
	GitURL  string         `mapstructure:"git_url"`
	Include string         `mapstructure:"include"`
	Rest    map[string]any `mapstructure:",remain"`
}

func abs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

func Load(path string, text *string, env config.Env, repoDir string) (*Spec, error) {
	if env == nil {
		env = config.OSEnv()
	}
	raw, err := config.ReadTOML(path, text)
	if err != nil {
		return nil, err
	}
	if !config.IsMulti(raw) {
		cfg, err := config.Build(config.ExpandEnv(raw, env).(map[string]any), path, "")
		if err != nil {
			return nil, err
		}
		fj := cfg.Forgejo
		secretenv.Hide(fj.TokenEnv, fj.WebhookSecretEnv)
		name := "repo"
		if fj.Repo != "" && nameRE.MatchString(fj.Repo) {
			name = fj.Repo
		}
		if repoDir == "" {
			repoDir = "."
		}
		return &Spec{Path: path, Repos: []RepoEntry{{name, cfg, abs(repoDir)}}, BotUser: fj.BotUser, Listen: fj.Listen,
			WebhookSecretEnv: fj.WebhookSecretEnv, PollInterval: fj.PollInterval, MaxConcurrentGates: 1,
			ExternalURL: fj.ExternalURL, Forges: map[string]*ForgeSpec{}}, nil
	}
	return multi(config.ExpandEnv(raw, env).(map[string]any), path, env)
}

func multi(d map[string]any, path string, env config.Env) (*Spec, error) {
	var f multiFile
	if err := config.Decode(d, &f, "multi-repo config: table(s)"); err != nil {
		return nil, err
	}
	inst := instanceTable{BotUser: "murasame", Listen: "127.0.0.1:8737", WebhookSecretEnv: "MURASAME_WEBHOOK_SECRET",
		PollInterval: 60, MaxConcurrentGates: 1, DB: db.DefaultName}
	if err := config.Decode(f.Instance, &inst, "instance"); err != nil {
		return nil, err
	}
	if inst.StateDir == "" {
		return nil, config.Errf("instance.state_dir: required (the shared state DB and, by default, the repos' clones)")
	}
	if inst.PollInterval <= 0 {
		return nil, config.Errf("instance.poll_interval: more than 0 s, not %d", inst.PollInterval)
	}
	if inst.MaxConcurrentGates < 1 {
		return nil, config.Errf("instance.max_concurrent_gates: at least 1")
	}
	here := abs(".")
	if path != "" {
		here = filepath.Dir(abs(path))
	}
	sd := inst.StateDir
	if !filepath.IsAbs(sd) {
		sd = filepath.Join(here, sd)
	}
	spec := &Spec{Path: path, Multi: true, BotUser: inst.BotUser, Listen: inst.Listen,
		WebhookSecretEnv: inst.WebhookSecretEnv, PollInterval: inst.PollInterval,
		MaxConcurrentGates: inst.MaxConcurrentGates, ExternalURL: inst.ExternalURL, StateDir: sd,
		DB: db.Path(sd, inst.DB), Forges: map[string]*ForgeSpec{}}
	if len(f.Forges) == 0 {
		return nil, config.Errf("forges: at least one [forges.<name>] table (url, token_env)")
	}
	for _, fname := range slices.Sorted(maps.Keys(f.Forges)) {
		what := "forges." + fname
		ft := forgeTable{TokenEnv: "MURASAME_FORGEJO_TOKEN"}
		if err := config.Decode(f.Forges[fname], &ft, what); err != nil {
			return nil, err
		}
		if ft.URL == "" {
			return nil, config.Errf("%s.url: required", what)
		}
		if ft.GitURL == "" {
			ft.GitURL = ft.URL
		}
		spec.Forges[fname] = &ForgeSpec{fname, ft.URL, ft.TokenEnv, ft.GitURL}
		spec.ForgeOrder = append(spec.ForgeOrder, fname)
		secretenv.Hide(ft.TokenEnv)
	}
	secretenv.Hide(spec.WebhookSecretEnv)
	if len(f.Repos) == 0 {
		return nil, config.Errf("repos: at least one [[repos]] entry")
	}
	seenNames := map[string]string{}
	seenRepos := map[string]string{}
	seenKeys := map[string]string{}
	var places []repoPlaces
	for i, r := range f.Repos {
		e, err := repo(r, i, spec, here, env)
		if err != nil {
			return nil, err
		}
		if other, ok := seenNames[strings.ToLower(e.Name)]; ok {
			if other == e.Name {
				return nil, config.Errf("repos[%d].name: %s twice", i, strconv.Quote(e.Name))
			}
			return nil, config.Errf("repos[%d].name: %s and %s differ only in case (their files would collide on a "+
				"case-insensitive filesystem)", i, strconv.Quote(other), strconv.Quote(e.Name))
		}
		seenNames[strings.ToLower(e.Name)] = e.Name
		fj := e.Cfg.Forgejo
		host := db.ForgeHost(fj.URL)
		k := host + "\x00" + strings.ToLower(fj.Owner) + "\x00" + strings.ToLower(fj.Repo)
		if other, ok := seenRepos[k]; ok {
			return nil, config.Errf("repos %s and %s: the same repository (%s/%s/%s)", strconv.Quote(other), strconv.Quote(e.Name),
				host, fj.Owner, fj.Repo)
		}
		seenRepos[k] = e.Name
		key := EffectiveRepoKey(e)
		if other, ok := seenKeys[key]; ok {
			return nil, config.Errf("repos %s and %s: the same state DB key %s (each repository needs its own rows: "+
				"set a different [repo] key, or none)", strconv.Quote(other), strconv.Quote(e.Name), strconv.Quote(key))
		}
		seenKeys[key] = e.Name
		places = append(places, placesOf(e))
		spec.Repos = append(spec.Repos, e)
	}
	if err := checkPlaces(places); err != nil {
		return nil, err
	}
	return spec, nil
}

func EffectiveRepoKey(e RepoEntry) string {
	if k := config.Expand(e.Cfg.RepoKey, config.Vars{"repo": e.Clone}); k != "" {
		return k
	}
	fj := e.Cfg.Forgejo
	return db.ForgeKey(db.ForgeHost(fj.URL), fj.Owner, fj.Repo)
}

type repoPlaces struct {
	name     string
	clone    string
	stateDir string
	integ    string
}

func placesOf(e RepoEntry) repoPlaces {
	sd := config.Expand(e.Cfg.StateDir, config.Vars{"repo": e.Clone})
	if !filepath.IsAbs(sd) {
		sd = filepath.Join(e.Clone, sd)
	}
	integ := config.Expand(e.Cfg.Integ, config.Vars{"repo": e.Clone, "state_dir": sd})
	if !filepath.IsAbs(integ) {
		integ = filepath.Join(e.Clone, integ)
	}
	return repoPlaces{e.Name, filepath.Clean(e.Clone), filepath.Clean(sd), filepath.Clean(integ)}
}

type place struct{ what, path string }

func (p repoPlaces) places() []place {
	return []place{{"clone", p.clone}, {"state dir", p.stateDir}, {"integration tree", p.integ}}
}

func samePlace(a, b string) bool {
	return strings.EqualFold(a, b)
}

func within(p, root string) bool {
	rel, err := filepath.Rel(strings.ToLower(root), strings.ToLower(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func checkPlaces(ps []repoPlaces) error {
	for i, a := range ps {
		for _, b := range ps[:i] {
			if samePlace(a.clone, b.clone) {
				return config.Errf("repos %s and %s: the same clone %s (each repository needs its own [[repos]] clone)",
					strconv.Quote(b.name), strconv.Quote(a.name), a.clone)
			}
			if samePlace(a.stateDir, b.stateDir) {
				return config.Errf("repos %s and %s: the same state dir %s (each repository needs its own: logs, lock, "+
					"integration tree)", strconv.Quote(b.name), strconv.Quote(a.name), a.stateDir)
			}
			if samePlace(a.integ, b.integ) {
				return config.Errf("repos %s and %s: the same integration tree %s (each repository needs its own [repo] integ)",
					strconv.Quote(b.name), strconv.Quote(a.name), a.integ)
			}
		}
	}
	for _, a := range ps {
		for _, b := range ps {
			if a.name == b.name {
				continue
			}
			for _, x := range a.places() {
				for _, y := range b.places() {
					if within(x.path, y.path) {
						return config.Errf("repos %s and %s: the %s %s is inside %s's %s %s", strconv.Quote(a.name),
							strconv.Quote(b.name), x.what, x.path, strconv.Quote(b.name), y.what, y.path)
					}
				}
			}
		}
	}
	return nil
}

func repo(r any, i int, spec *Spec, here string, env config.Env) (RepoEntry, error) {
	what := fmt.Sprintf("repos[%d]", i)
	var id repoIdentity
	if err := config.Decode(r, &id, what); err != nil {
		return RepoEntry{}, err
	}
	var unknown []string
	for _, k := range slices.Sorted(maps.Keys(id.Tables)) {
		if !slices.Contains([]string{"forge", "clone", "git_url", "include"}, k) && !slices.Contains(config.ProjectTables, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return RepoEntry{}, config.Errf("%s: unknown key(s): %s", what, strings.Join(unknown, ", "))
	}
	m := repositoryRE.FindStringSubmatch(id.Repository)
	if m == nil {
		if id.Repository != "" {
			return RepoEntry{}, config.Errf("%s.repository: \"<owner>/<name>\" on the forge, not %q", what, id.Repository)
		}
		return RepoEntry{}, config.Errf("%s.repository: required, \"<owner>/<name>\" on the forge", what)
	}
	owner, rp := m[1], m[2]
	name := id.Name
	if name == "" {
		name = rp
	}
	if !nameRE.MatchString(name) {
		return RepoEntry{}, config.Errf("%s.name: a short name (letters, digits, . _ -), not %s", what, strconv.Quote(name))
	}
	what = "repos[" + name + "]"
	var e repoEntry
	if err := config.Decode(id.Tables, &e, what); err != nil {
		return RepoEntry{}, err
	}
	fname := e.Forge
	if fname == "" && len(spec.Forges) == 1 {
		fname = spec.ForgeOrder[0]
	}
	forge, ok := spec.Forges[fname]
	if !ok {
		if fname != "" {
			return RepoEntry{}, config.Errf("%s.forge: one of %s", what, strings.Join(spec.ForgeOrder, ", "))
		}
		return RepoEntry{}, config.Errf("%s.forge: several forges: say which", what)
	}
	clone := e.Clone
	if clone == "" {
		clone = "{state_dir}/{name}.git"
	}
	if bad := config.UnknownPlaceholder(clone, []string{"state_dir", "name"}); bad != "" {
		return RepoEntry{}, config.Errf("%s.clone: unknown placeholder %s (here: {state_dir} {name})", what, bad)
	}
	clone = config.Expand(clone, config.Vars{"state_dir": spec.StateDir, "name": name})
	if !filepath.IsAbs(clone) {
		clone = filepath.Join(spec.StateDir, clone)
	}
	gitURL, src := e.GitURL, e.Include
	if gitURL == "" {
		gitURL = fmt.Sprintf("%s/%s/%s.git", strings.TrimRight(forge.GitURL, "/"), owner, rp)
	}
	inline := map[string]any{}
	for k, v := range e.Rest {
		inline[k] = v
	}
	fjInline, err := config.Table(inline["forgejo"], what+".forgejo")
	if err != nil {
		return RepoEntry{}, err
	}
	var bad []string
	for k := range fjInline {
		if slices.Contains(InstanceKeys, k) || slices.Contains(IdentityKeys, k) {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		return RepoEntry{}, config.Errf("%s.forgejo: %s: set in [instance], [forges.*] or this [[repos]] entry (repository, git_url)",
			what, strings.Join(bad, ", "))
	}
	rr, err := config.Table(inline["repo"], what+".repo")
	if err != nil {
		return RepoEntry{}, err
	}
	if _, has := rr["db"]; has {
		return RepoEntry{}, config.Errf("%s.repo.db: a multi-repo instance has one state DB: [instance] db", what)
	}
	base := map[string]any{}
	if src != "" {
		if !filepath.IsAbs(src) {
			src = filepath.Join(here, src)
		}
		raw, err := config.ReadTOML(src, nil)
		if err != nil {
			return RepoEntry{}, err
		}
		if config.IsMulti(raw) {
			return RepoEntry{}, config.Errf("%s.include: %s is a multi-repo config, not a project config", what, src)
		}
		base = config.ExpandEnv(raw, env).(map[string]any)
		bf, err := config.Table(base["forgejo"], src+": forgejo")
		if err != nil {
			return RepoEntry{}, err
		}
		fj := map[string]any{}
		for k, v := range bf {
			if name, ok := v.(string); ok && (k == "token_env" || k == "webhook_secret_env") {
				secretenv.Hide(name)
			}
			if !slices.Contains(InstanceKeys, k) && !slices.Contains(IdentityKeys, k) {
				fj[k] = v
			}
		}
		base["forgejo"] = fj
		br, err := config.Table(base["repo"], src+": repo")
		if err != nil {
			return RepoEntry{}, err
		}
		rp := map[string]any{}
		for k, v := range br {
			if k != "db" {
				rp[k] = v
			}
		}
		base["repo"] = rp
	}
	merged := merge(base, inline)
	fj := map[string]any{}
	if mf, ok := merged["forgejo"].(map[string]any); ok {
		for k, v := range mf {
			fj[k] = v
		}
	}
	for k, v := range map[string]any{"url": forge.URL, "token_env": forge.TokenEnv, "bot_user": spec.BotUser,
		"owner": owner, "repo": rp, "git_remote_url": gitURL, "listen": spec.Listen,
		"webhook_secret_env": spec.WebhookSecretEnv, "poll_interval": int64(spec.PollInterval),
		"external_url": spec.ExternalURL} {
		fj[k] = v
	}
	merged["forgejo"] = fj
	rp2 := map[string]any{}
	if mr, ok := merged["repo"].(map[string]any); ok {
		for k, v := range mr {
			rp2[k] = v
		}
	}
	rp2["db"] = config.Literal(spec.DB)
	merged["repo"] = rp2
	from := src
	if from == "" {
		from = spec.Path
	}
	cfg, err := config.Build(merged, from, what)
	if err != nil {
		return RepoEntry{}, err
	}
	return RepoEntry{name, cfg, clone}, nil
}

type GateSlots struct {
	N         int
	mu        sync.Mutex
	changed   chan struct{}
	Holders   []string
	Waiting   []string
	listeners []func()
	closed    bool
	gating    map[string]bool
}

func NewGateSlots(n int) *GateSlots {
	if n < 1 {
		n = 1
	}
	return &GateSlots{N: n, changed: make(chan struct{}), gating: map[string]bool{}}
}

func (g *GateSlots) AddListener(f func()) {
	g.mu.Lock()
	g.listeners = append(g.listeners, f)
	g.mu.Unlock()
}

func remove(xs []string, x string) []string {
	if i := slices.Index(xs, x); i >= 0 {
		return append(xs[:i:i], xs[i+1:]...)
	}
	return xs
}

func (g *GateSlots) notify() {
	close(g.changed)
	g.changed = make(chan struct{})
}

const Closing = "the service is stopping"

func (g *GateSlots) Close() {
	g.mu.Lock()
	if !g.closed {
		g.closed = true
		for _, who := range append([]string{}, g.Waiting...) {
			g.step(who, states.SlotWithdraw)
		}
		g.notify()
	}
	g.mu.Unlock()
}

func (g *GateSlots) Closed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

func (g *GateSlots) at(who string) states.Slot {
	switch {
	case slices.Index(g.Holders, who) >= 0:
		return states.SlotHolding
	case slices.Index(g.Waiting, who) >= 0:
		return states.SlotWaiting
	}
	return states.SlotOut
}

func (g *GateSlots) step(who string, ev states.SlotEvent) {
	states.SlotMachine.Must(g.at(who), ev)
	if ev != states.SlotJoin {
		delete(g.gating, who)
	}
	switch ev {
	case states.SlotJoin:
		g.Waiting = append(g.Waiting, who)
	case states.SlotGrant:
		g.Waiting = remove(g.Waiting, who)
		g.Holders = append(g.Holders, who)
	case states.SlotRelease:
		g.Holders = remove(g.Holders, who)
	case states.SlotWithdraw:
		g.Waiting = remove(g.Waiting, who)
	}
}

func (g *GateSlots) State(who string) states.Slot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.at(who)
}

func (g *GateSlots) Ready(who string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	if g.at(who) == states.SlotHolding {
		return true
	}
	if g.at(who) == states.SlotOut {
		g.step(who, states.SlotJoin)
	}
	return g.freeFor(who, false)
}

func (g *GateSlots) TryAcquire(who string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	if g.at(who) == states.SlotHolding {
		return true
	}
	if g.at(who) == states.SlotOut {
		g.step(who, states.SlotJoin)
	}
	g.gating[who] = true
	if g.freeFor(who, true) {
		g.step(who, states.SlotGrant)
		return true
	}
	return false
}

func (g *GateSlots) Acquire(who string, poll func() string, every time.Duration) string {
	if every <= 0 {
		every = time.Second
	}
	for {
		if g.TryAcquire(who) {
			return ""
		}
		if g.Closed() {
			return Closing
		}
		if poll != nil {
			if why := poll(); why != "" {
				g.Withdraw(who)
				return why
			}
		}
		g.mu.Lock()
		ch := g.changed
		wait := g.at(who) != states.SlotHolding && !g.freeFor(who, true)
		g.mu.Unlock()
		if wait {
			select {
			case <-ch:
			case <-time.After(every):
			}
		}
	}
}

func (g *GateSlots) freeFor(who string, gate bool) bool {
	free := g.N - len(g.Holders)
	i := slices.Index(g.Waiting, who)
	if i < 0 || free <= 0 {
		return false
	}
	ahead := 0
	for _, w := range g.Waiting[:i] {
		if !gate || g.gating[w] {
			ahead++
		}
	}
	return ahead < free
}

func (g *GateSlots) Release(who string) {
	g.mu.Lock()
	if g.at(who) == states.SlotHolding {
		g.step(who, states.SlotRelease)
	}
	g.notify()
	var ls []func()
	if len(g.Waiting) > 0 {
		ls = append(ls, g.listeners...)
	}
	g.mu.Unlock()
	for _, f := range ls {
		f()
	}
}

func (g *GateSlots) Withdraw(who string) {
	g.mu.Lock()
	wake := false
	if g.at(who) == states.SlotWaiting {
		g.step(who, states.SlotWithdraw)
		wake = len(g.Waiting) > 0
		g.notify()
	}
	var ls []func()
	if wake {
		ls = append(ls, g.listeners...)
	}
	g.mu.Unlock()
	for _, f := range ls {
		f()
	}
}

type Snapshot struct {
	Max     int      `json:"max"`
	Running []string `json:"running"`
	Waiting []string `json:"waiting"`
}

func (g *GateSlots) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Snapshot{g.N, append([]string{}, g.Holders...), append([]string{}, g.Waiting...)}
}

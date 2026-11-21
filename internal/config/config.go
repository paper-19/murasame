package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"murasame/internal/secretenv"
)

var (
	envRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(\}|:-)`)
	phRE  = regexp.MustCompile(`\{\{|\}\}|\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func Errf(format string, a ...any) error { return &Error{fmt.Sprintf(format, a...)} }

type Env map[string]string

func OSEnv() Env {
	out := Env{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

func ExpandEnv(v any, env Env) any {
	return expandEnvAt(v, env, nil)
}

func expandEnvAt(v any, env Env, path []string) any {
	switch x := v.(type) {
	case string:
		return expandString(x, env, placeholderField(path))
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = expandEnvAt(e, env, path)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = expandEnvAt(e, env, append(path[:len(path):len(path)], k))
		}
		return out
	}
	return v
}

func placeholderField(path []string) bool {
	if len(path) > 1 && path[0] == "repos" {
		if len(path) == 2 && path[1] == "clone" {
			return true
		}
		path = path[1:]
	}
	if len(path) < 2 {
		return false
	}
	switch path[0] {
	case "repo":
		return len(path) == 2 && slices.Contains([]string{"state_dir", "integ", "db", "key", "command_prefix"}, path[1])
	case "gate":
		return path[1] == "env"
	case "gates":
		return len(path) >= 3 && (path[2] == "command" || path[2] == "affected_command" || path[2] == "env")
	case "conflicts":
		return path[1] == "run" || path[1] == "env"
	case "hooks":
		return len(path) >= 3 && (path[2] == "run" || path[2] == "env" || path[1] == "post_land" && path[2] == "dir")
	}
	return false
}

func Literal(s string) string {
	return strings.NewReplacer("{", "{{", "}", "}}").Replace(s)
}

func expandString(s string, env Env, placeholders bool) string {
	idx := envRefs(s)
	if idx == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range idx {
		b.WriteString(s[last:m[0]])
		got, set := env[s[m[2]:m[3]]], true
		if got == "" && m[4] >= 0 {
			got, set = s[m[4]:m[5]], false
		}
		if set && placeholders {
			got = Literal(got)
		}
		b.WriteString(got)
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

func envRefs(s string) [][]int {
	var out [][]int
	for at := 0; at < len(s); {
		m := envRE.FindStringSubmatchIndex(s[at:])
		if m == nil {
			break
		}
		for i := range m {
			m[i] += at
		}
		if s[m[4]:m[5]] == "}" {
			out = append(out, []int{m[0], m[1], m[2], m[3], -1, -1})
			at = m[1]
			continue
		}
		end := defaultEnd(s, m[5])
		if end < 0 {
			at = m[0] + 1
			continue
		}
		out = append(out, []int{m[0], end + 1, m[2], m[3], m[5], end})
		at = end + 1
	}
	return out
}

func defaultEnd(s string, from int) int {
	depth, fallback := 0, -1
	for i := from; i < len(s); i++ {
		switch {
		case strings.HasPrefix(s[i:], "{{"):
			i++
		case s[i] == '{':
			depth++
		case depth > 0 && s[i] == '}':
			depth--
		case strings.HasPrefix(s[i:], "}}"):
			fallback = i
			i++
		case s[i] == '}':
			return i
		}
	}
	return fallback
}

type Vars map[string]string

func (v Vars) With(kv ...string) Vars {
	out := make(Vars, len(v)+len(kv)/2)
	for k, x := range v {
		out[k] = x
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func Expand(s string, v Vars) string {
	return phRE.ReplaceAllStringFunc(s, func(m string) string {
		switch m {
		case "{{":
			return "{"
		case "}}":
			return "}"
		}
		if x, ok := v[m[1:len(m)-1]]; ok {
			return x
		}
		return m
	})
}

func ExpandAll(args []string, v Vars) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = Expand(a, v)
	}
	return out
}

type Cmd struct {
	Args []string
}

func (c Cmd) Empty() bool { return c.Args == nil }

type GateSpec struct {
	Name            string            `mapstructure:"-"`
	Command         Cmd               `mapstructure:"command"`
	Env             map[string]string `mapstructure:"env"`
	Timeout         int               `mapstructure:"timeout"`
	Retries         int               `mapstructure:"retries"`
	RetryWait       int               `mapstructure:"retry_wait"`
	CommandPrefix   []string          `mapstructure:"-"`
	AffectedCommand Cmd               `mapstructure:"affected_command"`
	FullWhen        []string          `mapstructure:"full_when"`
	FullEvery       int               `mapstructure:"full_every"`
}

func strsJSON(xs []string) any {
	if xs == nil {
		return nil
	}
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

func strMapJSON(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (g *GateSpec) ConfigHash(commonEnv map[string]string) string {
	return hashJSON(g.hashFields(commonEnv))
}

func (g *GateSpec) AffectedHash(commonEnv map[string]string) string {
	top := g.hashFields(commonEnv)
	top["scope"], top["affected_command"] = "affected", g.AffectedCommand.Args
	return hashJSON(top)
}

func (g *GateSpec) hashFields(commonEnv map[string]string) map[string]any {
	return map[string]any{"name": g.Name, "command": g.Command.Args, "env": strMapJSON(g.Env),
		"common_env": strMapJSON(commonEnv), "command_prefix": strsJSON(g.CommandPrefix)}
}

func hashJSON(top map[string]any) string {
	blob, _ := json.Marshal(top)
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])[:16]
}

type ConflictRule struct {
	Label    string            `mapstructure:"label"`
	Paths    []string          `mapstructure:"paths"`
	Strategy string            `mapstructure:"strategy"`
	Exclude  []string          `mapstructure:"exclude"`
	Run      Cmd               `mapstructure:"run"`
	Take     string            `mapstructure:"take"`
	Env      map[string]string `mapstructure:"env"`
	Timeout  int               `mapstructure:"timeout"`
}

var Strategies = []string{"keep-both", "ours", "theirs", "command", "stop"}

type PostMergeHook struct {
	Name    string            `mapstructure:"name"`
	Run     Cmd               `mapstructure:"run"`
	Env     map[string]string `mapstructure:"env"`
	Timeout int               `mapstructure:"timeout"`
}

type PostLandHook struct {
	Name     string            `mapstructure:"name"`
	Run      Cmd               `mapstructure:"run"`
	Dir      string            `mapstructure:"dir"`
	Checkout bool              `mapstructure:"checkout"`
	Env      map[string]string `mapstructure:"env"`
	Timeout  int               `mapstructure:"timeout"`
}

type RecordsSpec struct {
	Enabled bool `mapstructure:"enabled"`
	MaxAge  int  `mapstructure:"max_age"`
}

var Permissions = []string{"read", "write", "admin", "owner"}

type ForgejoSpec struct {
	URL                     string   `mapstructure:"url"`
	Owner                   string   `mapstructure:"owner"`
	Repo                    string   `mapstructure:"repo"`
	TokenEnv                string   `mapstructure:"token_env"`
	BotUser                 string   `mapstructure:"bot_user"`
	GitRemoteURL            string   `mapstructure:"git_remote_url"`
	Reviewers               []string `mapstructure:"reviewers"`
	ReviewersFromPermission string   `mapstructure:"reviewers_from_permission"`
	StatusContext           string   `mapstructure:"status_context"`
	BatchMax                int      `mapstructure:"batch_max"`
	PollInterval            int      `mapstructure:"poll_interval"`
	Listen                  string   `mapstructure:"listen"`
	WebhookSecretEnv        string   `mapstructure:"webhook_secret_env"`
	CommentOnStart          bool     `mapstructure:"comment_on_start"`
	LogExcerptLines         int      `mapstructure:"log_excerpt_lines"`
	MarkMerged              bool     `mapstructure:"mark_merged"`
	InfraBackoff            int      `mapstructure:"infra_backoff"`
	ExternalURL             string   `mapstructure:"external_url"`
	PollDuringGate          int      `mapstructure:"poll_during_gate"`
	ReplyCooldown           int      `mapstructure:"reply_cooldown"`
}

type Config struct {
	Path          string
	Target        []string
	Integ         string
	StateDir      string
	DB            string
	RepoKey       string
	CreateInteg   bool
	LockWait      int
	DefaultBatch  bool
	PairCheck     bool
	Gates         map[string]*GateSpec
	GateOrder     []string
	DefaultGate   string
	GateEnv       map[string]string
	Conflicts     []*ConflictRule
	NoFF          bool
	PostMerge     []*PostMergeHook
	PostLand      []*PostLandHook
	Records       RecordsSpec
	Forgejo       ForgejoSpec
	CommandPrefix []string
}

func (c *Config) Copy() *Config {
	cp := *c
	return &cp
}

var ProjectTables = []string{"repo", "gate", "gates", "conflicts", "hooks", "records", "forgejo", "merge"}

func ReadTOML(path string, text *string) (map[string]any, error) {
	var data []byte
	if text != nil {
		data = []byte(*text)
	} else {
		if path == "" {
			return nil, Errf("no config given")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, Errf("%s: %s", path, osReason(err))
		}
		data = b
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		where := path
		if where == "" {
			where = "config"
		}
		return nil, Errf("%s: %s", where, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func osReason(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func IsMulti(raw map[string]any) bool {
	for _, k := range []string{"instance", "forges", "repos"} {
		if _, ok := raw[k]; ok {
			return true
		}
	}
	return false
}

type LoadOptions struct {
	Text *string
	Env  Env
}

func Load(path string, opt LoadOptions) (*Config, error) {
	env := opt.Env
	if env == nil {
		env = OSEnv()
	}
	raw, err := ReadTOML(path, opt.Text)
	if err != nil {
		return nil, err
	}
	if IsMulti(raw) {
		where := path
		if where == "" {
			where = "config"
		}
		return nil, Errf("%s: a multi-repo service config ([instance], [[repos]]): it is for `murasame serve` / "+
			"`tick` / `check-config` / `history`", where)
	}
	cfg, err := Build(ExpandEnv(raw, env).(map[string]any), path, "")
	if err != nil {
		return nil, err
	}
	secretenv.Hide(cfg.Forgejo.TokenEnv, cfg.Forgejo.WebhookSecretEnv)
	return cfg, nil
}

func Build(d map[string]any, path, what string) (*Config, error) {
	cfg, err := build(d, path)
	if err != nil && what != "" {
		return nil, Errf("%s: %s", what, err)
	}
	return cfg, err
}

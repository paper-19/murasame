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
	return path[0] == "repo" && len(path) == 2 && slices.Contains([]string{"state_dir", "integ", "db", "key"}, path[1])
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

type CISpec struct {
	Staging      string   `mapstructure:"staging"`
	Contexts     []string `mapstructure:"contexts"`
	Timeout      int      `mapstructure:"timeout"`
	StartTimeout int      `mapstructure:"start_timeout"`
	PollInterval int      `mapstructure:"poll_interval"`
}

func (c CISpec) Hash() string {
	contexts := append([]string{}, c.Contexts...)
	slices.Sort(contexts)
	out := make([]any, len(contexts))
	for i, x := range contexts {
		out[i] = x
	}
	return hashJSON(map[string]any{"gate": "ci", "contexts": out})
}

func hashJSON(top map[string]any) string {
	blob, _ := json.Marshal(top)
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])[:16]
}

type ConflictRule struct {
	Label    string   `mapstructure:"label"`
	Paths    []string `mapstructure:"paths"`
	Strategy string   `mapstructure:"strategy"`
	Exclude  []string `mapstructure:"exclude"`
}

var Strategies = []string{"keep-both", "ours", "theirs", "stop"}

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
	Path      string
	Target    []string
	Integ     string
	StateDir  string
	DB        string
	RepoKey   string
	PairCheck bool
	CI        CISpec
	Conflicts []*ConflictRule
	NoFF      bool
	Records   RecordsSpec
	Forgejo   ForgejoSpec
}

func (c *Config) Copy() *Config {
	cp := *c
	return &cp
}

var ProjectTables = []string{"repo", "ci", "conflicts", "records", "forgejo", "merge"}

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

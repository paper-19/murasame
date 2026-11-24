package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"murasame/internal/config"
	"murasame/internal/instance"
)

func load(t *testing.T, text string) (*config.Config, error) {
	t.Helper()
	return config.Load("", config.LoadOptions{Text: &text, Env: config.Env{}})
}

func TestCIDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := load(t, "")
	if err != nil {
		t.Fatal(err)
	}
	want := config.DefaultCI()
	if cfg.CI.Staging != want.Staging || cfg.CI.Timeout != want.Timeout || cfg.CI.StartTimeout != want.StartTimeout ||
		cfg.CI.PollInterval != want.PollInterval || len(cfg.CI.Contexts) != 0 {
		t.Fatalf("got %+v, want %+v", cfg.CI, want)
	}
}

func TestCITable(t *testing.T) {
	t.Parallel()
	cfg, err := load(t, `[ci]
staging = "queue/staging"
contexts = ["ci / test (push)", "ci / lint (push)"]
timeout = 600
start_timeout = 60
poll_interval = 5
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CI.Staging != "queue/staging" || len(cfg.CI.Contexts) != 2 || cfg.CI.Timeout != 600 ||
		cfg.CI.StartTimeout != 60 || cfg.CI.PollInterval != 5 {
		t.Fatalf("got %+v", cfg.CI)
	}
}

func TestCIHashIgnoresContextOrder(t *testing.T) {
	t.Parallel()
	a := config.CISpec{Contexts: []string{"x", "y"}}
	b := config.CISpec{Contexts: []string{"y", "x"}}
	c := config.CISpec{Contexts: []string{"x"}}
	if a.Hash() != b.Hash() {
		t.Fatal("the hash depends on the order of the contexts")
	}
	if a.Hash() == c.Hash() {
		t.Fatal("different contexts, same hash")
	}
}

func TestConfigErrors(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, text, want string }{
		{"staging is a target", "[ci]\nstaging = \"main\"\n", "also a target branch"},
		{"bad staging name", "[ci]\nstaging = \"a..b\"\n", "ci.staging: a branch name"},
		{"staging with a space", "[ci]\nstaging = \"a b\"\n", "ci.staging: a branch name"},
		{"staging with an empty part", "[ci]\nstaging = \"a//b\"\n", "ci.staging: a branch name"},
		{"staging with a hidden part", "[ci]\nstaging = \"a/.b\"\n", "ci.staging: a branch name"},
		{"staging as a full ref", "[ci]\nstaging = \"refs/heads/x\"\n", "ci.staging: a branch name"},
		{"staging under a target", "[repo]\ntarget = [\"release\"]\n[ci]\nstaging = \"release/staging\"\n",
			"one is a directory of the other"},
		{"a target under staging", "[repo]\ntarget = [\"ci/main\"]\n[ci]\nstaging = \"ci\"\n",
			"one is a directory of the other"},
		{"duplicate context", "[ci]\ncontexts = [\"x\", \"x\"]\n", "listed twice"},
		{"empty context", "[ci]\ncontexts = [\" \"]\n", "empty context"},
		{"zero timeout", "[ci]\ntimeout = 0\n", "ci.timeout"},
		{"zero poll", "[ci]\npoll_interval = 0\n", "ci.poll_interval"},
		{"start after timeout", "[ci]\ntimeout = 10\nstart_timeout = 20\n", "ci.start_timeout"},
		{"own context", "[ci]\ncontexts = [\"murasame\"]\n", "murasame's own status context"},
		{"unknown ci key", "[ci]\ncommand = [\"make\"]\n", "ci: unknown key(s): command"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, c.text)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}

func TestConflictStrategiesThatNeedNoCommands(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"keep-both", "ours", "theirs", "stop"} {
		if _, err := load(t, "[[conflicts]]\nlabel = \"x\"\npaths = [\"a\"]\nstrategy = \""+s+"\"\n"); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func TestMultiRepoCITable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.toml")
	text := `[instance]
state_dir = "state"

[forges.main]
url = "http://forge.test"
token_env = "TOKEN"
git_url = "http://forge.test"

[[repos]]
repository = "team/app"
[repos.ci]
contexts = ["ci / test (push)"]
`
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := instance.Load(path, nil, config.Env{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.Repos[0].Cfg.CI.Contexts; len(got) != 1 || got[0] != "ci / test (push)" {
		t.Fatalf("contexts: %v", got)
	}
}

func TestMultiRepoIncludeWithUnknownTableIsRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.toml"), []byte("[bogus]\nx = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := `[instance]
state_dir = "state"

[forges.main]
url = "http://forge.test"
token_env = "TOKEN"

[[repos]]
repository = "team/app"
include = "app.toml"
`
	path := filepath.Join(dir, "multi.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := instance.Load(path, nil, config.Env{}, "")
	if err == nil || !strings.Contains(err.Error(), "unknown table(s): bogus") {
		t.Fatalf("got %v, want an unknown-table error", err)
	}
}

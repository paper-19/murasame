package tests

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type forgeSetup struct {
	actions     bool
	perm        string
	workflow    string
	protections string
	stuck       bool
	branch      string
}

func (fs forgeSetup) target() string {
	if fs.branch != "" {
		return fs.branch
	}
	return "main"
}

func forgeForChecks(t *testing.T, fs forgeSetup) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token check-token" {
			http.Error(w, `{"message":"token is required"}`, 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch p := r.URL.Path; {
		case p == "/api/v1/repos/team/app":
			fmt.Fprintf(w, `{"full_name":"team/app","has_actions":%t,"default_branch":"main"}`, fs.actions)
		case p == "/api/v1/repos/team/app/collaborators/murasame/permission":
			fmt.Fprintf(w, `{"permission":%q}`, fs.perm)
		case strings.HasPrefix(p, "/api/v1/repos/team/app/contents/") && r.URL.Query().Get("ref") != fs.target():
			http.Error(w, `{"message":"not found"}`, 404)
		case p == "/api/v1/repos/team/app/contents/.forgejo/workflows" && fs.workflow != "":
			fmt.Fprint(w, `[{"name":"ci.yml","path":".forgejo/workflows/ci.yml","type":"file"}]`)
		case p == "/api/v1/repos/team/app/contents/.forgejo/workflows/ci.yml" && fs.workflow != "":
			fmt.Fprintf(w, `{"content":%q,"encoding":"base64"}`, base64.StdEncoding.EncodeToString([]byte(fs.workflow)))
		case p == "/api/v1/repos/team/app/commits" && r.URL.Query().Get("sha") == fs.target():
			fmt.Fprint(w, `[{"sha":"89abcdef0123456789abcdef0123456789abcdef"}]`)
		case p == "/api/v1/repos/team/app/commits" && r.URL.Query().Get("sha") == "murasame/staging":
			fmt.Fprint(w, `[{"sha":"0123456789abcdef0123456789abcdef01234567"}]`)
		case p == "/api/v1/repos/team/app/commits/0123456789abcdef0123456789abcdef01234567/statuses":
			at := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
			if fs.stuck {
				at = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
			}
			if r.URL.Query().Get("page") != "1" {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprintf(w, `[{"id":2,"context":"ci / test (push)","status":"pending","description":"Blocked by required conditions","created_at":%q},`+
				`{"id":1,"context":"ci / build (push)","status":"pending","description":"Waiting to run","created_at":%q}]`, at, at)
		case p == "/api/v1/repos/team/app/branch_protections" && fs.protections == "admin only":
			http.Error(w, `{"message":"user should be an owner or a collaborator with admin write of a repository"}`, 403)
		case p == "/api/v1/repos/team/app/branch_protections":
			fmt.Fprint(w, fs.protections)
		default:
			http.Error(w, `{"message":"not found"}`, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func checkForgeCLI(t *testing.T, fs forgeSetup, contexts string) (int, string) {
	t.Helper()
	env := "MURASAME_CHECK_" + strings.ToUpper(strings.ReplaceAll(t.Name(), "/", "_"))
	os.Setenv(env, "check-token")
	t.Cleanup(func() { os.Unsetenv(env) })
	text := fmt.Sprintf("[repo]\ntarget = [\"main\", \"master\"]\n\n[ci]\ncontexts = %s\n\n[forgejo]\nurl = %q\nowner = \"team\"\nrepo = \"app\"\ntoken_env = %q\n",
		contexts, forgeForChecks(t, fs), env)
	path := filepath.Join(t.TempDir(), "m.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runCLI(t, "check-config", "--forge", "--config", path)
	return code, out + errs
}

const goodWorkflow = "name: ci\non:\n  push:\n    branches: [murasame/staging]\njobs:\n  test:\n    runs-on: docker\n"

func TestForgeCheckPasses(t *testing.T) {
	t.Parallel()
	code, out := checkForgeCLI(t, forgeSetup{actions: true, perm: "write", workflow: goodWorkflow, protections: "[]"},
		`["ci / test (push)"]`)
	if code != 0 || strings.Contains(out, "FAIL") || strings.Contains(out, "WARN") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{"the bot's token reads team/app", "@murasame has write permission",
		".forgejo/workflows/ci.yml mention(s) murasame/staging"} {
		if !strings.Contains(out, want) {
			t.Fatalf("lacks %q:\n%s", want, out)
		}
	}
}

func TestForgeCheckFindsProblems(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		fs       forgeSetup
		contexts string
		want     string
		fails    bool
	}{
		{"actions off", forgeSetup{actions: false, perm: "write", workflow: goodWorkflow, protections: "[]"}, "[]",
			"Actions are disabled", true},
		{"read only bot", forgeSetup{actions: true, perm: "read", workflow: goodWorkflow, protections: "[]"}, "[]",
			"@murasame has read permission", true},
		{"no workflow", forgeSetup{actions: true, perm: "write", protections: "[]"}, "[]", "no workflow in", true},
		{"workflow on other branches", forgeSetup{actions: true, perm: "write", protections: "[]",
			workflow: "name: ci\non:\n  push:\n    branches: [main]\n"}, "[]",
			"no workflow on main mentions murasame/staging or runs on every push", false},
		{"workflow on every push", forgeSetup{actions: true, perm: "write", protections: "[]",
			workflow: "name: ci\non: [push, pull_request]\n"}, "[]", "run(s) on every push, murasame/staging included", false},
		{"second target", forgeSetup{actions: true, perm: "write", protections: "[]", workflow: goodWorkflow,
			branch: "master"}, "[]", "mention(s) murasame/staging", false},
		{"context of another workflow", forgeSetup{actions: true, perm: "write", workflow: goodWorkflow,
			protections: "[]"}, `["CI / test (push)"]`, `no workflow is named "CI" (there are: ci)`, false},
		{"no runner", forgeSetup{actions: true, perm: "write", workflow: goodWorkflow, protections: "[]", stuck: true},
			"[]", "have waited 60 min to start: is a runner", false},
		{"protections need admin", forgeSetup{actions: true, perm: "write", workflow: goodWorkflow,
			protections: "admin only"}, "[]", "need admin access to read", false},
		{"protected staging", forgeSetup{actions: true, perm: "write", workflow: goodWorkflow,
			protections: `[{"rule_name":"murasame/*"}]`}, "[]", `the protection rule "murasame/*" covers`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out := checkForgeCLI(t, c.fs, c.contexts)
			if !strings.Contains(out, c.want) {
				t.Fatalf("lacks %q:\n%s", c.want, out)
			}
			if (code != 0) != c.fails {
				t.Fatalf("exit %d, want failing=%t:\n%s", code, c.fails, out)
			}
		})
	}
}

func TestForgeCheckWithoutAToken(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "m.toml")
	text := "[forgejo]\nurl = \"http://forge.test\"\nowner = \"team\"\nrepo = \"app\"\ntoken_env = \"MURASAME_UNSET_TOKEN_X\"\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "check-config", "--forge", "--config", path)
	if code != 1 || !strings.Contains(out, "$MURASAME_UNSET_TOKEN_X is empty") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

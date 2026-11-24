package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"murasame/internal/config"
	"murasame/internal/service"
	"murasame/internal/states"
)

// serveForgejo puts FakeForge behind Forgejo's REST API, so the service talks to it
// through the real HTTP client. Commit statuses are written in the shape Forgejo's
// modules/structs/status.go gives them, newest first.
func serveForgejo(t *testing.T, f *FakeForge, token string) *httptest.Server {
	t.Helper()
	base := `^/api/v1/repos/team/app`
	routes := []struct {
		method string
		re     *regexp.Regexp
		fn     func(w http.ResponseWriter, r *http.Request, m []string)
	}{
		{"GET", regexp.MustCompile(base + `/pulls$`), func(w http.ResponseWriter, r *http.Request, m []string) {
			ps, _ := f.OpenPulls()
			page(w, r, ps)
		}},
		{"GET", regexp.MustCompile(base + `/pulls/(\d+)$`), func(w http.ResponseWriter, r *http.Request, m []string) {
			n, _ := strconv.Atoi(m[1])
			p, err := f.Pull(n)
			if err != nil {
				http.Error(w, `{"message":"not found"}`, 404)
				return
			}
			writeJSON(w, p)
		}},
		{"POST", regexp.MustCompile(base + `/pulls/(\d+)/merge$`), func(w http.ResponseWriter, r *http.Request, m []string) {
			var body struct {
				Do            string
				MergeCommitID string
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Do != "manually-merged" || body.MergeCommitID == "" {
				http.Error(w, "bad merge request", 422)
				return
			}
			n, _ := strconv.Atoi(m[1])
			f.MarkMerged(n, body.MergeCommitID)
			w.WriteHeader(200)
		}},
		{"GET", regexp.MustCompile(base + `/issues/(\d+)/comments$`), func(w http.ResponseWriter, r *http.Request, m []string) {
			n, _ := strconv.Atoi(m[1])
			cs, _ := f.Comments(n)
			writeJSON(w, cs)
		}},
		{"POST", regexp.MustCompile(base + `/issues/(\d+)/comments$`), func(w http.ResponseWriter, r *http.Request, m []string) {
			var body struct{ Body string }
			json.NewDecoder(r.Body).Decode(&body)
			n, _ := strconv.Atoi(m[1])
			f.Comment(n, body.Body)
			w.WriteHeader(201)
			fmt.Fprint(w, `{}`)
		}},
		{"POST", regexp.MustCompile(base + `/statuses/([0-9a-f]{40})$`), func(w http.ResponseWriter, r *http.Request, m []string) {
			var raw map[string]string
			json.NewDecoder(r.Body).Decode(&raw)
			f.Status(m[1], raw["state"], raw["context"], raw["description"], raw["target_url"])
			w.WriteHeader(201)
			fmt.Fprint(w, `{}`)
		}},
		{"GET", regexp.MustCompile(base + `/collaborators/([^/]+)/permission$`), func(w http.ResponseWriter, r *http.Request, m []string) {
			p, _ := f.Permission(m[1])
			fmt.Fprintf(w, `{"permission":%q}`, p)
		}},
		{"GET", regexp.MustCompile(base + `/commits/([0-9a-f]{40})/statuses$`), func(w http.ResponseWriter, r *http.Request, m []string) {
			sts, err := f.Statuses(m[1])
			if err != nil {
				http.Error(w, "bad gateway", 502)
				return
			}
			items := make([]map[string]any, len(sts))
			for i, st := range sts {
				// Forgejo Actions links a run with a path, not a full URL.
				link := strings.Replace(st.TargetURL, "http://forge.test/actions/runs/", "/team/app/actions/runs/", 1)
				items[i] = map[string]any{"id": st.ID, "status": st.State, "target_url": link,
					"description": st.Description, "url": "http://forge.test/api/v1/...", "context": st.Context,
					"creator": nil, "created_at": "2026-11-21T10:00:00Z", "updated_at": "2026-11-21T10:00:00Z"}
			}
			page(w, r, items)
		}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token "+token {
			http.Error(w, "unauthorized", 401)
			return
		}
		for _, rt := range routes {
			if m := rt.re.FindStringSubmatch(r.URL.Path); m != nil && r.Method == rt.method {
				rt.fn(w, r, m)
				return
			}
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func page[T any](w http.ResponseWriter, r *http.Request, all []T) {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if p < 1 {
		p = 1
	}
	if limit < 1 {
		limit = 30
	}
	from, to := min((p-1)*limit, len(all)), min(p*limit, len(all))
	w.Header().Set("X-Total-Count", strconv.Itoa(len(all)))
	out := all[from:to]
	if out == nil {
		out = []T{}
	}
	writeJSON(w, out)
}

func TestQueueOverTheForgejoHTTPAPI(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Pending = 2
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	f.OpenPR(2, map[string]string{"BROKEN": "b\n"})
	srv := serveForgejo(t, f, "tok")
	text := fmt.Sprintf(`[ci]
staging = %q
contexts = [%q]
timeout = 60
start_timeout = 5
poll_interval = 1

[forgejo]
url = %q
owner = "team"
repo = "app"
git_remote_url = %q
reviewers_from_permission = "write"
`, f.Staging, ciContext, srv.URL, f.Bare)
	cfg, err := config.Load("", config.LoadOptions{Text: &text, Env: config.Env{}})
	if err != nil {
		t.Fatal(err)
	}
	f.perms["alice"] = "write"
	q := &Queue{Forge: f, t: t, Out: &bytes.Buffer{}}
	q.Svc, err = service.New(cfg, filepath.Join(t.TempDir(), "clone.git"), service.Options{
		Env: config.Env{"MURASAME_FORGEJO_TOKEN": "tok"}, Stream: q.Out, Sleep: func(float64) { time.Sleep(time.Millisecond) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Svc.Close)
	f.Approve(1)
	f.Approve(2)
	q.Tick()

	q.WantState(1, states.Merged)
	q.WantState(2, states.SetAside)
	if f.Marked(1) == "" || f.Marked(1) != f.Main() {
		t.Fatalf("PR 1 was not marked merged at main over HTTP: %q vs %s", f.Marked(1), f.Main())
	}
	var sawCI bool
	for _, c := range f.CommentsBy(2, "murasame") {
		if strings.Contains(c, srv.URL+"/team/app/actions/runs/") {
			sawCI = true
		}
	}
	if !sawCI {
		t.Fatalf("no set-aside comment with the CI run link: %v", f.CommentsBy(2, "murasame"))
	}
}

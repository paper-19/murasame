package tests

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"murasame/internal/config"
	"murasame/internal/forgejo"
	"murasame/internal/service"
	"murasame/internal/states"
	"murasame/internal/store"
)

const ciContext = "ci / test (push)"

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

type ciMode int

const (
	ciNormal ciMode = iota
	ciSilent
	ciFlakyAPI
	ciFlakyFirstRun
	ciSkipped
	ciNeedsFailed
	ciNoRunner
)

type ciRun struct {
	n, polls int
	done     bool
}

// FakeForge is an in-memory Forgejo API backed by a bare git repository. Each push
// to the staging branch starts a CI run for the pushed commit, as a workflow on
// push would; the run advances when the queue reads the commit's statuses. A tree
// with a file named BROKEN fails, any other tree passes.
type FakeForge struct {
	t        *testing.T
	Bare     string
	Work     string
	Staging  string
	Mode     ciMode
	Pending  int
	OnPoll   func(sha string, n int)
	mu       sync.Mutex
	pulls    map[int]*forgejo.Pull
	comments map[int][]forgejo.Comment
	statuses map[string][]forgejo.CommitStatus
	ownPosts map[string][]forgejo.CommitStatus
	marked   map[int]string
	nextID   int
	runs     map[string]*ciRun
	ciRuns   []string
	perms    map[string]string
}

func NewFakeForge(t *testing.T) *FakeForge {
	t.Helper()
	root := t.TempDir()
	f := &FakeForge{t: t, Bare: filepath.Join(root, "forge.git"), Work: filepath.Join(root, "work"),
		Staging: "murasame/staging", pulls: map[int]*forgejo.Pull{}, comments: map[int][]forgejo.Comment{},
		statuses: map[string][]forgejo.CommitStatus{}, ownPosts: map[string][]forgejo.CommitStatus{},
		marked: map[int]string{}, runs: map[string]*ciRun{}, perms: map[string]string{}}
	git(t, root, "init", "-q", "--bare", "-b", "main", f.Bare)
	hook := "#!/bin/sh\nwhile read old new ref; do echo \"$new $ref\" >> \"$GIT_DIR/pushes.log\"; done\n"
	if err := os.WriteFile(filepath.Join(f.Bare, "hooks", "post-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "-q", "-b", "main", f.Work)
	f.write("README.md", "hello\n")
	git(t, f.Work, "add", "-A")
	git(t, f.Work, "commit", "-q", "-m", "initial")
	git(t, f.Work, "push", "-q", f.Bare, "main:refs/heads/main")
	return f
}

func (f *FakeForge) write(path, text string) {
	f.t.Helper()
	full := filepath.Join(f.Work, path)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// OpenPR makes a branch off main with the given files and opens PR n for it.
func (f *FakeForge) OpenPR(n int, files map[string]string) string {
	f.t.Helper()
	branch := fmt.Sprintf("feature-%d", n)
	git(f.t, f.Work, "checkout", "-q", "-B", branch, "main")
	for p, x := range files {
		f.write(p, x)
	}
	git(f.t, f.Work, "add", "-A")
	git(f.t, f.Work, "commit", "-q", "-m", "change "+branch)
	sha := git(f.t, f.Work, "rev-parse", "HEAD")
	git(f.t, f.Work, "push", "-q", "-f", f.Bare, fmt.Sprintf("%s:refs/pull/%d/head", sha, n))
	git(f.t, f.Work, "checkout", "-q", "main")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls[n] = &forgejo.Pull{Number: n, Title: "PR " + branch, State: "open", Head: forgejo.Branch{Ref: branch, Sha: sha},
		Base: forgejo.Branch{Ref: "main"}, User: forgejo.User{Login: "author"}}
	return sha
}

// ReopenAs opens PR n with an existing head commit.
func (f *FakeForge) ReopenAs(n int, sha string) {
	f.t.Helper()
	git(f.t, f.Work, "push", "-q", "-f", f.Bare, fmt.Sprintf("%s:refs/pull/%d/head", sha, n))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls[n] = &forgejo.Pull{Number: n, Title: fmt.Sprintf("PR %d", n), State: "open",
		Head: forgejo.Branch{Ref: fmt.Sprintf("again-%d", n), Sha: sha}, Base: forgejo.Branch{Ref: "main"},
		User: forgejo.User{Login: "author"}}
}

func (f *FakeForge) Say(n int, user, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	ts := time.Now().UTC().Format(time.RFC3339)
	f.comments[n] = append(f.comments[n], forgejo.Comment{ID: f.nextID, Body: body, User: forgejo.User{Login: user},
		CreatedAt: ts, UpdatedAt: ts})
}

func (f *FakeForge) Approve(n int) {
	f.mu.Lock()
	sha := f.pulls[n].Head.Sha
	f.mu.Unlock()
	f.Say(n, "alice", "@murasame approve "+sha)
}

func (f *FakeForge) Main() string { return git(f.t, f.Bare, "rev-parse", "refs/heads/main") }

func (f *FakeForge) StagingTip() string {
	cmd := exec.Command("git", "rev-parse", "-q", "--verify", "refs/heads/"+f.Staging)
	cmd.Dir = f.Bare
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func (f *FakeForge) Contains(rev, path string) bool {
	cmd := exec.Command("git", "cat-file", "-e", rev+":"+path)
	cmd.Dir = f.Bare
	return cmd.Run() == nil
}

func (f *FakeForge) CIRuns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.ciRuns...)
}

func (f *FakeForge) Marked(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.marked[n]
}

func (f *FakeForge) OwnStatuses(sha string) []forgejo.CommitStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forgejo.CommitStatus{}, f.ownPosts[sha]...)
}

func (f *FakeForge) CommentsBy(n int, user string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.comments[n] {
		if c.User.Login == user {
			out = append(out, c.Body)
		}
	}
	return out
}

func (f *FakeForge) OpenPulls() ([]forgejo.Pull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forgejo.Pull
	for _, p := range f.pulls {
		if p.State == "open" && !p.Merged {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (f *FakeForge) Pull(n int) (forgejo.Pull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pulls[n]
	if !ok {
		return forgejo.Pull{}, &forgejo.Error{Method: "GET", Path: fmt.Sprintf("/pulls/%d", n), Status: 404}
	}
	return *p, nil
}

func (f *FakeForge) MarkMerged(n int, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked[n] = sha
	if p, ok := f.pulls[n]; ok {
		p.Merged, p.State = true, "closed"
	}
	return nil
}

func (f *FakeForge) Comments(n int) ([]forgejo.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forgejo.Comment{}, f.comments[n]...), nil
}

func (f *FakeForge) Comment(n int, body string) error {
	f.Say(n, "murasame", body)
	return nil
}

func (f *FakeForge) Status(sha, state, context, description, targetURL string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ownPosts[sha] = append(f.ownPosts[sha], forgejo.CommitStatus{Context: context, State: state,
		Description: description, TargetURL: targetURL})
	return nil
}

func (f *FakeForge) Permission(user string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.perms[user]; ok {
		return p, nil
	}
	return "none", nil
}

// stagingPushes counts the pushes that set the staging branch to sha.
func (f *FakeForge) stagingPushes(sha string) int {
	b, _ := os.ReadFile(filepath.Join(f.Bare, "pushes.log"))
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if line == sha+" refs/heads/"+f.Staging {
			n++
		}
	}
	return n
}

// Preset gives a commit statuses that exist before the queue gates it, such as the
// PR's own CI runs.
func (f *FakeForge) Preset(sha string, sts ...forgejo.CommitStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, st := range sts {
		f.nextID++
		st.ID = int64(f.nextID)
		f.statuses[sha] = append([]forgejo.CommitStatus{st}, f.statuses[sha]...)
	}
}

func (f *FakeForge) Statuses(sha string) ([]forgejo.CommitStatus, error) {
	if f.Mode == ciFlakyAPI {
		return nil, &forgejo.Error{Method: "GET", Path: "/statuses", Status: 502, Body: "bad gateway"}
	}
	pushes := f.stagingPushes(sha)
	broken := f.Contains(sha, "BROKEN")
	f.mu.Lock()
	defer f.mu.Unlock()
	run := f.runs[sha]
	if run == nil {
		run = &ciRun{}
		f.runs[sha] = run
	}
	if f.Mode != ciSilent && pushes > run.n {
		run.n, run.polls, run.done = pushes, 0, false
		f.ciRuns = append(f.ciRuns, sha)
	}
	if run.n == 0 || run.done {
		return append([]forgejo.CommitStatus{}, f.statuses[sha]...), nil
	}
	run.polls++
	if run.polls == 1 {
		// A workflow takes a moment to start: the first read sees no new status.
		return append([]forgejo.CommitStatus{}, f.statuses[sha]...), nil
	}
	f.nextID++
	st := forgejo.CommitStatus{ID: int64(f.nextID), Context: ciContext, State: "pending", Description: "Running",
		TargetURL: fmt.Sprintf("http://forge.test/actions/runs/%s-%d", sha[:7], run.n)}
	if f.Mode == ciNoRunner {
		st.Description = "Waiting to run"
		// As Forgejo 16 reports a job whose if: is false: skipped when the run is created.
		f.nextID++
		f.statuses[sha] = append([]forgejo.CommitStatus{{ID: int64(f.nextID), Context: "ci / lint (push)",
			State: "success", Description: "Has been skipped", TargetURL: st.TargetURL}}, f.statuses[sha]...)
		f.nextID++
		st.ID = int64(f.nextID)
	} else if run.polls > f.Pending+1 {
		run.done = true
		st.State, st.Description = "success", "Successful in 1m"
		if broken || f.Mode == ciFlakyFirstRun && run.n == 1 {
			st.State, st.Description = "failure", "Failing after 1m"
		}
		if f.Mode == ciSkipped || f.Mode == ciNeedsFailed {
			// As Forgejo 15 reports a skipped job.
			st.State, st.Description = "success", "Has been skipped"
		}
		if f.Mode == ciNeedsFailed {
			f.nextID++
			f.statuses[sha] = append([]forgejo.CommitStatus{{ID: int64(f.nextID), Context: "ci / build (push)",
				State: "failure", Description: "Failing after 1m", TargetURL: st.TargetURL}}, f.statuses[sha]...)
			f.nextID++
			st.ID = int64(f.nextID)
		}
	}
	f.statuses[sha] = append([]forgejo.CommitStatus{st}, f.statuses[sha]...)
	out, n, hook := append([]forgejo.CommitStatus{}, f.statuses[sha]...), run.polls-1, f.OnPoll
	if hook != nil {
		f.mu.Unlock()
		hook(sha, n)
		f.mu.Lock()
	}
	return out, nil
}

type Queue struct {
	Svc   *service.Service
	Out   *bytes.Buffer
	Forge *FakeForge
	t     *testing.T
}

func baseConfig(f *FakeForge, extra string) string {
	return fmt.Sprintf(`[repo]
target = ["main"]

[ci]
staging = %q
contexts = [%q]
timeout = 60
start_timeout = 2
poll_interval = 1

[forgejo]
url = "http://forge.test"
owner = "team"
repo = "app"
git_remote_url = %q
reviewers = ["alice"]
infra_backoff = 300
%s`, f.Staging, ciContext, f.Bare, extra)
}

func NewQueue(t *testing.T, f *FakeForge, toml string) *Queue {
	t.Helper()
	return NewQueueAt(t, f, toml, filepath.Join(t.TempDir(), "clone.git"))
}

// NewQueueAt runs the service on an existing clone, as a restarted process would.
func NewQueueAt(t *testing.T, f *FakeForge, toml, clone string) *Queue {
	t.Helper()
	cfg, err := config.Load("", config.LoadOptions{Text: &toml, Env: config.Env{}})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	out := &bytes.Buffer{}
	svc, err := service.New(cfg, clone, service.Options{API: f, Env: config.Env{},
		Stream: out, Sleep: func(float64) { time.Sleep(time.Millisecond) }})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	t.Cleanup(svc.Close)
	return &Queue{Svc: svc, Out: out, Forge: f, t: t}
}

func (q *Queue) Tick() {
	q.t.Helper()
	if err := q.Svc.Tick(); err != nil {
		q.t.Fatalf("tick: %v\n%s", err, q.Out.String())
	}
}

func (q *Queue) PR(n int) *store.PR {
	q.t.Helper()
	st, err := q.Svc.Store()
	if err != nil {
		q.t.Fatal(err)
	}
	data, err := st.Load()
	if err != nil {
		q.t.Fatal(err)
	}
	pr, ok := data.PRs[fmt.Sprint(n)]
	if !ok {
		pr, ok = data.PRs[fmt.Sprintf("#%d", n)]
	}
	if !ok {
		q.t.Fatalf("no PR %d in the state DB; have %v\n%s", n, keys(data.PRs), q.Out.String())
	}
	return pr
}

func keys(m map[string]*store.PR) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (q *Queue) WantState(n int, want states.State) {
	q.t.Helper()
	if got := q.PR(n).State; got != want {
		q.t.Fatalf("PR %d: state %s, want %s\nreason: %s\n--- log ---\n%s", n, got, want, q.PR(n).Reason, q.Out.String())
	}
}

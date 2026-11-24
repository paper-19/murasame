package service

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"murasame/internal/commands"
	"murasame/internal/config"
	"murasame/internal/db"
	"murasame/internal/engine"
	"murasame/internal/forgejo"
	"murasame/internal/gate"
	"murasame/internal/gitx"
	"murasame/internal/panics"
	"murasame/internal/safefile"
	"murasame/internal/secretenv"
	"murasame/internal/states"
	"murasame/internal/store"
)

var Perms = []string{"none", "read", "write", "admin", "owner"}

const NetTimeout = 10 * time.Minute

var stopNoise = regexp.MustCompile(`log: .*?\.log\b|\b\d+\b`)

func StopKey(why string) string { return stopNoise.ReplaceAllString(why, "#") }

func GitAuthEnv(url, user, token string) map[string]string {
	env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	if (strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")) && token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
		env["GIT_CONFIG_COUNT"] = "1"
		env["GIT_CONFIG_KEY_0"] = "http.extraHeader"
		env["GIT_CONFIG_VALUE_0"] = "Authorization: Basic " + basic
	}
	return env
}

func PairComment(partners []string, target string) string {
	who := engine.AndList(partners)
	has := "have"
	if len(partners) == 1 {
		has = "has"
	}
	return fmt.Sprintf("This PR fails only when merged together with %s. On its own, it passes. %s %s already "+
		"landed, so merge `%s` into this PR, fix the clash, and approve again.", who, who, has, target)
}

type Options struct {
	API    forgejo.API
	Env    config.Env
	Stream io.Writer
	Now    func() float64
	Sleep  func(float64)
	Name   string
}

type Service struct {
	Cfg     *config.Config
	FJ      config.ForgejoSpec
	API     forgejo.API
	Name    string
	Host    string
	RepoDir string
	GitEnv  map[string]string
	Env     config.Env
	Stream  io.Writer
	Now     func() float64
	Sleep   func(float64)
	Context string
	Data    *store.ServiceData
	NetTime time.Duration

	target    atomic.Pointer[string]
	permCache map[string]string
	permDeny  map[string]bool
	prs       map[string]*store.PR
	st        *store.ServiceState
	stMu      sync.Mutex
	liveMu    sync.Mutex
	live      Live
	pollNow   atomic.Bool
	ciNow     atomic.Bool
	stopping  atomic.Pointer[string]
	stopCh    chan struct{}
	stopOnce  sync.Once
	lagged    map[int]string
	lastPoll  time.Time
	halt      batchStop
	skipped   map[int]string
	outMu     sync.Mutex
	stg       *stage
}

type stage struct {
	pr    int
	moves []states.Move
	after []func()
}

func New(cfg *config.Config, repoDir string, o Options) (*Service, error) {
	env := o.Env
	if env == nil {
		env = config.OSEnv()
	}
	fj := cfg.Forgejo
	var missing []string
	check := []struct{ k, v string }{{"url", fj.URL}, {"owner", fj.Owner}, {"repo", fj.Repo}, {"git_remote_url", fj.GitRemoteURL}}
	if o.API != nil {
		check = check[3:]
	}
	for _, c := range check {
		if c.v == "" {
			missing = append(missing, c.k)
		}
	}
	if len(missing) > 0 {
		return nil, config.Errf("forgejo: %s not set", strings.Join(missing, ", "))
	}
	c := cfg.Copy()
	secretenv.Hide(fj.TokenEnv, fj.WebhookSecretEnv)
	token := env[fj.TokenEnv]
	s := &Service{Cfg: c, FJ: fj, API: o.API, Env: env, Stream: o.Stream, Now: o.Now, Sleep: o.Sleep,
		permCache: map[string]string{}, prs: map[string]*store.PR{}, Data: &store.ServiceData{PRs: map[string]*store.PR{}},
		NetTime: NetTimeout, stopCh: make(chan struct{})}
	if s.API == nil {
		s.API = forgejo.New(fj.URL, fj.Owner, fj.Repo, token)
	}
	s.Name = o.Name
	if s.Name == "" {
		s.Name = fj.Repo
	}
	if s.Name == "" {
		s.Name = "repo"
	}
	s.Host = "unknown-host"
	if fj.URL != "" {
		s.Host = db.ForgeHost(fj.URL)
	}
	abs, err := filepath.Abs(repoDir)
	if err != nil {
		return nil, err
	}
	s.RepoDir = abs
	s.GitEnv = GitAuthEnv(fj.GitRemoteURL, fj.BotUser, token)
	if s.Stream == nil {
		s.Stream = os.Stdout
	}
	if s.Now == nil {
		s.Now = db.Now
	}
	if s.Sleep == nil {
		s.Sleep = func(x float64) { time.Sleep(time.Duration(x * float64(time.Second))) }
	}
	s.Context = fj.StatusContext
	s.live = Live{Phase: states.PhaseStarting}
	return s, nil
}

func (s *Service) Say(msg string) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	fmt.Fprintf(s.Stream, "[murasame] %s\n", s.Redact(msg))
}

type LiveGate struct {
	Since  float64
	Log    string
	RunURL string
	PRs    []int
}

type Live struct {
	Phase     states.Phase
	What      string
	LastError string
	Gate      *LiveGate
}

func (s *Service) setLive(fn func(l *Live)) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	fn(&s.live)
}

func (s *Service) Live() Live {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	return s.live
}

func (s *Service) StateDir() string {
	sd := config.Expand(s.Cfg.StateDir, config.Vars{"repo": s.RepoDir})
	if !filepath.IsAbs(sd) {
		sd = filepath.Join(s.RepoDir, sd)
	}
	return sd
}

func (s *Service) Store() (*store.ServiceState, error) {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	if s.st == nil {
		vars := config.Vars{"repo": s.RepoDir, "state_dir": s.StateDir()}
		d, err := db.Open(db.Path(s.StateDir(), config.Expand(s.Cfg.DB, vars)), false)
		if err != nil {
			return nil, err
		}
		repo, err := db.Scope(d, s.RepoKey(), "forgejo", db.RepoInfo{ForgeURL: s.FJ.URL, Owner: s.FJ.Owner,
			Name: s.FJ.Repo, Path: s.RepoDir})
		if err != nil {
			d.Close()
			return nil, err
		}
		s.st = store.NewServiceState(repo)
	}
	return s.st, nil
}

func (s *Service) store() *store.ServiceState { return must(s.Store()) }

func (s *Service) Close() {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	if s.st != nil {
		s.st.DB.Close()
		s.st = nil
	}
}

func (s *Service) RepoKey() string {
	if k := config.Expand(s.Cfg.RepoKey, config.Vars{"repo": s.RepoDir}); k != "" {
		return k
	}
	return db.ForgeKey(s.Host, s.FJ.Owner, s.FJ.Repo)
}

func (s *Service) Bot() string { return s.FJ.BotUser }

func (s *Service) LogURL(log string) string {
	if log == "" || s.FJ.ExternalURL == "" {
		return ""
	}
	return strings.TrimRight(s.FJ.ExternalURL, "/") + "/repos/" + url.PathEscape(s.Name) + "/logs/" + url.PathEscape(filepath.Base(log))
}

func (s *Service) Git() *gitx.Git {
	return gitx.New(s.RepoDir, map[string]string{"GIT_TERMINAL_PROMPT": "0"})
}

func (s *Service) AuthGit() *gitx.Git {
	g := s.pushGit()
	g.Done = s.stopCh
	return g
}

func (s *Service) pushGit() *gitx.Git {
	g := gitx.New(s.RepoDir, s.GitEnv)
	g.NetTimeout = s.NetTime
	return g
}

func (s *Service) Target() string {
	if t := s.resolved(); t != "" {
		return t
	}
	return s.Cfg.Target[0]
}

func (s *Service) resolved() string {
	if p := s.target.Load(); p != nil {
		return *p
	}
	return ""
}

func (s *Service) ResolveTarget() bool {
	if len(s.Cfg.Target) < 2 {
		return true
	}
	args := []string{"ls-remote", "--heads", s.FJ.GitRemoteURL}
	for _, t := range s.Cfg.Target {
		args = append(args, "refs/heads/"+t)
	}
	r := s.AuthGit().AuthRun(args...)
	if !r.OK() {
		s.Say(fmt.Sprintf("cannot list the forge's branches (%s): target %s", tail(strings.TrimSpace(r.Out), 200), s.Target()))
		return s.resolved() != ""
	}
	have := map[string]bool{}
	for _, line := range gitx.Lines(r.Out) {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			have[strings.TrimSpace(ref)] = true
		}
	}
	found := ""
	for _, t := range s.Cfg.Target {
		if have["refs/heads/"+t] {
			found = t
			break
		}
	}
	switch {
	case found == "":
		s.Say(fmt.Sprintf("the forge has none of the targets %s: target %s", strings.Join(s.Cfg.Target, ", "), s.Target()))
	case found != s.resolved():
		if was := s.resolved(); was != "" {
			s.Say(fmt.Sprintf("target branch: %s (was %s)", found, was))
		}
		s.target.Store(&found)
	}
	return s.resolved() != ""
}

func tail(x string, n int) string {
	r := []rune(x)
	if len(r) > n {
		return string(r[len(r)-n:])
	}
	return x
}

func head(x string, n int) string {
	r := []rune(x)
	if len(r) > n {
		return string(r[:n])
	}
	return x
}

func (s *Service) EnsureRepo() {
	if !exists(filepath.Join(s.RepoDir, "HEAD")) && !exists(filepath.Join(s.RepoDir, ".git")) {
		os.MkdirAll(s.RepoDir, 0o755)
		s.Git().MustRun("init", "-q", "--bare")
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func forgeErr(fn func()) *forgejo.Error { return panics.Catch[*forgejo.Error](fn) }

func (s *Service) later(pr *store.PR, fn func()) {
	if st := s.stg; st != nil && st.pr == pr.Number {
		st.after = append(st.after, fn)
		return
	}
	fn()
}

func (s *Service) status(pr *store.PR, st, desc, log string) {
	n, sha, desc, link := pr.Number, pr.Head, s.Redact(desc), s.LogURL(log)
	if link == "" && log != "" {
		link = gate.RunURL(log)
	}
	s.later(pr, func() {
		if err := s.API.Status(sha, st, s.Context, desc, link); err != nil {
			s.Say(fmt.Sprintf("status for #%d: %s", n, err))
		}
	})
}

func (s *Service) comment(pr *store.PR, body string) {
	n, body := pr.Number, s.Redact(body)
	s.later(pr, func() {
		if err := s.API.Comment(n, body); err != nil {
			s.Say(fmt.Sprintf("comment on #%d: %s", n, err))
		}
	})
}

func (s *Service) atomically(pr *store.PR, fn func()) {
	if s.stg != nil {
		fn()
		return
	}
	st := &stage{pr: pr.Number}
	keep, halted := pr.Clone(), s.halt.snapshot()
	done := false
	s.stg = st
	defer func() {
		s.stg = nil
		if !done {
			*pr = *keep
			s.halt.restore(halted, s.Stopping())
		}
	}()
	fn()
	if len(st.moves) > 0 || len(st.after) > 0 {
		check(s.store().SaveMoves(pr, st.moves))
	}
	done = true
	s.stg = nil
	for _, f := range st.after {
		f()
	}
}

var userinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^\s/@'"]*@`)

func stripUserinfo(x string) string { return userinfo.ReplaceAllString(x, "$1") }

func (s *Service) Redact(x string) string {
	if u := s.FJ.GitRemoteURL; u != "" {
		var forms []string
		bases := []string{u, stripUserinfo(u)}
		if p, ok := strings.CutPrefix(u, "file://"); ok {
			bases = append(bases, p)
		}
		for _, f := range bases {
			forms = append(forms, f, strings.TrimSuffix(strings.TrimRight(f, "/"), ".git"))
		}
		sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
		web := strings.TrimRight(s.FJ.URL, "/")
		for _, f := range forms {
			if f != "" && (web == "" || f != web && !strings.HasPrefix(f, web+"/")) {
				x = strings.ReplaceAll(x, f, "<git remote>")
			}
		}
	}
	return stripUserinfo(x)
}

type redactWriter struct{ s *Service }

func (w redactWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(w.s.Stream, w.s.Redact(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *Service) move(pr *store.PR, ev states.Event, why string, fields func(*store.PR)) bool {
	why = s.Redact(why)
	var next states.State
	var err error
	if st := s.stg; st != nil && st.pr == pr.Number {
		var moved *store.PR
		var m states.Move
		if moved, m, err = store.Next(pr, ev, why, fields); err == nil {
			*pr = *moved
			st.moves = append(st.moves, m)
			next = moved.State
		}
	} else {
		next, err = s.store().Transition(pr, ev, why, fields)
	}
	if err != nil {
		var it *states.InvalidTransition
		if errors.As(err, &it) {
			s.Say(it.Error())
			return false
		}
		panic(err)
	}
	s.later(pr, func() { s.sayMove(pr, ev, next, why) })
	return true
}

func (s *Service) sayMove(pr *store.PR, ev states.Event, next states.State, why string) {
	msg := fmt.Sprintf("#%d: %s -> %s", pr.Number, ev, next)
	if why != "" {
		msg += " (" + why + ")"
	}
	s.Say(msg)
}

func (s *Service) Tick() (err error) {
	fd, err := s.cycleLock()
	if err != nil {
		msg := head(s.Redact(err.Error()), 300)
		s.setLive(func(l *Live) { l.LastError = msg })
		return err
	}
	if fd == nil {
		return nil
	}
	defer fd.Close()
	s.phase(states.PhaseSync, "")
	defer s.phase(states.PhaseRest, "")
	defer func() {
		if r := recover(); r != nil {
			err = panics.AsError(r)
		}
		if err != nil {
			msg := head(s.Redact(fmt.Sprintf("%v", err)), 300)
			s.setLive(func(l *Live) { l.LastError = msg })
		}
	}()
	return s.tick()
}

func (s *Service) cycleLock() (*os.File, error) {
	path := filepath.Join(s.StateDir(), ".service.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := store.OpenRegular(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	var err2 error
	for try := 0; try < 100; try++ {
		if err2 = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err2 != syscall.EWOULDBLOCK {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := err2; err != nil {
		if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, fmt.Errorf("cannot lock %s: %w", path, err)
		}
		buf := make([]byte, 200)
		n, _ := f.Read(buf)
		holder := strings.TrimSpace(strings.ToValidUTF8(string(buf[:n]), "�"))
		f.Close()
		if holder == "" {
			holder = path
		}
		s.Say(fmt.Sprintf("another murasame process is in a cycle of this repository (%s): this cycle does nothing", holder))
		return nil, nil
	}
	f.Truncate(0)
	f.WriteAt([]byte(fmt.Sprintf("pid %d since %s\n", os.Getpid(), time.Now().Format("2006-01-02 15:04:05"))), 0)
	return f, nil
}

func (s *Service) tick() error {
	s.EnsureRepo()
	if !s.ResolveTarget() {
		s.Say(fmt.Sprintf("the target branch is not known yet (one of %s): nothing is synced this cycle",
			strings.Join(s.Cfg.Target, ", ")))
		return nil
	}
	s.Data = must(s.store().Load())
	prs := s.Data.PRs
	s.permCache = map[string]string{}
	s.permDeny = map[string]bool{}
	s.lagged = map[int]string{}
	s.skipped = map[int]string{}
	for _, k := range sortedKeys(prs) {
		pr := prs[k]
		if pr.State == states.Testing {
			s.move(pr, states.Recover, "the service restarted during a batch", nil)
		}
	}
	must(store.NewHistory(s.store().Repo).Interrupted("service", "interrupted: the service restarted during the batch"))
	if fe := forgeErr(func() { s.sync(prs) }); fe != nil {
		s.Say(fmt.Sprintf("sync failed: %s", fe))
		check(s.store().Save(s.Data))
		return fmt.Errorf("sync failed: %w", fe)
	}
	check(s.store().Save(s.Data))
	s.expireBackoff()
	if backoffOf(s.Data) == states.BackingOff {
		s.Say(fmt.Sprintf("waiting until %s after: %s", db.Time(s.Data.Until()).Format("15:04:05"), s.Data.BackoffWhy))
		return nil
	}
	batch := s.Pick(prs)
	if len(batch) == 0 {
		return nil
	}
	if s.Stopping() != "" {
		return nil
	}
	s.RunBatch(batch)
	check(s.store().Save(s.Data))
	return nil
}

func sortedKeys(m map[string]*store.PR) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.TrimPrefix(keys[i], "#"))
		b, _ := strconv.Atoi(strings.TrimPrefix(keys[j], "#"))
		return a < b
	})
	return keys
}

func inStates(pr *store.PR, sts ...states.State) bool {
	st := pr.State
	for _, x := range sts {
		if st == x {
			return true
		}
	}
	return false
}

func (s *Service) sync(prs map[string]*store.PR) {
	target := s.Target()
	open := map[int]forgejo.Pull{}
	for _, p := range must(s.API.OpenPulls()) {
		if p.Base.Ref == target {
			open[p.Number] = p
		}
	}
	for _, key := range sortedKeys(prs) {
		pr := prs[key]
		n, _ := strconv.Atoi(key)
		if _, ok := open[n]; ok {
			continue
		}
		if pr.State == states.Merged && markOf(pr) == states.MarkPending {
			s.perPR(pr, func() { s.retryMark(pr, nil) })
			continue
		}
		if inStates(pr, states.Merged, states.Closed) {
			continue
		}
		s.perPR(pr, func() { s.syncUnlisted(pr, n, target) })
	}
	nums := make([]int, 0, len(open))
	for n := range open {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		p := open[n]
		key := strconv.Itoa(n)
		pr, ok := prs[key]
		if !ok {
			pr = &store.PR{Number: n, State: states.Open, Head: p.Head.Sha, Rollup: "maybe"}
			prs[key] = pr
		}
		s.perPR(pr, func() { s.syncListed(pr, p) })
	}
}

func (s *Service) perPR(pr *store.PR, fn func()) {
	fe := forgeErr(fn)
	if fe == nil {
		return
	}
	msg := fmt.Sprintf("#%d: the forge failed while reading it (%s): left as it is and not picked this cycle", pr.Number, fe)
	s.Say(msg)
	s.skipped[pr.Number] = head(s.Redact(msg), 300)
	s.setLive(func(l *Live) { l.LastError = s.skipped[pr.Number] })
}

func (s *Service) syncListed(pr *store.PR, p forgejo.Pull) {
	if pr.State == states.Merged {
		if markOf(pr) == states.MarkPending {
			s.retryMark(pr, &p)
		}
		return
	}
	if pr.State == states.Closed {
		s.move(pr, states.Reopen, "reopened", nil)
	}
	pr.Title = p.Title
	pr.Branch = p.Head.Ref
	pr.Author = p.User.Login
	s.readPR(pr, p.Head.Sha)
	check(s.store().SavePR(pr))
}

func (s *Service) syncUnlisted(pr *store.PR, n int, target string) {
	full, err := s.API.Pull(n)
	if forgejo.HasStatus(err, 404) {
		s.move(pr, states.Close, "not found on the forge (deleted or unreadable)", nil)
		return
	}
	check(err)
	switch {
	case full.Merged:
		s.move(pr, states.MergedElsewhere, "merged on the forge", nil)
	case states.Can(pr.State, states.Close):
		if full.State == "open" {
			base := full.Base.Ref
			if base == target {
				s.Say(fmt.Sprintf("#%d: not in the forge's list of open PRs, but open on %s: left as it is this cycle", n, target))
				return
			}
			if base == "" {
				base = "?"
			}
			was := pr.State
			s.move(pr, states.Close, "retargeted to "+base, retarget)
			if was == states.Queued || was == states.SetAside || was == states.Held {
				s.comment(pr, fmt.Sprintf("Out of the queue: this PR now targets `%s`; the queue merges into `%s` "+
					"only. Retarget it to `%s` and approve it again (%s) to queue it.", base, target, target, s.approveHint(pr)))
			}
		} else {
			s.move(pr, states.Close, "closed on the forge", nil)
		}
	}
}

func (s *Service) readPR(pr *store.PR, listed string) {
	comments := must(s.API.Comments(pr.Number))
	h := listed
	if s.unread(pr, comments) {
		if p := must(s.API.Pull(pr.Number)); p.Head.Sha != "" {
			h = p.Head.Sha
		}
	}
	if h != "" {
		s.syncHead(pr, h)
	}
	s.syncComments(pr, comments)
}

func (s *Service) unread(pr *store.PR, comments []forgejo.Comment) bool {
	for _, c := range comments {
		if c.ID > pr.LastComment && !strings.EqualFold(c.User.Login, s.Bot()) {
			return true
		}
	}
	return false
}

func (s *Service) retryMark(pr *store.PR, listed *forgejo.Pull) {
	full := listed
	if full == nil {
		got, err := s.API.Pull(pr.Number)
		if forgejo.HasStatus(err, 404) {
			s.Say(fmt.Sprintf("#%d: not found on the forge: no longer marked merged", pr.Number))
			stepMark(pr, states.MarkGone, "")
			check(s.store().SavePR(pr))
			return
		}
		check(err)
		full = &got
	}
	switch now := full.Head.Sha; {
	case full.Merged:
		stepMark(pr, states.MarkSeen, "")
		check(s.store().SavePR(pr))
	case now != "" && now != pr.Head:
		s.atomically(pr, func() { s.abandonMark(pr, now) })
	default:
		s.tryMark(pr, full.State != "" && full.State != "open")
	}
}

func (s *Service) abandonMark(pr *store.PR, now string) {
	landed := pr.MarkMerged
	stepMark(pr, states.MarkHeadMoved, "")
	t := s.Target()
	s.Say(fmt.Sprintf("#%d: new head %s since %s landed: not marked merged", pr.Number, head(now, 12), head(pr.Head, 12)))
	s.comment(pr, fmt.Sprintf("Merged %s into `%s` (%s), but new commits (%s) were pushed after it landed; they are "+
		"**not** in `%s`, so this PR is not marked merged (that would close it without them). The queue is done "+
		"with this PR: open a new one for the new commits.", head(pr.Head, 12), t, head(landed, 12), head(now, 12), t))
}

func (s *Service) syncHead(pr *store.PR, h string) {
	if h == pr.Head {
		return
	}
	s.atomically(pr, func() { s.newHead(pr, h) })
}

func (s *Service) newHead(pr *store.PR, h string) {
	old := pr.Head
	pr.Head = h
	if inStates(pr, states.Queued, states.SetAside, states.Held) && pr.ApprovedHead != h {
		was := pr.State
		s.move(pr, states.Push, "new head "+head(h, 12), outdate)
		if a := s.gateApprover(pr, h); a.User != "" && was != states.Held {
			by := a.User
			s.approve(pr, by)
			s.comment(pr, fmt.Sprintf("New commits (%s): the earlier approval was for %s; @%s approved %s while its batch "+
				"was running, so it is queued now%s.", head(h, 12), head(old, 12), by, head(h, 12), priorityNote(pr)))
			return
		}
		s.comment(pr, fmt.Sprintf("New commits (%s): the approval was for %s. Out of the queue until %s.",
			head(h, 12), head(old, 12), s.approveHint(pr)))
		return
	}
	if pr.ApprovedHead != h {
		outdate(pr)
	}
}

func (s *Service) approveHint(pr *store.PR) string {
	return "`" + commands.ApproveLine(s.Bot(), pr.Head) + "`"
}

func (s *Service) Authorized(user string) bool {
	if user == "" {
		return false
	}
	for _, r := range s.FJ.Reviewers {
		if strings.EqualFold(r, user) {
			return true
		}
	}
	level := s.FJ.ReviewersFromPermission
	if level == "" {
		return false
	}
	key := strings.ToLower(user)
	if s.permDeny[key] {
		return false
	}
	if _, ok := s.permCache[key]; !ok {
		p, err := s.API.Permission(user)
		if forgejo.HasStatus(err, 403) {
			if s.permDeny == nil {
				s.permDeny = map[string]bool{}
			}
			s.permDeny[key] = true
			s.Say(fmt.Sprintf("cannot read %s's permission (%s): reviewers_from_permission needs the bot to have admin "+
				"access to the repository; %s is not authorized this cycle", user, err, user))
			return false
		}
		s.permCache[key] = must(p, err)
	}
	got := s.permCache[key]
	gi, li := slices.Index(Perms, got), slices.Index(Perms, level)
	return gi >= 0 && li >= 0 && gi >= li
}

func (s *Service) permNote(user string) string {
	if !s.permDeny[strings.ToLower(user)] {
		return ""
	}
	return fmt.Sprintf(" The forge refused to tell the bot @%s's permission: `reviewers_from_permission` needs the bot "+
		"to have admin access to the repository.", user)
}

func edited(c forgejo.Comment) bool {
	made, ok1 := parseISO(c.CreatedAt)
	changed, ok2 := parseISO(c.UpdatedAt)
	return ok1 && ok2 && changed > made
}

func (s *Service) syncComments(pr *store.PR, comments []forgejo.Comment) {
	for _, c := range comments {
		if c.ID <= pr.LastComment {
			continue
		}
		s.atomically(pr, func() { s.readComment(pr, c, c.ID) })
	}
}

func (s *Service) readComment(pr *store.PR, c forgejo.Comment, cid int) {
	user := c.User.Login
	if strings.EqualFold(user, s.Bot()) {
		pr.LastComment = cid
		return
	}
	cmds := commands.Parse(c.Body, s.Bot())
	trusted := len(cmds) > 0 && s.Authorized(user)
	pr.LastComment = cid
	if len(cmds) > 0 && !trusted && s.quiet(pr, user) {
		s.Say(fmt.Sprintf("#%d: comment %d by %s not answered (one answer per %d s to someone who is not a reviewer)",
			pr.Number, cid, user, s.FJ.ReplyCooldown))
		return
	}
	if edited(c) {
		if len(cmds) > 0 {
			s.Say(fmt.Sprintf("#%d: comment %d by %s was edited: its commands are not read", pr.Number, cid, user))
			s.comment(pr, fmt.Sprintf("@%s: comment %d was edited after it was posted, so its commands are not read "+
				"(anyone with write access can edit a comment). Post them in a new comment.", user, cid))
		}
		return
	}
	var unknown []string
	for _, cmd := range cmds {
		if cmd.Unknown {
			unknown = append(unknown, commands.UnknownReply(cmd, s.Bot(), pr.Head))
		} else {
			s.Command(pr, user, cmd, cid)
		}
	}
	if len(unknown) == 1 {
		s.comment(pr, "@"+user+": "+unknown[0])
	} else if len(unknown) > 1 {
		var b strings.Builder
		for _, u := range unknown {
			b.WriteString("\n- " + u)
		}
		s.comment(pr, "@"+user+":\n"+b.String())
	}
}

func (s *Service) quiet(pr *store.PR, user string) bool {
	every := s.FJ.ReplyCooldown
	if every <= 0 {
		return false
	}
	if pr.Replied == nil {
		pr.Replied = map[string]float64{}
	}
	now := s.Now()
	key := strings.ToLower(user)
	if now-pr.Replied[key] < float64(every) {
		return true
	}
	pr.Replied[key] = now
	return false
}

func priorityNote(pr *store.PR) string {
	if p := pr.Priority; p != 0 {
		return fmt.Sprintf(" (priority %d)", p)
	}
	return ""
}

func (s *Service) Command(pr *store.PR, user string, cmd commands.Line, cid int) {
	if commands.Privileged[cmd.Name] && !s.Authorized(user) {
		s.comment(pr, fmt.Sprintf("@%s: `%s` is for reviewers only (`[forgejo] reviewers`).%s", user, cmd.Name, s.permNote(user)))
		return
	}
	st := pr.State
	n := pr.Number
	bot := s.Bot()
	switch cmd.Name {
	case "approve":
		s.approveCommand(pr, user, cmd.Arg)
	case "unapprove":
		if states.Can(st, states.Unapprove) {
			s.unapprove(pr, user, st == states.Testing)
		} else {
			s.comment(pr, fmt.Sprintf("@%s: `unapprove` does not apply: #%d is %s (not approved).", user, n, st))
		}
	case "cancel":
		if states.Can(st, states.Cancel) {
			s.cancel(pr, user, st == states.Testing)
		} else {
			s.comment(pr, fmt.Sprintf("@%s: nothing to cancel: #%d is %s, neither queued nor being tested.", user, n, st))
		}
	case "retry":
		switch {
		case st == states.Held:
			s.move(pr, states.Retry, "retry by "+user, func(p *store.PR) { p.HeldBy, p.HeldAt = "", 0 })
			s.status(pr, "pending", "queued again (retry by "+user+")", "")
			s.comment(pr, fmt.Sprintf("Back in the queue (`retry` by @%s), in its place from the approval by @%s.", user,
				pr.ApprovedBy))
		case states.Can(st, states.Retry):
			s.move(pr, states.Retry, "retry by "+user, func(p *store.PR) {
				grant(p, user)
				p.ApprovedAt = s.Now()
			})
			s.status(pr, "pending", "queued again (retry by "+user+")", "")
			s.comment(pr, fmt.Sprintf("Back in the queue (`retry` by @%s).", user))
		default:
			s.comment(pr, fmt.Sprintf("@%s: nothing to retry: #%d is %s.", user, n, st))
		}
	case "priority":
		p, _ := strconv.Atoi(cmd.Arg)
		pr.Priority = p
	case "rollup":
		pr.Rollup = cmd.Arg
	case "status":
		s.comment(pr, "@"+user+": "+s.StatusText(pr))
	case "ping":
		msg := fmt.Sprintf("@%s: pong. #%d is %s", user, n, st)
		if st == states.SetAside && pr.Reason != "" {
			msg += " (" + pr.Reason + ")"
		}
		if st == states.Held {
			msg += "; `@" + bot + " retry` queues it."
		} else {
			msg += "; " + s.approveHint(pr) + " queues it."
		}
		s.comment(pr, msg)
	}
}

const stoppedNote = "Its batch is stopped; the other PRs in it go back to the queue."

func (s *Service) stopBatch(how, user string, pr *store.PR) {
	s.halt.cancel(fmt.Sprintf("%s by %s on #%d", how, user, pr.Number))
}

func (s *Service) cancel(pr *store.PR, user string, duringBatch bool) {
	why := "cancel by " + user
	stopped := ""
	if duringBatch {
		why += " during its batch"
		stopped = stoppedNote + " "
	}
	s.move(pr, states.Cancel, why, func(p *store.PR) { p.HeldBy, p.HeldAt = user, s.Now() })
	s.status(pr, "pending", fmt.Sprintf("held: cancelled by @%s; say `@%s retry` to queue it again", user, s.Bot()), "")
	s.comment(pr, fmt.Sprintf("Cancelled (`cancel` by @%s); approval kept. %sSay `@%s retry` to queue it again.", user, stopped, s.Bot()))
	if duringBatch {
		s.stopBatch("cancel", user, pr)
	}
}

func (s *Service) unapprove(pr *store.PR, user string, duringBatch bool) {
	why := "unapprove by " + user
	stopped, desc := "", "out of the queue: unapprove by "+user
	if duringBatch {
		why += " during its batch"
		stopped = stoppedNote + " "
		desc += "; its batch is stopped"
	}
	s.move(pr, states.Unapprove, why, withdraw)
	s.status(pr, "error", desc, "")
	s.comment(pr, fmt.Sprintf("Approval withdrawn (`unapprove` by @%s); out of the queue. %s%s queues it again.",
		user, stopped, s.approveHint(pr)))
	if duringBatch {
		s.stopBatch("unapprove", user, pr)
	}
}

func (s *Service) StatusText(pr *store.PR) string {
	st := pr.State
	n := pr.Number
	h := head(pr.Head, 12)
	bot := s.Bot()
	var out string
	switch st {
	case states.Queued:
		prs := s.Data.PRs
		if len(prs) == 0 {
			prs = map[string]*store.PR{strconv.Itoa(n): pr}
		}
		line := Line(prs)
		pos := 0
		for i, x := range line {
			if x.Number == n {
				pos = i + 1
				break
			}
		}
		if pos > 0 {
			out = fmt.Sprintf("#%d is queued: %d of %d in line", n, pos, len(line))
		} else {
			out = fmt.Sprintf("#%d is queued", n)
		}
		at := ""
		if pr.ApprovedAt != 0 {
			at = " at " + db.Time(pr.ApprovedAt).Format("2006-01-02 15:04")
		}
		out += fmt.Sprintf(" (priority %d; %s approved by @%s%s)", pr.Priority, h, pr.ApprovedBy, at)
	case states.Testing:
		var others []string
		for _, x := range sortedKeys(s.prs) {
			p := s.prs[x]
			if p.Number != n && p.State == states.Testing {
				others = append(others, fmt.Sprintf("#%d", p.Number))
			}
		}
		out = fmt.Sprintf("#%d is being tested now", n)
		if len(others) > 0 {
			out += " with " + engine.AndList(others)
		}
		if g := s.Live().Gate; g != nil && g.RunURL != "" && slices.Contains(g.PRs, n) {
			out += " (CI: " + g.RunURL + ")"
		}
	case states.SetAside:
		reason := pr.Reason
		if reason == "" {
			reason = "-"
		}
		out = fmt.Sprintf("#%d is set aside: %s; `@%s retry`, or a fix and `@%s approve <its new head>`, queues it again", n,
			reason, bot, bot)
	case states.Held:
		at := ""
		if pr.HeldAt != 0 {
			at = " at " + db.Time(pr.HeldAt).Format("2006-01-02 15:04")
		}
		out = fmt.Sprintf("#%d is held: cancelled by @%s%s; the approval (%s by @%s) is kept; `@%s retry` queues it again",
			n, pr.HeldBy, at, h, pr.ApprovedBy, bot)
	case states.Open:
		out = fmt.Sprintf("#%d is not approved (%s); %s queues it", n, h, s.approveHint(pr))
	default:
		out = fmt.Sprintf("#%d is %s", n, st)
	}
	if pr.Rollup == "never" {
		out += "; it is always gated alone (`rollup never`)"
	}
	if until := s.Data.Until(); until > s.Now() {
		out += fmt.Sprintf(". The queue waits until %s after: %s", db.Time(until).Format("15:04"), s.Data.BackoffWhy)
	}
	return out + "."
}

func (s *Service) approveCommand(pr *store.PR, user, sha string) {
	st, n, bot := pr.State, pr.Number, s.Bot()
	if !commands.FullSha(sha) {
		what := "`approve` names the commit it approves"
		if sha != "" {
			what = fmt.Sprintf("`approve %s` is not a full commit SHA; `approve` names the whole SHA of the commit it approves", sha)
		}
		s.comment(pr, fmt.Sprintf("@%s: not approved: %s, so that it can never count for commits pushed after it was "+
			"written. The head of #%d is %s: %s queues it.", user, what, n, head(pr.Head, 12), s.approveHint(pr)))
		return
	}
	if st == states.Testing && sha != pr.Head {
		duringBatch(pr, store.GateApprove{Sha: sha, User: user})
		s.comment(pr, fmt.Sprintf("@%s: #%d is being tested now at %s (approved by @%s); the approval of %s counts once "+
			"the batch is over, if that is then its head.", user, n, head(pr.Head, 12), pr.ApprovedBy, head(sha, 12)))
		return
	}
	if sha != pr.Head {
		s.comment(pr, fmt.Sprintf("@%s: not approved: %s is not the head of #%d; its head is %s (new commits were pushed "+
			"since?). %s queues the head.", user, head(sha, 12), n, head(pr.Head, 12), s.approveHint(pr)))
		return
	}
	switch {
	case states.Can(st, states.Approve):
		s.approve(pr, user)
		s.comment(pr, fmt.Sprintf("Queued: %s approved by @%s%s.", head(pr.Head, 12), user, priorityNote(pr)))
	case st == states.Queued:
		was := pr.ApprovedBy
		grant(pr, user)
		s.status(pr, "pending", "queued (approved by "+user+")", "")
		s.comment(pr, fmt.Sprintf("@%s: #%d is already queued (%s); the approval is now by @%s (was @%s), in its place in line.",
			user, n, head(pr.Head, 12), user, was))
	case st == states.Testing:
		s.comment(pr, fmt.Sprintf("@%s: #%d is being tested now at %s (approved by @%s).", user, n, head(pr.Head, 12),
			pr.ApprovedBy))
	case st == states.Held:
		s.comment(pr, fmt.Sprintf("@%s: #%d is held (cancelled by @%s); its approval is kept. Say `@%s retry` to queue it again.",
			user, n, pr.HeldBy, bot))
	default:
		s.comment(pr, fmt.Sprintf("@%s: `approve` does not apply: #%d is %s.", user, n, st))
	}
}

func (s *Service) approve(pr *store.PR, by string) {
	s.move(pr, states.Approve, "approved by "+by, func(p *store.PR) {
		grant(p, by)
		p.ApprovedAt, p.Reason = s.Now(), ""
	})
	s.status(pr, "pending", "queued (approved by "+by+")", "")
}

func (s *Service) gateApprover(pr *store.PR, sha string) store.GateApprove {
	for _, a := range pr.GateApproves {
		if a.Sha != sha {
			continue
		}
		ok := false
		if fe := forgeErr(func() { ok = s.Authorized(a.User) }); fe != nil {
			s.Say(fmt.Sprintf("#%d: cannot read @%s's permission (%s): the approval during the batch does not count", pr.Number,
				a.User, fe))
			continue
		}
		if ok {
			return a
		}
	}
	return store.GateApprove{}
}

func parseISO(v string) (float64, bool) {
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return float64(t.UnixNano()) / 1e9, true
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05Z07:00"} {
		if t, err := time.Parse(layout, v); err == nil {
			return float64(t.UnixNano()) / 1e9, true
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			return float64(t.UnixNano()) / 1e9, true
		}
	}
	return 0, false
}

func Line(prs map[string]*store.PR) []*store.PR {
	var ready []*store.PR
	for _, pr := range prs {
		if pr.State == states.Queued {
			ready = append(ready, pr)
		}
	}
	sort.SliceStable(ready, func(i, j int) bool {
		a, b := ready[i], ready[j]
		if pa, pb := a.Priority, b.Priority; pa != pb {
			return pa > pb
		}
		if ta, tb := a.ApprovedAt, b.ApprovedAt; ta != tb {
			return ta < tb
		}
		return a.Number < b.Number
	})
	return ready
}

func (s *Service) Pick(prs map[string]*store.PR) []*store.PR {
	line := Line(prs)
	var ready []*store.PR
	for _, pr := range line {
		if _, skip := s.skipped[pr.Number]; !skip && !s.lags(pr) {
			ready = append(ready, pr)
		}
	}
	if len(ready) == 0 {
		return nil
	}
	if ready[0].Rollup == "never" {
		return ready[:1]
	}
	n := max(1, s.FJ.BatchMax)
	var batch []*store.PR
	for i, pr := range ready {
		if i >= n || pr.Rollup == "never" {
			break
		}
		batch = append(batch, pr)
	}
	return batch
}

func (s *Service) lags(pr *store.PR) bool {
	h, ok := s.lagged[pr.Number]
	return ok && h == pr.Head
}

func (s *Service) fetch(batch []*store.PR) (map[int]bool, bool) {
	t := s.Target()
	specs := []string{fmt.Sprintf("+refs/heads/%s:refs/heads/%s", t, t)}
	for _, pr := range batch {
		specs = append(specs, fmt.Sprintf("+refs/pull/%d/head:refs/murasame/pr/%d", pr.Number, pr.Number))
	}
	r := s.AuthGit().AuthRun(append([]string{"fetch", "-q", "--no-tags", s.FJ.GitRemoteURL}, specs...)...)
	if r.OK() {
		return nil, true
	}
	if r.Code != gitx.NetStoppedCode {
		missing, lr := s.missingPRRefs(batch)
		if len(missing) > 0 {
			var rest []*store.PR
			for _, pr := range batch {
				if !missing[pr.Number] {
					rest = append(rest, pr)
				}
			}
			more, ok := s.fetch(rest)
			if !ok {
				return nil, false
			}
			for n := range more {
				missing[n] = true
			}
			return missing, true
		}
		if lr.Code == gitx.NetStoppedCode {
			r = lr
		}
	}
	out := strings.TrimSpace(r.Out)
	s.Say("fetch failed: " + tail(out, 300))
	if r.Code != gitx.NetStoppedCode {
		s.backoff("fetch failed: " + tail(out, 200))
	}
	return nil, false
}

func (s *Service) missingPRRefs(batch []*store.PR) (map[int]bool, gitx.Result) {
	target := "refs/heads/" + s.Target()
	args := []string{"ls-remote", s.FJ.GitRemoteURL, target}
	for _, pr := range batch {
		args = append(args, fmt.Sprintf("refs/pull/%d/head", pr.Number))
	}
	r := s.AuthGit().AuthRun(args...)
	if !r.OK() {
		return nil, r
	}
	seen := map[string]bool{}
	for _, l := range gitx.Lines(r.Out) {
		if _, ref, ok := strings.Cut(l, "\t"); ok {
			seen[strings.TrimSpace(ref)] = true
		}
	}
	if !seen[target] {
		return nil, r
	}
	missing := map[int]bool{}
	for _, pr := range batch {
		if !seen[fmt.Sprintf("refs/pull/%d/head", pr.Number)] {
			missing[pr.Number] = true
		}
	}
	return missing, r
}

func (s *Service) refLags(pr *store.PR, got string) {
	h := pr.Head
	shown := got
	if shown == "" {
		shown = "-"
	}
	if s.lagged == nil {
		s.lagged = map[int]string{}
	}
	s.lagged[pr.Number] = h
	s.Say(fmt.Sprintf("#%d: the forge's refs/pull/%d/head is at %s, not at its head %s: the rest of the queue goes first",
		pr.Number, pr.Number, head(shown, 12), head(h, 12)))
	if pr.RefLag != h {
		pr.RefLag = h
		check(s.store().SavePR(pr))
		s.status(pr, "pending", fmt.Sprintf("queued: waiting for the forge's PR ref (refs/pull/%d/head) to catch up with %s",
			pr.Number, head(h, 12)), "")
	}
}

func (s *Service) backoff(why string) {
	why = s.Redact(why)
	states.BackoffMachine.Must(backoffOf(s.Data), states.BackoffInfra)
	until := s.Now() + float64(s.FJ.InfraBackoff)
	s.Data.BackoffUntil = &until
	s.Data.BackoffWhy = why
	check(s.store().SetBackoff(until, why))
}

func (s *Service) RunBatch(batch []*store.PR) {
	s.halt.begin(s.Stopping())
	defer func() {
		s.halt.end()
		s.prs = map[string]*store.PR{}
	}()
	s.runBatch(batch)
}

func intPtr(v int) *int { return &v }

func clearApproval(p *store.PR) { p.ApprovedHead, p.HeldBy, p.HeldAt = "", "", 0 }

func (s *Service) runBatch(batch []*store.PR) {
	g := s.Git()
	t := s.Target()
	var ready []*store.PR
	for len(batch) > 0 {
		missing, ok := s.fetch(batch)
		if !ok {
			return
		}
		ready = nil
		for _, pr := range batch {
			if missing[pr.Number] {
				s.refLags(pr, "")
				continue
			}
			if got := g.Rev(fmt.Sprintf("refs/murasame/pr/%d", pr.Number)); got != pr.Head {
				s.refLags(pr, got)
				continue
			}
			ready = append(ready, pr)
		}
		if len(ready) == len(batch) {
			break
		}
		batch = s.Pick(s.Data.PRs)
		ready = nil
	}
	if len(ready) == 0 {
		return
	}
	for _, pr := range ready {
		pr.RefLag = ""
	}
	st := s.store()
	type moved struct {
		pr, next *store.PR
	}
	var started []*store.PR
	var moves []moved
	var runID int64
	const mode = "batch"
	why := fmt.Sprintf("batch of %d", len(ready))
	check(st.DB.Tx(func(tx *db.Tx) error {
		started, moves = nil, nil
		for _, pr := range ready {
			next, err := st.TransitionTx(tx, pr, states.Start, why, batchStarts)
			if err != nil {
				var it *states.InvalidTransition
				if errors.As(err, &it) {
					s.Say(it.Error())
					continue
				}
				return err
			}
			moves = append(moves, moved{pr, next})
			started = append(started, pr)
		}
		if len(started) == 0 {
			return nil
		}
		members := make([]store.Member, len(started))
		for i, pr := range moves {
			p := pr.next
			members[i] = store.Member{Name: fmt.Sprintf("#%d", p.Number), Ref: fmt.Sprintf("refs/murasame/pr/%d", p.Number),
				PR: intPtr(p.Number), Branch: &p.Branch, Head: &p.Head, Priority: intPtr(p.Priority),
				ApprovedAt: &p.ApprovedAt, ApprovedBy: &p.ApprovedBy}
		}
		var err error
		runID, err = store.NewHistory(st.Repo).BeginTx(tx, "service", mode, t, g.Rev("refs/heads/"+t), "service batch", members)
		return err
	}))
	for _, m := range moves {
		*m.pr = *m.next
		s.sayMove(m.pr, states.Start, m.pr.State, why)
	}
	if len(started) == 0 {
		return
	}
	for _, pr := range started {
		s.status(pr, "pending", fmt.Sprintf("testing in a batch of %d", len(started)), "")
		if s.FJ.CommentOnStart {
			var others []string
			for _, p := range started {
				if p.Number != pr.Number {
					others = append(others, fmt.Sprintf("#%d", p.Number))
				}
			}
			msg := "Testing " + head(pr.Head, 12)
			if len(others) > 0 {
				msg += " with " + strings.Join(others, " ")
			}
			s.comment(pr, msg+".")
		}
	}
	s.prs = map[string]*store.PR{}
	for _, pr := range started {
		s.prs[fmt.Sprintf("#%d", pr.Number)] = pr
	}
	s.lastPoll = time.Now()
	s.phase(states.PhaseRun, "")
	items := make([]engine.Item, len(started))
	for i, pr := range started {
		items[i] = engine.NewItem(fmt.Sprintf("#%d", pr.Number), fmt.Sprintf("refs/murasame/pr/%d", pr.Number),
			fmt.Sprintf("Merge #%d: %s (murasame)", pr.Number, pr.Title), fmt.Sprintf("pr-%d", pr.Number))
	}
	cfg := s.Cfg.Copy()
	cfg.Target = []string{t}
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = panics.AsError(r)
			}
		}()
		eng, err := engine.New(cfg, s.RepoDir, engine.Options{
			Lander: &approvedLander{PushLander: &engine.PushLander{Repo: s.pushGit(), Remote: s.FJ.GitRemoteURL, Target: t}, s: s},
			CI:     ciForge{s}, Ignore: s.Context, Events: s,
			Stream: redactWriter{s}, GitEnv: g.Env, Sleep: s.Sleep, RepoKey: s.RepoKey(), ResetStaleInteg: true})
		if err != nil {
			return err
		}
		res, err := eng.Run(items, engine.RunOptions{Args: "service batch", RunID: runID})
		if err != nil {
			return err
		}
		if res.Stopped != "" && s.halt.reason() == "" {
			s.backoff(res.Stopped)
		}
		return nil
	}()
	if err != nil {
		s.Say(fmt.Sprintf("the engine failed: %v", err))
		s.backoff(fmt.Sprintf("engine error: %v", err))
	}
	for _, pr := range started {
		if pr.State == states.Testing {
			s.move(pr, states.Requeue, "not decided in this run", nil)
			s.status(pr, "pending", "queued: will be tried again", "")
		}
	}
}

type approvedLander struct {
	*engine.PushLander
	s *Service
}

func (l *approvedLander) Land(sha string) string {
	if why := l.s.recheckApprovals(sha); why != "" {
		return why
	}
	return l.PushLander.Land(sha)
}

func (s *Service) refused(sha string) string {
	if why := s.halt.cancelled(); why != "" {
		return fmt.Sprintf("refusing to push %s: %s", head(sha, 12), why)
	}
	return ""
}

func (s *Service) recheckApprovals(sha string) string {
	if why := s.refused(sha); why != "" {
		return why
	}
	if fe := s.readBatchComments("before the push"); fe != nil {
		return fmt.Sprintf("cannot read the comments of the batch's PRs again (%s): refusing to push %s", fe, head(sha, 12))
	}
	if why := s.refused(sha); why != "" {
		return why
	}
	for _, k := range sortedKeys(s.prs) {
		pr := s.prs[k]
		if pr.State != states.Testing {
			continue
		}
		if fe := forgeErr(func() { s.stillLandable(pr) }); fe != nil {
			return fmt.Sprintf("cannot check #%d on the forge again (%s): refusing to push %s", pr.Number, fe, head(sha, 12))
		}
	}
	if why := s.refused(sha); why != "" {
		return why
	}
	if s.FJ.ReviewersFromPermission == "" {
		return ""
	}
	s.permCache, s.permDeny = map[string]string{}, map[string]bool{}
	g := s.Git()
	for _, k := range sortedKeys(s.prs) {
		pr := s.prs[k]
		if pr.State != states.Testing || !g.IsAncestor(pr.Head, sha) {
			continue
		}
		by := pr.ApprovedBy
		if by == "" {
			continue
		}
		allowed := false
		if fe := forgeErr(func() { allowed = s.Authorized(by) }); fe != nil {
			return fmt.Sprintf("cannot check again that @%s may still approve #%d (%s): refusing to push %s", by, pr.Number, fe,
				head(sha, 12))
		}
		if !allowed && s.permDeny[strings.ToLower(by)] {
			return fmt.Sprintf("cannot check again that @%s may still approve #%d (the forge refused to tell the bot the "+
				"permission: reviewers_from_permission needs the bot to have admin access): refusing to push %s", by,
				pr.Number, head(sha, 12))
		}
		if !allowed {
			s.revoked(pr, by)
		}
	}
	return s.refused(sha)
}

func (s *Service) stillLandable(pr *store.PR) {
	n := pr.Number
	p := must(s.API.Pull(n))
	switch {
	case p.Merged:
		s.move(pr, states.MergedElsewhere, "merged on the forge during its batch", nil)
		s.stopFor(fmt.Sprintf("#%d was merged on the forge during its batch", n))
		return
	case p.State != "" && p.State != "open":
		s.move(pr, states.Close, "closed on the forge during its batch", nil)
		s.status(pr, "error", "closed during its batch: not merged", "")
		s.comment(pr, fmt.Sprintf("Not merged: this PR was closed while its batch was running. %s Reopen it and "+
			"approve it again (%s) to queue it.", stoppedNote, s.approveHint(pr)))
		s.stopFor(fmt.Sprintf("#%d was closed during its batch", n))
		return
	case p.Base.Ref != "" && p.Base.Ref != s.Target():
		t, base := s.Target(), p.Base.Ref
		s.move(pr, states.Close, "retargeted to "+base+" during its batch", retarget)
		s.status(pr, "error", "retargeted during its batch: not merged", "")
		s.comment(pr, fmt.Sprintf("Not merged: this PR was retargeted to `%s` while its batch was running; the queue "+
			"merges into `%s` only. %s Retarget it to `%s` and approve it again (%s) to queue it.", base, t, stoppedNote, t,
			s.approveHint(pr)))
		s.stopFor(fmt.Sprintf("#%d was retargeted to %s during its batch", n, base))
		return
	}
	if pr.ApprovedHead != pr.Head {
		s.move(pr, states.Requeue, fmt.Sprintf("the tested %s is not the approved %s", head(pr.Head, 12),
			head(pr.ApprovedHead, 12)), nil)
		s.stopFor(fmt.Sprintf("#%d: the tested %s is not the approved %s", n, head(pr.Head, 12), head(pr.ApprovedHead, 12)))
	}
}

func (s *Service) stopFor(why string) { s.halt.cancel(why) }

func (s *Service) revoked(pr *store.PR, by string) {
	level := s.FJ.ReviewersFromPermission
	s.move(pr, states.Unapprove, fmt.Sprintf("%s no longer has %s permission", by, level), withdraw)
	s.status(pr, "error", fmt.Sprintf("out of the queue: @%s may no longer approve; its batch is stopped", by), "")
	s.comment(pr, fmt.Sprintf("Approval withdrawn: @%s approved it but no longer has `%s` permission on the repository, so "+
		"it was not merged. %s A reviewer's %s queues it again.%s", by, level, stoppedNote, s.approveHint(pr), s.permNote(by)))
	s.halt.cancel(fmt.Sprintf("the approval of #%d is no longer valid", pr.Number))
}

func (s *Service) Gating(items []engine.Item, log string) {
	var prs []int
	for _, it := range items {
		if pr, ok := s.prs[it.Name]; ok {
			prs = append(prs, pr.Number)
		}
	}
	s.setLive(func(l *Live) { l.Gate = &LiveGate{Since: s.Now(), Log: filepath.Base(log), PRs: prs} })
	for _, it := range items {
		if pr, ok := s.prs[it.Name]; ok {
			s.status(pr, "pending", fmt.Sprintf("CI running on the merged result (with %d other PR(s))", len(items)-1), log)
		}
	}
}

func (s *Service) CIStarted(items []engine.Item, url string) {
	s.setLive(func(l *Live) {
		if l.Gate != nil {
			l.Gate.RunURL = url
		}
	})
	for _, it := range items {
		if pr, ok := s.prs[it.Name]; ok {
			n, sha, desc := pr.Number, pr.Head, s.Redact(fmt.Sprintf("CI running on the merged result (with %d other PR(s))",
				len(items)-1))
			s.later(pr, func() {
				if err := s.API.Status(sha, "pending", s.Context, desc, url); err != nil {
					s.Say(fmt.Sprintf("status for #%d: %s", n, err))
				}
			})
		}
	}
}

func (s *Service) GateDone() { s.setLive(func(l *Live) { l.Gate = nil }) }

type ciForge struct{ s *Service }

func (f ciForge) Push(sha, branch string) error {
	ref := "refs/heads/" + branch
	push := func(spec string) (gitx.Result, error) {
		r := f.s.AuthGit().AuthRun("push", "--porcelain", "--force", f.s.FJ.GitRemoteURL, spec)
		if !r.OK() {
			return r, fmt.Errorf("git push %s failed: %s", spec, gitx.LastLine(r.Out))
		}
		return r, nil
	}
	r, err := push(sha + ":" + ref)
	if err != nil {
		return err
	}
	if !upToDate(r.Out, ref) {
		return nil
	}
	if _, err := push(":" + ref); err != nil {
		return err
	}
	_, err = push(sha + ":" + ref)
	return err
}

func upToDate(porcelain, ref string) bool {
	for _, line := range gitx.Lines(porcelain) {
		if f := strings.Split(line, "\t"); len(f) >= 2 && strings.HasSuffix(f[1], ":"+ref) {
			return strings.HasPrefix(f[0], "=")
		}
	}
	return false
}

func (f ciForge) Statuses(sha string) ([]forgejo.CommitStatus, error) {
	sts, err := f.s.API.Statuses(sha)
	if err != nil {
		return nil, err
	}
	origin := ""
	if u, perr := url.Parse(f.s.FJ.URL); perr == nil && u.Scheme != "" && u.Host != "" {
		origin = u.Scheme + "://" + u.Host
	}
	for i, st := range sts {
		if origin != "" && strings.HasPrefix(st.TargetURL, "/") && !strings.HasPrefix(st.TargetURL, "//") {
			sts[i].TargetURL = origin + st.TargetURL
		}
	}
	return sts, nil
}

func (s *Service) Nudge() {
	s.pollNow.Store(true)
	s.ciNow.Store(true)
}

func (s *Service) CINow() bool { return s.ciNow.Swap(false) }

func (s *Service) Stop(why string) {
	s.stopping.Store(&why)
	s.halt.signal(why)
	s.stopOnce.Do(func() { close(s.stopCh) })
}

func (s *Service) Stopping() string {
	if p := s.stopping.Load(); p != nil {
		return *p
	}
	return ""
}

func (s *Service) Poll() string {
	if why := s.halt.reason(); why != "" {
		return why
	}
	every := s.FJ.PollDuringGate
	if every <= 0 {
		return ""
	}
	if !s.pollNow.Load() && time.Since(s.lastPoll) < time.Duration(every)*time.Second {
		return ""
	}
	s.readBatchComments("during the gate")
	return s.halt.reason()
}

func (s *Service) readBatchComments(when string) *forgejo.Error {
	s.pollNow.Store(false)
	s.lastPoll = time.Now()
	for _, k := range sortedKeys(s.prs) {
		pr := s.prs[k]
		fe := forgeErr(func() {
			defer func() { check(s.store().SavePR(pr)) }()
			if inStates(pr, states.Testing, states.Merged, states.Closed) {
				s.syncComments(pr, must(s.API.Comments(pr.Number)))
				return
			}
			s.readPR(pr, "")
		})
		if fe != nil {
			s.Say(fmt.Sprintf("#%d: reading comments %s: %s", pr.Number, when, fe))
			return fe
		}
	}
	return nil
}

func (s *Service) Outcome(item engine.Item, o engine.Outcome) {
	pr, ok := s.prs[item.Name]
	if !ok || pr.State != states.Testing {
		return
	}
	if fe := forgeErr(func() { s.outcome(pr, o) }); fe != nil {
		s.Say(fmt.Sprintf("#%d: reporting its outcome: %s", pr.Number, fe))
		if pr.State == states.Testing {
			if o.Status == "merged" {
				s.headUnknown(pr, s.mergedWhat(pr, o), fe, &o)
			} else {
				s.headUnknown(pr, fmt.Sprintf("%s (%s)", o.Status, head(o.Sha, 12)), fe, nil)
			}
		}
	}
}

func (s *Service) outcome(pr *store.PR, o engine.Outcome) {
	o.Why, o.Line = s.Redact(o.Why), s.Redact(o.Line)
	o.Partners = append([]string{}, o.Partners...)
	for i, p := range o.Partners {
		o.Partners[i] = s.Redact(p)
	}
	switch {
	case o.Status == "merged":
		s.merged(pr, o)
	case o.Status == "set_aside":
		s.setAside(pr, o)
	case o.Status == "skipped":
		if s.movedOn(pr, fmt.Sprintf("%s was already in `%s` (%s)", head(pr.Head, 12), s.Target(), o.Why), nil) {
			return
		}
		target := pr.Head
		if t := s.Git().Rev("refs/heads/" + s.Target()); t != "" {
			target = t
		}
		s.move(pr, states.MergedElsewhere, o.Why, s.markIntent(target))
		s.status(pr, "success", o.Why, "")
		s.markMerged(pr, target)
	case s.halt.reason() != "":
		why := s.halt.reason()
		s.move(pr, states.Requeue, "its batch was stopped ("+why+")", nil)
		s.status(pr, "pending", head("queued: its batch was stopped ("+why+")", 255), "")
	default:
		key := StopKey(o.Why)
		first := o.Why != "" && states.NewStopReason(key, pr.LastStop)
		var fields func(*store.PR)
		if first {
			fields = func(p *store.PR) { p.LastStop = key }
		}
		s.move(pr, states.Requeue, o.Why, fields)
		s.status(pr, "pending", head("queued: the queue stopped: "+o.Why, 255), o.Log)
		if first {
			s.comment(pr, "The queue stopped (not this PR's fault; it stays queued):\n\n> "+o.Why)
		}
	}
}

func (s *Service) forgeHead(pr *store.PR) (string, error) {
	p, err := s.API.Pull(pr.Number)
	if err != nil {
		return "", err
	}
	sha := p.Head.Sha
	if sha == "" {
		return "", errors.New("no head sha in the pull")
	}
	return sha, nil
}

func (s *Service) movedOn(pr *store.PR, what string, o *engine.Outcome) bool {
	now, err := s.forgeHead(pr)
	if err != nil {
		s.Say(fmt.Sprintf("#%d: reading its head: %v", pr.Number, err))
		s.headUnknown(pr, what, err, o)
		return true
	}
	if now == pr.Head {
		return false
	}
	old := pr.Head
	s.atomically(pr, func() { s.pushedDuring(pr, what, o, old, now) })
	return true
}

func (s *Service) pushedDuring(pr *store.PR, what string, o *engine.Outcome, old, now string) {
	t := s.Target()
	if o != nil {
		s.status(pr, "success", fmt.Sprintf("merged into %s (%s)", t, head(o.Sha, 12)), o.Log)
	}
	s.move(pr, states.Push, fmt.Sprintf("%s landed; new head %s pushed during its batch", head(old, 12), head(now, 12)),
		func(p *store.PR) {
			outdate(p)
			p.Head, p.Reason = now, ""
		})
	line := ""
	if o != nil && o.Line != "" {
		line = "\n\n```\n" + o.Line + "\n```"
	}
	if a := s.gateApprover(pr, now); a.User != "" {
		by := a.User
		s.approve(pr, by)
		s.comment(pr, fmt.Sprintf("%s.%s\n\nNew commits were pushed while it was being tested (%s); they are **not** in `%s`. "+
			"@%s approved %s during the batch, so it is queued now%s.", what, line, head(now, 12), t, by, head(now, 12),
			priorityNote(pr)))
		return
	}
	s.comment(pr, fmt.Sprintf("%s.%s\n\nNew commits were pushed while it was being tested (%s); they are **not** in `%s`, "+
		"so this PR stays open. To queue them: %s.", what, line, head(now, 12), t, s.approveHint(pr)))
}

func (s *Service) headUnknown(pr *store.PR, what string, err error, o *engine.Outcome) {
	t := s.Target()
	why := "its head on the forge could not be read; not marked merged"
	if o != nil {
		why = head(o.Sha, 12) + " landed; " + why
	}
	s.move(pr, states.Requeue, why, nil)
	s.status(pr, "pending", head("queued: "+why+"; checked again next cycle", 255), "")
	if o == nil {
		return
	}
	line := ""
	if o.Line != "" {
		line = "\n\n```\n" + o.Line + "\n```"
	}
	s.comment(pr, fmt.Sprintf("%s.%s\n\nIts current head could not be read from the forge (%s), so it is **not** marked "+
		"merged: commits pushed during its batch would not be in `%s`. It stays queued; the next cycle reads its head "+
		"again: new commits take it out of the queue, otherwise it is marked merged.", what, line, head(err.Error(), 200), t))
}

func (s *Service) merged(pr *store.PR, o engine.Outcome) {
	if s.movedOn(pr, s.mergedWhat(pr, o), &o) {
		return
	}
	s.move(pr, states.Land, head(o.Sha, 12), s.markIntent(o.Sha))
	s.status(pr, "success", fmt.Sprintf("merged into %s (%s)", s.Target(), head(o.Sha, 12)), o.Log)
	body := fmt.Sprintf("Merged into `%s` (%s).\n\n```\n%s\n```", s.Target(), head(o.Sha, 12), o.Line)
	s.comment(pr, body)
	s.markMerged(pr, o.Sha)
}

func (s *Service) setAside(pr *store.PR, o engine.Outcome) {
	s.move(pr, states.SetAsideEv, o.Why, func(p *store.PR) { p.Reason = o.Why })
	t := s.Target()
	var body string
	if len(o.Partners) > 0 {
		s.status(pr, "failure", head("set aside: fails only when merged together with "+engine.AndList(o.Partners), 255), o.Log)
		body = PairComment(o.Partners, t)
	} else {
		s.status(pr, "failure", head("set aside: "+o.Why, 255), o.Log)
		body = fmt.Sprintf("Set aside: %s\n\nThe rest of the batch went on without it. Push a fix and approve its new head "+
			"(`@%s approve <sha>`), or `@%s retry` if it was not this PR's fault.", o.Why, s.Bot(), s.Bot())
	}
	if ex := s.logTail(o.Log); ex != "" {
		body += "\n\n<details><summary>gate log (last lines)</summary>\n\n```\n" + ex + "\n```\n</details>"
	}
	s.comment(pr, body)
}

func (s *Service) logTail(log string) string {
	if log == "" {
		return ""
	}
	raw, cut, err := safefile.ReadTail(log, 64<<10)
	if err != nil {
		return ""
	}
	lines := gitx.Lines(strings.ToValidUTF8(string(raw), "\uFFFD"))
	if cut && len(lines) > 1 {
		lines = lines[1:]
	}
	if n := s.FJ.LogExcerptLines; n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.ReplaceAll(strings.Join(lines, "\n"), "```", "'''")
}

func (s *Service) mergedWhat(pr *store.PR, o engine.Outcome) string {
	return fmt.Sprintf("Merged %s into `%s` (%s)", head(pr.Head, 12), s.Target(), head(o.Sha, 12))
}

func (s *Service) markIntent(sha string) func(*store.PR) {
	if !s.FJ.MarkMerged {
		return nil
	}
	return func(p *store.PR) { stepMark(p, states.MarkLanded, sha) }
}

func (s *Service) markMerged(pr *store.PR, sha string) {
	if !s.FJ.MarkMerged || markOf(pr) != states.MarkPending || pr.MarkMerged != sha {
		return
	}
	s.tryMark(pr, false)
}

func (s *Service) tryMark(pr *store.PR, closed bool) {
	if !s.FJ.MarkMerged {
		return
	}
	err := s.API.MarkMerged(pr.Number, pr.MarkMerged)
	var fe *forgejo.Error
	switch {
	case err == nil:
		stepMark(pr, states.MarkMarked, "")
	case closed && (errors.As(err, &fe) && fe.Status >= 400 && fe.Status < 500 && fe.Status != 429 ||
		pr.MarkTries+1 >= markTriesWhenClosed):
		s.Say(fmt.Sprintf("#%d: marking merged: %s; the PR is closed on the forge: no longer tried", pr.Number, err))
		stepMark(pr, states.MarkGiveUp, "")
	default:
		s.Say(fmt.Sprintf("#%d: marking merged: %s (tried again next cycle)", pr.Number, err))
		stepMark(pr, states.MarkForgeErr, "")
		if !closed {
			pr.MarkTries = 0
		}
	}
	check(s.store().SavePR(pr))
}

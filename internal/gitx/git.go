package gitx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"murasame/internal/secretenv"
)

var AuthSafe = []string{"-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "-c", "core.askPass="}

var CloneConfigKeys = map[string]bool{"core.repositoryformatversion": true, "core.filemode": true, "core.bare": true,
	"core.ignorecase": true, "core.precomposeunicode": true, "core.logallrefupdates": true, "core.symlinks": true,
	"extensions.objectformat": true, "extensions.refstorage": true, "gc.auto": true}

type Error struct {
	Args   []string
	Code   int
	Output string
}

func (e *Error) Error() string {
	out := strings.TrimSpace(e.Output)
	if len(out) > 400 {
		out = out[len(out)-400:]
	}
	return fmt.Sprintf("git %s failed (%d): %s", strings.Join(e.Args, " "), e.Code, out)
}

func Catch(err *error) {
	if r := recover(); r != nil {
		if ge, ok := r.(*Error); ok {
			*err = ge
			return
		}
		panic(r)
	}
}

type Result struct {
	Code int
	Out  string
}

func (r Result) OK() bool { return r.Code == 0 }

type Git struct {
	Cwd        string
	Env        map[string]string
	NetTimeout time.Duration
	Done       <-chan struct{}
}

const NetTimeoutCode = 124

const NetStoppedCode = 125

const LowSpeedTime = 60

const OutputGrace = 2 * time.Second

func (r Result) Signaled() bool { return r.Code < 0 }

var errStopping = errors.New("the service is stopping")

func New(cwd string, env map[string]string) *Git {
	e := map[string]string{}
	for k, v := range env {
		e[k] = v
	}
	return &Git{Cwd: cwd, Env: e}
}

type RunOpt struct {
	Input *string
}

func (g *Git) Run(args ...string) Result { return g.RunIn(nil, args...) }

func (g *Git) RunIn(input *string, args ...string) Result {
	r, _ := g.run(input, false, args...)
	return r
}

func (g *Git) RunStdout(args ...string) (Result, string) { return g.run(nil, true, args...) }

func (g *Git) run(input *string, split bool, args ...string) (Result, string) {
	return g.runCtx(context.Background(), g.localEnv(), input, split, args...)
}

var localDropped = map[string]bool{"GIT_CONFIG": true, "GIT_ATTR_SOURCE": true, "GIT_EXEC_PATH": true,
	"GIT_TEMPLATE_DIR": true}

func (g *Git) localEnv() []string {
	env := secretenv.Inherited()
	for k := range env {
		if isAuthConfigEnv(k) || localDropped[k] {
			delete(env, k)
		}
	}
	for k, v := range g.Env {
		env[k] = v
	}
	return secretenv.List(env)
}

func (g *Git) runCtx(ctx context.Context, env []string, input *string, split bool, args ...string) (Result, string) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.Cwd}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if ctx.Done() != nil {
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	}
	cmd.WaitDelay = OutputGrace
	cmd.Env = env
	var buf, stdout bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if split {
		both := &lockedWriter{w: &buf}
		cmd.Stdout = io.MultiWriter(both, &stdout)
		cmd.Stderr = both
	}
	if input != nil {
		cmd.Stdin = strings.NewReader(*input)
	}
	err := cmd.Run()
	code := ExitCode(cmd, err)
	if cmd.ProcessState == nil && err != nil {
		buf.WriteString(err.Error())
	}
	return Result{code, normalizeNewlines(buf.String())}, stdout.String()
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func normalizeNewlines(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

func (g *Git) MustRun(args ...string) Result {
	r := g.Run(args...)
	if !r.OK() {
		panic(&Error{Args: args, Code: r.Code, Output: r.Out})
	}
	return r
}

func (g *Git) MustStdout(args ...string) string {
	r, out := g.RunStdout(args...)
	if !r.OK() {
		panic(&Error{Args: args, Code: r.Code, Output: r.Out})
	}
	return out
}

func splitNUL(out string) []string {
	var res []string
	for _, x := range strings.Split(out, "\x00") {
		if x != "" {
			res = append(res, x)
		}
	}
	return res
}

var keptGitEnv = map[string]bool{"GIT_CONFIG_GLOBAL": true, "GIT_CONFIG_SYSTEM": true, "GIT_CONFIG_NOSYSTEM": true,
	"GIT_SSL_CAINFO": true, "GIT_SSL_CAPATH": true}

var droppedEnv = map[string]bool{"SSH_ASKPASS": true, "SSH_ASKPASS_REQUIRE": true, "SSLKEYLOGFILE": true}

func isProxyEnv(k string) bool {
	switch strings.ToLower(k) {
	case "http_proxy", "https_proxy", "all_proxy":
		return true
	}
	return false
}

func isAuthConfigEnv(k string) bool {
	return k == "GIT_CONFIG_COUNT" || k == "GIT_CONFIG_PARAMETERS" || strings.HasPrefix(k, "GIT_CONFIG_KEY_") ||
		strings.HasPrefix(k, "GIT_CONFIG_VALUE_")
}

func cleanInherited() (map[string]string, []string) {
	env := map[string]string{}
	var proxies []string
	for k, v := range secretenv.Inherited() {
		switch {
		case isProxyEnv(k):
			if v != "" {
				proxies = append(proxies, k)
			}
		case strings.HasPrefix(k, "GIT_") && !keptGitEnv[k], droppedEnv[k]:
		default:
			env[k] = v
		}
	}
	sort.Strings(proxies)
	return env, proxies
}

func (g *Git) inspectEnv() []string {
	env, _ := cleanInherited()
	for k, v := range g.Env {
		if !isAuthConfigEnv(k) {
			env[k] = v
		}
	}
	env["GIT_TERMINAL_PROMPT"] = "0"
	return secretenv.List(env)
}

func (g *Git) authEnv() []string {
	env, _ := cleanInherited()
	for k, v := range g.Env {
		env[k] = v
	}
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GIT_HTTP_LOW_SPEED_LIMIT"] = "1"
	env["GIT_HTTP_LOW_SPEED_TIME"] = fmt.Sprint(g.lowSpeedSecs())
	return secretenv.List(env)
}

type configEntry struct{ scope, key string }

func (g *Git) listConfig() ([]configEntry, error) {
	r, out := g.runCtx(context.Background(), g.inspectEnv(), nil, true, "config", "--list", "--show-scope", "--name-only", "-z")
	if !r.OK() {
		msg := strings.TrimSpace(r.Out)
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return nil, fmt.Errorf("git config --list failed (%d): %s", r.Code, msg)
	}
	var fields []string
	if out != "" {
		fields = strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	}
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("git config --list printed %d fields, not scope and key pairs", len(fields))
	}
	var entries []configEntry
	for i := 0; i < len(fields); i += 2 {
		entries = append(entries, configEntry{fields[i], strings.ToLower(fields[i+1])})
	}
	return entries, nil
}

func unexpected(entries []configEntry) []string {
	seen := map[string]bool{}
	for _, e := range entries {
		switch e.scope {
		case "local", "worktree":
			if !CloneConfigKeys[e.key] {
				seen[e.key] = true
			}
		case "command":
			seen[e.scope+" "+e.key] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

var riskyKey = regexp.MustCompile(`^(url\..*|remote\..*|http(\..*)?\.(proxy[^.]*|extraheader|curloptresolve|sslverify)|` +
	`include\.path|includeif\..*|core\.(sshcommand|gitproxy|fsmonitor|askpass|hookspath|alternaterefscommand)|` +
	`credential(\..*)?\.helper|(fetch|transfer)\.bundleuri|bundle\..*|push\.gpgsign|protocol(\..*)?\.allow|` +
	`trace2\..*|hook\..*|uploadpack\.packobjectshook)$`)

func risky(entries []configEntry) []string {
	seen := map[string]bool{}
	for _, e := range entries {
		switch e.scope {
		case "local", "worktree", "command":
		case "unknown":
			if e.key != "credential.helper" && riskyKey.MatchString(e.key) {
				seen[e.scope+" "+e.key] = true
			}
		default:
			if riskyKey.MatchString(e.key) {
				seen[e.scope+" "+e.key] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func (g *Git) UnexpectedConfig() ([]string, error) {
	entries, err := g.listConfig()
	if err != nil {
		return nil, err
	}
	return unexpected(entries), nil
}

func (g *Git) RiskyUserConfig() ([]string, error) {
	entries, err := g.listConfig()
	if err != nil {
		return nil, err
	}
	return risky(entries), nil
}

func (g *Git) credentialRefusal() string {
	if _, proxies := cleanInherited(); len(proxies) > 0 {
		return fmt.Sprintf("refusing to use the bot's credentials: the service's environment sets %s, which would send "+
			"the forge commands and their credentials through a proxy; unset it in the service's environment",
			strings.Join(proxies, ", "))
	}
	entries, err := g.listConfig()
	if err != nil {
		return fmt.Sprintf("refusing to use the bot's credentials in %s: cannot read the git config that git would "+
			"apply (%v); fix or remove the unreadable config file", g.Cwd, err)
	}
	if bad := unexpected(entries); len(bad) > 0 {
		return fmt.Sprintf("refusing to use the bot's credentials in %s: its git config has keys the bot did not "+
			"set (%s); a gate or hook may have written them: inspect and remove them", g.Cwd, strings.Join(bad, ", "))
	}
	if bad := risky(entries); len(bad) > 0 {
		return fmt.Sprintf("refusing to use the bot's credentials: the bot user's git config has keys that can send "+
			"them elsewhere or run a command (%s); a gate or hook may have written them: inspect and remove them",
			strings.Join(bad, ", "))
	}
	return ""
}

func (g *Git) AuthRun(args ...string) Result {
	if msg := g.credentialRefusal(); msg != "" {
		return Result{1, msg}
	}
	full := append(append([]string{}, AuthSafe...), g.lowSpeed()...)
	full = append(full, args...)
	if g.NetTimeout <= 0 && g.Done == nil {
		r, _ := g.runCtx(context.Background(), g.authEnv(), nil, false, full...)
		return r
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	if g.NetTimeout > 0 {
		var done context.CancelFunc
		ctx, done = context.WithTimeout(ctx, g.NetTimeout)
		defer done()
	}
	if g.Done != nil {
		go func() {
			select {
			case <-g.Done:
				cancel(errStopping)
			case <-ctx.Done():
			}
		}()
	}
	r, _ := g.runCtx(ctx, g.authEnv(), nil, false, full...)
	if r.OK() {
		return r
	}
	verb := "git"
	if len(args) > 0 {
		verb += " " + args[0]
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return Result{NetTimeoutCode, withLine(r.Out, fmt.Sprintf("%s timed out after %s and was killed (the forge or "+
			"the network is not answering)", verb, g.NetTimeout))}
	case errors.Is(context.Cause(ctx), errStopping):
		return Result{NetStoppedCode, withLine(r.Out, verb+" was stopped: the service is stopping")}
	}
	return r
}

func withLine(out, line string) string {
	if out = strings.TrimRight(out, "\n"); out != "" {
		return out + "\n" + line
	}
	return line
}

func (g *Git) lowSpeedSecs() int {
	secs := LowSpeedTime
	if g.NetTimeout > 0 {
		secs = min(secs, max(1, int(g.NetTimeout/time.Second)))
	}
	return secs
}

func (g *Git) lowSpeed() []string {
	return []string{"-c", "http.lowSpeedLimit=1", "-c", fmt.Sprintf("http.lowSpeedTime=%d", g.lowSpeedSecs())}
}

func (g *Git) Out(args ...string) string {
	return strings.TrimSpace(normalizeNewlines(g.MustStdout(args...)))
}

func (g *Git) OK(args ...string) bool { return g.Run(args...).OK() }

func (g *Git) answer(args ...string) (string, bool) {
	r, out := g.RunStdout(args...)
	switch r.Code {
	case 0:
		return strings.TrimSpace(out), true
	case 1:
		return "", false
	}
	panic(&Error{Args: args, Code: r.Code, Output: r.Out})
}

func (g *Git) Rev(rev string) string {
	out, _ := g.answer("rev-parse", "--verify", "-q", rev)
	return out
}

func (g *Git) Short(rev string, n int) string {
	if n > 0 {
		return g.Out("rev-parse", fmt.Sprintf("--short=%d", n), rev)
	}
	return g.Out("rev-parse", "--short", rev)
}

func (g *Git) CommitOf(rev string) string { return g.Rev(rev + "^{commit}") }

func (g *Git) TreeOf(rev string) string { return g.Out("rev-parse", rev+"^{tree}") }

func (g *Git) IsAncestor(a, b string) bool {
	_, yes := g.answer("merge-base", "--is-ancestor", a, b)
	return yes
}

func (g *Git) Head() string { return g.Out("rev-parse", "HEAD") }

func (g *Git) SymbolicHead() string {
	out, _ := g.answer("symbolic-ref", "-q", "--short", "HEAD")
	return out
}

func (g *Git) HeadRef() string {
	out, _ := g.answer("symbolic-ref", "-q", "HEAD")
	return out
}

func (g *Git) Reattach(ref string) {
	if g.HeadRef() == ref {
		return
	}
	if ref == "" {
		g.MustRun("update-ref", "--no-deref", "HEAD", g.Head())
		return
	}
	g.MustRun("symbolic-ref", "HEAD", ref)
}

func (g *Git) CommonDir() string {
	return g.Out("rev-parse", "--path-format=absolute", "--git-common-dir")
}

func (g *Git) MainCheckout() string {
	common := g.CommonDir()
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	if g.Out("rev-parse", "--is-bare-repository") == "true" {
		return common
	}
	if g.Out("rev-parse", "--path-format=absolute", "--git-dir") == common {
		return g.TopLevel()
	}
	if wt, ok := g.answer("config", "--file", filepath.Join(common, "config"), "--get", "core.worktree"); ok && wt != "" {
		if !filepath.IsAbs(wt) {
			wt = filepath.Join(common, wt)
		}
		return filepath.Clean(wt)
	}
	panic(&Error{Args: []string{"rev-parse", "--git-common-dir"}, Code: 128, Output: "run murasame from the main " +
		"checkout: the git directory " + common + " was made with --separate-git-dir, so a linked worktree cannot " +
		"tell where the main checkout is"})
}

func (g *Git) DirtyTracked() bool {
	return strings.TrimSpace(g.MustStdout("status", "--porcelain", "--untracked-files=no")) != ""
}

func (g *Git) StatusPaths() []string {
	fields := strings.Split(g.MustStdout("status", "--porcelain", "-z", "--untracked-files=all"), "\x00")
	var out []string
	for i := 0; i < len(fields); {
		entry := fields[i]
		i++
		if len(entry) < 4 {
			continue
		}
		out = append(out, entry[3:])
		if (entry[0] == 'R' || entry[0] == 'C') && i < len(fields) {
			out = append(out, fields[i])
			i++
		}
	}
	return out
}

func (g *Git) Unmerged() []string {
	return splitNUL(g.MustStdout("diff", "--name-only", "-z", "--diff-filter=U"))
}

func (g *Git) Stages(path string) map[string]bool {
	out := map[string]bool{}
	for stage := range g.StageModes(path) {
		out[stage] = true
	}
	return out
}

func (g *Git) StageModes(path string) map[string]string {
	out := map[string]string{}
	for _, entry := range splitNUL(g.MustStdout("ls-files", "-u", "-z", "--", ":(literal)"+path)) {
		meta, name, _ := strings.Cut(entry, "\t")
		f := strings.Fields(meta)
		if len(f) >= 3 && name == path {
			out[f[2]] = f[0]
		}
	}
	return out
}

func (g *Git) Commit(message string) {
	r := g.RunIn(&message, "commit", "-q", "-F", "-")
	if !r.OK() {
		panic(&Error{Args: []string{"commit", "-q", "-F", "-"}, Code: r.Code, Output: r.Out})
	}
}

func (g *Git) StagedChanges() bool { return !g.OK("diff", "--cached", "--quiet") }

func LastLine(out string) string {
	lines := Lines(strings.TrimSpace(out))
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

func Lines(s string) []string {
	s = strings.TrimSuffix(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func Literal(path string) string { return ":(literal)" + path }

func fileSum(path string) string {
	st, err := os.Lstat(path)
	switch {
	case err != nil:
		return "-"
	case st.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return "symlink"
		}
		return fmt.Sprintf("symlink %x", sha256.Sum256([]byte(target)))
	case st.IsDir():
		return "dir"
	case !st.Mode().IsRegular():
		return "special " + st.Mode().Type().String()
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "-"
	}
	defer f.Close()
	if now, err := f.Stat(); err != nil || !now.Mode().IsRegular() {
		return "special"
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "-"
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (g *Git) Snapshot() map[string]string {
	out := map[string]string{}
	for _, p := range g.StatusPaths() {
		out[p] = fileSum(filepath.Join(g.Cwd, p))
	}
	return out
}

func (g *Git) TopLevel() string { return g.Out("rev-parse", "--show-toplevel") }

func (g *Git) Untracked() []string {
	return splitNUL(g.MustStdout("ls-files", "--others", "--exclude-standard", "--directory", "-z"))
}

func (g *Git) WithoutCredentials() *Git {
	out := New(g.Cwd, nil)
	for k, v := range g.Env {
		if !isAuthConfigEnv(k) {
			out.Env[k] = v
		}
	}
	return out
}

func (g *Git) ChangedSince(before map[string]string) []string {
	var out []string
	for p, sum := range g.Snapshot() {
		if was, ok := before[p]; !ok || was != sum {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func (g *Git) Stage(paths []string) {
	if len(paths) == 0 {
		return
	}
	args := []string{"add", "-A", "--"}
	for _, p := range paths {
		args = append(args, Literal(p))
	}
	g.MustRun(args...)
}

func ExitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState == nil {
		if err != nil {
			return 127
		}
		return 0
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return cmd.ProcessState.ExitCode()
}

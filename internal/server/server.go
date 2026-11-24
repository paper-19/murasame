package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"murasame/internal/config"
	"murasame/internal/db"
	"murasame/internal/instance"
	"murasame/internal/safefile"
	"murasame/internal/secretenv"
	"murasame/internal/service"
	"murasame/internal/states"
	"murasame/internal/status"
)

const MaxBody = 4 << 20

const LogChunk = 64 << 10

var (
	WriteTimeout    = time.Minute
	LogWriteTimeout = 30 * time.Second
)

var outMu sync.Mutex

func SignatureOK(secret string, body []byte, sig string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := []byte(hex.EncodeToString(mac.Sum(nil)))
	got := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(sig)), "sha256=")
	return subtle.ConstantTimeCompare(want, []byte(got)) == 1
}

const MaxLine = 64 << 10

type PrefixStream struct {
	Out  io.Writer
	Name string
	buf  []byte
	cr   bool
	mu   sync.Mutex
}

func (p *PrefixStream) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var sb strings.Builder
	rest := b
	for len(rest) > 0 {
		if p.cr {
			p.cr = false
			if rest[0] == '\n' {
				rest = rest[1:]
				continue
			}
		}
		i := bytes.IndexAny(rest, "\r\n")
		chunk := rest
		if i >= 0 {
			chunk = rest[:i]
		}
		for len(p.buf)+len(chunk) > MaxLine {
			take := MaxLine - len(p.buf)
			for take > 0 && take < len(chunk) && !utf8.RuneStart(chunk[take]) {
				take--
			}
			if take == 0 && len(p.buf) == 0 {
				take = MaxLine - len(p.buf)
			}
			p.buf = append(p.buf, chunk[:take]...)
			p.line(&sb)
			chunk = chunk[take:]
		}
		p.buf = append(p.buf, chunk...)
		if i < 0 {
			break
		}
		p.line(&sb)
		p.cr = rest[i] == '\r'
		rest = rest[i+1:]
	}
	p.emit(sb.String())
	return len(b), nil
}

func (p *PrefixStream) line(sb *strings.Builder) {
	fmt.Fprintf(sb, "[%s] %s\n", p.Name, p.buf)
	p.buf = p.buf[:0]
}

func (p *PrefixStream) emit(text string) {
	if text == "" {
		return
	}
	outMu.Lock()
	io.WriteString(p.Out, text)
	outMu.Unlock()
}

func (p *PrefixStream) Flush() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf) == 0 {
		return nil
	}
	var sb strings.Builder
	p.line(&sb)
	p.emit(sb.String())
	return nil
}

func (p *PrefixStream) Close() error { return p.Flush() }

type Worker struct {
	Service  *service.Service
	Interval time.Duration
	wake     chan struct{}
	stopping chan struct{}
	done     chan struct{}
	ticks    int64
	mu       sync.Mutex
	state    *states.Tracker[states.Worker, states.WorkerEvent]
	run      func() error
}

func NewWorker(svc *service.Service, interval time.Duration) *Worker {
	return &Worker{Service: svc, Interval: interval, wake: make(chan struct{}, 1), stopping: make(chan struct{}),
		done: make(chan struct{}), state: states.WorkerMachine.Start(), run: svc.Tick}
}

func (w *Worker) Ticks() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ticks
}

func (w *Worker) State() states.Worker {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state.State()
}

func (w *Worker) fire(ev states.WorkerEvent) (from, to states.Worker, ok bool) {
	from = w.state.State()
	if w.state.Fire(ev) != nil {
		return from, from, false
	}
	return from, w.state.State(), true
}

func (w *Worker) Wake() {
	w.Service.Nudge()
	w.signal()
}

func (w *Worker) signal() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if from, to, ok := w.fire(states.WorkerWake); ok && from == states.WorkerIdle && to == states.WorkerWoken {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

func (w *Worker) start() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _, ok := w.fire(states.WorkerStart)
	return ok
}

func (w *Worker) begin() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case <-w.wake:
	default:
	}
	_, _, ok := w.fire(states.WorkerBegin)
	return ok
}

func (w *Worker) ticked() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ticks++
	w.fire(states.WorkerTicked)
}

func (w *Worker) timer() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fire(states.WorkerTimer)
}

func (w *Worker) woken() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state.State() == states.WorkerWoken
}

func (w *Worker) loop() {
	defer w.exit()
	for w.begin() {
		if err := w.tick(); err != nil {
			w.Service.Say(fmt.Sprintf("tick failed: %v", err))
		}
		w.ticked()
		if w.woken() {
			continue
		}
		t := time.NewTimer(w.Interval)
		select {
		case <-w.wake:
		case <-w.stopping:
		case <-t.C:
			w.timer()
		}
		t.Stop()
	}
}

func (w *Worker) exit() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state.Must(states.WorkerExit)
	close(w.done)
}

func (w *Worker) tick() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return w.run()
}

func (w *Worker) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	from, to, ok := w.fire(states.WorkerStop)
	if !ok || from == states.WorkerStopping {
		return
	}
	close(w.stopping)
	if to == states.WorkerJoined {
		close(w.done)
	}
}

type Runtime struct {
	Services map[string]*service.Service
	Order    []string
	Workers  map[string]*Worker
	mu       sync.Mutex
	state    *states.Tracker[states.Runtime, states.RuntimeEvent]
}

func NewRuntime(services []*service.Service, interval time.Duration) *Runtime {
	rt := &Runtime{Services: map[string]*service.Service{}, Workers: map[string]*Worker{},
		state: states.RuntimeMachine.Start()}
	for _, s := range services {
		rt.Services[s.Name] = s
		rt.Order = append(rt.Order, s.Name)
		rt.Workers[s.Name] = NewWorker(s, interval)
	}
	return rt
}

func (rt *Runtime) List() []*service.Service {
	out := make([]*service.Service, len(rt.Order))
	for i, n := range rt.Order {
		out[i] = rt.Services[n]
	}
	return out
}

func (rt *Runtime) Ticks() int64 {
	var n int64
	for _, w := range rt.Workers {
		n += w.Ticks()
	}
	return n
}

func (rt *Runtime) Wake(names []string) {
	if names == nil {
		names = rt.Order
	}
	for _, n := range names {
		if w, ok := rt.Workers[n]; ok {
			w.Wake()
		}
	}
}

func (rt *Runtime) Route(body []byte) []string {
	var hook struct {
		Repository struct {
			FullName string `json:"full_name"`
			HTMLURL  string `json:"html_url"`
			CloneURL string `json:"clone_url"`
		} `json:"repository"`
	}
	if json.Unmarshal(body, &hook) != nil {
		return nil
	}
	repo := hook.Repository
	full := repo.FullName
	if full == "" {
		return nil
	}
	hits := []string{}
	for _, n := range rt.Order {
		s := rt.Services[n]
		if strings.EqualFold(s.FJ.Owner+"/"+s.FJ.Repo, full) {
			hits = append(hits, n)
		}
	}
	if len(hits) > 1 {
		u := repo.HTMLURL
		if u == "" {
			u = repo.CloneURL
		}
		host := db.ForgeHost(u)
		var same []string
		for _, n := range hits {
			if rt.Services[n].Host == host {
				same = append(same, n)
			}
		}
		if len(same) > 0 {
			hits = same
		}
	}
	return hits
}

func (rt *Runtime) State() states.Runtime {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.state.State()
}

func (rt *Runtime) fire(ev states.RuntimeEvent) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.state.Fire(ev)
}

func (rt *Runtime) Start() error {
	if err := rt.fire(states.RuntimeStart); err != nil {
		return err
	}
	for _, n := range rt.Order {
		if w := rt.Workers[n]; w.start() {
			go w.loop()
		}
	}
	return nil
}

func (rt *Runtime) Stop() {
	rt.fire(states.RuntimeStop)
	for _, w := range rt.Workers {
		w.Stop()
	}
}

func (rt *Runtime) Join() error {
	rt.mu.Lock()
	if !rt.state.Can(states.RuntimeJoin) {
		defer rt.mu.Unlock()
		return rt.state.Fire(states.RuntimeJoin)
	}
	rt.mu.Unlock()
	for _, w := range rt.Workers {
		<-w.done
	}
	return rt.fire(states.RuntimeJoin)
}

func Loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host, _, _ = strings.Cut(host, "%")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var ForgeHeaders = []string{"X-Forgejo-Event", "X-Gitea-Event", "X-Gogs-Event", "X-GitHub-Event", "X-Forgejo-Delivery",
	"X-Gitea-Delivery"}

func FromForge(h http.Header) bool {
	for _, k := range ForgeHeaders {
		if _, ok := h[http.CanonicalHeaderKey(k)]; ok {
			return true
		}
	}
	return false
}

func hostName(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	return strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
}

func LoopbackHost(hostport string) bool {
	host := hostName(hostport)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func ExternalHosts(services []*service.Service) map[string]bool {
	out := map[string]bool{}
	for _, s := range services {
		if s.FJ.ExternalURL == "" {
			continue
		}
		if u, err := url.Parse(s.FJ.ExternalURL); err == nil && u.Host != "" {
			out[hostName(u.Host)] = true
		}
	}
	return out
}

func HostAllowed(hostport string, external map[string]bool) bool {
	if hostport == "" {
		return false
	}
	return LoopbackHost(hostport) || external[hostName(hostport)]
}

type recorder struct {
	http.ResponseWriter
	code int
}

func (r *recorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func Handler(rt *Runtime, secret string) http.Handler {
	say := InstanceSay(rt.List())
	external := ExternalHosts(rt.List())
	send := func(w http.ResponseWriter, code int, body []byte, ctype string, headers map[string]string) {
		if ctype == "" {
			ctype = "text/plain; charset=utf-8"
		}
		h := w.Header()
		h.Set("Content-Type", ctype)
		h.Set("Content-Length", strconv.Itoa(len(body)))
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(ctype, "text/html") {
			h.Set("Content-Security-Policy", status.CSP)
		}
		for k, v := range headers {
			h.Set(k, v)
		}
		w.WriteHeader(code)
		w.Write(body)
	}
	sendJSON := func(w http.ResponseWriter, obj any, code int) {
		text, err := json.MarshalIndent(obj, "", "  ")
		if err != nil {
			text, code = []byte(`{"error": "unencodable response"}`), http.StatusInternalServerError
		}
		send(w, code, text, "application/json; charset=utf-8", nil)
	}
	sendLog := func(w http.ResponseWriter, svc *service.Service, name string) {
		if !status.LogName.MatchString(name) {
			send(w, 404, []byte("not found\n"), "", nil)
			return
		}
		f, fi, err := safefile.OpenRead(filepath.Join(svc.StateDir(), name))
		if err != nil {
			send(w, 404, []byte("not found\n"), "", nil)
			return
		}
		defer f.Close()
		left := fi.Size()
		h := w.Header()
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Content-Length", strconv.FormatInt(left, 10))
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		rc := http.NewResponseController(w)
		rc.SetWriteDeadline(time.Now().Add(LogWriteTimeout))
		w.WriteHeader(200)
		buf := make([]byte, LogChunk)
		for left > 0 {
			n, err := f.Read(buf[:min(int64(LogChunk), left)])
			if n > 0 {
				rc.SetWriteDeadline(time.Now().Add(LogWriteTimeout))
				if _, werr := w.Write(buf[:n]); werr != nil {
					break
				}
				left -= int64(n)
			}
			if err != nil {
				break
			}
		}
	}
	get := func(w http.ResponseWriter, r *http.Request) {
		if !Loopback(r.RemoteAddr) {
			send(w, 403, []byte("the status page answers loopback clients only\n"), "", nil)
			return
		}
		if FromForge(r.Header) {
			send(w, 403, []byte("the status page does not answer the forge's webhook deliveries\n"), "", nil)
			return
		}
		if !HostAllowed(r.Host, external) {
			send(w, 403, []byte("the status page answers only for a loopback host name or the external_url's host\n"), "",
				nil)
			return
		}
		var parts []string
		if trimmed := strings.Trim(r.URL.EscapedPath(), "/"); trimmed != "" {
			for _, x := range strings.Split(trimmed, "/") {
				parts = append(parts, urlUnquote(x))
			}
		}
		defer func() {
			if rec := recover(); rec != nil {
				say(fmt.Sprintf("status page: %v", rec))
				send(w, 500, []byte("status page error (see the service's log)\n"), "", nil)
			}
		}()
		switch {
		case len(parts) == 0:
			send(w, 200, []byte(status.Render(status.View(rt.List()))), "text/html; charset=utf-8", nil)
			return
		case len(parts) == 2 && parts[0] == "api" && parts[1] == "status":
			sendJSON(w, status.View(rt.List()), 200)
			return
		case len(parts) == 4 && parts[0] == "repos" && parts[2] == "logs":
			if svc, ok := rt.Services[parts[1]]; ok {
				sendLog(w, svc, parts[3])
				return
			}
		}
		send(w, 404, []byte("not found\n"), "", nil)
	}
	post := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/webhook" {
			send(w, 404, []byte("not found\n"), "", nil)
			return
		}
		if r.ContentLength > MaxBody {
			send(w, 413, []byte("too large\n"), "", nil)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				send(w, 413, []byte("too large\n"), "", nil)
			} else {
				send(w, 400, []byte("could not read the body\n"), "", nil)
			}
			return
		}
		if secret != "" {
			sig := r.Header.Get("X-Forgejo-Signature")
			if sig == "" {
				sig = r.Header.Get("X-Gitea-Signature")
			}
			if !SignatureOK(secret, body, sig) {
				send(w, 403, []byte("bad signature\n"), "", nil)
				return
			}
		}
		names := rt.Route(body)
		if names != nil && len(names) == 0 {
			send(w, 202, []byte("not a repository served here\n"), "", nil)
			return
		}
		rt.Wake(names)
		send(w, 202, []byte("queued a sync\n"), "", nil)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "murasame")
		rec := &recorder{ResponseWriter: w, code: 200}
		switch {
		case r.URL.EscapedPath() == "/healthz":
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				send(rec, 200, []byte("ok\n"), "", nil)
			} else {
				send(rec, 405, []byte("method not allowed\n"), "", map[string]string{"Allow": "GET, HEAD"})
			}
		case r.Method == http.MethodGet:
			get(rec, r)
		case r.Method == http.MethodPost:
			post(rec, r)
		default:
			send(rec, 501, []byte("unsupported method\n"), "", nil)
		}
		say(fmt.Sprintf("http: \"%s %s %s\" %d -", r.Method, r.URL.RequestURI(), r.Proto, rec.code))
	})
}

func urlUnquote(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return strings.ToValidUTF8(b.String(), "�")
}

func Start(services []*service.Service, listen, secret string, interval time.Duration) (*http.Server, net.Listener, *Runtime, error) {
	rt := NewRuntime(services, interval)
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		i := strings.LastIndex(listen, ":")
		if i < 0 {
			return nil, nil, nil, fmt.Errorf("listen: %s: not host:port", listen)
		}
		host, port = listen[:i], listen[i+1:]
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, nil, nil, err
	}
	srv := &http.Server{Handler: Handler(rt, secret),
		ReadHeaderTimeout: 30 * time.Second, ReadTimeout: 2 * time.Minute, WriteTimeout: WriteTimeout, IdleTimeout: 2 * time.Minute}
	go srv.Serve(ln)
	if err := rt.Start(); err != nil {
		srv.Close()
		return nil, nil, nil, err
	}
	return srv, ln, rt, nil
}

func ServicesOf(spec *instance.Spec, env config.Env, stream io.Writer) ([]*service.Service, error) {
	if stream == nil {
		stream = os.Stdout
	}
	var out []*service.Service
	for _, e := range spec.Repos {
		var st io.Writer = stream
		if spec.Multi {
			st = &PrefixStream{Out: stream, Name: e.Name}
		}
		s, err := service.New(e.Cfg, e.Clone, service.Options{Env: env, Name: e.Name, Stream: st})
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

var servicesOf = ServicesOf

func Serve(spec *instance.Spec, once bool) int {
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sig)
	services, err := servicesOf(spec, nil, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "murasame: %s\n", err)
		return 2
	}
	defer flushStreams(services)
	say := InstanceSay(services)
	secretenv.HardenProcess()
	if once {
		return TickUntilStopped(services, sig, say, func() { os.Exit(1) })
	}
	secret := os.Getenv(spec.WebhookSecretEnv)
	if secret == "" {
		say(fmt.Sprintf("WARNING: no webhook secret ($%s): any POST /webhook wakes the workers (they only re-read the "+
			"API, but set a secret)", spec.WebhookSecretEnv))
	}
	srv, _, rt, err := Start(services, spec.Listen, secret, time.Duration(spec.PollInterval)*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "murasame: %s\n", err)
		return 1
	}
	var names []string
	for _, s := range services {
		names = append(names, s.Name)
	}
	what := "repositories"
	if len(services) == 1 {
		what = "repository"
	}
	say(fmt.Sprintf("listening on %s; %d %s (%s); polling every %d s; gates run in Forgejo Actions; status page: "+
		"loopback clients only", spec.Listen, len(services), what, strings.Join(names, ", "), spec.PollInterval))
	<-sig
	say("stopping: no new batch starts; waiting for the current ticks (a second signal stops at once)")
	if Shutdown(srv, rt, sig) {
		fmt.Fprintln(os.Stderr, "stopped")
		return 0
	}
	fmt.Fprintln(os.Stderr, "stopped at once (a second signal): a CI run may still be going on the staging branch")
	return 1
}

func Shutdown(srv *http.Server, rt *Runtime, again <-chan os.Signal) bool {
	for _, svc := range rt.Services {
		svc.Stop(instance.Closing)
	}
	rt.Stop()
	joined := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		srv.Shutdown(ctx)
		cancel()
		rt.Join()
		close(joined)
	}()
	select {
	case <-joined:
		return true
	case <-again:
		return false
	}
}

func InstanceSay(services []*service.Service) func(string) {
	if len(services) == 0 {
		return func(string) {}
	}
	ps, ok := services[0].Stream.(*PrefixStream)
	if !ok {
		return services[0].Say
	}
	return func(msg string) {
		outMu.Lock()
		defer outMu.Unlock()
		fmt.Fprintf(ps.Out, "[murasame] %s\n", msg)
	}
}

func TickUntilStopped(services []*service.Service, sig <-chan os.Signal, say func(string),
	exit func()) int {
	var stopped atomic.Bool
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-sig:
		case <-done:
			return
		}
		stopped.Store(true)
		say("stopping: the batch waiting on CI is stopped and requeued (a second signal stops at once)")
		for _, svc := range services {
			svc.Stop(instance.Closing)
		}
		select {
		case <-sig:
			fmt.Fprintln(os.Stderr, "stopped at once (a second signal): a CI run may still be going on the staging branch")
			exit()
		case <-done:
		}
	}()
	code := TickOnce(services)
	close(done)
	<-finished
	if stopped.Load() {
		fmt.Fprintln(os.Stderr, "stopped")
		return 1
	}
	return code
}

func TickOnce(services []*service.Service) int {
	code := 0
	for _, s := range services {
		if s.Stopping() != "" {
			continue
		}
		if err := s.Tick(); err != nil {
			s.Say(fmt.Sprintf("tick failed: %v", err))
			code = 1
		}
	}
	return code
}

func flushStreams(services []*service.Service) {
	for _, s := range services {
		if f, ok := s.Stream.(interface{ Flush() error }); ok {
			f.Flush()
		}
	}
}

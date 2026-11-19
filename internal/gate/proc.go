package gate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"murasame/internal/gitx"
	"murasame/internal/secretenv"
	"murasame/internal/states"
)

const TimeoutCode = 124

const CancelCode = 125

const KeptLines = 50

const MaxLine = 64 << 10

var waits = struct {
	term, kill, settle, drain, drainAfterKill, echo time.Duration
}{2 * time.Second, 5 * time.Second, 2 * time.Second, 10 * time.Second, 5 * time.Second, 5 * time.Second}

const EchoQueue = 1024

type Status struct {
	State       states.AttemptState
	Code        int
	TimedOut    bool
	Cancelled   bool
	StartFailed bool
	Survivors   []int
	Unconfirmed string
}

func endStatus(at *states.Tracker[states.AttemptState, states.AttemptEvent], code int, survivors []int,
	unconfirmed string) Status {
	st := Status{State: at.State(), Code: code, Survivors: survivors, Unconfirmed: unconfirmed}
	switch states.AttemptOutcome(st.State) {
	case states.AttemptStartFailed:
		st.StartFailed, st.Code = true, 127
	case states.AttemptTimedOut:
		st.TimedOut, st.Code = true, TimeoutCode
	case states.AttemptCancelled:
		st.Cancelled, st.Code = true, CancelCode
	}
	return st
}

func cleanupEvent(survivors []int, unconfirmed string) states.AttemptEvent {
	switch {
	case len(survivors) > 0:
		return states.AttemptSurvivors
	case unconfirmed != "":
		return states.AttemptUnconfirmed
	}
	return states.AttemptClean
}

func (s Status) Clean() bool { return len(s.Survivors) == 0 && s.Unconfirmed == "" }

func (s Status) Verdict() states.Verdict { return states.GateVerdict(s.State, s.Code) }

func (s Status) CommandVerdict() states.Verdict { return states.CommandVerdict(s.State, s.Code) }

func (s Status) Leftover() string {
	switch {
	case len(s.Survivors) > 0 && s.Unconfirmed != "":
		return fmt.Sprintf("process group(s) %s that could not be killed (and %s)", Groups(s.Survivors), s.Unconfirmed)
	case len(s.Survivors) > 0:
		return fmt.Sprintf("process group(s) %s that could not be killed", Groups(s.Survivors))
	case s.Unconfirmed != "":
		return "processes that may still run: " + s.Unconfirmed
	}
	return ""
}

const (
	heldWhy = "a process that could not be found still holds its output open"
)

func splitLines(r io.Reader, max int, emit func([]byte) bool) {
	buf := make([]byte, 0, max+32<<10)
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		start := 0
		for {
			rest := buf[start:]
			i := bytes.IndexByte(rest, '\n')
			if i >= 0 && i <= max {
				if !emit(rest[:i]) {
					return
				}
				start += i + 1
				continue
			}
			if len(rest) <= max {
				break
			}
			cut := max
			for k := 0; k < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(rest[cut]); k++ {
				cut--
			}
			if cut == 0 || !utf8.RuneStart(rest[cut]) {
				cut = max
			}
			if !emit(rest[:cut]) {
				return
			}
			start += cut
		}
		buf = append(buf[:0], buf[start:]...)
		if err != nil {
			if len(buf) > 0 {
				emit(buf)
			}
			return
		}
	}
}

type StreamOpts struct {
	Cwd       string
	Env       map[string]string
	Log       string
	Append    bool
	Echo      func(string)
	Timeout   time.Duration
	Poll      func() bool
	PollEvery time.Duration
	Prefix    []string
	Reap      bool
	Kill      func(pid int, sig syscall.Signal) error
	Direct    bool
}

func wrap(argv []string, o StreamOpts) ([]string, string, error) {
	if len(o.Prefix) == 0 || len(argv) == 0 || o.Direct {
		return append(append([]string{}, o.Prefix...), argv...), "", nil
	}
	if execHelper == "" {
		return o.Prefix, "", errors.New("murasame's own executable, which runs the command inside the command_prefix, " +
			"cannot be found")
	}
	nonce := newNonce()
	inner := append([]string{execHelper, ExecArg, nonce, "--"}, argv...)
	return append(append([]string{}, o.Prefix...), inner...), nonce, nil
}

func exitFirst(exited <-chan struct{}, exitAt *time.Time, fired time.Time) bool {
	select {
	case <-exited:
		return !exitAt.After(fired)
	default:
		return false
	}
}

func RunStreamed(argv []string, o StreamOpts) (Status, []string) {
	at := states.AttemptMachine.Start()
	env := secretenv.ChildEnv(o.Env)
	argv, nonce, wrapErr := wrap(argv, o)
	if o.PollEvery <= 0 {
		o.PollEvery = time.Second
	}
	notStarted := func(err error) (Status, []string) {
		msg := fmt.Sprintf("cannot run %s: %s", first(argv), err)
		if o.Echo != nil {
			o.Echo(msg)
		}
		at.Must(states.AttemptSpawnFailed)
		at.Must(states.AttemptClean)
		return endStatus(at, 127, nil, ""), []string{msg}
	}
	if err := ensureLogDir(filepath.Dir(o.Log)); err != nil {
		return notStarted(err)
	}
	flags := os.O_CREATE | os.O_WRONLY
	if o.Append {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	lf, err := openLog(o.Log, flags)
	if err != nil {
		return notStarted(err)
	}
	defer lf.Close()
	fail := func(err error) (Status, []string) {
		st, lines := notStarted(err)
		lf.WriteString(lines[0] + "\n")
		return st, lines
	}
	if wrapErr != nil {
		return fail(wrapErr)
	}
	if len(argv) == 0 {
		return fail(errors.New("no command"))
	}
	program, err := findProgram(argv[0], env, o.Cwd)
	if err != nil {
		return fail(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	cmd := exec.Command(program, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Dir = o.Cwd
	cmd.Env = env
	cmd.Stdout = pw
	cmd.Stderr = pw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return fail(err)
	}
	at.Must(states.AttemptSpawned)
	pid := cmd.Process.Pid
	pw.Close()

	var mu sync.Mutex
	var lines []string
	closed := false
	execFailed := false
	marker := execMarker(nonce)
	ec := startEcho(o.Echo)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		splitLines(pr, MaxLine, func(raw []byte) bool {
			chunk := strings.TrimSuffix(string(raw), "\r")
			for _, line := range strings.Split(chunk, "\r") {
				line = strings.ToValidUTF8(line, "�")
				mu.Lock()
				if closed {
					mu.Unlock()
					return false
				}
				if nonce != "" && strings.HasPrefix(line, marker) {
					execFailed = true
				}
				if len(lines) == KeptLines {
					copy(lines, lines[1:])
					lines = lines[:KeptLines-1]
				}
				lines = append(lines, line)
				lf.WriteString(line + "\n")
				ec.send(line)
				mu.Unlock()
			}
			return true
		})
	}()
	exited := make(chan struct{})
	var waitErr error
	var exitAt time.Time
	go func() {
		waitErr = cmd.Wait()
		exitAt = time.Now()
		close(exited)
	}()

	var deadline <-chan time.Time
	if o.Timeout > 0 {
		t := time.NewTimer(o.Timeout)
		defer t.Stop()
		deadline = t.C
	}
	tk := time.NewTicker(o.PollEvery)
	defer tk.Stop()
	poll := func() bool {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			left := killGroup(pid, exited, o)
			held := false
			select {
			case <-readerDone:
			case <-time.After(waits.drainAfterKill):
				held = true
			}
			mu.Lock()
			closed = true
			mu.Unlock()
			pr.Close()
			ec.finish()
			var why []string
			if len(left) > 0 {
				why = append(why, fmt.Sprintf("process group(s) %s could not be killed", Groups(left)))
			}
			if held {
				why = append(why, heldWhy)
			}
			if len(why) == 0 {
				panic(r)
			}
			if err, ok := r.(error); ok {
				panic(fmt.Errorf("%w (and %s)", err, strings.Join(why, "; ")))
			}
			panic(fmt.Errorf("%v (and %s)", r, strings.Join(why, "; ")))
		}()
		return o.Poll()
	}
	var ended states.AttemptEvent
loop:
	for {
		select {
		case <-exited:
			ended = states.AttemptExit
			break loop
		case fired := <-deadline:
			ended = states.AttemptDeadline
			if exitFirst(exited, &exitAt, fired) {
				ended = states.AttemptExit
			}
			break loop
		case <-tk.C:
			if o.Poll != nil && poll() {
				ended = states.AttemptCancel
				if exitFirst(exited, &exitAt, time.Now()) {
					ended = states.AttemptExit
				}
				break loop
			}
		}
	}
	code := 0
	var survivors []int
	switch ended {
	case states.AttemptExit:
		code = gitx.ExitCode(cmd, waitErr)
		if o.Reap {
			survivors = killGroup(pid, exited, o)
		}
	default:
		survivors = killGroup(pid, exited, o)
	}
	held := false
	select {
	case <-readerDone:
	case <-time.After(waits.drain):
		for _, g := range killGroup(pid, exited, o) {
			if !slices.Contains(survivors, g) {
				survivors = append(survivors, g)
			}
		}
		select {
		case <-readerDone:
		case <-time.After(waits.drainAfterKill):
			held = true
		}
	}
	unconfirmed := ""
	if held {
		unconfirmed = heldWhy
	}
	mu.Lock()
	closed = true
	got := append([]string{}, lines...)
	if ended == states.AttemptExit && code == ExecFailedCode && execFailed {
		ended = states.AttemptExecFailed
	}
	mu.Unlock()
	pr.Close()
	ec.finish()
	at.Must(ended)
	at.Must(cleanupEvent(survivors, unconfirmed))
	return endStatus(at, code, survivors, unconfirmed), got
}

type echoer struct {
	echo      func(string)
	q         chan string
	done      chan struct{}
	dropped   int
	abandoned atomic.Bool
	once      sync.Once
}

func startEcho(echo func(string)) *echoer {
	e := &echoer{echo: echo}
	if echo == nil {
		return e
	}
	e.q = make(chan string, EchoQueue)
	e.done = make(chan struct{})
	go func() {
		defer close(e.done)
		for line := range e.q {
			if !e.abandoned.Load() {
				e.echo(line)
			}
		}
	}()
	return e
}

func skippedNote(n int) string {
	return fmt.Sprintf("-- %d line(s) of output not shown here; the log has them all", n)
}

func (e *echoer) offer(line string) bool {
	select {
	case e.q <- line:
		return true
	default:
		return false
	}
}

func (e *echoer) send(line string) {
	if e.q == nil {
		return
	}
	if e.dropped > 0 {
		if !e.offer(skippedNote(e.dropped)) {
			e.dropped++
			return
		}
		e.dropped = 0
	}
	if !e.offer(line) {
		e.dropped++
	}
}

func (e *echoer) finish() {
	if e.q == nil {
		return
	}
	e.once.Do(func() {
		deadline := time.NewTimer(waits.echo)
		defer deadline.Stop()
		if e.dropped > 0 {
			select {
			case e.q <- skippedNote(e.dropped):
			case <-deadline.C:
				e.abandoned.Store(true)
			}
		}
		close(e.q)
		if e.abandoned.Load() {
			return
		}
		select {
		case <-e.done:
		case <-deadline.C:
			e.abandoned.Store(true)
		}
	})
}

func first(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return argv[0]
}

func newNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (o StreamOpts) signal(g int, sig syscall.Signal) error {
	kill := o.Kill
	if kill == nil {
		kill = syscall.Kill
	}
	err := kill(-g, sig)
	if err == syscall.EPERM && sig != 0 && len(o.Prefix) > 0 && prefixKill(o.Prefix, g, sig) {
		return nil
	}
	return err
}

func (o StreamOpts) alive(g int) bool {
	kill := o.Kill
	if kill == nil {
		kill = syscall.Kill
	}
	err := kill(-g, 0)
	return err == nil || err == syscall.EPERM
}

func prefixKill(prefix []string, g int, sig syscall.Signal) bool {
	name := "TERM"
	if sig == syscall.SIGKILL {
		name = "KILL"
	}
	argv := append(append([]string{}, prefix...), "kill", "-s", name, "--", "-"+strconv.Itoa(g))
	env := secretenv.ChildEnv(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	program, err := findProgram(argv[0], env, "")
	if err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, program, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Env = env
	return cmd.Run() == nil
}

func killGroup(pid int, exited <-chan struct{}, o StreamOpts) []int {
	for _, step := range []struct {
		sig  syscall.Signal
		wait time.Duration
	}{{syscall.SIGTERM, waits.term}, {syscall.SIGKILL, waits.kill}} {
		if err := o.signal(pid, step.sig); err == syscall.ESRCH {
			return nil
		}
		select {
		case <-exited:
		case <-time.After(step.wait):
		}
		for end := time.Now().Add(step.wait); o.alive(pid) && time.Now().Before(end); {
			time.Sleep(50 * time.Millisecond)
		}
		if !o.alive(pid) {
			return nil
		}
	}
	for end := time.Now().Add(waits.settle); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if !o.alive(pid) {
			return nil
		}
	}
	return []int{pid}
}

package reporter

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"murasame/internal/store"
)

const LogName = "merges.log"

func FreshLog(dir, stem string) string {
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, stem+".log")
	for n := 2; ; n++ {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
		if err == nil {
			f.Close()
			return p
		}
		if !exists(p) {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s-%d.log", stem, n))
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func OpenAppend(p string) (*os.File, error) {
	return store.OpenRegular(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

type History func(kind, branches, line, at string)

type Reporter struct {
	StateDir string
	Stream   io.Writer
	Sink     func(string)
	History  History
	mu       sync.Mutex
	broken   bool
}

func New(stateDir string, stream io.Writer, sink func(string), history History) *Reporter {
	if stream == nil {
		stream = os.Stdout
	}
	return &Reporter{StateDir: stateDir, Stream: stream, Sink: sink, History: history}
}

func (r *Reporter) Path() string { return filepath.Join(r.StateDir, LogName) }

func (r *Reporter) Say(line string) {
	r.mu.Lock()
	if !r.broken {
		if _, err := fmt.Fprintln(r.Stream, line); errors.Is(err, syscall.EPIPE) {
			r.broken = true
		}
	}
	r.mu.Unlock()
	if r.Sink != nil {
		r.Sink(line)
	}
}

func plain(c rune) bool { return unicode.IsGraphic(c) && !unicode.IsSpace(c) && c != '"' && c != '=' }

func value(v string) string {
	if v == "" || !utf8.ValidString(v) || strings.IndexFunc(v, func(c rune) bool { return !plain(c) }) >= 0 {
		return strconv.Quote(v)
	}
	return v
}

func (r *Reporter) Record(event, branches string, fields ...string) string {
	at := time.Now()
	var b strings.Builder
	b.WriteString(at.Format("2006-01-02 15:04:05") + " " + event)
	for i := 0; i+1 < len(fields); i += 2 {
		b.WriteString(" " + fields[i] + "=" + value(fields[i+1]))
	}
	line := b.String()
	if err := r.appendLine(line); err != nil {
		r.Say(fmt.Sprintf("-- WARNING: could not write %s: %v", r.Path(), err))
	}
	if r.History != nil {
		r.History(event, branches, line, at.Format("2006-01-02 15:04:05"))
	}
	return line
}

func (r *Reporter) appendLine(line string) error {
	if err := os.MkdirAll(r.StateDir, 0o755); err != nil {
		return err
	}
	f, err := OpenAppend(r.Path())
	if err != nil {
		return err
	}
	_, werr := f.WriteString(line + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

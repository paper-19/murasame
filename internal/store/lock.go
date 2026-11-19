package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"murasame/internal/safefile"
	"murasame/internal/states"
)

type LockTimeout struct{ Msg string }

func (e *LockTimeout) Error() string { return e.Msg }

type UnsafeFile struct {
	Path string
	Why  string
}

func (e *UnsafeFile) Error() string { return fmt.Sprintf("%s %s: refusing to use it", e.Path, e.Why) }

func OpenRegular(path string, flags int, perm os.FileMode) (*os.File, error) {
	f, err := safefile.Open(path, flags, perm)
	var refused *safefile.Refused
	if !errors.As(err, &refused) {
		return f, err
	}
	switch refused.Reason {
	case safefile.ErrSymlink:
		return nil, fmt.Errorf("%s is a symbolic link: refusing to write through it", path)
	case safefile.ErrHardLinked:
		return nil, &UnsafeFile{path, fmt.Sprintf("has %d hard links", refused.Links)}
	}
	return nil, &UnsafeFile{path, refused.Reason.Error()}
}

type QueueLock struct {
	Path  string
	Owner string
	fd    *os.File
	dir   *os.File
	state *states.Tracker[states.Lock, states.LockEvent]
	line  string
}

func NewQueueLock(stateDir string) *QueueLock {
	return &QueueLock{Path: filepath.Join(stateDir, ".lock"), Owner: filepath.Join(stateDir, ".lock.owner"),
		state: states.LockMachine.Start()}
}

func (l *QueueLock) State() states.Lock { return l.state.State() }

func (l *QueueLock) Holder() string {
	if l.state.State() == states.LockHeld {
		return l.line
	}
	f, err := OpenRegular(l.Owner, os.O_RDONLY, 0)
	if err != nil {
		return l.Path
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4<<10))
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return l.Path
	}
	return strings.TrimSpace(string(b))
}

func (l *QueueLock) drop() {
	if l.fd != nil {
		l.fd.Close()
		l.fd = nil
	}
	if l.dir != nil {
		l.dir.Close()
		l.dir = nil
	}
}

func (l *QueueLock) lockDir() (bool, error) {
	path := filepath.Dir(l.Path)
	d, err := os.Open(path)
	if err != nil {
		return false, err
	}
	if fi, err := d.Stat(); err != nil || !fi.IsDir() {
		d.Close()
		if err == nil {
			err = fmt.Errorf("%s is not a directory", path)
		}
		return false, err
	}
	if err := syscall.Flock(int(d.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		d.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN || err == syscall.EINTR {
			return false, nil
		}
		return false, fmt.Errorf("cannot lock %s: %w", path, err)
	}
	l.dir = d
	return true, nil
}

func (l *QueueLock) refuse(err error) error {
	l.drop()
	l.state.Must(states.LockRefuse)
	return err
}

func (l *QueueLock) names() (bool, error) {
	lst, err := os.Lstat(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fi, err := l.fd.Stat()
	if err != nil {
		return false, err
	}
	return os.SameFile(lst, fi), nil
}

func (l *QueueLock) try() (bool, error) {
	for attempt := 0; attempt < 10; attempt++ {
		if l.fd == nil {
			fd, err := OpenRegular(l.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
			if err != nil {
				return false, err
			}
			l.fd = fd
		}
		if err := syscall.Flock(int(l.fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN || err == syscall.EINTR {
				return false, nil
			}
			return false, fmt.Errorf("cannot lock %s: %w", l.Path, err)
		}
		same, err := l.names()
		if err != nil {
			return false, err
		}
		if same {
			held, err := l.lockDir()
			if err != nil {
				l.drop()
				return false, err
			}
			if held {
				return true, nil
			}
			l.drop()
			return false, nil
		}
		l.drop()
	}
	return false, fmt.Errorf("%s keeps being replaced: refusing to use it", l.Path)
}

func (l *QueueLock) Acquire(wait int, args string, say func(string)) error {
	if s := l.state.State(); s == states.LockTimedOut || s == states.LockRefused {
		l.state.Must(states.LockRetry)
	}
	if l.state.State() != states.LockFree {
		return l.state.Fire(states.LockTake)
	}
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
		return l.refuse(err)
	}
	got, err := l.try()
	if err != nil {
		return l.refuse(err)
	}
	if !got {
		l.state.Must(states.LockBusy)
		limit := ""
		if wait > 0 {
			limit = fmt.Sprintf(" (at most %d s)", wait)
		}
		say(fmt.Sprintf("-- another merge queue is running (%s): waiting for it to finish%s", l.Holder(), limit))
		t0 := time.Now()
		for !got {
			if wait > 0 && time.Since(t0) >= time.Duration(wait)*time.Second {
				l.drop()
				l.state.Must(states.LockExpire)
				return &LockTimeout{fmt.Sprintf("another merge queue still holds %s after %d s: not started", l.Path, wait)}
			}
			time.Sleep(200 * time.Millisecond)
			if got, err = l.try(); err != nil {
				return l.refuse(err)
			}
		}
		say("-- the other merge queue finished: starting")
	}
	l.line = fmt.Sprintf("pid %d since %s: %s", os.Getpid(), time.Now().Format("2006-01-02 15:04:05"), args)
	l.state.Must(states.LockTake)
	f, err := OpenRegular(l.Owner, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return l.refuse(err)
	}
	_, err = fmt.Fprintln(f, l.line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return l.refuse(err)
	}
	return nil
}

func (l *QueueLock) Release() {
	if !l.state.Can(states.LockRelease) {
		return
	}
	if f, err := OpenRegular(l.Owner, os.O_WRONLY|os.O_TRUNC, 0); err == nil {
		f.Close()
	}
	l.drop()
	l.state.Must(states.LockRelease)
}

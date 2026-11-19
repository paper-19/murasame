package safefile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

var (
	ErrSymlink      = errors.New("is a symbolic link")
	ErrNotRegular   = errors.New("is not a regular file")
	ErrHardLinked   = errors.New("has more than one hard link")
	ErrLinksUnknown = errors.New("has a link count that cannot be read")
	ErrChanged      = errors.New("changed while it was opened")
)

type Refused struct {
	Path   string
	Reason error
	Links  uint64
}

func (e *Refused) Error() string {
	if e.Reason == ErrHardLinked {
		return fmt.Sprintf("%s has %d hard links", e.Path, e.Links)
	}
	return e.Path + " " + e.Reason.Error()
}

func (e *Refused) Unwrap() error { return e.Reason }

func Links(fi os.FileInfo) (uint64, bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st != nil {
		return uint64(st.Nlink), true
	}
	return 0, false
}

func Check(fi os.FileInfo, path string) error {
	if fi.Mode()&os.ModeSymlink != 0 {
		return &Refused{Path: path, Reason: ErrSymlink}
	}
	if !fi.Mode().IsRegular() {
		return &Refused{Path: path, Reason: ErrNotRegular}
	}
	n, ok := Links(fi)
	if !ok {
		return &Refused{Path: path, Reason: ErrLinksUnknown}
	}
	if n != 1 {
		return &Refused{Path: path, Reason: ErrHardLinked, Links: n}
	}
	return nil
}

func Open(path string, flags int, perm os.FileMode) (*os.File, error) {
	open := (flags | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC) &^ syscall.O_TRUNC
	var fd int
	var err error
	for {
		fd, err = syscall.Open(path, open, uint32(perm.Perm()))
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, syscall.ELOOP):
			return nil, &Refused{Path: path, Reason: ErrSymlink}
		case errors.Is(err, syscall.ENXIO), errors.Is(err, syscall.EOPNOTSUPP):
			return nil, &Refused{Path: path, Reason: ErrNotRegular}
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return nil, &os.PathError{Op: "fstat", Path: path, Err: err}
	}
	var refuse error
	switch {
	case st.Mode&syscall.S_IFMT != syscall.S_IFREG:
		refuse = &Refused{Path: path, Reason: ErrNotRegular}
	case st.Nlink != 1:
		refuse = &Refused{Path: path, Reason: ErrHardLinked, Links: uint64(st.Nlink)}
	}
	if refuse == nil {
		if err := syscall.SetNonblock(fd, false); err != nil {
			refuse = &os.PathError{Op: "fcntl", Path: path, Err: err}
		}
	}
	if refuse == nil && flags&syscall.O_TRUNC != 0 {
		if err := syscall.Ftruncate(fd, 0); err != nil {
			refuse = &os.PathError{Op: "truncate", Path: path, Err: err}
		}
	}
	if refuse != nil {
		syscall.Close(fd)
		return nil, refuse
	}
	return os.NewFile(uintptr(fd), path), nil
}

func OpenRead(path string) (*os.File, os.FileInfo, error) {
	lst, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if err := Check(lst, path); err != nil {
		return nil, nil, err
	}
	f, err := Open(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err == nil {
		err = Check(fi, path)
	}
	if err == nil && !os.SameFile(lst, fi) {
		err = &Refused{Path: path, Reason: ErrChanged}
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, fi, nil
}

func ReadTail(path string, max int64) ([]byte, bool, error) {
	f, fi, err := OpenRead(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	if max <= 0 {
		b, err := io.ReadAll(f)
		return b, false, err
	}
	cut := fi.Size() > max
	if cut {
		if _, err := f.Seek(-max, io.SeekEnd); err != nil {
			return nil, false, err
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, max))
	return b, cut, err
}

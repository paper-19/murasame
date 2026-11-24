package store

import (
	"errors"
	"fmt"
	"os"

	"murasame/internal/safefile"
)

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

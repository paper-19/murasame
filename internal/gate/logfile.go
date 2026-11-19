package gate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"murasame/internal/safefile"
)

func ensureLogDir(dir string) error {
	lst, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		lst, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("the log directory %s is a symbolic link: refusing to write through it", dir)
	}
	if !lst.IsDir() {
		return fmt.Errorf("the log directory %s is not a directory", dir)
	}
	return nil
}

func logError(err error) error {
	var refused *safefile.Refused
	if !errors.As(err, &refused) {
		return err
	}
	path := refused.Path
	switch refused.Reason {
	case safefile.ErrSymlink:
		return fmt.Errorf("%s is a symbolic link: refusing to write through it", path)
	case safefile.ErrHardLinked:
		return fmt.Errorf("%s has %d hard links: refusing to use it as a log", path, refused.Links)
	}
	return fmt.Errorf("%s %s: refusing to use it as a log", path, refused.Reason)
}

func openLog(path string, flags int) (*os.File, error) {
	f, err := safefile.Open(path, flags, 0o644)
	if err != nil {
		return nil, logError(err)
	}
	return f, nil
}

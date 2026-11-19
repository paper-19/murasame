package glob

import (
	"errors"
	"path"
	"strings"
)

func Match(pattern, name string) bool {
	return matchParts(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchParts(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return len(name) > 0
			}
			for i := 0; i <= len(name); i++ {
				if matchParts(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

func Check(pattern string) error {
	if pattern == "" {
		return errors.New("an empty pattern")
	}
	if strings.HasPrefix(pattern, "/") {
		return errors.New("paths are relative to the repository root: no leading /")
	}
	for _, p := range strings.Split(pattern, "/") {
		switch {
		case p == "**":
			continue
		case p == "":
			return errors.New("an empty path segment (a trailing or doubled /)")
		case p == "." || p == "..":
			return errors.New("a . or .. segment (paths are as git shows them, e.g. docs/a.md)")
		case strings.Contains(p, "**"):
			return errors.New("** only as a whole path segment (docs/**/*.md, not docs/**.md)")
		}
		if _, err := path.Match(p, ""); err != nil {
			return errors.New("a malformed [...] class or a trailing \\")
		}
	}
	return nil
}

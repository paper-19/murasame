package gate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const ExecArg = "__murasame-exec"

const ExecFailedCode = 254

var execHelper = ownExecutable()

func ownExecutable() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}

func init() {
	if len(os.Args) > 1 && os.Args[1] == ExecArg {
		os.Exit(execMain(os.Args[2:], os.Environ(), os.Stderr, syscall.Exec))
	}
}

func execMarker(nonce string) string { return "murasame-exec " + nonce + ": " }

func execMain(args, env []string, stderr io.Writer, execve func(string, []string, []string) error) int {
	if len(args) < 3 || args[1] != "--" {
		fmt.Fprintf(stderr, "murasame: usage: %s <nonce> -- <program> [<arg>...]\n", ExecArg)
		return 2
	}
	nonce, argv := args[0], args[2:]
	path, err := findProgram(argv[0], env, "")
	if err == nil {
		err = execve(path, argv, env)
	}
	fmt.Fprintf(stderr, "%scannot run %s: %s\n", execMarker(nonce), argv[0], err)
	return ExecFailedCode
}

func findProgram(name string, env []string, cwd string) (string, error) {
	if name == "" {
		return "", errors.New("no program")
	}
	if strings.Contains(name, "/") {
		return name, nil
	}
	path := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			path = kv[5:]
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		at := p
		if !filepath.IsAbs(p) && cwd != "" {
			at = filepath.Join(cwd, p)
		}
		if st, err := os.Stat(at); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			if !strings.Contains(p, "/") {
				p = "./" + p
			}
			return p, nil
		}
	}
	return "", errors.New("not found in PATH")
}

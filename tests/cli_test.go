package tests

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"murasame/internal/cli"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := cli.Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCheckConfigShowsTheCIGate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "m.toml")
	text := "[ci]\ncontexts = [\"ci / test (push)\"]\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runCLI(t, "check-config", "--config", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"CI: pushes to murasame/staging", "ci / test (push)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("check-config output lacks %q:\n%s", want, out)
		}
	}
}

func TestBadConfigIsRejected(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "m.toml")
	if err := os.WriteFile(path, []byte("[bogus]\nx = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runCLI(t, "check-config", "--config", path)
	if code == 0 || !strings.Contains(errs, "unknown table(s): bogus") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

func TestHistoryShowsTheServiceBatch(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	f.OpenPR(2, map[string]string{"BROKEN": "b\n"})
	text := baseConfig(f, "")
	q := NewQueue(t, f, text)
	f.Approve(1)
	f.Approve(2)
	q.Tick()
	q.Svc.Close()

	path := filepath.Join(t.TempDir(), "m.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runCLI(t, "history", "--config", path, "--repo", q.Svc.RepoDir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"service, batch", "merged", "#1", "set_aside", "#2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("history lacks %q:\n%s", want, out)
		}
	}
	code, out, errs = runCLI(t, "history", "--config", path, "--repo", q.Svc.RepoDir, "--pr", "2")
	if code != 0 || !strings.Contains(out, "set_aside") {
		t.Fatalf("history --pr 2: exit %d\n%s%s", code, out, errs)
	}
}

func TestCheckConfigWithoutAStartTimeout(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "m.toml")
	if err := os.WriteFile(path, []byte("[ci]\nstart_timeout = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runCLI(t, "check-config", "--config", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if strings.Contains(out, "no status after") || !strings.Contains(out, "every job of the run") {
		t.Fatalf("check-config output:\n%s", out)
	}
}

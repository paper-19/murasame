package tests

import (
	"path/filepath"
	"testing"

	"murasame/internal/config"
	"murasame/internal/service"
)

func serviceWithRemote(t *testing.T, remote string) *service.Service {
	t.Helper()
	text := "[forgejo]\nurl = \"http://forge.test\"\nowner = \"team\"\nrepo = \"app\"\ngit_remote_url = \"" + remote + "\"\n"
	cfg, err := config.Load("", config.LoadOptions{Text: &text, Env: config.Env{}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := service.New(cfg, filepath.Join(t.TempDir(), "c.git"), service.Options{API: NewFakeForge(t), Env: config.Env{}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRedactKeepsTheForgesOwnLinks(t *testing.T) {
	t.Parallel()
	s := serviceWithRemote(t, "http://forge.test/team/app.git")
	link := "failure http://forge.test/team/app/actions/runs/5/jobs/0"
	if got := s.Redact(link); got != link {
		t.Fatalf("a run link was redacted: %q", got)
	}
}

func TestRedactHidesCredentialsAndPrivateRemotes(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"http://bot:s3cret@forge.test/team/app.git": "fetch http://bot:s3cret@forge.test/team/app.git failed",
		"/srv/git/app.git":                          "fetch /srv/git/app.git failed",
		"http://git.internal:3000/team/app.git":     "fetch http://git.internal:3000/team/app.git failed",
	}
	for remote, line := range cases {
		got := serviceWithRemote(t, remote).Redact(line)
		if got != "fetch <git remote> failed" {
			t.Fatalf("remote %s: got %q", remote, got)
		}
	}
}

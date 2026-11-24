package tests

import (
	"os"
	"testing"
)

// TestMain keeps the tests independent of the machine's git config: no global or
// system config, and a fixed identity for the merge commits the queue makes.
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "murasame", "GIT_AUTHOR_EMAIL": "murasame@example.com",
		"GIT_COMMITTER_NAME": "murasame", "GIT_COMMITTER_EMAIL": "murasame@example.com",
	} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}

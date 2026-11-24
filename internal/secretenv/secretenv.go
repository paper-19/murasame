package secretenv

import (
	"os"
	"sort"
	"strings"
	"sync"
)

var (
	mu     sync.Mutex
	secret = map[string]bool{"MURASAME_FORGEJO_TOKEN": true, "MURASAME_WEBHOOK_SECRET": true}
)

func Hide(names ...string) {
	mu.Lock()
	defer mu.Unlock()
	for _, n := range names {
		if n != "" {
			secret[n] = true
		}
	}
}

var RepoLocating = map[string]bool{"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true,
	"GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_COMMON_DIR": true,
	"GIT_NAMESPACE": true, "GIT_CEILING_DIRECTORIES": true, "GIT_DISCOVERY_ACROSS_FILESYSTEM": true,
	"GIT_PREFIX": true, "GIT_QUARANTINE_PATH": true, "GIT_SHALLOW_FILE": true, "GIT_GRAFT_FILE": true,
	"GIT_REPLACE_REF_BASE": true, "GIT_NO_REPLACE_OBJECTS": true}

func Inherited() map[string]string {
	full := map[string]string{}
	mu.Lock()
	defer mu.Unlock()
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if !secret[k] && !RepoLocating[k] {
			full[k] = v
		}
	}
	return full
}

func List(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

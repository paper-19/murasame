package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"murasame/internal/config"
	"murasame/internal/gitx"
	"murasame/internal/store"
)

const (
	dryPrefix    = "dry-"
	noLandPrefix = "no-land-"
)

func throwawayPid(name string) (int, bool) {
	for _, p := range []string{dryPrefix, noLandPrefix} {
		if rest, ok := strings.CutPrefix(name, p); ok {
			pid, err := strconv.Atoi(rest)
			return pid, err == nil && pid > 0
		}
	}
	return 0, false
}

func (e *Engine) addThrowaway(dir, rev string) func() {
	e.G.MustRun("worktree", "add", "-q", "--detach", dir, rev)
	admin := gitx.New(dir, e.GitEnv).Out("rev-parse", "--path-format=absolute", "--git-dir")
	var once sync.Once
	return func() { once.Do(func() { e.removeWorktree(dir, admin) }) }
}

func (e *Engine) removeWorktree(dir, admin string) {
	if e.G.OK("worktree", "remove", "--force", dir) {
		return
	}
	os.RemoveAll(dir)
	if admin != "" && filepath.Dir(admin) == filepath.Join(e.G.CommonDir(), "worktrees") {
		os.RemoveAll(admin)
	}
}

func (e *Engine) pruneThrowaways() {
	entries, err := os.ReadDir(e.StateDir)
	if err != nil {
		return
	}
	removed := false
	for _, d := range entries {
		pid, ok := throwawayPid(d.Name())
		if !ok || !d.IsDir() || store.PidAlive(pid) {
			continue
		}
		dir := filepath.Join(e.StateDir, d.Name())
		if st, err := os.Lstat(filepath.Join(dir, ".git")); err != nil || !st.Mode().IsRegular() {
			continue
		}
		e.Rep.Say(fmt.Sprintf("-- removing %s: left behind by an earlier run (process %d) that is gone", dir, pid))
		e.removeWorktree(dir, "")
		removed = true
	}
	if removed {
		e.G.Run("worktree", "prune")
	}
}

func (e *Engine) noLandBegin(items []Item) (remove func(), err error) {
	defer gitx.Catch(&err)
	if len(items) != 1 {
		return nil, fmt.Errorf("a no-land run takes exactly one branch, not %d", len(items))
	}
	if !e.Records.Enabled() {
		return nil, config.Errf("a no-land run needs [records] enabled = true: the pass record is what it is for")
	}
	pin := e.G.CommitOf("refs/heads/" + e.Target)
	if pin == "" {
		return nil, fmt.Errorf("no target branch %s", e.Target)
	}
	e.pin = pin
	e.Rep.Say(fmt.Sprintf("== no-land run on %s %s, in %s (records the pass; does not land)", e.Target, e.short(pin),
		e.IntegDir))
	return e.addThrowaway(e.IntegDir, pin), nil
}

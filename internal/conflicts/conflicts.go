package conflicts

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"murasame/internal/config"
	"murasame/internal/gitx"
	"murasame/internal/glob"
	"murasame/internal/states"
)

const DefaultMarkerSize = 7

func lines(text string) []string {
	parts := strings.Split(text, "\n")
	last := parts[len(parts)-1]
	parts = parts[:len(parts)-1]
	out := make([]string, 0, len(parts)+1)
	for _, p := range parts {
		out = append(out, p+"\n")
	}
	if last != "" {
		out = append(out, last)
	}
	return out
}

var repeatable = regexp.MustCompile(`^[\s\-|:=*#>` + "`" + `~_+.]*$`)

func JoinHunk(ours, theirs string) string {
	o, t := lines(ours), lines(theirs)
	if n := len(o); n > 0 && !strings.HasSuffix(o[n-1], "\n") {
		switch {
		case strings.HasSuffix(o[n-1], "\r"):
			o[n-1] += "\n"
		case n > 1 && strings.HasSuffix(o[n-2], "\r\n"):
			o[n-1] += "\r\n"
		default:
			o[n-1] += "\n"
		}
	}
	norm := func(x string) string { return strings.TrimRight(x, "\r\n") }
	written := map[string]int{}
	out := append([]string{}, o...)
	for _, x := range o {
		if n := norm(x); !repeatable.MatchString(n) {
			written[n]++
		}
	}
	for _, x := range t {
		n := norm(x)
		if !repeatable.MatchString(n) && written[n] > 0 {
			written[n]--
			continue
		}
		out = append(out, x)
	}
	return strings.Join(out, "")
}

func isMarker(line string, c byte, size int) bool {
	body := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if len(body) < size {
		return false
	}
	for i := 0; i < size; i++ {
		if body[i] != c {
			return false
		}
	}
	if c == '=' {
		return len(body) == size
	}
	return len(body) == size || body[size] == ' '
}

func within(side, stage string) bool {
	norm := func(x string) string { return strings.ReplaceAll(x, "\r", "") }
	return strings.Contains("\n"+norm(stage), "\n"+norm(side))
}

func JoinHunks(text string, size int, stages []string, join func(ours, theirs string) string) (string, bool) {
	if size <= 0 {
		size = DefaultMarkerSize
	}
	ls := lines(text)
	var out strings.Builder
	found := false
	for i := 0; i < len(ls); {
		l := ls[i]
		switch {
		case isMarker(l, '>', size) || isMarker(l, '|', size):
			return "", false
		case !isMarker(l, '<', size):
			out.WriteString(l)
			i++
			continue
		}
		var seps []int
		end := -1
	scan:
		for j := i + 1; j < len(ls); j++ {
			switch {
			case isMarker(ls[j], '<', size) || isMarker(ls[j], '|', size):
				return "", false
			case isMarker(ls[j], '=', size):
				seps = append(seps, j)
			case isMarker(ls[j], '>', size):
				end = j
				break scan
			}
		}
		if end < 0 || len(seps) == 0 {
			return "", false
		}
		sep := -1
		for _, s := range seps {
			fits := len(seps) == 1
			if !fits && len(stages) == 2 {
				fits = within(strings.Join(ls[i+1:s], ""), stages[0]) && within(strings.Join(ls[s+1:end], ""), stages[1])
			}
			if fits {
				if sep >= 0 {
					return "", false
				}
				sep = s
			}
		}
		if sep < 0 {
			return "", false
		}
		out.WriteString(join(strings.Join(ls[i+1:sep], ""), strings.Join(ls[sep+1:end], "")))
		found = true
		i = end + 1
	}
	return out.String(), found
}

func readText(path string) (string, os.FileMode, bool, error) {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, false, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	if !utf8.Valid(raw) {
		return "", 0, false, nil
	}
	return string(raw), st.Mode().Perm(), true, nil
}

func KeepBoth(path string, size int, stages []string) (bool, error) {
	text, perm, ok, err := readText(path)
	if err != nil || !ok {
		return false, err
	}
	return writeJoined(path, text, size, stages, perm)
}

func writeJoined(path, text string, size int, stages []string, perm os.FileMode) (bool, error) {
	joined, ok := JoinHunks(text, size, stages, JoinHunk)
	if !ok {
		return false, nil
	}
	if err := os.WriteFile(path, []byte(joined), perm); err != nil {
		return false, err
	}
	return true, nil
}

var markerChars = []byte{'<', '=', '>', '|'}

func collides(size int, texts ...string) bool {
	for _, t := range texts {
		for _, l := range lines(t) {
			for _, c := range markerChars {
				if isMarker(l, c, size) {
					return true
				}
			}
		}
	}
	return false
}

func freeSize(size int, texts ...string) int {
	n := size + 1
	for collides(n, texts...) {
		n++
	}
	return n
}

func lineEnd(l string) string {
	switch {
	case strings.HasSuffix(l, "\r\n"):
		return "\r\n"
	case strings.HasSuffix(l, "\n"):
		return "\n"
	}
	return ""
}

func remark(text string, from, to int, chars []byte) string {
	var b strings.Builder
	for _, l := range lines(text) {
		for _, c := range chars {
			if isMarker(l, c, from) {
				l = strings.Repeat(string(c), to) + lineEnd(l)
				break
			}
		}
		b.WriteString(l)
	}
	return b.String()
}

func sameMerge(git string, size int, again string, n int) bool {
	labels := []byte{'<', '>', '|'}
	return remark(git, size, size, labels) == remark(remark(again, n, size, markerChars), size, size, labels)
}

func remerge(tree *gitx.Git, path string, size int) (bool, error) {
	f, err := os.CreateTemp("", "murasame-attributes-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	_, err = fmt.Fprintf(f, "* conflict-marker-size=%d\n", size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, err
	}
	attrs := "core.attributesFile=" + f.Name()
	if markerSize(tree, path, "-c", attrs) != size {
		return false, nil
	}
	r := tree.Run("-c", attrs, "-c", "merge.conflictstyle=merge", "-c", "rerere.enabled=false", "checkout", "-q", "-m", "--",
		gitx.Literal(path))
	if !r.OK() {
		return false, fmt.Errorf("git checkout -m failed: %s", gitx.LastLine(r.Out))
	}
	return true, nil
}

func keepBoth(tree *gitx.Git, path string) (bool, error) {
	full := filepath.Join(tree.Cwd, path)
	size := MarkerSize(tree, path)
	stages := stageTexts(tree, path)
	text, perm, ok, err := readText(full)
	if err != nil || !ok {
		return false, err
	}
	base := ""
	if r, out := tree.RunStdout("cat-file", "blob", ":1:"+path); r.OK() {
		base = out
	}
	if !collides(size, append([]string{base}, stages...)...) {
		return writeJoined(full, text, size, stages, perm)
	}
	n := freeSize(size, append([]string{base}, stages...)...)
	if ok, err := remerge(tree, path, n); err != nil || !ok {
		return false, err
	}
	again, _, ok, err := readText(full)
	if err != nil || !ok {
		return false, err
	}
	if !sameMerge(text, size, again, n) {
		return false, nil
	}
	return writeJoined(full, again, n, stages, perm)
}

func MarkerSize(tree *gitx.Git, path string) int { return markerSize(tree, path) }

func markerSize(tree *gitx.Git, path string, config ...string) int {
	args := append(append([]string{}, config...), "check-attr", "-z", "conflict-marker-size", "--", path)
	r, out := tree.RunStdout(args...)
	if f := strings.Split(out, "\x00"); r.OK() && len(f) >= 3 {
		if n, err := strconv.Atoi(f[2]); err == nil && n > 0 {
			return n
		}
	}
	return DefaultMarkerSize
}

func stageTexts(tree *gitx.Git, path string) []string {
	var out []string
	for _, s := range []string{"2", "3"} {
		r, text := tree.RunStdout("cat-file", "blob", ":"+s+":"+path)
		if !r.OK() {
			return nil
		}
		out = append(out, text)
	}
	return out
}

func regularBoth(tree *gitx.Git, path string) bool {
	modes := tree.StageModes(path)
	for _, s := range []string{"2", "3"} {
		if m := modes[s]; m != "100644" && m != "100755" {
			return false
		}
	}
	return true
}

func Matches(rule *config.ConflictRule, path string) bool {
	in := false
	for _, g := range rule.Paths {
		if glob.Match(g, path) {
			in = true
			break
		}
	}
	if !in {
		return false
	}
	for _, g := range rule.Exclude {
		if glob.Match(g, path) {
			return false
		}
	}
	return true
}

type Merge struct {
	State states.MergeState
	Note  string
	Why   string
}

func (m *Merge) fire(on states.MergeEvent) { m.State = states.MergeMachine.Must(m.State, on) }

func (m *Merge) end(on states.MergeEvent, why string) Merge {
	m.fire(on)
	m.Why = why
	return *m
}

type Merger struct {
	Cfg *config.Config
}

func abort(tree *gitx.Git, orig, on string) {
	tree.Run("merge", "--abort")
	tree.Reattach(on)
	tree.MustRun("reset", "-q", "--hard", orig)
	tree.MustRun("clean", "-ffdq")
}

func unrelated(tree *gitx.Git, orig, ref string) (bool, string) {
	r := tree.Run("merge-base", orig, ref)
	switch {
	case r.Code == 1 && strings.TrimSpace(r.Out) == "":
		return true, ""
	case r.OK():
		return false, ""
	}
	return false, fmt.Sprintf("git merge-base %s %s failed (exit %d: %s)", short(orig), ref, r.Code, gitx.LastLine(r.Out))
}

func (mg *Merger) Merge(tree *gitx.Git, ref, branch, slug, message string) Merge {
	m := Merge{State: states.MergeMachine.Initial}
	args := []string{"-c", "merge.conflictstyle=merge", "-c", "rerere.enabled=false", "merge", "-q"}
	if mg.Cfg.NoFF {
		args = append(args, "--no-ff")
	}
	if message == "" {
		message = MergeMessage(branch)
	}
	args = append(args, "-m", message)
	orig, on := tree.Head(), tree.HeadRef()
	r := tree.Run(append(args, ref)...)
	if r.OK() {
		m.fire(states.MergeOnClean)
		m.fire(states.MergeOnDone)
		return m
	}
	if len(tree.Unmerged()) == 0 {
		m.fire(states.MergeOnRefuse)
		tail := gitx.LastLine(r.Out)
		abort(tree, orig, on)
		none, why := unrelated(tree, orig, ref)
		switch {
		case none:
			return m.end(states.MergeOnUnrelated, "the branch has no history in common with the target: "+tail)
		case why != "":
			return m.end(states.MergeOnInfra, "git merge failed without conflicts ("+tail+"), and "+why+
				": an infrastructure failure, not the branch's")
		}
		return m.end(states.MergeOnInfra, "git merge failed without conflicts: "+tail)
	}
	m.fire(states.MergeOnConflict)
	m.fire(states.MergeOnResolve)
	mg.resolve(&m, tree)
	if m.State != states.MergeResolved {
		abort(tree, orig, on)
		return m
	}
	tree.Commit(message + "\n\nConflicts resolved by the merge queue: " + m.Note)
	m.fire(states.MergeOnCommit)
	m.fire(states.MergeOnDone)
	return m
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "-"
	}
	return s
}

func MergeMessage(branch string) string {
	return fmt.Sprintf("Merge branch '%s' (merge queue)", branch)
}

func (mg *Merger) rule(path string) *config.ConflictRule {
	for _, r := range mg.Cfg.Conflicts {
		if Matches(r, path) {
			return r
		}
	}
	return nil
}

var sideStage = map[string]string{"ours": "2", "theirs": "3"}

var sideRev = map[string]string{"ours": "HEAD", "theirs": "MERGE_HEAD"}

func inSide(tree *gitx.Git, side, path string) bool {
	return tree.Run("cat-file", "-e", sideRev[side]+":"+path).OK()
}

func onASide(tree *gitx.Git, path string) bool {
	return inSide(tree, "ours", path) || inSide(tree, "theirs", path)
}

func takeable(tree *gitx.Git, path, side string) bool {
	switch tree.StageModes(path)[sideStage[side]] {
	case "100644", "100755", "120000":
		return true
	}
	return false
}

func take(tree *gitx.Git, path, side string) {
	tree.MustRun("checkout", "-q", "--"+side, "--", gitx.Literal(path))
	tree.MustRun("add", "--", gitx.Literal(path))
}

func (mg *Merger) resolve(m *Merge, tree *gitx.Git) {
	var bad []string
	groups := map[*config.ConflictRule][]string{}
	for _, path := range tree.Unmerged() {
		if !onASide(tree, path) {
			bad = append(bad, path)
			continue
		}
		rule := mg.rule(path)
		if rule == nil {
			bad = append(bad, path)
			continue
		}
		stages := tree.Stages(path)
		switch {
		case rule.Strategy == "keep-both" && stages["2"] && stages["3"] && regularBoth(tree, path):
			ok, err := keepBoth(tree, path)
			if err != nil {
				m.end(states.MergeOnInfra, fmt.Sprintf("keep-both could not read or write %s (%s): an infrastructure "+
					"failure, not a conflict", path, err))
				return
			}
			if !ok {
				bad = append(bad, path)
				continue
			}
			tree.MustRun("add", "--", gitx.Literal(path))
		case (rule.Strategy == "ours" || rule.Strategy == "theirs") && takeable(tree, path, rule.Strategy):
			take(tree, path, rule.Strategy)
		default:
			bad = append(bad, path)
			continue
		}
		groups[rule] = append(groups[rule], path)
	}
	if len(bad) > 0 {
		m.end(states.MergeOnPerson, strings.Join(bad, " "))
		return
	}
	var parts []string
	for _, rule := range mg.Cfg.Conflicts {
		paths, ok := groups[rule]
		if !ok {
			continue
		}
		part := rule.Label + ": " + strings.Join(paths, " ")
		parts = append(parts, part)
	}
	m.fire(states.MergeOnResolved)
	m.Note = strings.Join(parts, "; ")
}

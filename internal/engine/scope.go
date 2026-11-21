package engine

import (
	"fmt"
	"strings"

	"murasame/internal/config"
	"murasame/internal/gate"
	"murasame/internal/gitx"
	"murasame/internal/glob"
)

type scopeInput struct {
	spec    *config.GateSpec
	base    string
	changed []string
	diffOK  bool
	dirty   bool

	since    int
	hasFull  bool
	sinceErr error
}

func decideScope(in scopeInput) gate.Scope {
	spec := in.spec
	full := func(why string) gate.Scope { return gate.Scope{Base: in.base, Why: why} }
	switch {
	case spec.AffectedCommand.Empty():
		return gate.Scope{Base: in.base}
	case in.base == "" || !in.diffOK:
		return full("could not compute what changed: running everything")
	case in.dirty:
		return full("integ has uncommitted changes: running everything")
	}
	for _, p := range in.changed {
		for _, pat := range spec.FullWhen {
			if glob.Match(pat, p) {
				return full(fmt.Sprintf("%s changed, and full_when has %s", p, pat))
			}
		}
	}
	if spec.FullEvery > 0 {
		switch {
		case in.sinceErr != nil:
			return full(fmt.Sprintf("full_every: %s: running everything", in.sinceErr))
		case !in.hasFull:
			return full("full_every: no full run of this gate on record")
		case in.since >= spec.FullEvery:
			return full(fmt.Sprintf("full_every: %d affected-tests landing(s) since the last full run (at most %d)",
				in.since, spec.FullEvery))
		}
	}
	return gate.Scope{Affected: true, Base: in.base, Why: fmt.Sprintf("%d path(s) changed since %s", len(in.changed),
		short(in.base))}
}

func (e *Engine) scope(name string) gate.Scope {
	spec := e.Cfg.Gates[name]
	in := scopeInput{spec: spec}
	if r, out := e.Integ.RunStdout("merge-base", e.targetRef(), "HEAD"); r.OK() {
		in.base = strings.TrimSpace(out)
	}
	if spec.AffectedCommand.Empty() {
		return decideScope(in)
	}
	if in.base != "" {
		if r, out := e.Integ.RunStdout("diff-tree", "-r", "-z", "--name-only", "--no-renames", in.base, "HEAD"); r.OK() {
			in.changed, in.diffOK = nulFields(out), true
		}
	}
	in.dirty = e.Integ.DirtyTracked()
	if spec.FullEvery > 0 {
		in.since, in.hasFull, in.sinceErr = e.Records.SinceFull(name, e.gateHash(name), e.affectedHash(name),
			targetHistory{e.G, e.targetRef()})
	}
	return decideScope(in)
}

func (e *Engine) affectedHash(name string) string {
	return e.Cfg.Gates[name].AffectedHash(e.Cfg.GateEnv)
}

func (e *Engine) scopeHash(name string, sc gate.Scope) string {
	if sc.Affected {
		return e.affectedHash(name)
	}
	return e.gateHash(name)
}

type targetHistory struct {
	g      *gitx.Git
	target string
}

func (t targetHistory) OnTarget(commit string) bool {
	if commit == "" || !t.g.OK("cat-file", "-e", commit+"^{commit}") {
		return false
	}
	return t.g.IsAncestor(commit, t.target)
}

func (t targetHistory) Since(commit string) map[string]bool {
	out := map[string]bool{}
	for _, c := range strings.Fields(t.g.Out("rev-list", t.target, "^"+commit)) {
		out[c] = true
	}
	return out
}

func nulFields(out string) []string {
	var res []string
	for _, x := range strings.Split(out, "\x00") {
		if x != "" {
			res = append(res, x)
		}
	}
	return res
}

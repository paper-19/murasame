package states

import (
	"fmt"
	"sort"
	"strings"
)

type Edge[S, E comparable] struct {
	From S
	On   E
}

type Machine[S, E comparable] struct {
	Name    string
	Initial S
	Final   []S
	Edges   map[Edge[S, E]]S
}

func (m *Machine[S, E]) Next(from S, on E) (S, bool) {
	to, ok := m.Edges[Edge[S, E]{from, on}]
	return to, ok
}

func (m *Machine[S, E]) Can(from S, on E) bool {
	_, ok := m.Next(from, on)
	return ok
}

func (m *Machine[S, E]) Step(from S, on E) (S, error) {
	to, ok := m.Next(from, on)
	if !ok {
		var zero S
		return zero, &InvalidTransition{fmt.Sprintf("%s: %v does not apply when %v", m.Name, on, from)}
	}
	return to, nil
}

func (m *Machine[S, E]) Must(from S, on E) S {
	to, err := m.Step(from, on)
	if err != nil {
		panic(err)
	}
	return to
}

func (m *Machine[S, E]) IsFinal(s S) bool {
	for _, f := range m.Final {
		if f == s {
			return true
		}
	}
	return false
}

func sorted[T comparable](set map[T]bool) []T {
	out := make([]T, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]) < fmt.Sprint(out[j]) })
	return out
}

func (m *Machine[S, E]) States() []S {
	set := map[S]bool{m.Initial: true}
	for e, to := range m.Edges {
		set[e.From], set[to] = true, true
	}
	for _, f := range m.Final {
		set[f] = true
	}
	return sorted(set)
}

func (m *Machine[S, E]) Events() []E {
	set := map[E]bool{}
	for e := range m.Edges {
		set[e.On] = true
	}
	return sorted(set)
}

func (m *Machine[S, E]) Reachable() map[S]bool {
	seen := map[S]bool{m.Initial: true}
	work := []S{m.Initial}
	for len(work) > 0 {
		s := work[0]
		work = work[1:]
		for e, to := range m.Edges {
			if e.From == s && !seen[to] {
				seen[to] = true
				work = append(work, to)
			}
		}
	}
	return seen
}

func (m *Machine[S, E]) Check() error {
	var bad []string
	reach := m.Reachable()
	out := map[S]bool{}
	for e := range m.Edges {
		out[e.From] = true
		if m.IsFinal(e.From) {
			bad = append(bad, fmt.Sprintf("final state %v has an edge on %v", e.From, e.On))
		}
	}
	for _, s := range m.States() {
		if !reach[s] {
			bad = append(bad, fmt.Sprintf("%v is not reachable from %v", s, m.Initial))
		}
		if !m.IsFinal(s) && !out[s] {
			bad = append(bad, fmt.Sprintf("%v is not final and has no edge out", s))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("%s: %s", m.Name, strings.Join(bad, "; "))
}

type Tracker[S, E comparable] struct {
	m *Machine[S, E]
	s S
}

func (m *Machine[S, E]) Start() *Tracker[S, E] { return &Tracker[S, E]{m, m.Initial} }

func (m *Machine[S, E]) At(s S) *Tracker[S, E] { return &Tracker[S, E]{m, s} }

func (t *Tracker[S, E]) State() S { return t.s }

func (t *Tracker[S, E]) Can(on E) bool { return t.m.Can(t.s, on) }

func (t *Tracker[S, E]) Fire(on E) error {
	to, err := t.m.Step(t.s, on)
	if err != nil {
		return err
	}
	t.s = to
	return nil
}

func (t *Tracker[S, E]) Must(on E) S {
	t.s = t.m.Must(t.s, on)
	return t.s
}

func (t *Tracker[S, E]) Done() bool { return t.m.IsFinal(t.s) }

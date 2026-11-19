package cli

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
)

type opt struct {
	flag   string
	dest   string
	kind   string
	help   string
	choice []string
	meta   string
	nonneg bool
}

type usageError struct {
	prog  string
	msg   string
	usage string
}

func (e *usageError) Error() string { return e.msg }

type parser struct {
	prog    string
	opts    []opt
	posName string
	posHelp string
}

type parsed struct {
	str   map[string]string
	bools map[string]*bool
	lists map[string][]string
	pos   []string
}

func (p *parser) add(o opt) { p.opts = append(p.opts, o) }

func (p *parser) usage() string {
	var parts []string
	for _, o := range p.opts {
		switch o.kind {
		case "value", "int", "append":
			m := o.meta
			if m == "" {
				m = strings.ToUpper(o.dest)
			}
			if len(o.choice) > 0 {
				m = "{" + strings.Join(o.choice, ",") + "}"
			}
			parts = append(parts, "["+o.flag+" "+m+"]")
		default:
			parts = append(parts, "["+o.flag+"]")
		}
	}
	if p.posName != "" {
		parts = append(parts, p.posName+" ["+p.posName+" ...]")
	}
	return "usage: " + p.prog + " [-h] " + strings.Join(parts, " ")
}

func (p *parser) help(w io.Writer) {
	fmt.Fprintln(w, p.usage())
	if p.posName != "" {
		fmt.Fprintf(w, "\npositional arguments:\n  %-22s %s\n", p.posName, p.posHelp)
	}
	fmt.Fprintln(w, "\noptions:\n  -h, --help             show this help message and exit")
	for _, o := range p.opts {
		name := o.flag
		if o.kind == "value" || o.kind == "int" || o.kind == "append" {
			m := o.meta
			if m == "" {
				m = strings.ToUpper(o.dest)
			}
			name += " " + m
		}
		fmt.Fprintf(w, "  %-22s %s\n", name, o.help)
	}
}

func (p *parser) fail(format string, a ...any) error {
	return &usageError{prog: p.prog, msg: fmt.Sprintf(format, a...), usage: p.usage()}
}

func (p *parser) find(name string) (*opt, error) {
	for i := range p.opts {
		if p.opts[i].flag == name {
			return &p.opts[i], nil
		}
	}
	var hits []*opt
	for i := range p.opts {
		if strings.HasPrefix(p.opts[i].flag, name) {
			hits = append(hits, &p.opts[i])
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	if len(hits) > 1 {
		var names []string
		for _, h := range hits {
			names = append(names, h.flag)
		}
		sort.Strings(names)
		return nil, p.fail("ambiguous option: %s could match %s", name, strings.Join(names, ", "))
	}
	return nil, nil
}

func (p *parser) parse(args []string, known bool) (*parsed, error) {
	r := &parsed{str: map[string]string{}, bools: map[string]*bool{}, lists: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			r.pos = append(r.pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			r.pos = append(r.pos, a)
			continue
		}
		if a == "-h" || a == "--help" {
			return nil, &helpRequest{p}
		}
		name, val, hasVal := strings.Cut(a, "=")
		o, err := p.find(name)
		if err != nil {
			return nil, err
		}
		if o == nil {
			if known {
				continue
			}
			return nil, p.fail("unrecognized arguments: %s", a)
		}
		switch o.kind {
		case "value", "int", "append":
			if !hasVal {
				if i+1 >= len(args) || (strings.HasPrefix(args[i+1], "-") && args[i+1] != "-" && !isNumber(args[i+1])) {
					return nil, p.fail("argument %s: expected one argument", o.flag)
				}
				i++
				val = args[i]
			}
			if len(o.choice) > 0 && !slices.Contains(o.choice, val) {
				var q []string
				for _, c := range o.choice {
					q = append(q, "'"+c+"'")
				}
				return nil, p.fail("argument %s: invalid choice: '%s' (choose from %s)", o.flag, val, strings.Join(q, ", "))
			}
			if o.kind == "int" {
				n, err := strconv.Atoi(strings.TrimSpace(val))
				if err != nil {
					return nil, p.fail("argument %s: invalid int value: '%s'", o.flag, val)
				}
				if o.nonneg && n < 0 {
					return nil, p.fail("argument %s: must be 0 or more: '%s'", o.flag, val)
				}
			}
			if o.kind == "append" {
				r.lists[o.dest] = append(r.lists[o.dest], val)
			} else {
				r.str[o.dest] = val
			}
		default:
			if hasVal {
				return nil, p.fail("argument %s: ignored explicit argument '%s'", o.flag, val)
			}
			switch o.kind {
			case "bool":
				t := true
				r.bools[o.dest] = &t
			case "false":
				f := false
				r.bools[o.dest] = &f
			}
		}
	}
	return r, nil
}

func isNumber(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

type helpRequest struct{ p *parser }

func (h *helpRequest) Error() string { return "help" }

func (r *parsed) flag(dest string) bool {
	b := r.bools[dest]
	return b != nil && *b
}

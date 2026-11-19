package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"murasame/internal/config"
	"murasame/internal/db"
	"murasame/internal/gitx"
	"murasame/internal/panics"
	"murasame/internal/store"
)

type HistoryArgs struct {
	Repo    string
	Branch  string
	PR      *int
	Limit   int
	Log     bool
	JSON    bool
	RepoKey string
	Repos   bool
}

func mainCheckout(repo string) (main string, ok bool) {
	failed := panics.Catch[*gitx.Error](func() { main = gitx.New(repo, nil).MainCheckout() })
	return main, failed == nil
}

func stateDirOf(cfg *config.Config, repo string) (sd, main string, ok bool) {
	main, ok = mainCheckout(repo)
	if !ok {
		main, _ = filepath.Abs(repo)
	}
	sd = config.Expand(cfg.StateDir, config.Vars{"repo": main})
	if !filepath.IsAbs(sd) {
		sd = filepath.Join(main, sd)
	}
	return sd, main, ok
}

func targetOf(cfg *config.Config, main string, isGit bool) (target string) {
	if len(cfg.Target) == 0 {
		return ""
	}
	target = cfg.Target[len(cfg.Target)-1]
	if !isGit {
		return target
	}
	found := ""
	panics.Catch[*gitx.Error](func() {
		g := gitx.New(main, nil)
		for _, t := range cfg.Target {
			if g.Rev("refs/heads/"+t) != "" {
				found = t
				return
			}
		}
	})
	if found != "" {
		return found
	}
	return target
}

func historyVars(cfg *config.Config, repo string) (string, config.Vars) {
	sd, main, ok := stateDirOf(cfg, repo)
	integ := config.Expand(cfg.Integ, config.Vars{"repo": main, "state_dir": sd})
	if !filepath.IsAbs(integ) {
		integ = filepath.Join(main, integ)
	}
	return sd, config.Vars{"repo": main, "state_dir": sd, "integ": integ, "target": targetOf(cfg, main, ok)}
}

func ht(x float64) string {
	if x == 0 {
		return "-"
	}
	return db.Time(x).Format("2006-01-02 15:04:05")
}

func htp(x *float64) string {
	if x == nil {
		return "-"
	}
	return ht(*x)
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func choose(cfg *config.Config, a HistoryArgs, vars config.Vars, repos []db.RepoRow, keys []string) ([]db.RepoRow, string) {
	byKey := map[string]db.RepoRow{}
	for _, r := range repos {
		byKey[r.Key] = r
	}
	if a.RepoKey == "all" {
		return repos, ""
	}
	if keys != nil && a.RepoKey == "" {
		var out []db.RepoRow
		for _, k := range keys {
			if r, ok := byKey[k]; ok {
				out = append(out, r)
			}
		}
		return out, ""
	}
	if a.RepoKey != "" {
		if r, ok := byKey[a.RepoKey]; ok {
			return []db.RepoRow{r}, ""
		}
		return nil, fmt.Sprintf("no repository %s in the state DB (--repos lists them)", strconv.Quote(a.RepoKey))
	}
	if len(repos) <= 1 {
		return repos, ""
	}
	want := []string{config.Expand(cfg.RepoKey, vars)}
	if cfg.Forgejo.Owner != "" {
		want = append(want, db.ForgeKey(db.ForgeHost(cfg.Forgejo.URL), cfg.Forgejo.Owner, cfg.Forgejo.Repo))
	}
	if main, ok := mainCheckout(a.Repo); ok {
		want = append(want, db.LocalKey(main))
	}
	for _, k := range want {
		if r, ok := byKey[k]; ok && k != "" {
			return []db.RepoRow{r}, ""
		}
	}
	return nil, "the state DB holds several repositories: choose one with --repo-key (--repos lists them) or 'all'"
}

func newestFirst[T any](rows []T, key func(T) float64, id func(T) int64) {
	sort.SliceStable(rows, func(i, j int) bool {
		ki, kj := key(rows[i]), key(rows[j])
		if ki != kj {
			return ki > kj
		}
		return id(rows[i]) > id(rows[j])
	})
}

func limited[T any](rows []T, limit int) []T {
	if len(rows) > limit && limit >= 0 {
		return rows[:limit]
	}
	return rows
}

func History(cfg *config.Config, a HistoryArgs, dbPath string, keys []string, stdout, stderr io.Writer) int {
	sd, vars := historyVars(cfg, a.Repo)
	var path string
	if dbPath == "" {
		path = db.Path(sd, config.Expand(cfg.DB, vars))
	} else {
		sd, path = filepath.Dir(dbPath), dbPath
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "murasame: %s: %s\n", path, err)
		return 2
	}
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return fail(err)
	}
	empty := func(why string) int {
		if a.JSON {
			what := "batches"
			if a.Log {
				what = "lines"
			}
			fmt.Fprintf(stdout, "{\"repos\": [], \"%s\": []}\n", what)
		} else {
			fmt.Fprintf(stdout, "no history yet: %s %s\n", why, path)
		}
		return 0
	}
	if err != nil {
		return empty("no state DB at")
	}
	d, err := db.Open(path, true)
	if errors.Is(err, db.ErrEmpty) {
		return empty("an empty state DB at")
	}
	if err != nil {
		return fail(err)
	}
	defer d.Close()
	repos, err := db.Select[db.RepoRow](d, "SELECT * FROM repos ORDER BY id")
	if err != nil {
		return fail(err)
	}
	if a.Repos {
		if a.JSON {
			if repos == nil {
				repos = []db.RepoRow{}
			}
			printJSON(stdout, map[string]any{"repos": repos})
		} else {
			for _, r := range repos {
				p := ""
				if str(r.Path) != "" {
					p = ", " + str(r.Path)
				}
				fmt.Fprintf(stdout, "%s  (%s%s)\n", r.Key, r.Kind, p)
			}
		}
		return 0
	}
	chosen, why := choose(cfg, a, vars, repos, keys)
	if why != "" {
		fmt.Fprintf(stderr, "murasame: %s\n", why)
		return 2
	}
	var batches []store.Batch
	var lines []store.Merge
	var moves []store.Transition
	for _, r := range chosen {
		h := store.NewHistory(&db.Repo{DB: d, ID: r.ID, Key: r.Key})
		if a.Log {
			got, err := h.Merges(a.Branch, a.PR, a.Limit)
			if err != nil {
				return fail(err)
			}
			for _, x := range got {
				x.Repo = r.Key
				lines = append(lines, x)
			}
		} else {
			got, err := h.Batches(a.Branch, a.PR, a.Limit)
			if err != nil {
				return fail(err)
			}
			for _, x := range got {
				x.Repo = r.Key
				batches = append(batches, x)
			}
		}
		if a.PR != nil {
			ts, err := store.NewServiceState(h.Repo).Transitions(*a.PR)
			if err != nil {
				return fail(err)
			}
			for _, m := range ts {
				m.Repo = r.Key
				moves = append(moves, m)
			}
		}
	}
	newestFirst(lines, func(m store.Merge) float64 { return m.AtUnix }, func(m store.Merge) int64 { return m.ID })
	newestFirst(batches, func(b store.Batch) float64 { return b.StartedAt }, func(b store.Batch) int64 { return b.ID })
	lines, batches = limited(lines, a.Limit), limited(batches, a.Limit)
	if len(chosen) > 1 {
		sort.SliceStable(moves, func(i, j int) bool { return moves[i].AtUnix < moves[j].AtUnix })
	}
	if len(moves) > a.Limit && a.Limit >= 0 {
		moves = moves[len(moves)-a.Limit:]
	}
	if a.JSON {
		out := map[string]any{}
		if a.Log {
			if lines == nil {
				lines = []store.Merge{}
			}
			out["lines"] = lines
		} else {
			if batches == nil {
				batches = []store.Batch{}
			}
			out["batches"] = batches
		}
		ks := make([]string, len(chosen))
		for i, r := range chosen {
			ks[i] = r.Key
		}
		out["repos"] = ks
		if a.PR != nil {
			if moves == nil {
				moves = []store.Transition{}
			}
			out["pr"] = map[string]any{"number": *a.PR, "transitions": moves}
		}
		printJSON(stdout, out)
		return 0
	}
	many := len(chosen) > 1
	prefix := func(repo string) string {
		if many {
			return "[" + repo + "] "
		}
		return ""
	}
	if a.Log {
		for i := len(lines) - 1; i >= 0; i-- {
			fmt.Fprintln(stdout, prefix(lines[i].Repo)+lines[i].Line)
		}
	} else {
		for _, b := range batches {
			ran := ht(b.StartedAt) + " .. unfinished"
			if b.FinishedAt != nil {
				ran = spanText(b.StartedAt, *b.FinishedAt)
			}
			base, res := head12(b.Base), head12(str(b.Result))
			moved := base
			if res != "" && res != base {
				moved = base + " -> " + res
			} else if base == "" {
				moved = "-"
			}
			code := ""
			if b.Code != nil {
				code = ", exit " + fmt.Sprint(*b.Code)
			}
			gates := "?"
			if b.Gates != nil {
				gates = fmt.Sprint(*b.Gates)
			}
			fmt.Fprintf(stdout, "%srun %d (%s, %s) %s: %s %s, %s gate run(s)%s\n", prefix(b.Repo), b.ID,
				b.Source, b.Mode, ran, b.Target, moved, gates, code)
			for _, m := range b.Members {
				pr := ""
				if m.PR != nil {
					by := str(m.ApprovedBy)
					if by == "" {
						by = "-"
					}
					priority := int64(0)
					if m.Priority != nil {
						priority = *m.Priority
					}
					pr = fmt.Sprintf(" (PR #%d %s, priority %d, approved %s by %s)", *m.PR, str(m.Branch),
						priority, htp(m.ApprovedAt), by)
				}
				why := ""
				if w := str(m.Why); w != "" {
					why = ": " + w
				}
				fmt.Fprintf(stdout, "  %-12s %s%s%s\n", m.Status, m.Name, pr, why)
			}
			for _, x := range []*string{b.Stopped, b.Note} {
				if s := str(x); s != "" {
					fmt.Fprintf(stdout, "  note: %s\n", s)
				}
			}
		}
	}
	for _, mv := range moves {
		w := ""
		if mv.Why != "" {
			w = " (" + mv.Why + ")"
		}
		fmt.Fprintf(stdout, "#%d %s %s: %s -> %s%s\n", *a.PR, mv.At, mv.Event, mv.Old, mv.New, w)
	}
	if len(batches) == 0 && len(lines) == 0 && len(moves) == 0 && a.Limit != 0 {
		if a.Branch != "" || a.PR != nil {
			fmt.Fprintln(stdout, "(nothing recorded for that filter)")
		} else {
			fmt.Fprintln(stdout, "(nothing recorded)")
		}
	}
	return 0
}

func head12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func printJSON(w io.Writer, v any) {
	text, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(w, err)
		return
	}
	fmt.Fprintln(w, string(text))
}

func spanText(start, finish float64) string {
	from, to := ht(start), ht(finish)
	if len(to) > 11 && len(from) > 11 && to[:11] == from[:11] {
		to = to[11:]
	}
	return from + " .. " + to
}

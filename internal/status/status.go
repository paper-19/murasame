package status

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"murasame/internal/db"
	"murasame/internal/instance"
	"murasame/internal/safefile"
	"murasame/internal/service"
	"murasame/internal/states"
	"murasame/internal/store"
)

const Refresh = 15

const Batches = 10

var LogName = regexp.MustCompile(`^[A-Za-z0-9_.+#-]+\.log$`)

func SafeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	host := u.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n != 0 {
			host += ":" + strconv.Itoa(n)
		}
	}
	return u.Scheme + "://" + host + strings.TrimRight(u.EscapedPath(), "/")
}

func quote(s string) string { return strings.ReplaceAll(url.PathEscape(s), "+", "%2B") }

func LogLink(svc *service.Service, log string) string {
	name := filepath.Base(log)
	if log == "" || !LogName.MatchString(name) {
		return ""
	}
	path := filepath.Join(svc.StateDir(), name)
	if st, err := os.Lstat(path); err != nil || safefile.Check(st, path) != nil {
		return ""
	}
	return "/repos/" + quote(svc.Name) + "/logs/" + quote(name)
}

func prURL(svc *service.Service, n int) string {
	base := SafeURL(svc.FJ.URL)
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/%s/pulls/%d", base, quote(svc.FJ.Owner), quote(svc.FJ.Repo), n)
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func inState(prs map[string]*store.PR, st states.State) []*store.PR {
	var out []*store.PR
	for _, p := range prs {
		if p.State == st {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func pending(prs map[string]*store.PR) []*store.PR {
	out := append(inState(prs, states.Testing), service.Line(prs)...)
	out = append(out, inState(prs, states.SetAside)...)
	return append(out, inState(prs, states.Held)...)
}

type GateView struct {
	Elapsed float64 `json:"elapsed"`
	LogURL  string  `json:"log_url"`
	PRs     []int   `json:"prs"`
	Since   float64 `json:"since"`
}

type BackoffView struct {
	Until float64 `json:"until"`
	Why   string  `json:"why"`
}

type PRView struct {
	ApprovedBy string `json:"approved_by"`
	Number     int    `json:"number"`
	Priority   int    `json:"priority"`
	Reason     string `json:"reason"`
	State      string `json:"state"`
	Title      string `json:"title"`
	URL        string `json:"url"`
}

type MemberView struct {
	LogURL string `json:"log_url"`
	Name   string `json:"name"`
	PR     *int64 `json:"pr"`
	Status string `json:"status"`
	Why    string `json:"why"`
}

type BatchView struct {
	Code       *int64       `json:"code"`
	FinishedAt *float64     `json:"finished_at"`
	ID         int64        `json:"id"`
	Members    []MemberView `json:"members"`
	Source     string       `json:"source"`
	StartedAt  float64      `json:"started_at"`
	Stopped    string       `json:"stopped"`
}

type RepoView struct {
	Backoff    *BackoffView `json:"backoff"`
	Batches    []BatchView  `json:"batches"`
	Gate       *GateView    `json:"gate"`
	LastError  string       `json:"last_error"`
	Name       string       `json:"name"`
	Phase      string       `json:"phase"`
	PRs        []PRView     `json:"prs"`
	Repository string       `json:"repository"`
	Target     string       `json:"target"`
}

type Page struct {
	GateSlots *instance.Snapshot `json:"gate_slots"`
	Now       float64            `json:"now"`
	Repos     []RepoView         `json:"repos"`
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func repoView(svc *service.Service, now float64) RepoView {
	st := must(svc.Store())
	data := must(st.Load())
	live := svc.Live()
	v := RepoView{Name: svc.Name, Repository: svc.Host + "/" + svc.FJ.Owner + "/" + svc.FJ.Repo, Target: svc.Target(),
		Phase: live.PhaseText(), LastError: live.LastError, PRs: []PRView{}, Batches: []BatchView{}}
	if g := live.Gate; g != nil {
		v.Gate = &GateView{Since: g.Since, Elapsed: now - g.Since, PRs: g.PRs, LogURL: LogLink(svc, g.Log)}
	}
	if until := data.Until(); until > now {
		v.Backoff = &BackoffView{Until: until, Why: data.BackoffWhy}
	}
	for _, p := range pending(data.PRs) {
		v.PRs = append(v.PRs, PRView{Number: p.Number, Title: p.Title, State: string(p.State),
			Reason: svc.Redact(p.Reason), Priority: p.Priority, ApprovedBy: p.ApprovedBy, URL: prURL(svc, p.Number)})
	}
	for _, b := range must(store.NewHistory(st.Repo).Batches("", nil, Batches)) {
		ms := []MemberView{}
		for _, m := range b.Members {
			ms = append(ms, MemberView{Name: m.Name, PR: m.PR, Status: m.Status, Why: svc.Redact(deref(m.Why)),
				LogURL: LogLink(svc, deref(m.Log))})
		}
		v.Batches = append(v.Batches, BatchView{ID: b.ID, Source: b.Source, StartedAt: b.StartedAt,
			FinishedAt: b.FinishedAt, Code: b.Code, Stopped: svc.Redact(deref(b.Stopped)), Members: ms})
	}
	return v
}

func View(services []*service.Service, slots *instance.GateSlots) Page {
	p := Page{Now: db.Now(), Repos: make([]RepoView, len(services))}
	for i, s := range services {
		p.Repos[i] = repoView(s, p.Now)
	}
	if slots != nil {
		snap := slots.Snapshot()
		p.GateSlots = &snap
	}
	return p
}

const css = `body{margin:0 auto;max-width:1100px;padding:16px;font:15px/1.45 system-ui,sans-serif;background:#fff;color:#111}
@media (prefers-color-scheme:dark){body{background:#151517;color:#e8e8e8}a{color:#8fb2ff}}
table{border-collapse:collapse;width:100%;margin:0 0 16px}th,td{text-align:left;padding:4px 8px;border-bottom:1px solid #8884;vertical-align:top}
.mute{opacity:.65}code{font:13px ui-monospace,monospace}`

var CSP = func() string {
	sum := sha256.Sum256([]byte(css))
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
		"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}()

func E(x any) string {
	if x == nil {
		return ""
	}
	return html.EscapeString(fmt.Sprint(x))
}

func a(href string, text any) string {
	if href == "" {
		return E(text)
	}
	return `<a href="` + E(href) + `">` + E(text) + `</a>`
}

func t(x float64) string {
	if x == 0 {
		return ""
	}
	return db.Time(x).Format("2006-01-02 15:04:05")
}

func now(r RepoView) string {
	parts := []string{E(r.Phase)}
	if g := r.Gate; g != nil {
		s := fmt.Sprintf("gate running %d s", int(g.Elapsed))
		if g.LogURL != "" {
			s += " (" + a(g.LogURL, "log") + ")"
		}
		parts = append(parts, s)
	}
	if bo := r.Backoff; bo != nil {
		parts = append(parts, "backing off until "+E(t(bo.Until))+": "+E(bo.Why))
	}
	if r.LastError != "" {
		parts = append(parts, "last error: "+E(r.LastError))
	}
	return strings.Join(parts, " · ")
}

func Render(v Page) string {
	var b strings.Builder
	if s := v.GateSlots; s != nil {
		fmt.Fprintf(&b, `<p class="mute">gate slots: %d of %s in use</p>`, len(s.Running), E(s.Max))
	}
	for _, r := range v.Repos {
		fmt.Fprintf(&b, `<h2>%s <span class="mute"><code>%s</code> → %s</span></h2><p>%s</p>`, E(r.Name),
			E(r.Repository), E(r.Target), now(r))
		if len(r.PRs) == 0 {
			b.WriteString(`<p class="mute">nothing queued</p>`)
		} else {
			b.WriteString(`<table><tr><th>PR</th><th>title</th><th>state</th><th>reason</th></tr>`)
			for _, p := range r.PRs {
				fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
					a(p.URL, "#"+fmt.Sprint(p.Number)), E(p.Title), E(p.State), E(p.Reason))
			}
			b.WriteString(`</table>`)
		}
		if len(r.Batches) == 0 {
			continue
		}
		b.WriteString(`<table><tr><th>batch</th><th>started</th><th>result</th><th>members</th></tr>`)
		for _, bt := range r.Batches {
			res := "running"
			if bt.FinishedAt != nil {
				res = "-"
				if bt.Code != nil {
					res = "exit " + fmt.Sprint(*bt.Code)
				}
			}
			if bt.Stopped != "" {
				res += ": " + bt.Stopped
			}
			var ms []string
			for _, m := range bt.Members {
				x := E(m.Name) + " " + E(m.Status)
				if m.Why != "" {
					x += ` <span class="mute">` + E(m.Why) + `</span>`
				}
				if m.LogURL != "" {
					x += " (" + a(m.LogURL, "log") + ")"
				}
				ms = append(ms, x)
			}
			fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`, E(bt.ID), E(t(bt.StartedAt)),
				E(res), strings.Join(ms, "<br>"))
		}
		b.WriteString(`</table>`)
	}
	return fmt.Sprintf(`<!doctype html><html lang="en"><head><meta charset="utf-8">`+
		`<meta name="viewport" content="width=device-width, initial-scale=1"><meta http-equiv="refresh" content="%d">`+
		`<title>murasame</title><style>%s</style></head><body><h1>murasame</h1>%s`+
		`<p class="mute">%s · reloads every %d s · <a href="/api/status">JSON</a></p></body></html>`,
		Refresh, css, b.String(), E(t(v.Now)), Refresh)
}

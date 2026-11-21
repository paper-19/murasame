package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"murasame/internal/db"
	"murasame/internal/states"
)

type GateApprove struct {
	Sha  string `json:"sha"`
	User string `json:"user"`
}

type Extra struct {
	GateApproves []GateApprove      `json:"gate_approves,omitempty"`
	Replied      map[string]float64 `json:"replied,omitempty"`
	RefLag       string             `json:"ref_lag,omitempty"`
	MarkMerged   string             `json:"mark_merged,omitempty"`
	Approval     states.Approval    `json:"approval,omitempty"`
	MarkTries    int                `json:"mark_tries,omitempty"`
}

func (e *Extra) Scan(src any) error {
	var raw []byte
	switch x := src.(type) {
	case string:
		raw = []byte(x)
	case []byte:
		raw = x
	}
	if len(raw) > 0 {
		json.Unmarshal(raw, e)
	}
	return nil
}

type PR struct {
	Number       int          `db:"number"`
	State        states.State `db:"state"`
	Head         string       `db:"head"`
	Title        string       `db:"title"`
	Branch       string       `db:"branch"`
	Author       string       `db:"author"`
	Priority     int          `db:"priority"`
	Rollup       string       `db:"rollup"`
	LastComment  int          `db:"last_comment"`
	ApprovedBy   string       `db:"approved_by"`
	ApprovedHead string       `db:"approved_head"`
	ApprovedAt   float64      `db:"approved_at"`
	Reason       string       `db:"reason"`
	LastStop     string       `db:"last_stop"`
	HeldBy       string       `db:"held_by"`
	HeldAt       float64      `db:"held_at"`
	Extra        `db:"extra"`
	History      []states.Move
}

func (p *PR) Clone() *PR {
	c := *p
	c.History = append([]states.Move{}, p.History...)
	c.GateApproves = append([]GateApprove(nil), p.GateApproves...)
	if p.Replied != nil {
		c.Replied = make(map[string]float64, len(p.Replied))
		for k, v := range p.Replied {
			c.Replied[k] = v
		}
	}
	return &c
}

const prColumns = "number, state, head, coalesce(title, '') AS title, coalesce(branch, '') AS branch, " +
	"coalesce(author, '') AS author, priority, rollup, last_comment, coalesce(approved_by, '') AS approved_by, " +
	"coalesce(approved_head, '') AS approved_head, coalesce(approved_at, 0) AS approved_at, " +
	"coalesce(reason, '') AS reason, coalesce(last_stop, '') AS last_stop, coalesce(held_by, '') AS held_by, " +
	"coalesce(held_at, 0) AS held_at, extra"

func nullZero(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

func upsertPR(tx db.Execer, repoID int64, pr *PR) error {
	st := pr.State
	if st == "" {
		st = states.Open
	}
	rollup := pr.Rollup
	if rollup == "" {
		rollup = "maybe"
	}
	extra, err := json.Marshal(pr.Extra)
	if err != nil {
		extra = []byte("{}")
	}
	_, err = tx.Exec("INSERT INTO prs (repo_id, number, state, head, title, branch, author, priority, rollup, "+
		"last_comment, approved_by, approved_head, approved_at, reason, last_stop, held_by, held_at, extra, updated_at) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (repo_id, number) DO UPDATE SET "+
		"state = excluded.state, head = excluded.head, title = excluded.title, branch = excluded.branch, "+
		"author = excluded.author, priority = excluded.priority, rollup = excluded.rollup, "+
		"last_comment = excluded.last_comment, approved_by = excluded.approved_by, "+
		"approved_head = excluded.approved_head, approved_at = excluded.approved_at, reason = excluded.reason, "+
		"last_stop = excluded.last_stop, held_by = excluded.held_by, held_at = excluded.held_at, "+
		"extra = excluded.extra, updated_at = excluded.updated_at",
		repoID, pr.Number, string(st), pr.Head, pr.Title, pr.Branch, pr.Author, pr.Priority, rollup, pr.LastComment,
		pr.ApprovedBy, pr.ApprovedHead, pr.ApprovedAt, pr.Reason, pr.LastStop, pr.HeldBy, nullZero(pr.HeldAt),
		string(extra), db.Now())
	return err
}

type ServiceData struct {
	PRs          map[string]*PR
	BackoffUntil *float64
	BackoffWhy   string
}

func (d *ServiceData) Until() float64 {
	if d.BackoffUntil == nil {
		return 0
	}
	return *d.BackoffUntil
}

type ServiceState struct {
	Repo *db.Repo
	DB   *db.DB
}

func NewServiceState(repo *db.Repo) *ServiceState { return &ServiceState{Repo: repo, DB: repo.DB} }

func (s *ServiceState) Load() (*ServiceData, error) {
	rid := s.Repo.ID
	rows, err := db.Select[PR](s.DB, "SELECT "+prColumns+" FROM prs WHERE repo_id = ? ORDER BY number", rid)
	if err != nil {
		return nil, err
	}
	prs := map[string]*PR{}
	for i := range rows {
		pr := &rows[i]
		pr.History = []states.Move{}
		prs[strconv.Itoa(pr.Number)] = pr
	}
	type move struct {
		PR    int    `db:"pr"`
		At    string `db:"at"`
		Event string `db:"event"`
		Old   string `db:"old"`
		New   string `db:"new"`
		Why   string `db:"why"`
	}
	hist, err := db.Select[move](s.DB, "SELECT pr, at, event, old, new, why FROM (SELECT *, row_number() OVER "+
		"(PARTITION BY pr ORDER BY id DESC) AS rn FROM pr_transitions WHERE repo_id = ?) WHERE rn <= 30 "+
		"ORDER BY pr, id", rid)
	if err != nil {
		return nil, err
	}
	for _, h := range hist {
		if pr, ok := prs[strconv.Itoa(h.PR)]; ok {
			pr.History = append(pr.History, states.Move{h.At, h.Event, h.Old, h.New, h.Why})
		}
	}
	data := &ServiceData{PRs: prs}
	type backoff struct {
		Until *float64 `db:"backoff_until"`
		Why   *string  `db:"backoff_why"`
	}
	r, err := db.Select[backoff](s.DB, "SELECT backoff_until, backoff_why FROM repos WHERE id = ?", rid)
	if err != nil {
		return nil, err
	}
	if len(r) > 0 && r[0].Until != nil {
		data.BackoffUntil = r[0].Until
		if r[0].Why != nil {
			data.BackoffWhy = *r[0].Why
		}
	}
	return data, nil
}

func (s *ServiceState) Save(data *ServiceData) error {
	return s.DB.Tx(func(tx *db.Tx) error {
		for _, pr := range data.PRs {
			if err := upsertPR(tx, s.Repo.ID, pr); err != nil {
				return err
			}
		}
		if data.BackoffUntil != nil {
			_, err := tx.Exec("UPDATE repos SET backoff_until = ?, backoff_why = ? WHERE id = ?",
				*data.BackoffUntil, data.BackoffWhy, s.Repo.ID)
			return err
		}
		return nil
	})
}

func (s *ServiceState) SavePR(pr *PR) error {
	return s.DB.Tx(func(tx *db.Tx) error { return upsertPR(tx, s.Repo.ID, pr) })
}

func (s *ServiceState) SetBackoff(until float64, why string) error {
	return s.DB.Tx(func(tx *db.Tx) error {
		_, err := tx.Exec("UPDATE repos SET backoff_until = ?, backoff_why = ? WHERE id = ?", until, why, s.Repo.ID)
		return err
	})
}

func (s *ServiceState) ClearBackoff() error {
	return s.DB.Tx(func(tx *db.Tx) error {
		_, err := tx.Exec("UPDATE repos SET backoff_until = NULL, backoff_why = NULL WHERE id = ?", s.Repo.ID)
		return err
	})
}

func (s *ServiceState) Transition(pr *PR, event states.Event, why string, fields func(*PR)) (states.State, error) {
	var next *PR
	err := s.DB.Tx(func(tx *db.Tx) error {
		var err error
		next, err = s.TransitionTx(tx, pr, event, why, fields)
		return err
	})
	if err != nil {
		return "", err
	}
	*pr = *next
	return next.State, nil
}

func Next(pr *PR, event states.Event, why string, fields func(*PR)) (*PR, states.Move, error) {
	next := pr.Clone()
	st, hist, err := states.Transition(next.Number, next.State, next.History, event, why)
	if err != nil {
		return nil, states.Move{}, err
	}
	next.State, next.History = st, hist
	if fields != nil {
		fields(next)
	}
	return next, hist[len(hist)-1], nil
}

func (s *ServiceState) TransitionTx(tx *db.Tx, pr *PR, event states.Event, why string, fields func(*PR)) (*PR, error) {
	next, last, err := Next(pr, event, why, fields)
	if err != nil {
		return nil, err
	}
	if err := upsertPR(tx, s.Repo.ID, next); err != nil {
		return nil, err
	}
	if err := db.AddTransition(tx, s.Repo.ID, next.Number, last[:]); err != nil {
		return nil, err
	}
	return next, nil
}

func (s *ServiceState) SaveMoves(pr *PR, moves []states.Move) error {
	return s.DB.Tx(func(tx *db.Tx) error {
		if err := upsertPR(tx, s.Repo.ID, pr); err != nil {
			return err
		}
		for _, m := range moves {
			if err := db.AddTransition(tx, s.Repo.ID, pr.Number, m[:]); err != nil {
				return err
			}
		}
		return nil
	})
}

type Transition struct {
	At     string  `json:"at"`
	AtUnix float64 `json:"at_unix"`
	Event  string  `json:"event"`
	ID     int64   `json:"id"`
	New    string  `json:"new"`
	Old    string  `json:"old"`
	PR     int64   `json:"pr"`
	Repo   string  `json:"repo,omitempty"`
	RepoID int64   `json:"repo_id"`
	Why    string  `json:"why"`
}

func (s *ServiceState) Transitions(number int) ([]Transition, error) {
	return db.Select[Transition](s.DB, "SELECT * FROM pr_transitions WHERE repo_id = ? AND pr = ? ORDER BY id",
		s.Repo.ID, number)
}

type Outcome struct {
	Status   states.OutcomeStatus
	Why      string
	Log      string
	Line     string
	Sha      string
	Partners []string
}

type Member struct {
	Name       string
	Ref        string
	PR         *int
	Branch     *string
	Head       *string
	Priority   *int
	ApprovedAt *float64
	ApprovedBy *string
}

type History struct {
	Repo *db.Repo
	DB   *db.DB
}

func NewHistory(repo *db.Repo) *History { return &History{Repo: repo, DB: repo.DB} }

func (h *History) Begin(source, mode, target, base, args string, members []Member) (int64, error) {
	var id int64
	err := h.DB.Tx(func(tx *db.Tx) error {
		var err error
		id, err = h.BeginTx(tx, source, mode, target, base, args, members)
		return err
	})
	return id, err
}

func ptr[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func (h *History) BeginTx(tx *db.Tx, source, mode, target, base, args string, members []Member) (int64, error) {
	res, err := tx.Exec("INSERT INTO batches (repo_id, source, mode, target, base, args, pid, host, started_at) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", h.Repo.ID, source, mode, target, base, args, osPid(), db.Host, db.Now())
	if err != nil {
		return 0, err
	}
	bid, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	for pos, m := range members {
		if seen[m.Name] {
			continue
		}
		seen[m.Name] = true
		if _, err := tx.Exec("INSERT INTO batch_members (batch, position, name, ref, pr, branch, head, priority, "+
			"approved_at, approved_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", bid, pos, m.Name, m.Ref, ptr(m.PR),
			ptr(m.Branch), ptr(m.Head), ptr(m.Priority), ptr(m.ApprovedAt), ptr(m.ApprovedBy)); err != nil {
			return 0, err
		}
	}
	return bid, nil
}

func memberStatus(tx db.Execer, batch int64, name string) (states.OutcomeStatus, bool, error) {
	type row struct {
		Status string `db:"status"`
	}
	rows, err := db.Select[row](tx, "SELECT status FROM batch_members WHERE batch = ? AND name = ?", batch, name)
	if err != nil || len(rows) == 0 {
		return "", false, err
	}
	return states.OutcomeOf(rows[0].Status), true, nil
}

func setOutcome(tx db.Execer, batch int64, name string, o Outcome) error {
	to, err := states.OutcomeMachine.Step(states.OutcomePending, o.Status)
	if err != nil {
		return err
	}
	st, err := runState(tx, batch)
	if err != nil {
		return err
	}
	if st != states.BatchRunRunning {
		return &states.InvalidTransition{Msg: fmt.Sprintf("queue run %d is %s: %s's outcome cannot change", batch, st, name)}
	}
	res, err := tx.Exec("UPDATE batch_members SET status = ?, why = ?, sha = ?, log = ?, line = ?, partners = ? "+
		"WHERE batch = ? AND name = ? AND status = ?", string(to), o.Why, o.Sha, o.Log, o.Line,
		strings.Join(o.Partners, " "), batch, name, string(states.OutcomePending))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	now, ok, err := memberStatus(tx, batch, name)
	if err != nil {
		return err
	}
	if !ok {
		return &states.InvalidTransition{Msg: fmt.Sprintf("queue run %d has no member %s", batch, name)}
	}
	_, err = states.OutcomeMachine.Step(now, o.Status)
	return err
}

func (h *History) Outcome(batch int64, name string, o Outcome) error {
	return h.DB.Tx(func(tx *db.Tx) error { return setOutcome(tx, batch, name, o) })
}

func runState(tx db.Execer, batch int64) (states.BatchRunState, error) {
	type row struct {
		Finished *float64 `db:"finished_at"`
		Code     *int64   `db:"code"`
	}
	rows, err := db.Select[row](tx, "SELECT finished_at, code FROM batches WHERE id = ?", batch)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", &states.InvalidTransition{Msg: "queue run: no run " + strconv.FormatInt(batch, 10)}
	}
	return states.BatchRunOf(rows[0].Finished != nil, rows[0].Code != nil), nil
}

func (h *History) Finish(batch int64, code, gates int, stopped, result, note string, outcomes map[string]Outcome) error {
	return h.DB.Tx(func(tx *db.Tx) error {
		st, err := runState(tx, batch)
		if err != nil {
			return err
		}
		if _, err := states.BatchRunMachine.Step(st, states.BatchRunFinish); err != nil {
			return err
		}
		for name, o := range outcomes {
			now, ok, err := memberStatus(tx, batch, name)
			if err != nil {
				return err
			}
			if ok && now == states.OutcomePending {
				if err := setOutcome(tx, batch, name, o); err != nil {
					return err
				}
			}
		}
		untried := states.OutcomeMachine.Must(states.OutcomePending, states.OutcomeNotTried)
		if _, err := tx.Exec("UPDATE batch_members SET status = ?, why = ? WHERE batch = ? AND status = ?", string(untried),
			db.NullIfEmpty(note), batch, string(states.OutcomePending)); err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE batches SET finished_at = ?, code = ?, gates = ?, stopped = ?, result = ?, note = ? "+
			"WHERE id = ?", db.Now(), code, gates, db.NullIfEmpty(stopped), result, db.NullIfEmpty(note), batch)
		return err
	})
}

func (h *History) Interrupted(source, why string) (int, error) {
	return h.interrupted(source, why, nil)
}

func (h *History) InterruptedGone(source, why string) (int, error) {
	return h.interrupted(source, why, func(pid *int64, host *string) bool {
		return host != nil && *host == db.Host && pid != nil && !pidAlive(int(*pid))
	})
}

func (h *History) interrupted(source, why string, gone func(pid *int64, host *string) bool) (int, error) {
	n := 0
	err := h.DB.Tx(func(tx *db.Tx) error {
		type row struct {
			ID   int64   `db:"id"`
			Pid  *int64  `db:"pid"`
			Host *string `db:"host"`
		}
		rows, err := db.Select[row](tx, "SELECT id, pid, host FROM batches WHERE repo_id = ? AND source = ? AND "+
			"finished_at IS NULL", h.Repo.ID, source)
		if err != nil {
			return err
		}
		var ids []int64
		for _, r := range rows {
			if gone == nil || gone(r.Pid, r.Host) {
				ids = append(ids, r.ID)
			}
		}
		member := states.OutcomeMachine.Must(states.OutcomePending, states.OutcomeInterrupted)
		for _, id := range ids {
			st, err := runState(tx, id)
			if err != nil {
				return err
			}
			if _, err := states.BatchRunMachine.Step(st, states.BatchRunInterrupt); err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE batch_members SET status = ? WHERE batch = ? AND status = ?", string(member), id,
				string(states.OutcomePending)); err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE batches SET finished_at = ?, note = ? WHERE id = ?", db.Now(), why,
				id); err != nil {
				return err
			}
		}
		n = len(ids)
		return nil
	})
	return n, err
}

func (h *History) Line(batch int64, kind, branches, line, at string) error {
	var b any
	if batch != 0 {
		b = batch
	}
	return h.DB.Tx(func(tx *db.Tx) error {
		_, err := tx.Exec("INSERT INTO merges (repo_id, batch, at, at_unix, kind, branches, line) VALUES (?, ?, ?, ?, ?, ?, ?)",
			h.Repo.ID, b, at, db.Now(), kind, branches, line)
		return err
	})
}

func memberFilter(branch string, pr *int) (string, []any) {
	var conds []string
	var args []any
	if branch != "" {
		conds = append(conds, "(m.name = ? OR m.branch = ? OR m.ref = ?)")
		args = append(args, branch, branch, branch)
	}
	if pr != nil {
		conds = append(conds, "m.pr = ?")
		args = append(args, *pr)
	}
	return strings.Join(conds, " AND "), args
}

type Words []string

func (w *Words) Scan(src any) error {
	var x string
	switch v := src.(type) {
	case string:
		x = v
	case []byte:
		x = string(v)
	}
	*w = strings.Fields(x)
	return nil
}

type BatchMember struct {
	ApprovedAt *float64 `json:"approved_at"`
	ApprovedBy *string  `json:"approved_by"`
	Batch      int64    `json:"batch"`
	Branch     *string  `json:"branch"`
	Head       *string  `json:"head"`
	Line       *string  `json:"line"`
	Log        *string  `json:"log"`
	Name       string   `json:"name"`
	Partners   Words    `json:"partners"`
	Position   int64    `json:"position"`
	PR         *int64   `json:"pr"`
	Priority   *int64   `json:"priority"`
	Ref        string   `json:"ref"`
	Sha        *string  `json:"sha"`
	Status     string   `json:"status"`
	Why        *string  `json:"why"`
}

type Batch struct {
	Args       string        `json:"args"`
	Base       string        `json:"base"`
	Code       *int64        `json:"code"`
	FinishedAt *float64      `json:"finished_at"`
	Gates      *int64        `json:"gates"`
	Host       *string       `json:"host"`
	ID         int64         `json:"id"`
	Members    []BatchMember `json:"members"`
	Mode       string        `json:"mode"`
	Note       *string       `json:"note"`
	Pid        *int64        `json:"pid"`
	Repo       string        `json:"repo,omitempty"`
	RepoID     int64         `json:"repo_id"`
	Result     *string       `json:"result"`
	Source     string        `json:"source"`
	StartedAt  float64       `json:"started_at"`
	Stopped    *string       `json:"stopped"`
	Target     string        `json:"target"`
}

func (h *History) Batches(branch string, pr *int, limit int) ([]Batch, error) {
	where, args := memberFilter(branch, pr)
	q := "SELECT * FROM batches WHERE repo_id = ?"
	if where != "" {
		q += " AND id IN (SELECT m.batch FROM batch_members m WHERE " + where + ")"
	}
	all := append([]any{h.Repo.ID}, args...)
	all = append(all, limit)
	rows, err := db.Select[Batch](h.DB, q+" ORDER BY id DESC LIMIT ?", all...)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		ms, err := db.Select[BatchMember](h.DB, "SELECT * FROM batch_members WHERE batch = ? ORDER BY position", rows[i].ID)
		if err != nil {
			return nil, err
		}
		if ms == nil {
			ms = []BatchMember{}
		}
		rows[i].Members = ms
	}
	return rows, nil
}

type Merge struct {
	At       string  `json:"at"`
	AtUnix   float64 `json:"at_unix"`
	Batch    *int64  `json:"batch"`
	Branches string  `json:"branches"`
	ID       int64   `json:"id"`
	Kind     string  `json:"kind"`
	Line     string  `json:"line"`
	Repo     string  `json:"repo,omitempty"`
	RepoID   int64   `json:"repo_id"`
}

func (h *History) Merges(branch string, pr *int, limit int) ([]Merge, error) {
	conds := []string{"repo_id = ?"}
	args := []any{h.Repo.ID}
	var names []string
	if branch != "" {
		names = append(names, branch)
	}
	if pr != nil {
		names = append(names, "#"+strconv.Itoa(*pr))
	}
	for _, n := range names {
		conds = append(conds, "instr(' ' || branches || ' ', ' ' || ? || ' ') > 0")
		args = append(args, n)
	}
	args = append(args, limit)
	return db.Select[Merge](h.DB, "SELECT * FROM merges WHERE "+strings.Join(conds, " AND ")+" ORDER BY id DESC LIMIT ?", args...)
}

func osPid() int { return os.Getpid() }

func PidAlive(pid int) bool { return pidAlive(pid) }

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return true
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

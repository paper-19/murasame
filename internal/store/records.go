package store

import (
	"fmt"
	"slices"

	"murasame/internal/config"
	"murasame/internal/db"
)

type Found struct {
	OK       bool
	Why      string
	When     string
	Log      string
	GateHash string
}

type GateRecords struct {
	Spec config.RecordsSpec
	Now  func() float64
	dbFn func() (*db.Repo, error)
}

func NewGateRecords(spec config.RecordsSpec, dbFn func() (*db.Repo, error)) *GateRecords {
	return &GateRecords{Spec: spec, Now: db.Now, dbFn: dbFn}
}

func (g *GateRecords) Enabled() bool { return g.Spec.Enabled }

func (g *GateRecords) db() *db.Repo {
	if g.dbFn == nil {
		panic(fmt.Errorf("GateRecords: no state DB given"))
	}
	r, err := g.dbFn()
	if err != nil {
		panic(err)
	}
	return r
}

func (g *GateRecords) Write(tree, commit, gate, gateHash, log string) error {
	repo := g.db()
	return repo.DB.Tx(func(tx *db.Tx) error {
		_, err := tx.Exec("INSERT OR REPLACE INTO gate_records (repo_id, tree, gate, commit_sha, gate_hash, time, log) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?)", repo.ID, tree, gate, commit, gateHash, int64(g.Now()), log)
		return err
	})
}

func (g *GateRecords) Find(tree, gate string, accept ...string) (found Found) {
	defer func() {
		if r := recover(); r != nil {
			found = Found{Why: fmt.Sprintf("the state DB could not be read (%v)", r)}
		}
	}()
	repo := g.db()
	type record struct {
		GateHash string `db:"gate_hash"`
		Time     int64  `db:"time"`
		Log      string `db:"log"`
	}
	rows, err := db.Select[record](repo.DB, "SELECT gate_hash, time, log FROM gate_records WHERE repo_id = ? AND tree = ? AND gate = ?",
		repo.ID, tree, gate)
	if err != nil {
		panic(err)
	}
	if len(rows) == 0 {
		return Found{Why: "no record"}
	}
	r := rows[0]
	t := r.Time
	age := int64(g.Now()) - t
	switch {
	case !slices.Contains(accept, r.GateHash):
		return Found{Why: fmt.Sprintf("the %s record was written for other required CI contexts", gate),
			GateHash: r.GateHash}
	case age < -300 || age > int64(g.Spec.MaxAge):
		return Found{Why: fmt.Sprintf("the %s record is %d s old (at most %d)", gate, age, g.Spec.MaxAge),
			GateHash: r.GateHash}
	}
	return Found{OK: true, When: db.Time(float64(t)).Format("2006-01-02 15:04:05"), Log: r.Log, GateHash: r.GateHash}
}

func (g *GateRecords) Landed(tree, gate, commit string) error {
	repo := g.db()
	_, err := repo.DB.Exec("UPDATE gate_records SET commit_sha = ? WHERE repo_id = ? AND tree = ? AND gate = ?", commit,
		repo.ID, tree, gate)
	return err
}

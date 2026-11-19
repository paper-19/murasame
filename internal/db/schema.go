package db

import "fmt"

const SchemaVersion = 1

var schema = []string{
	`CREATE TABLE repos (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            key TEXT NOT NULL UNIQUE,
            kind TEXT NOT NULL,
            forge_url TEXT,
            owner TEXT,
            name TEXT,
            path TEXT,
            backoff_until REAL,
            backoff_why TEXT,
            created_at REAL NOT NULL
        )`,
	`CREATE TABLE prs (
            repo_id INTEGER NOT NULL REFERENCES repos (id),
            number INTEGER NOT NULL,
            state TEXT NOT NULL CHECK (state IN ('open', 'queued', 'testing', 'set_aside', 'held', 'merged',
                                                 'closed')),
            head TEXT NOT NULL DEFAULT '',
            title TEXT,
            branch TEXT,
            author TEXT,
            priority INTEGER NOT NULL DEFAULT 0,
            rollup TEXT NOT NULL DEFAULT 'maybe',
            last_comment INTEGER NOT NULL DEFAULT 0,
            approved_by TEXT,
            approved_head TEXT,
            approved_at REAL,
            reason TEXT,
            last_stop TEXT,
            held_by TEXT,
            held_at REAL,
            extra TEXT NOT NULL DEFAULT '{}',
            updated_at REAL NOT NULL,
            PRIMARY KEY (repo_id, number)
        )`,
	"CREATE INDEX prs_line ON prs (repo_id, state, priority DESC, approved_at, number)",
	`CREATE TABLE pr_transitions (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            repo_id INTEGER NOT NULL,
            pr INTEGER NOT NULL,
            at TEXT NOT NULL,
            at_unix REAL NOT NULL,
            event TEXT NOT NULL,
            old TEXT NOT NULL,
            new TEXT NOT NULL,
            why TEXT NOT NULL DEFAULT '',
            FOREIGN KEY (repo_id, pr) REFERENCES prs (repo_id, number)
        )`,
	"CREATE INDEX pr_transitions_pr ON pr_transitions (repo_id, pr, id)",
	`CREATE TABLE batches (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            repo_id INTEGER NOT NULL REFERENCES repos (id),
            source TEXT NOT NULL,
            mode TEXT NOT NULL,
            target TEXT NOT NULL DEFAULT '',
            base TEXT NOT NULL DEFAULT '',
            result TEXT,
            args TEXT NOT NULL DEFAULT '',
            pid INTEGER,
            host TEXT,
            started_at REAL NOT NULL,
            finished_at REAL,
            code INTEGER,
            gates INTEGER,
            stopped TEXT,
            note TEXT
        )`,
	"CREATE INDEX batches_repo ON batches (repo_id, id)",
	`CREATE TABLE batch_members (
            batch INTEGER NOT NULL REFERENCES batches (id),
            position INTEGER NOT NULL,
            name TEXT NOT NULL,
            ref TEXT NOT NULL DEFAULT '',
            pr INTEGER,
            branch TEXT,
            head TEXT,
            priority INTEGER,
            approved_at REAL,
            approved_by TEXT,
            status TEXT NOT NULL DEFAULT 'pending',
            why TEXT,
            sha TEXT,
            log TEXT,
            line TEXT,
            partners TEXT,
            PRIMARY KEY (batch, position)
        )`,
	"CREATE INDEX batch_members_name ON batch_members (name)",
	"CREATE INDEX batch_members_branch ON batch_members (branch)",
	"CREATE INDEX batch_members_pr ON batch_members (pr)",
	`CREATE TABLE merges (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            repo_id INTEGER NOT NULL REFERENCES repos (id),
            batch INTEGER REFERENCES batches (id),
            at TEXT NOT NULL,
            at_unix REAL NOT NULL,
            kind TEXT NOT NULL,
            branches TEXT NOT NULL DEFAULT '',
            line TEXT NOT NULL
        )`,
	"CREATE INDEX merges_batch ON merges (batch)",
	"CREATE INDEX merges_repo ON merges (repo_id, id)",
	`CREATE TABLE gate_records (
            repo_id INTEGER NOT NULL REFERENCES repos (id),
            tree TEXT NOT NULL,
            gate TEXT NOT NULL,
            commit_sha TEXT NOT NULL,
            gate_hash TEXT NOT NULL,
            time INTEGER NOT NULL,
            log TEXT NOT NULL,
            PRIMARY KEY (repo_id, tree, gate)
        ) WITHOUT ROWID`,
}

func versionError(path string, v int) error {
	return &Error{fmt.Sprintf("%s has schema version %d, this murasame uses %d: move it aside and start again", path, v, SchemaVersion)}
}

func (d *DB) initSchema() error {
	return d.Tx(func(tx *Tx) error {
		rows, err := Ints(tx, "PRAGMA user_version")
		if err != nil || len(rows) == 0 {
			return err
		}
		v := int(rows[0])
		if v == SchemaVersion {
			return nil
		}
		if v != 0 {
			return versionError(d.Path, v)
		}
		tables, err := Ints(tx, "SELECT count(*) FROM sqlite_master")
		if err != nil {
			return err
		}
		if len(tables) == 0 || tables[0] > 0 {
			return versionError(d.Path, v)
		}
		for _, q := range schema {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		_, err = tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion))
		return err
	})
}

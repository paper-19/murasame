package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const DefaultName = "murasame.sqlite3"

const BusyTimeoutMS = 30_000

var Host = func() string { h, _ := os.Hostname(); return h }()

type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

var ErrEmpty = &Error{"the state DB is empty: no history yet"}

func ForgeHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "unknown-host"
	}
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" {
		if port, err := strconv.Atoi(p); err == nil && port >= 0 && port <= 65535 {
			if !(u.Scheme == "http" && port == 80) && !(u.Scheme == "https" && port == 443) {
				host = fmt.Sprintf("%s:%d", host, port)
			}
		}
	}
	if host == "" {
		return "unknown-host"
	}
	return host
}

func LocalKey(main string) string { return "local:" + main }

func ForgeKey(host, owner, name string) string {
	return fmt.Sprintf("forgejo:%s/%s/%s", host, owner, name)
}

func Path(stateDir, name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(stateDir, name)
}

type Execer interface {
	Exec(q string, args ...any) (sql.Result, error)
	Rows(q string, args ...any) (*sql.Rows, error)
}

type DB struct {
	Path     string
	Readonly bool
	sql      *sql.DB
}

var BeforeCommit func() error

var uriPath = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

func fileURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "file:" + uriPath.Replace(filepath.ToSlash(path))
	}
	abs = filepath.ToSlash(abs)
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + abs
	}
	return "file://" + uriPath.Replace(abs)
}

func dsn(path string, readonly bool) string {
	q := "_pragma=busy_timeout(" + strconv.Itoa(BusyTimeoutMS) + ")&_pragma=foreign_keys(1)"
	if readonly {
		return fileURI(path) + "?mode=ro&" + q
	}
	return fileURI(path) + "?" + q + "&_pragma=synchronous(FULL)"
}

func (d *DB) setWAL() error {
	var err error
	for i := 0; i < 50; i++ {
		var mode string
		if err = d.sql.QueryRow("PRAGMA journal_mode = WAL").Scan(&mode); err == nil {
			if !strings.EqualFold(mode, "wal") {
				return &Error{fmt.Sprintf("%s: SQLite refused WAL mode (journal_mode is %s): murasame needs it so that readers never "+
					"wait for a running queue; put the state DB on a local filesystem", d.Path, mode)}
			}
			return nil
		}
		if m := err.Error(); !strings.Contains(m, "locked") && !strings.Contains(m, "busy") {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

func Open(path string, readonly bool) (*DB, error) {
	d := &DB{Path: path, Readonly: readonly}
	if readonly {
		if _, err := os.Stat(path); err != nil {
			return nil, &Error{"no state DB at " + path}
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s, err := sql.Open("sqlite", dsn(path, readonly))
	if err != nil {
		return nil, err
	}
	d.sql = s
	if err := d.checkVersion(s); err != nil {
		s.Close()
		return nil, err
	}
	if readonly {
		return d, nil
	}
	if err := d.setWAL(); err != nil {
		s.Close()
		return nil, err
	}
	if err := d.initSchema(); err != nil {
		s.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	return d.sql.Close()
}

func (d *DB) checkVersion(q interface {
	QueryRow(string, ...any) *sql.Row
}) error {
	var v, tables int
	if err := q.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v == SchemaVersion {
		return nil
	}
	if v != 0 {
		return versionError(d.Path, v)
	}
	if err := q.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&tables); err != nil {
		return err
	}
	if tables > 0 {
		return versionError(d.Path, v)
	}
	if d.Readonly {
		return ErrEmpty
	}
	return nil
}

func (d *DB) SchemaVersion() (int, error) {
	var v int
	err := d.sql.QueryRow("PRAGMA user_version").Scan(&v)
	return v, err
}

func (d *DB) Exec(q string, args ...any) (sql.Result, error) { return d.sql.Exec(q, args...) }

func (d *DB) Rows(q string, args ...any) (*sql.Rows, error) { return d.sql.Query(q, args...) }

func fieldsByColumn(t reflect.Type) map[string]int {
	out := map[string]int{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := f.Tag.Get("db")
		if name == "" {
			name, _, _ = strings.Cut(f.Tag.Get("json"), ",")
		}
		if name != "" && name != "-" {
			out[name] = i
		}
	}
	return out
}

func Select[T any](x Execer, q string, args ...any) ([]T, error) {
	rows, err := x.Rows(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	fields := fieldsByColumn(reflect.TypeOf((*T)(nil)).Elem())
	var out []T
	for rows.Next() {
		var v T
		rv := reflect.ValueOf(&v).Elem()
		dest := make([]any, len(cols))
		for i, c := range cols {
			if f, ok := fields[c]; ok {
				dest[i] = rv.Field(f).Addr().Interface()
			} else {
				dest[i] = new(any)
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func Ints(x Execer, q string, args ...any) ([]int64, error) {
	rows, err := x.Rows(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

type Tx struct {
	conn *sql.Conn
	ctx  context.Context
}

func (t *Tx) Exec(q string, args ...any) (sql.Result, error) {
	return t.conn.ExecContext(t.ctx, q, args...)
}

func (t *Tx) Rows(q string, args ...any) (*sql.Rows, error) {
	return t.conn.QueryContext(t.ctx, q, args...)
}

func (d *DB) Tx(fn func(tx *Tx) error) (err error) {
	ctx := context.Background()
	conn, err := d.sql.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	tx := &Tx{conn, ctx}
	done := false
	defer func() {
		if !done {
			conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if BeforeCommit != nil {
		if err := BeforeCommit(); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	done = true
	return nil
}

type Repo struct {
	DB  *DB
	ID  int64
	Key string
}

type RepoRow struct {
	BackoffUntil *float64 `json:"backoff_until"`
	BackoffWhy   *string  `json:"backoff_why"`
	CreatedAt    float64  `json:"created_at"`
	ForgeURL     *string  `json:"forge_url"`
	ID           int64    `json:"id"`
	Key          string   `json:"key"`
	Kind         string   `json:"kind"`
	Name         *string  `json:"name"`
	Owner        *string  `json:"owner"`
	Path         *string  `json:"path"`
}

type RepoInfo struct {
	ForgeURL, Owner, Name, Path string
}

func NullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func Scope(d *DB, key, kind string, info RepoInfo) (*Repo, error) {
	r, err := Ints(d, "SELECT id FROM repos WHERE key = ?", key)
	if err != nil {
		return nil, err
	}
	if len(r) == 0 {
		err := d.Tx(func(tx *Tx) error {
			_, err := tx.Exec("INSERT OR IGNORE INTO repos (key, kind, forge_url, owner, name, path, created_at) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?)", key, kind, NullIfEmpty(info.ForgeURL), NullIfEmpty(info.Owner),
				NullIfEmpty(info.Name), NullIfEmpty(info.Path), Now())
			return err
		})
		if err != nil {
			return nil, err
		}
		if r, err = Ints(d, "SELECT id FROM repos WHERE key = ?", key); err != nil {
			return nil, err
		}
	}
	return &Repo{d, r[0], key}, nil
}

func AddTransition(tx Execer, repoID int64, number int, h []string) error {
	for len(h) < 5 {
		h = append(h, "")
	}
	_, err := tx.Exec("INSERT INTO pr_transitions (repo_id, pr, at, at_unix, event, old, new, why) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?)", repoID, number, h[0], Now(), h[1], h[2], h[3], h[4])
	return err
}

func Now() float64 { return float64(time.Now().UnixMicro()) / 1e6 }

func Time(f float64) time.Time { return time.UnixMicro(int64(f * 1e6)) }

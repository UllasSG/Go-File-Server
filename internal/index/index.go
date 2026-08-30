package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	_ "modernc.org/sqlite"
)

type Entry struct {
	Path   string
	Parent string
	Name   string
	Ext    string
	Size   int64
	MTime  int64
	IsDir  bool
}

type Index struct {
	db *sql.DB
}

const schema = `CREATE TABLE IF NOT EXISTS entries (
  path   TEXT PRIMARY KEY,  -- '/photos/img.jpg' — leading slash, no trailing
  parent TEXT NOT NULL,     -- '/photos'  (root itself has parent '')
  name   TEXT NOT NULL,     -- 'img.jpg'
  ext    TEXT NOT NULL,     -- 'jpg', lowercased; '' for dirs and extensionless
  size   INTEGER NOT NULL,  -- bytes; 0 for dirs
  mtime  INTEGER NOT NULL,  -- unix seconds
  is_dir INTEGER NOT NULL,  -- 0 or 1
  gen    INTEGER NOT NULL   -- scan generation, for mark-and-sweep
) STRICT;

CREATE INDEX IF NOT EXISTS idx_parent ON entries(parent, name);
CREATE INDEX IF NOT EXISTS idx_name   ON entries(name);
CREATE INDEX IF NOT EXISTS idx_gen    ON entries(gen);
`

func Open(p string) (*Index, error) {
	dsn := "file:" + p + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Index{db: db}, nil
}

func (i *Index) Close() error {
	return i.db.Close()
}

func (i *Index) Stat(ctx context.Context, p string) (Entry, error) {
	query := "SELECT path, parent, name, ext, size, mtime, is_dir FROM entries WHERE path = ?"
	res := Entry{}
	err := i.db.QueryRowContext(ctx, query, p).Scan(&res.Path, &res.Parent, &res.Name, &res.Ext, &res.Size, &res.MTime, &res.IsDir)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Entry{}, fs.ErrNotExist
		}
		return Entry{}, err

	}
	return res, nil

}

const upsertQuery = `
INSERT INTO entries(path, parent, name, ext, size, mtime, is_dir, gen)
VALUES(?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(path) DO UPDATE SET
  size  = excluded.size,
  mtime = excluded.mtime,
  gen   = excluded.gen`

func (i *Index) Upsert(ctx context.Context, e Entry, gen int64) error {

	_, err := i.db.ExecContext(ctx, upsertQuery, e.Path, e.Parent, e.Name, e.Ext, e.Size, e.MTime, e.IsDir, gen)
	return err
}

func (i *Index) Delete(ctx context.Context, p string) error {
	query := "DELETE FROM entries WHERE path = ?"
	_, err := i.db.ExecContext(ctx, query, p)
	return err
}

func (i *Index) List(ctx context.Context, parent string, sort string, order string, limit int64, offset int64) ([]Entry, int64, error) {
	sortOptions := map[string]string{"name": "name", "size": "size", "mtime": "mtime"}
	orderOptions := map[string]string{"asc": "asc", "desc": "desc"}
	selectedSort, ok := sortOptions[sort]
	if !ok {
		selectedSort = sortOptions["name"]
	}
	selectedOrder, ok := orderOptions[order]
	if !ok {
		selectedOrder = orderOptions["asc"]
	}

	query := fmt.Sprintf("SELECT path, parent, name, ext, size, mtime, is_dir FROM entries WHERE parent = ? ORDER BY is_dir DESC, %s %s LIMIT ? OFFSET ?", selectedSort, selectedOrder)
	if limit <= 0 {
		limit = 100
	} else if limit > 1000 {
		limit = 1000
	}

	rows, err := i.db.QueryContext(ctx, query, parent, limit, offset)

	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := []Entry{}

	for rows.Next() {
		entry := Entry{}

		if err := rows.Scan(
			&entry.Path,
			&entry.Parent,
			&entry.Name,
			&entry.Ext,
			&entry.Size,
			&entry.MTime,
			&entry.IsDir,
		); err != nil {
			return nil, 0, err
		}

		result = append(result, entry)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	query = "SELECT COUNT(*) FROM entries WHERE parent = ?"
	var count int64
	err = i.db.QueryRowContext(ctx, query, parent).Scan(&count)
	if err != nil {
		return nil, 0, err
	}
	return result, count, nil

}

func (i *Index) Search(ctx context.Context, q string, ext string, limit int64, offset int64) ([]Entry, int64, error) {

	query := `
SELECT path, parent, name, ext, size, mtime, is_dir FROM entries
WHERE name LIKE ? ESCAPE '\'
  AND (? = '' OR ext = ?)
  AND is_dir = 0
ORDER BY name
LIMIT ? OFFSET ?`
	r := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	)
	q = r.Replace(q)
	pattern := "%" + q + "%"
	rows, err := i.db.QueryContext(ctx, query, pattern, ext, ext, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := []Entry{}

	for rows.Next() {
		entry := Entry{}

		if err := rows.Scan(
			&entry.Path,
			&entry.Parent,
			&entry.Name,
			&entry.Ext,
			&entry.Size,
			&entry.MTime,
			&entry.IsDir,
		); err != nil {
			return nil, 0, err
		}

		result = append(result, entry)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	query = `
SELECT COUNT(*) FROM entries
WHERE name LIKE ? ESCAPE '\'
  AND (? = '' OR ext = ?)
  AND is_dir = 0`
	var count int64
	err = i.db.QueryRowContext(ctx, query, pattern, ext, ext).Scan(&count)
	if err != nil {
		return nil, 0, err
	}
	return result, count, nil

}

func (i *Index) NextGen(ctx context.Context) (int64, error) {
	query := "SELECT COALESCE(MAX(gen), 0) + 1 FROM entries"
	var nextGen int64
	err := i.db.QueryRowContext(ctx, query).Scan(&nextGen)
	return nextGen, err
}

func (i *Index) Sweep(ctx context.Context, gen int64) (int64, error) {
	query := "DELETE FROM entries WHERE gen < ?"
	res, err := i.db.ExecContext(ctx, query, gen)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

type ScanTx struct {
	tx   *sql.Tx
	stmt *sql.Stmt
}

func (i *Index) BeginScan(ctx context.Context) (*ScanTx, error) {
	scTx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	scStmt, err := scTx.PrepareContext(ctx, upsertQuery)
	if err != nil {
		_ = scTx.Rollback()
		return nil, err
	}
	return &ScanTx{tx: scTx, stmt: scStmt}, nil

}

func (tx *ScanTx) Upsert(ctx context.Context, e Entry, gen int64) error {
	_, err := tx.stmt.ExecContext(ctx, e.Path, e.Parent, e.Name, e.Ext, e.Size, e.MTime, e.IsDir, gen)
	return err
}

func (tx *ScanTx) Commit() error {
	_ = tx.stmt.Close()
	return tx.tx.Commit()
}

func (tx *ScanTx) Rollback() error {
	_ = tx.stmt.Close()
	err := tx.tx.Rollback()
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return err

}

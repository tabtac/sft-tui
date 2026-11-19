package queue

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"sft-tui/internal/pal"
)

type Store struct {
	db   *sql.DB
	lock *os.File
	path string
}

func OpenStore(dir, identity string) (*Store, error) {
	if err := pal.EnsurePrivateDir(dir); err != nil {
		return nil, err
	}

	lockPath := filepath.Join(dir, ".queue-"+identity+".lock")
	lock, err := pal.OpenLockFile(lockPath)
	if err != nil {
		return nil, err
	}
	if err := pal.LockFile(lock); err != nil {
		lock.Close()
		return nil, fmt.Errorf("queue for identity %q is locked by another instance: %w", identity, err)
	}

	failed := true
	defer func() {
		if failed {
			pal.UnlockFile(lock)
			lock.Close()
		}
	}()

	dbPath, err := filepath.Abs(filepath.Join(dir, "queue-"+identity+".sqlite3"))
	if err != nil {
		return nil, err
	}
	for _, fName := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := pal.CheckRegularOrAbsent(fName); err != nil {
			return nil, err
		}
	}

	dsn, err := pal.SQLiteDSN(dbPath, "_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(3000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	closeFailure := func(e error) (*Store, error) {
		db.Close()
		return nil, e
	}

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return closeFailure(err)
	}
	if version > 1 {
		return closeFailure(fmt.Errorf("queue database is from a newer version (%d > 1)", version))
	}

	schema := `
		PRAGMA journal_mode=WAL;
		PRAGMA synchronous=FULL;
		PRAGMA foreign_keys=ON;
		PRAGMA busy_timeout=3000;
		CREATE TABLE IF NOT EXISTS tasks(id TEXT PRIMARY KEY, record BLOB NOT NULL);
		CREATE TABLE IF NOT EXISTS history(seq INTEGER PRIMARY KEY AUTOINCREMENT, record BLOB NOT NULL, updated INTEGER NOT NULL);
		PRAGMA user_version=1;
	`
	if _, err := db.Exec(schema); err != nil {
		return closeFailure(err)
	}

	var syncMode int
	if err := db.QueryRow("PRAGMA synchronous;").Scan(&syncMode); err != nil {
		return closeFailure(err)
	}
	if syncMode != 2 && syncMode != 3 {
		return closeFailure(fmt.Errorf("queue sqlite synchronous pragma failed: expected FULL (2), got %d", syncMode))
	}
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		return closeFailure(err)
	}
	if strings.ToUpper(journalMode) != "WAL" {
		return closeFailure(fmt.Errorf("queue sqlite journal_mode pragma failed: expected WAL, got %s", journalMode))
	}

	failed = false
	return &Store{
		db:   db,
		lock: lock,
		path: dbPath,
	}, nil
}

func (s *Store) Close() error {
	err := s.db.Close()
	pal.UnlockFile(s.lock)
	if e := s.lock.Close(); err == nil {
		err = e
	}
	return err
}

func (s *Store) Load(history bool) ([]Task, error) {
	query := "SELECT record FROM tasks ORDER BY id"
	var rows *sql.Rows
	var err error
	if history {
		query = "SELECT record FROM history WHERE updated >= ? ORDER BY seq DESC LIMIT ?"
		rows, err = s.db.Query(query, time.Now().Add(-HistoryRetention).Unix(), HistoryLimit)
	} else {
		rows, err = s.db.Query(query)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []Task
	for rows.Next() {
		var raw []byte
		var t Task
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if len(raw) > MaxRecordBytes {
			return nil, fmt.Errorf("queue task record exceeds max allowed size (%d > %d)", len(raw), MaxRecordBytes)
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("unmarshal task record failed: %w", err)
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

func (s *Store) Write(tasks []Task, history bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	wroteHistory := false
	for _, t := range tasks {
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		if len(raw) > MaxRecordBytes {
			return fmt.Errorf("queue task record exceeds max allowed size (%d > %d)", len(raw), MaxRecordBytes)
		}
		if t.Terminal() {
			_, err = tx.Exec("DELETE FROM tasks WHERE id=?", t.ID)
		} else {
			_, err = tx.Exec("INSERT INTO tasks(id, record) VALUES(?, ?) ON CONFLICT(id) DO UPDATE SET record=excluded.record", t.ID, raw)
		}
		if err != nil {
			return err
		}
		if history || t.State == Skipped {
			wroteHistory = true
			if _, err := tx.Exec("INSERT INTO history(record, updated) VALUES(?, ?)", raw, t.Updated.Unix()); err != nil {
				return err
			}
		}
	}

	if wroteHistory {
		cutoff := time.Now().Add(-HistoryRetention).Unix()
		_, err := tx.Exec(`
			DELETE FROM history 
			WHERE updated < ? OR seq NOT IN (SELECT seq FROM history ORDER BY seq DESC LIMIT ?)
		`, cutoff, HistoryLimit)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

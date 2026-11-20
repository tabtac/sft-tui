package pal

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// SQLiteDSN constructs a cross-platform SQLite file: URI for modernc.org/sqlite.
// On Windows it converts drive-letter and UNC paths so drive letters are never
// misparsed as a URI authority (e.g. file:///C:/... instead of file://C:/...).
func SQLiteDSN(dbPath, query string) (string, error) {
	return sqliteDSNWithSep(dbPath, query, os.PathSeparator, filepath.Abs)
}

func sqliteDSNWithSep(dbPath, query string, sep rune, absFn func(string) (string, error)) (string, error) {
	if strings.TrimSpace(dbPath) == "" {
		return "", fmt.Errorf("sqlite database path required")
	}
	if strings.ContainsRune(dbPath, 0) {
		return "", fmt.Errorf("sqlite database path contains null byte")
	}

	if sep == '\\' {
		// Preserve Windows Win32 extended-length prefix (\\?\...) expected by SQLite winFullPathname.
		if hasWindowsLongPathPrefix(dbPath) {
			u := url.URL{
				Scheme:   "file",
				Path:     "/" + dbPath,
				RawQuery: query,
			}
			return u.String(), nil
		}
		normalized := strings.ReplaceAll(dbPath, `\`, "/")
		switch {
		case hasWindowsDrivePrefix(normalized):
			if len(normalized) < 3 || normalized[2] != '/' {
				return "", fmt.Errorf("sqlite drive path %q must be absolute", dbPath)
			}
			uriPath := "/" + normalized
			u := url.URL{Scheme: "file", Path: uriPath, RawQuery: query}
			return u.String(), nil
		case strings.HasPrefix(normalized, "///"):
			uriPath := "/" + strings.TrimLeft(normalized, "/")
			u := url.URL{Scheme: "file", Path: uriPath, RawQuery: query}
			return u.String(), nil
		case strings.HasPrefix(normalized, "//"):
			// Emit file:////server/share/... so SQLite sees an empty authority and UNC path //server/share/...
			u := url.URL{Scheme: "file", Path: normalized, RawQuery: query}
			return u.String(), nil
		}
		dbPath = normalized
	}

	var uriPath string
	switch {
	case strings.HasPrefix(dbPath, "///"):
		uriPath = "/" + strings.TrimLeft(dbPath, "/")
	case strings.HasPrefix(dbPath, "//"):
		uriPath = dbPath
	case strings.HasPrefix(dbPath, "/"):
		uriPath = dbPath
	default:
		if absFn == nil {
			return "", fmt.Errorf("sqlite path %q must be absolute", dbPath)
		}
		absPath, err := absFn(dbPath)
		if err != nil {
			return "", fmt.Errorf("resolve sqlite database path %q: %w", dbPath, err)
		}
		return sqliteDSNWithSep(absPath, query, sep, nil)
	}

	u := url.URL{
		Scheme:   "file",
		Path:     uriPath,
		RawQuery: query,
	}
	return u.String(), nil
}

func hasWindowsDrivePrefix(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func hasWindowsLongPathPrefix(p string) bool {
	return strings.HasPrefix(p, `\\?\`)
}

// OpenVerifiedSQLite 以单连接 WAL + FULL 持久性打开 SQLite 数据库并验证关键 PRAGMA 生效。
func OpenVerifiedSQLite(dbPath, dsnQuery, schemaSQL string, preExec func(*sql.DB) error) (*sql.DB, error) {
	dsn, err := SQLiteDSN(dbPath, dsnQuery)
	if err != nil {
		return nil, fmt.Errorf("failed to build sqlite dsn: %w", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}
	// 保持连接池单连接写，避免多连接并发写入锁争用
	db.SetMaxOpenConns(1)

	fail := func(e error) (*sql.DB, error) {
		db.Close()
		return nil, e
	}
	if preExec != nil {
		if err := preExec(db); err != nil {
			return fail(err)
		}
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		return fail(fmt.Errorf("failed to execute schema: %w", err))
	}

	var syncMode int
	if err := db.QueryRow("PRAGMA synchronous;").Scan(&syncMode); err != nil {
		return fail(fmt.Errorf("failed to verify pragma synchronous: %w", err))
	}
	// 2 == FULL, 3 == EXTRA (部分系统)
	if syncMode != 2 && syncMode != 3 {
		return fail(fmt.Errorf("sqlite synchronous pragma failed: expected FULL (2), got %d", syncMode))
	}

	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		return fail(fmt.Errorf("failed to verify pragma journal_mode: %w", err))
	}
	if strings.ToUpper(journalMode) != "WAL" {
		return fail(fmt.Errorf("sqlite journal_mode pragma failed: expected WAL, got %s", journalMode))
	}

	return db, nil
}

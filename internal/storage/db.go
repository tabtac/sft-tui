package storage

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"

	"sft-tui/internal/pal"
)

// schemaSQL 定义元数据持久化表结构与 SQLite 关键 PRAGMA 参数。
// 必须设置 PRAGMA synchronous=FULL：
// 在 WAL 模式下，NORMAL 仅在 checkpoint 时刷盘，单次事务 commit 不强制刷盘，
// 掉电后会导致已发送 ACK 的检查点事务丢失。FULL 强制每次事务提交将 WAL 刷盘，
// 真正兑现 SFT/1 协议对断电持久性 (Crash/Power-loss Durability) 的要求。
const schemaSQL = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=FULL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;

CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY CHECK(length(id) == 32 AND NOT (id GLOB '*[^0-9a-fA-F]*')),
    user_id TEXT NOT NULL,
    request_id TEXT NOT NULL CHECK(length(request_id) == 32 AND NOT (request_id GLOB '*[^0-9a-fA-F]*')),
    direction INTEGER NOT NULL CHECK(direction IN (1, 2)),
    file_name TEXT NOT NULL,
    file_size INTEGER NOT NULL CHECK(file_size >= 0),
    file_sha256 TEXT NOT NULL CHECK(length(file_sha256) == 64 AND NOT (file_sha256 GLOB '*[^0-9a-fA-F]*')),
    object_id TEXT NOT NULL CHECK(length(object_id) == 32 AND NOT (object_id GLOB '*[^0-9a-fA-F]*')),
    offset INTEGER NOT NULL DEFAULT 0 CHECK(offset >= 0 AND offset <= file_size),
    status TEXT NOT NULL CHECK(status IN ('ACTIVE','PAUSED','VERIFYING','COMPLETED','FAILED','CANCELLED','EXPIRED')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    UNIQUE(user_id, request_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_user_req ON tasks(user_id, request_id);
CREATE INDEX IF NOT EXISTS idx_tasks_user_status ON tasks(user_id, status);

CREATE TABLE IF NOT EXISTS published_files (
    object_id TEXT PRIMARY KEY CHECK(length(object_id) == 32 AND NOT (object_id GLOB '*[^0-9a-fA-F]*')),
    user_id TEXT NOT NULL,
    file_name TEXT NOT NULL,
    file_size INTEGER NOT NULL CHECK(file_size >= 0),
    file_sha256 TEXT NOT NULL CHECK(length(file_sha256) == 64 AND NOT (file_sha256 GLOB '*[^0-9a-fA-F]*')),
    created_at INTEGER NOT NULL,
    UNIQUE(user_id, file_name)
);

CREATE INDEX IF NOT EXISTS idx_published_user_file ON published_files(user_id, file_name);
`

// initDB 打开 SQLite 数据库，应用 Schema 并严格读取校验 PRAGMA 参数是否生效。
func initDB(dbPath string) (*sql.DB, error) {
	dsn, err := pal.SQLiteDSN(dbPath, "_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("failed to build sqlite dsn: %w", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// 保持连接池单连接写，避免多连接并发写入锁争用
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to execute schema: %w", err)
	}

	// 严格校验 PRAGMA synchronous 实际设置值
	var syncMode int
	if err := db.QueryRow("PRAGMA synchronous;").Scan(&syncMode); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to verify pragma synchronous: %w", err)
	}
	// 2 == FULL, 3 == EXTRA (部分系统)
	if syncMode != 2 && syncMode != 3 {
		db.Close()
		return nil, fmt.Errorf("sqlite synchronous pragma failed: expected FULL (2), got %d", syncMode)
	}

	// 严格校验 PRAGMA journal_mode 实际设置值
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to verify pragma journal_mode: %w", err)
	}
	if strings.ToUpper(journalMode) != "WAL" {
		db.Close()
		return nil, fmt.Errorf("sqlite journal_mode pragma failed: expected WAL, got %s", journalMode)
	}

	return db, nil
}

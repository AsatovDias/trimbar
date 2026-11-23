package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const schemaVersion = 1

const schemaSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	timestamp TEXT NOT NULL,
	session_id TEXT NOT NULL DEFAULT '',
	request_id TEXT NOT NULL DEFAULT '',
	method TEXT NOT NULL,
	path TEXT NOT NULL,
	status_code INTEGER NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	agent_role TEXT NOT NULL DEFAULT 'main',
	is_streaming INTEGER NOT NULL DEFAULT 0,

	git_repo TEXT NOT NULL DEFAULT '',
	git_branch TEXT NOT NULL DEFAULT '',
	cwd TEXT NOT NULL DEFAULT '',

	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,

	cost_usd REAL NOT NULL DEFAULT 0,
	cache_savings_usd REAL NOT NULL DEFAULT 0,
	net_cache_savings_usd REAL NOT NULL DEFAULT 0,
	shadow_savings_usd REAL NOT NULL DEFAULT 0,

	latency_ms INTEGER NOT NULL DEFAULT 0,
	ttfb_ms INTEGER NOT NULL DEFAULT 0,

	cache_breakpoints INTEGER NOT NULL DEFAULT 0,
	cache_miss_anomaly INTEGER NOT NULL DEFAULT 0,
	cache_miss_reason TEXT NOT NULL DEFAULT '',

	system_est_tokens INTEGER NOT NULL DEFAULT 0,
	tools_est_tokens INTEGER NOT NULL DEFAULT 0,
	messages_est_tokens INTEGER NOT NULL DEFAULT 0,
	tool_results_est_tokens INTEGER NOT NULL DEFAULT 0,

	duplicate_read_count INTEGER NOT NULL DEFAULT 0,
	duplicate_read_wasted_tokens INTEGER NOT NULL DEFAULT 0,
	duplicate_reads_json TEXT NOT NULL DEFAULT '[]',

	ansi_sequence_count INTEGER NOT NULL DEFAULT 0,
	ansi_wasted_tokens INTEGER NOT NULL DEFAULT 0,
	total_wasted_tokens INTEGER NOT NULL DEFAULT 0,

	error_body TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_requests_timestamp ON requests(timestamp);
CREATE INDEX IF NOT EXISTS idx_requests_session_id ON requests(session_id);
CREATE INDEX IF NOT EXISTS idx_requests_repo_branch ON requests(git_repo, git_branch);
CREATE INDEX IF NOT EXISTS idx_requests_model ON requests(model);
CREATE INDEX IF NOT EXISTS idx_requests_agent_role ON requests(agent_role);
`

// DB wraps the local SQLite database connection configured in WAL mode.
type DB struct {
	sqlDB *sql.DB
	path  string
}

// Open opens (or creates) the SQLite database at dbPath, enables WAL mode,
// and applies schema migrations. Pass ":memory:" for an in-memory test database.
func Open(dbPath string) (*DB, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("db path is empty")
	}

	isMemory := dbPath == ":memory:" || strings.HasPrefix(dbPath, "file::memory:")
	if !isMemory {
		dir := filepath.Dir(dbPath)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("create db directory %q: %w", dir, err)
			}
		}
	}

	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", dbPath, err)
	}

	// SQLite handles concurrent readers well in WAL mode, while serializing writers
	// through a single connection prevents SQLITE_BUSY contention on Windows/Linux.
	sqlDB.SetMaxOpenConns(1)

	ctx := context.Background()
	if err := configurePragmas(ctx, sqlDB, isMemory); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}

	if err := migrate(ctx, sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}

	return &DB{
		sqlDB: sqlDB,
		path:  dbPath,
	}, nil
}

// Close closes the underlying SQLite connection.
func (d *DB) Close() error {
	if d == nil || d.sqlDB == nil {
		return nil
	}
	return d.sqlDB.Close()
}

// Path returns the filesystem path of the SQLite database.
func (d *DB) Path() string {
	return d.path
}

// SQL returns the raw *sql.DB handle.
func (d *DB) SQL() *sql.DB {
	return d.sqlDB
}

// JournalMode returns the active SQLite journal_mode pragma (e.g., "wal").
func (d *DB) JournalMode(ctx context.Context) (string, error) {
	var mode string
	if err := d.sqlDB.QueryRowContext(ctx, "PRAGMA journal_mode;").Scan(&mode); err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(mode)), nil
}

func configurePragmas(ctx context.Context, db *sql.DB, isMemory bool) error {
	pragmas := []string{
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA synchronous = NORMAL;",
	}
	if !isMemory {
		pragmas = append([]string{"PRAGMA journal_mode = WAL;"}, pragmas...)
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("exec %q: %w", p, err)
		}
	}
	return nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema migration: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations (version) VALUES (?);`, schemaVersion); err != nil {
		return fmt.Errorf("record schema migration version: %w", err)
	}
	return tx.Commit()
}

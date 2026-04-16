//go:build sqliteonly

package integration

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/nextlevelbuilder/goclaw/internal/store/sqlitestore"
)

// Phase 08 — D6: SQLite v20 → v21 rebuild preserves rows + widens CHECKs.
//
// We pre-construct a DB at schema_version=20 with the OLD agent_hooks CHECK
// constraints (handler_type IN ('command','http','prompt'), source IN
// ('ui','api','seed')) plus 3 hand-inserted rows. Re-opening the DB must:
//   - run rebuildAgentHooksV21 outside-of-tx (PRAGMA foreign_keys = OFF)
//   - preserve all 3 rows with original IDs
//   - bump schema_version to 21
//   - now accept handler_type='script' + source='builtin' inserts
//
// Test isolated under sqliteonly build tag — runs only when the desktop /
// SQLite-only build path is exercised (`go test -tags sqliteonly`).

const v20AgentHooksDDL = `
CREATE TABLE agent_hooks (
    id           TEXT NOT NULL PRIMARY KEY,
    tenant_id    TEXT NOT NULL DEFAULT '0193a5b0-7000-7000-8000-000000000001',
    agent_id     TEXT,
    scope        TEXT NOT NULL CHECK (scope IN ('global', 'tenant', 'agent')),
    event        TEXT NOT NULL,
    handler_type TEXT NOT NULL CHECK (handler_type IN ('command', 'http', 'prompt')),
    config       TEXT NOT NULL DEFAULT '{}',
    matcher      TEXT,
    if_expr      TEXT,
    timeout_ms   INTEGER NOT NULL DEFAULT 5000,
    on_timeout   TEXT NOT NULL DEFAULT 'block' CHECK (on_timeout IN ('block', 'allow')),
    priority     INTEGER NOT NULL DEFAULT 0,
    enabled      INTEGER NOT NULL DEFAULT 1,
    version      INTEGER NOT NULL DEFAULT 1,
    source       TEXT NOT NULL DEFAULT 'ui' CHECK (source IN ('ui', 'api', 'seed')),
    metadata     TEXT NOT NULL DEFAULT '{}',
    created_by   TEXT,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX IF NOT EXISTS idx_hooks_lookup
    ON agent_hooks (tenant_id, agent_id, event)
    WHERE enabled = 1;
`

func TestHooksD6_SQLiteV21RebuildPreservesRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hooks-rebuild.db")

	// ─── Stage 1: pre-seed v20 state ─────────────────────────────────────────
	preIDs := []string{
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
	}

	{
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open v20 db: %v", err)
		}
		// schema_version table + entry at 20.
		if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY)`); err != nil {
			t.Fatalf("create schema_version: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version VALUES (20)`); err != nil {
			t.Fatalf("seed schema_version: %v", err)
		}
		// Minimal parent tables needed: seedMasterTenant() requires
		// (id, name, slug, status) on tenants; rebuilt agent_hooks FKs agents(id).
		if _, err := db.Exec(`CREATE TABLE tenants (id TEXT PRIMARY KEY, name TEXT, slug TEXT, status TEXT)`); err != nil {
			t.Fatalf("create tenants: %v", err)
		}
		if _, err := db.Exec(`CREATE TABLE agents (id TEXT PRIMARY KEY)`); err != nil {
			t.Fatalf("create agents: %v", err)
		}
		// Inline v20 agent_hooks DDL.
		if _, err := db.Exec(v20AgentHooksDDL); err != nil {
			t.Fatalf("create v20 agent_hooks: %v", err)
		}
		// 3 rows with handler_type='command' (allowed under v20 CHECK).
		for _, id := range preIDs {
			if _, err := db.Exec(
				`INSERT INTO agent_hooks (id, scope, event, handler_type) VALUES (?, 'tenant', 'pre_tool_use', 'command')`,
				id,
			); err != nil {
				t.Fatalf("insert v20 row %s: %v", id, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close v20 db: %v", err)
		}
	}

	// ─── Stage 2: re-open via EnsureSchema → triggers rebuild ───────────────
	db, err := sqlitestore.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer db.Close()
	if err := sqlitestore.EnsureSchema(db); err != nil {
		t.Fatalf("EnsureSchema (rebuild): %v", err)
	}

	// schema_version must now be 21.
	var ver int
	if err := db.QueryRow(`SELECT version FROM schema_version LIMIT 1`).Scan(&ver); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if ver != sqlitestore.SchemaVersion {
		t.Fatalf("schema_version=%d, want %d", ver, sqlitestore.SchemaVersion)
	}

	// All 3 pre-seeded rows must still be present with original IDs.
	for _, id := range preIDs {
		var got string
		err := db.QueryRow(`SELECT id FROM agent_hooks WHERE id = ?`, id).Scan(&got)
		if err != nil {
			t.Errorf("row %s missing after rebuild: %v", id, err)
		}
		if got != id {
			t.Errorf("row id mismatch: got %s want %s", got, id)
		}
	}

	// New CHECK widened: handler_type='script' + source='builtin' must insert.
	if _, err := db.Exec(
		`INSERT INTO agent_hooks (id, scope, event, handler_type, source) VALUES (?, 'global', 'user_prompt_submit', 'script', 'builtin')`,
		"deadbeef-aaaa-bbbb-cccc-000000000001",
	); err != nil {
		t.Fatalf("post-rebuild script+builtin insert: %v", err)
	}
}

//go:build sqlite || sqliteonly

package sqlitestore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

func newTestPrivateReplyStore(t *testing.T) (*SQLitePancakePrivateReplyStore, *sql.DB, uuid.UUID) {
	t.Helper()

	db, err := OpenDB(filepath.Join(t.TempDir(), "pancake_pr.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := EnsureSchema(db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	tenantID := uuid.New()
	if _, err := db.Exec(
		`INSERT INTO tenants (id, name, slug, status) VALUES (?, ?, ?, 'active')`,
		tenantID.String(), "pancake-pr-test", "pr"+tenantID.String()[:8],
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	return NewSQLitePancakePrivateReplyStore(db), db, tenantID
}

func TestSQLitePancakePrivateReply_MarkThenWas(t *testing.T) {
	s, _, tenantID := newTestPrivateReplyStore(t)
	ctx := store.WithTenantID(context.Background(), tenantID)

	if err := s.MarkSent(ctx, "page-1", "user-1"); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	was, err := s.WasSent(ctx, "page-1", "user-1", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("WasSent: %v", err)
	}
	if !was {
		t.Fatalf("WasSent = false; want true")
	}
}

func TestSQLitePancakePrivateReply_TTLExpiry(t *testing.T) {
	s, db, tenantID := newTestPrivateReplyStore(t)
	ctx := store.WithTenantID(context.Background(), tenantID)

	if err := s.MarkSent(ctx, "page-1", "user-1"); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	// Rewind sent_at by 10 days (string-formatted timestamp).
	old := time.Now().UTC().Add(-10 * 24 * time.Hour).Format(sqliteTimeFormat)
	if _, err := db.Exec(
		`UPDATE pancake_private_reply_sent SET sent_at = ? WHERE tenant_id = ?`,
		old, tenantID.String(),
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	was, err := s.WasSent(ctx, "page-1", "user-1", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("WasSent: %v", err)
	}
	if was {
		t.Fatalf("WasSent = true; want false after TTL expiry")
	}
}

func TestSQLitePancakePrivateReply_Idempotent(t *testing.T) {
	s, db, tenantID := newTestPrivateReplyStore(t)
	ctx := store.WithTenantID(context.Background(), tenantID)

	if err := s.MarkSent(ctx, "page-1", "user-1"); err != nil {
		t.Fatalf("MarkSent 1: %v", err)
	}
	if err := s.MarkSent(ctx, "page-1", "user-1"); err != nil {
		t.Fatalf("MarkSent 2: %v", err)
	}

	var rows int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pancake_private_reply_sent WHERE tenant_id = ?`,
		tenantID.String(),
	).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d; want 1 (idempotent)", rows)
	}
}

func TestSQLitePancakePrivateReply_MissingTenantID(t *testing.T) {
	s, _, _ := newTestPrivateReplyStore(t)
	ctx := context.Background()

	if _, err := s.WasSent(ctx, "page-1", "user-1", time.Hour); err == nil {
		t.Fatalf("WasSent no-tenant: want error, got nil")
	}
	if err := s.MarkSent(ctx, "page-1", "user-1"); err == nil {
		t.Fatalf("MarkSent no-tenant: want error, got nil")
	}
}

func TestSQLitePancakePrivateReply_DeleteExpired(t *testing.T) {
	s, db, tenantID := newTestPrivateReplyStore(t)
	ctx := store.WithTenantID(context.Background(), tenantID)

	for _, u := range []string{"u1", "u2", "u3", "u4", "u5"} {
		if err := s.MarkSent(ctx, "page-1", u); err != nil {
			t.Fatalf("MarkSent %s: %v", u, err)
		}
	}

	old := time.Now().UTC().Add(-10 * 24 * time.Hour).Format(sqliteTimeFormat)
	if _, err := db.Exec(
		`UPDATE pancake_private_reply_sent SET sent_at = ? WHERE sender_id IN ('u1','u2','u3')`,
		old,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	n, err := s.DeleteExpired(ctx, cutoff)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if n != 3 {
		t.Fatalf("DeleteExpired = %d; want 3", n)
	}
}

func TestSQLitePancakePrivateReply_TenantIsolation(t *testing.T) {
	s, db, tenantA := newTestPrivateReplyStore(t)

	tenantB := uuid.New()
	if _, err := db.Exec(
		`INSERT INTO tenants (id, name, slug, status) VALUES (?, ?, ?, 'active')`,
		tenantB.String(), "pancake-pr-b", "prb"+tenantB.String()[:8],
	); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	ctxA := store.WithTenantID(context.Background(), tenantA)
	ctxB := store.WithTenantID(context.Background(), tenantB)

	if err := s.MarkSent(ctxA, "page-1", "user-1"); err != nil {
		t.Fatalf("MarkSent A: %v", err)
	}

	was, err := s.WasSent(ctxB, "page-1", "user-1", time.Hour)
	if err != nil {
		t.Fatalf("WasSent B: %v", err)
	}
	if was {
		t.Fatalf("tenant B sees tenant A row — cross-tenant leak")
	}
}

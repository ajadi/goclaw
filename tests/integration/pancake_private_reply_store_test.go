//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

// cleanPancakePrivateReply wipes rows for the given tenant. Run in t.Cleanup.
func cleanPancakePrivateReply(t *testing.T, tenantID uuid.UUID) {
	t.Helper()
	db := testDB(t)
	if _, err := db.Exec(`DELETE FROM pancake_private_reply_sent WHERE tenant_id = $1`, tenantID); err != nil {
		t.Logf("cleanup: %v", err)
	}
}

// seedTenantOnly inserts a tenant row without an agent (dedup store doesn't need agents).
func seedTenantOnly(t *testing.T) uuid.UUID {
	t.Helper()
	db := testDB(t)
	tenantID := uuid.New()
	slug := "t" + tenantID.String()[:8]
	_, err := db.Exec(
		`INSERT INTO tenants (id, name, slug, status) VALUES ($1, $2, $3, 'active')
		 ON CONFLICT DO NOTHING`,
		tenantID, "pancake-pr-"+slug, slug,
	)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		cleanPancakePrivateReply(t, tenantID)
		db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	return tenantID
}

func TestPancakePrivateReply_MarkThenWas(t *testing.T) {
	db := testDB(t)
	tenantID := seedTenantOnly(t)
	s := pg.NewPancakePrivateReplyStore(db)

	ctx := store.WithTenantID(context.Background(), tenantID)

	if err := s.MarkSent(ctx, "page-1", "user-1"); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	was, err := s.WasSent(ctx, "page-1", "user-1", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("WasSent: %v", err)
	}
	if !was {
		t.Fatalf("WasSent = false; want true immediately after MarkSent")
	}
}

func TestPancakePrivateReply_TTLExpiry(t *testing.T) {
	db := testDB(t)
	tenantID := seedTenantOnly(t)
	s := pg.NewPancakePrivateReplyStore(db)

	ctx := store.WithTenantID(context.Background(), tenantID)
	if err := s.MarkSent(ctx, "page-1", "user-1"); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	// Rewind sent_at by simulating 10 days ago.
	if _, err := db.Exec(
		`UPDATE pancake_private_reply_sent SET sent_at = NOW() - INTERVAL '10 days'
		 WHERE tenant_id = $1 AND page_id = 'page-1' AND sender_id = 'user-1'`,
		tenantID,
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

func TestPancakePrivateReply_Idempotent(t *testing.T) {
	db := testDB(t)
	tenantID := seedTenantOnly(t)
	s := pg.NewPancakePrivateReplyStore(db)

	ctx := store.WithTenantID(context.Background(), tenantID)

	if err := s.MarkSent(ctx, "page-1", "user-1"); err != nil {
		t.Fatalf("MarkSent 1: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := s.MarkSent(ctx, "page-1", "user-1"); err != nil {
		t.Fatalf("MarkSent 2: %v", err)
	}

	var rows int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pancake_private_reply_sent
		 WHERE tenant_id = $1 AND page_id = 'page-1' AND sender_id = 'user-1'`,
		tenantID,
	).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d; want 1 (idempotent upsert)", rows)
	}
}

func TestPancakePrivateReply_MissingTenantID(t *testing.T) {
	db := testDB(t)
	s := pg.NewPancakePrivateReplyStore(db)

	ctx := context.Background() // no tenant set

	if _, err := s.WasSent(ctx, "page-1", "user-1", time.Hour); err == nil {
		t.Fatalf("WasSent with no tenant: expected error, got nil")
	}
	if err := s.MarkSent(ctx, "page-1", "user-1"); err == nil {
		t.Fatalf("MarkSent with no tenant: expected error, got nil")
	}
}

func TestPancakePrivateReply_DeleteExpired(t *testing.T) {
	db := testDB(t)
	tenantID := seedTenantOnly(t)
	s := pg.NewPancakePrivateReplyStore(db)

	ctx := store.WithTenantID(context.Background(), tenantID)

	for _, u := range []string{"u1", "u2", "u3", "u4", "u5"} {
		if err := s.MarkSent(ctx, "page-1", u); err != nil {
			t.Fatalf("MarkSent %s: %v", u, err)
		}
	}

	// Backdate 3 rows to 10 days ago.
	if _, err := db.Exec(
		`UPDATE pancake_private_reply_sent SET sent_at = NOW() - INTERVAL '10 days'
		 WHERE tenant_id = $1 AND sender_id IN ('u1','u2','u3')`,
		tenantID,
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

	var remaining int
	db.QueryRow(
		`SELECT COUNT(*) FROM pancake_private_reply_sent WHERE tenant_id = $1`, tenantID,
	).Scan(&remaining)
	if remaining != 2 {
		t.Fatalf("remaining = %d; want 2", remaining)
	}
}

func TestPancakePrivateReply_TenantIsolation(t *testing.T) {
	db := testDB(t)
	tenantA := seedTenantOnly(t)
	tenantB := seedTenantOnly(t)
	s := pg.NewPancakePrivateReplyStore(db)

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
		t.Fatalf("tenant B sees tenant A's row: cross-tenant leak")
	}
}

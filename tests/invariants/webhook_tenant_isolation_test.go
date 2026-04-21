//go:build integration

package invariants

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

const defaultTestDSN = "postgres://postgres:test@localhost:5433/goclaw_test?sslmode=disable"

var (
	sharedDB     *sql.DB
	sharedDBOnce sync.Once
	sharedDBErr  error
)

// testDB connects to the test PG instance, runs migrations once, and returns
// a shared *sql.DB. Skips test if PG is unreachable.
func testDB(t *testing.T) *sql.DB {
	t.Helper()

	sharedDBOnce.Do(func() {
		dsn := os.Getenv("TEST_DATABASE_URL")
		if dsn == "" {
			dsn = defaultTestDSN
		}

		db, err := sql.Open("pgx", dsn)
		if err != nil {
			sharedDBErr = err
			return
		}
		if err := db.Ping(); err != nil {
			sharedDBErr = err
			return
		}

		// Run migrations once for the entire test run.
		m, err := migrate.New("file://../../migrations", dsn)
		if err != nil {
			sharedDBErr = err
			return
		}
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			sharedDBErr = err
			return
		}
		m.Close()

		// Initialize pg package's sqlx wrapper.
		pg.InitSqlx(db)

		sharedDB = db
	})

	if sharedDBErr != nil {
		t.Skipf("test PG not available: %v", sharedDBErr)
	}
	return sharedDB
}

// seedTenantAgent creates a minimal tenant + agent for FK satisfaction.
func seedTenantAgent(t *testing.T, db *sql.DB) (tenantID, agentID uuid.UUID) {
	t.Helper()

	tenantID = uuid.New()
	agentID = uuid.New()
	agentKey := "test-" + agentID.String()[:8]

	_, err := db.Exec(
		`INSERT INTO tenants (id, name, slug, status) VALUES ($1, $2, $3, 'active')
		 ON CONFLICT DO NOTHING`,
		tenantID, "test-tenant-"+tenantID.String()[:8], "t"+tenantID.String()[:8])
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	_, err = db.Exec(
		`INSERT INTO agents (id, tenant_id, agent_key, agent_type, status, provider, model, owner_id)
		 VALUES ($1, $2, $3, 'predefined', 'active', 'test', 'test-model', 'test-owner')
		 ON CONFLICT DO NOTHING`,
		agentID, tenantID, agentKey)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	t.Cleanup(func() {
		db.Exec("DELETE FROM webhook_calls WHERE tenant_id = $1", tenantID)
		db.Exec("DELETE FROM webhooks WHERE tenant_id = $1", tenantID)
		db.Exec("DELETE FROM sessions WHERE tenant_id = $1", tenantID)
		db.Exec("DELETE FROM agents WHERE id = $1", agentID)
		db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})

	return tenantID, agentID
}

// seedWebhook creates a webhook for a tenant.
func seedWebhook(t *testing.T, db *sql.DB, tenantID uuid.UUID, kind string) uuid.UUID {
	t.Helper()

	webhookID := uuid.New()
	rawSecret := "wh_secret_" + webhookID.String()[:8]
	h := sha256.Sum256([]byte(rawSecret))
	hashHex := hex.EncodeToString(h[:])

	_, err := db.Exec(`
		INSERT INTO webhooks (id, tenant_id, kind, secret_prefix, secret_hash, status)
		VALUES ($1, $2, $3, $4, $5, 'active')
	`, webhookID, tenantID, kind, "wh_test", hashHex)
	if err != nil {
		t.Fatalf("seed webhook: %v", err)
	}

	return webhookID
}

// P0: TestWebhookTenantIsolationListGet ensures no tenant can list/get another tenant's webhook.
func TestWebhookTenantIsolationListGet(t *testing.T) {
	db := testDB(t)

	// Seed 2 independent tenants with their webhooks.
	tenantA, agentA := seedTenantAgent(t, db)
	tenantB, agentB := seedTenantAgent(t, db)

	webhookAID := seedWebhook(t, db, tenantA, "llm")
	webhookBID := seedWebhook(t, db, tenantB, "message")

	storeA := pg.NewPGWebhookStore(db)
	storeB := pg.NewPGWebhookStore(db)

	ctxA := context.Background()
	ctxA = storeA.WithTenantID(ctxA, tenantA)

	ctxB := context.Background()
	ctxB = storeB.WithTenantID(ctxB, tenantB)

	// Tenant A lists webhooks — should only see their own.
	listA, err := storeA.List(ctxA, store.WebhookListFilter{})
	if err != nil {
		t.Fatalf("Tenant A list failed: %v", err)
	}

	for _, w := range listA {
		if w.TenantID != tenantA {
			t.Errorf("P0 VIOLATION: Tenant A listed webhook with tenant_id=%v (not %v)", w.TenantID, tenantA)
		}
		if w.ID == webhookBID {
			t.Errorf("P0 VIOLATION: Tenant A listed Tenant B's webhook")
		}
	}

	// Tenant B lists webhooks — should only see their own.
	listB, err := storeB.List(ctxB, store.WebhookListFilter{})
	if err != nil {
		t.Fatalf("Tenant B list failed: %v", err)
	}

	for _, w := range listB {
		if w.TenantID != tenantB {
			t.Errorf("P0 VIOLATION: Tenant B listed webhook with tenant_id=%v (not %v)", w.TenantID, tenantB)
		}
		if w.ID == webhookAID {
			t.Errorf("P0 VIOLATION: Tenant B listed Tenant A's webhook")
		}
	}

	// Tenant B tries to GET Tenant A's webhook.
	ctxBGetA := storeB.WithTenantID(context.Background(), tenantB)
	_, err = storeB.GetByID(ctxBGetA, webhookAID)
	if err != sql.ErrNoRows {
		t.Errorf("P0 VIOLATION: Tenant B was able to GetByID Tenant A's webhook (expected ErrNoRows, got %v)", err)
	}
}

// P0: TestWebhookTenantIsolationRotateRevoke ensures no tenant can rotate/revoke another's webhook.
func TestWebhookTenantIsolationRotateRevoke(t *testing.T) {
	db := testDB(t)

	tenantA, _ := seedTenantAgent(t, db)
	tenantB, _ := seedTenantAgent(t, db)

	webhookAID := seedWebhook(t, db, tenantA, "llm")

	storeA := pg.NewPGWebhookStore(db)
	storeB := pg.NewPGWebhookStore(db)

	ctxA := context.Background()
	ctxA = storeA.WithTenantID(ctxA, tenantA)

	ctxB := context.Background()
	ctxB = storeB.WithTenantID(ctxB, tenantB)

	// Get the original webhook.
	origWH, err := storeA.GetByID(ctxA, webhookAID)
	if err != nil {
		t.Fatalf("Tenant A get their webhook: %v", err)
	}
	origHash := origWH.SecretHash

	// Tenant B tries to rotate Tenant A's webhook secret.
	newHash := "newsecret_hash_" + uuid.New().String()[:8]
	err = storeB.RotateSecret(ctxB, webhookAID, origHash, newHash)
	if err == nil {
		// This is a P0 violation — the rotate should have failed (ErrNoRows or equivalent).
		t.Errorf("P0 VIOLATION: Tenant B was able to rotate Tenant A's webhook secret")

		// Verify it actually changed (worse violation).
		updated, _ := storeA.GetByID(ctxA, webhookAID)
		if updated.SecretHash != origHash {
			t.Errorf("P0 VIOLATION: Secret hash actually changed when Tenant B called RotateSecret")
		}
	}

	// Tenant B tries to revoke Tenant A's webhook.
	err = storeB.Revoke(ctxB, webhookAID)
	if err == nil {
		// Check if it actually revoked.
		updated, _ := storeA.GetByID(ctxA, webhookAID)
		if updated.Revoked {
			t.Errorf("P0 VIOLATION: Tenant B was able to revoke Tenant A's webhook")
		}
	}
}

// P0: TestWebhookTenantIsolationUpdate ensures no tenant can update another's webhook.
func TestWebhookTenantIsolationUpdate(t *testing.T) {
	db := testDB(t)

	tenantA, _ := seedTenantAgent(t, db)
	tenantB, _ := seedTenantAgent(t, db)

	webhookAID := seedWebhook(t, db, tenantA, "llm")

	storeA := pg.NewPGWebhookStore(db)
	storeB := pg.NewPGWebhookStore(db)

	ctxA := context.Background()
	ctxA = storeA.WithTenantID(ctxA, tenantA)

	ctxB := context.Background()
	ctxB = storeB.WithTenantID(ctxB, tenantB)

	// Get original rate limit.
	origWH, err := storeA.GetByID(ctxA, webhookAID)
	if err != nil {
		t.Fatalf("get original webhook: %v", err)
	}
	origRPM := origWH.RateLimitPerMin

	// Tenant B tries to update Tenant A's rate limit.
	err = storeB.Update(ctxB, webhookAID, map[string]any{
		"rate_limit_per_min": 999,
	})
	if err == nil {
		// Check if it actually updated.
		updated, _ := storeA.GetByID(ctxA, webhookAID)
		if updated.RateLimitPerMin != origRPM {
			t.Errorf("P0 VIOLATION: Tenant B was able to update Tenant A's rate_limit_per_min from %d to %d",
				origRPM, updated.RateLimitPerMin)
		}
	}
}

// P0: TestWebhookTenantIsolationGetByHash ensures GetByHash never returns cross-tenant webhook.
func TestWebhookTenantIsolationGetByHash(t *testing.T) {
	db := testDB(t)

	tenantA, _ := seedTenantAgent(t, db)
	tenantB, _ := seedTenantAgent(t, db)

	// Create webhooks with known secrets.
	webhookAID := uuid.New()
	secretA := "wh_secret_a_" + webhookAID.String()[:8]
	hA := sha256.Sum256([]byte(secretA))
	hashA := hex.EncodeToString(hA[:])

	_, err := db.Exec(`
		INSERT INTO webhooks (id, tenant_id, kind, secret_prefix, secret_hash, status)
		VALUES ($1, $2, 'llm', 'wh_test', $3, 'active')
	`, webhookAID, tenantA, hashA)
	if err != nil {
		t.Fatalf("seed webhook A: %v", err)
	}

	storeA := pg.NewPGWebhookStore(db)
	storeB := pg.NewPGWebhookStore(db)

	ctxA := context.Background()
	ctxA = storeA.WithTenantID(ctxA, tenantA)

	ctxB := context.Background()
	ctxB = storeB.WithTenantID(ctxB, tenantB)

	// Tenant A gets webhook by hash — should succeed.
	whA, err := storeA.GetByHash(ctxA, hashA)
	if err != nil {
		t.Fatalf("Tenant A GetByHash failed: %v", err)
	}
	if whA.TenantID != tenantA {
		t.Errorf("Tenant A retrieved webhook with wrong tenant_id: %v", whA.TenantID)
	}

	// Tenant B gets same hash — should fail (tenant_id check in query).
	ctxBGetHash := storeB.WithTenantID(context.Background(), tenantB)
	whB, err := storeB.GetByHash(ctxBGetHash, hashA)
	if err != sql.ErrNoRows {
		t.Errorf("P0 VIOLATION: Tenant B GetByHash succeeded (expected ErrNoRows, got %v, webhook=%v)", err, whB)
	}
}

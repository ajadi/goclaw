//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	httphandlers "github.com/nextlevelbuilder/goclaw/internal/http"
	"github.com/nextlevelbuilder/goclaw/internal/crypto"
	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

// TestSecureCLIRevealRateLimit_AllowsBurstyRequests verifies 10 reveals/min are allowed.
// H9 test: 10 requests succeed, 11th is rate limited to 429.
func TestSecureCLIRevealRateLimit_AllowsBurstyRequests(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	binaryID := envBinaryFixture(t, db, tenantID)

	grantStore := pg.NewPGSecureCLIAgentGrantStore(db, testEncryptionKey)
	handler := httphandlers.NewSecureCLIGrantHandler(grantStore, nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Create 11 grants to test the rate limit threshold.
	grantIDs := make([]uuid.UUID, 11)
	for i := 0; i < 11; i++ {
		grantID := uuid.New()
		grantIDs[i] = grantID

		envJSON, _ := json.Marshal(map[string]string{"KEY": "value"})
		encryptedStr, _ := crypto.Encrypt(string(envJSON), testEncryptionKey)

		_, err := db.Exec(
			`INSERT INTO secure_cli_agent_grants
				(id, binary_id, agent_id, tenant_id, encrypted_env, enabled)
			 VALUES ($1, $2, $3, $4, $5, true)`,
			grantID, binaryID, agentID, tenantID, []byte(encryptedStr),
		)
		if err != nil {
			t.Fatalf("seed grant %d: %v", i, err)
		}
	}

	// Make 10 reveal requests — all should succeed.
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost,
			"/v1/cli-credentials/"+binaryID.String()+"/agent-grants/"+grantIDs[i].String()+"/env:reveal", nil)
		req = req.WithContext(tenantCtx(tenantID))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("reveal %d failed: expected 200, got %d", i, rr.Code)
		}
	}

	// 11th request should be rate limited to 429.
	req := httptest.NewRequest(http.MethodPost,
		"/v1/cli-credentials/"+binaryID.String()+"/agent-grants/"+grantIDs[10].String()+"/env:reveal", nil)
	req = req.WithContext(tenantCtx(tenantID))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 on 11th reveal, got %d", rr.Code)
	}
}

// TestSecureCLIRevealRateLimit_RateLimitPerCaller verifies rate limit is per IP/user.
// Two different callers (from different IPs via mocking) should have independent limits.
func TestSecureCLIRevealRateLimit_RateLimitPerCaller(t *testing.T) {
	t.Parallel()

	db := testDB(t)

	// Caller A: tenant A, make 10 reveals.
	tenantA, agentA := seedTenantAgent(t, db)
	binaryA := envBinaryFixture(t, db, tenantA)

	// Caller B: tenant B, make 10 reveals.
	tenantB, agentB := seedTenantAgent(t, db)
	binaryB := envBinaryFixture(t, db, tenantB)

	grantStore := pg.NewPGSecureCLIAgentGrantStore(db, testEncryptionKey)
	handler := httphandlers.NewSecureCLIGrantHandler(grantStore, nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Create 10 grants for each tenant.
	grantsA := make([]uuid.UUID, 10)
	grantsB := make([]uuid.UUID, 10)

	for i := 0; i < 10; i++ {
		grantA := uuid.New()
		grantsA[i] = grantA
		envJSON, _ := json.Marshal(map[string]string{"KEY": "val"})
		encryptedA, _ := crypto.Encrypt(string(envJSON), testEncryptionKey)
		db.Exec(
			`INSERT INTO secure_cli_agent_grants
				(id, binary_id, agent_id, tenant_id, encrypted_env, enabled)
			 VALUES ($1, $2, $3, $4, $5, true)`,
			grantA, binaryA, agentA, tenantA, []byte(encryptedA),
		)

		grantB := uuid.New()
		grantsB[i] = grantB
		encryptedB, _ := crypto.Encrypt(string(envJSON), testEncryptionKey)
		db.Exec(
			`INSERT INTO secure_cli_agent_grants
				(id, binary_id, agent_id, tenant_id, encrypted_env, enabled)
			 VALUES ($1, $2, $3, $4, $5, true)`,
			grantB, binaryB, agentB, tenantB, []byte(encryptedB),
		)
	}

	// Caller A makes 10 reveals with X-Forwarded-For header to simulate different IP.
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost,
			"/v1/cli-credentials/"+binaryA.String()+"/agent-grants/"+grantsA[i].String()+"/env:reveal", nil)
		req = req.WithContext(tenantCtx(tenantA))
		req.Header.Set("X-Forwarded-For", "192.168.1.1")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("caller A reveal %d: expected 200, got %d", i, rr.Code)
		}
	}

	// Caller B makes 10 reveals from a different IP — should all succeed
	// (independent rate limit bucket).
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost,
			"/v1/cli-credentials/"+binaryB.String()+"/agent-grants/"+grantsB[i].String()+"/env:reveal", nil)
		req = req.WithContext(tenantCtx(tenantB))
		req.Header.Set("X-Forwarded-For", "192.168.1.2")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("caller B reveal %d: expected 200, got %d", i, rr.Code)
		}
	}

	// Verify: caller A's 11th request is rate limited.
	grantA11 := uuid.New()
	envJSON, _ := json.Marshal(map[string]string{"KEY": "val"})
	encryptedA11, _ := crypto.Encrypt(string(envJSON), testEncryptionKey)
	db.Exec(
		`INSERT INTO secure_cli_agent_grants
			(id, binary_id, agent_id, tenant_id, encrypted_env, enabled)
		 VALUES ($1, $2, $3, $4, $5, true)`,
		grantA11, binaryA, agentA, tenantA, []byte(encryptedA11),
	)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/cli-credentials/"+binaryA.String()+"/agent-grants/"+grantA11.String()+"/env:reveal", nil)
	req = req.WithContext(tenantCtx(tenantA))
	req.Header.Set("X-Forwarded-For", "192.168.1.1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("caller A's 11th reveal: expected 429, got %d", rr.Code)
	}

	// Verify: caller B's 11th request still succeeds (independent limit).
	grantB11 := uuid.New()
	encryptedB11, _ := crypto.Encrypt(string(envJSON), testEncryptionKey)
	db.Exec(
		`INSERT INTO secure_cli_agent_grants
			(id, binary_id, agent_id, tenant_id, encrypted_env, enabled)
		 VALUES ($1, $2, $3, $4, $5, true)`,
		grantB11, binaryB, agentB, tenantB, []byte(encryptedB11),
	)

	req = httptest.NewRequest(http.MethodPost,
		"/v1/cli-credentials/"+binaryB.String()+"/agent-grants/"+grantB11.String()+"/env:reveal", nil)
	req = req.WithContext(tenantCtx(tenantB))
	req.Header.Set("X-Forwarded-For", "192.168.1.2")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("caller B's 11th reveal: expected 200 (independent limit), got %d", rr.Code)
	}
}

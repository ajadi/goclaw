//go:build integration

package integration

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	httphandlers "github.com/nextlevelbuilder/goclaw/internal/http"
	"github.com/nextlevelbuilder/goclaw/internal/crypto"
	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

// envBinaryFixture creates a test CLI binary for env override testing.
func envBinaryFixture(t *testing.T, db *sql.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	binaryID := uuid.New()
	_, err := db.Exec(
		`INSERT INTO secure_cli_binaries
			(id, tenant_id, binary_name, encrypted_env, description, enabled, is_global)
		 VALUES ($1, $2, $3, $4, 'env test binary', true, false)`,
		binaryID, tenantID, "env_test_"+binaryID.String()[:8], []byte(`{}`),
	)
	if err != nil {
		t.Fatalf("envBinaryFixture seed: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM secure_cli_agent_grants WHERE binary_id = $1", binaryID)
		db.Exec("DELETE FROM secure_cli_user_credentials WHERE binary_id = $1", binaryID)
		db.Exec("DELETE FROM secure_cli_binaries WHERE id = $1", binaryID)
	})
	return binaryID
}

// testHTTPRequest is a helper to make HTTP requests against the handler mux.
// Returns (statusCode, responseBody as json.RawMessage).
func testHTTPRequest(t *testing.T, mux *http.ServeMux, method, path string, body any) (int, json.RawMessage) {
	t.Helper()
	var reqBody []byte
	if body != nil {
		var err error
		reqBody, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	respBody := rr.Body.Bytes()
	return rr.Code, json.RawMessage(respBody)
}

// TestSecureCLIGrantEnv_CreateWithEnvVars verifies env_vars are encrypted on creation.
func TestSecureCLIGrantEnv_CreateWithEnvVars(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	binaryID := envBinaryFixture(t, db, tenantID)

	grantStore := pg.NewPGSecureCLIAgentGrantStore(db, testEncryptionKey)
	handler := httphandlers.NewSecureCLIGrantHandler(grantStore, nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Create grant with env_vars.
	createReq := map[string]any{
		"agent_id": agentID.String(),
		"env_vars": map[string]string{
			"API_KEY":    "secret123",
			"DEBUG_MODE": "true",
		},
	}

	code, _ := testHTTPRequest(t, mux, http.MethodPost,
		"/v1/cli-credentials/"+binaryID.String()+"/agent-grants", createReq)

	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("expected 201 or 200, got %d", code)
	}

	// Verify the grant was persisted with env_set=true and keys sorted.
	grants, err := grantStore.ListByBinary(tenantCtx(tenantID), binaryID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(grants))
	}

	grant := grants[0]
	if len(grant.EncryptedEnv) == 0 {
		t.Errorf("expected encrypted_env to be set, got empty")
	}

	// Verify env_keys are sorted.
	expectedKeys := []string{"API_KEY", "DEBUG_MODE"}
	if len(grant.EnvKeys) != len(expectedKeys) {
		t.Errorf("expected %d env keys, got %d", len(expectedKeys), len(grant.EnvKeys))
	}
	for i, k := range grant.EnvKeys {
		if k != expectedKeys[i] {
			t.Errorf("env_keys[%d]: expected %q, got %q", i, expectedKeys[i], k)
		}
	}
}

// TestSecureCLIGrantEnv_UpdateWithNullEnvVars verifies env_vars=null clears env.
func TestSecureCLIGrantEnv_UpdateWithNullEnvVars(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	binaryID := envBinaryFixture(t, db, tenantID)

	// Seed grant with env_vars.
	grantID := uuid.New()
	_, err := db.Exec(
		`INSERT INTO secure_cli_agent_grants
			(id, binary_id, agent_id, tenant_id, encrypted_env, enabled)
		 VALUES ($1, $2, $3, $4, $5, true)`,
		grantID, binaryID, agentID, tenantID, []byte(`{"KEY":"value"}`),
	)
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	grantStore := pg.NewPGSecureCLIAgentGrantStore(db, testEncryptionKey)
	handler := httphandlers.NewSecureCLIGrantHandler(grantStore, nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Update with env_vars=null to clear.
	updateReq := map[string]any{
		"env_vars": nil,
	}

	code, _ := testHTTPRequest(t, mux, http.MethodPut,
		"/v1/cli-credentials/"+binaryID.String()+"/agent-grants/"+grantID.String(), updateReq)

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}

	// Verify env is cleared.
	grant, err := grantStore.Get(tenantCtx(tenantID), grantID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if len(grant.EncryptedEnv) != 0 {
		t.Errorf("expected encrypted_env to be cleared, got %d bytes", len(grant.EncryptedEnv))
	}
}

// TestSecureCLIGrantEnv_UpdateAbsentEnvVars verifies omitting env_vars field preserves env.
func TestSecureCLIGrantEnv_UpdateAbsentEnvVars(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	binaryID := envBinaryFixture(t, db, tenantID)

	// Seed grant with env_vars.
	grantID := uuid.New()
	originalEnv := []byte(`{"PRESERVED_KEY":"secret_value"}`)
	_, err := db.Exec(
		`INSERT INTO secure_cli_agent_grants
			(id, binary_id, agent_id, tenant_id, encrypted_env, enabled)
		 VALUES ($1, $2, $3, $4, $5, true)`,
		grantID, binaryID, agentID, tenantID, originalEnv,
	)
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	grantStore := pg.NewPGSecureCLIAgentGrantStore(db, testEncryptionKey)
	handler := httphandlers.NewSecureCLIGrantHandler(grantStore, nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Update without env_vars field.
	updateReq := map[string]any{
		"enabled": false,
	}

	code, _ := testHTTPRequest(t, mux, http.MethodPut,
		"/v1/cli-credentials/"+binaryID.String()+"/agent-grants/"+grantID.String(), updateReq)

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}

	// Verify env is unchanged.
	grant, err := grantStore.Get(tenantCtx(tenantID), grantID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if len(grant.EncryptedEnv) == 0 {
		t.Errorf("expected encrypted_env to be preserved, got empty")
	}
}

// TestSecureCLIGrantEnv_DenylistPathRejection verifies PATH env var is rejected.
func TestSecureCLIGrantEnv_DenylistPathRejection(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	binaryID := envBinaryFixture(t, db, tenantID)

	grantStore := pg.NewPGSecureCLIAgentGrantStore(db, testEncryptionKey)
	handler := httphandlers.NewSecureCLIGrantHandler(grantStore, nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Try to create grant with PATH env var.
	createReq := map[string]any{
		"agent_id": agentID.String(),
		"env_vars": map[string]string{
			"PATH": "/evil",
		},
	}

	code, respBody := testHTTPRequest(t, mux, http.MethodPost,
		"/v1/cli-credentials/"+binaryID.String()+"/agent-grants", createReq)

	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", code)
	}

	// Verify response contains rejected_keys.
	var respData map[string]any
	if err := json.Unmarshal(respBody, &respData); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if _, ok := respData["rejected_keys"]; !ok {
		t.Errorf("response missing rejected_keys field")
	}
}

// TestSecureCLIGrantEnv_DenylistDyldRejection verifies DYLD_INSERT_LIBRARIES is rejected.
func TestSecureCLIGrantEnv_DenylistDyldRejection(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	binaryID := envBinaryFixture(t, db, tenantID)

	grantStore := pg.NewPGSecureCLIAgentGrantStore(db, testEncryptionKey)
	handler := httphandlers.NewSecureCLIGrantHandler(grantStore, nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Try to create grant with DYLD_INSERT_LIBRARIES env var.
	createReq := map[string]any{
		"agent_id": agentID.String(),
		"env_vars": map[string]string{
			"DYLD_INSERT_LIBRARIES": "/path/to/evil.dylib",
		},
	}

	code, _ := testHTTPRequest(t, mux, http.MethodPost,
		"/v1/cli-credentials/"+binaryID.String()+"/agent-grants", createReq)

	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for DYLD_INSERT_LIBRARIES, got %d", code)
	}
}

// TestSecureCLIGrantEnv_DenylistGoclawTokenRejection verifies GOCLAW_TOKEN is rejected.
func TestSecureCLIGrantEnv_DenylistGoclawTokenRejection(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	binaryID := envBinaryFixture(t, db, tenantID)

	grantStore := pg.NewPGSecureCLIAgentGrantStore(db, testEncryptionKey)
	handler := httphandlers.NewSecureCLIGrantHandler(grantStore, nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Try to create grant with GOCLAW_TOKEN env var.
	createReq := map[string]any{
		"agent_id": agentID.String(),
		"env_vars": map[string]string{
			"GOCLAW_TOKEN": "secret_token",
		},
	}

	code, _ := testHTTPRequest(t, mux, http.MethodPost,
		"/v1/cli-credentials/"+binaryID.String()+"/agent-grants", createReq)

	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for GOCLAW_TOKEN, got %d", code)
	}
}

// TestSecureCLIGrantEnv_RevealEndpointReturnsPlaintext verifies reveal decrypts correctly.
func TestSecureCLIGrantEnv_RevealEndpointReturnsPlaintext(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	binaryID := envBinaryFixture(t, db, tenantID)

	// Seed grant with env_vars.
	grantID := uuid.New()
	expectedEnv := map[string]string{
		"API_KEY":  "secret123",
		"API_URL":  "https://api.example.com",
	}

	// Encrypt the env via crypto package.
	envJSON, _ := json.Marshal(expectedEnv)
	encryptedStr, err := crypto.Encrypt(string(envJSON), testEncryptionKey)
	if err != nil {
		t.Fatalf("encrypt env: %v", err)
	}

	_, err = db.Exec(
		`INSERT INTO secure_cli_agent_grants
			(id, binary_id, agent_id, tenant_id, encrypted_env, enabled)
		 VALUES ($1, $2, $3, $4, $5, true)`,
		grantID, binaryID, agentID, tenantID, []byte(encryptedStr),
	)
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	grantStore := pg.NewPGSecureCLIAgentGrantStore(db, testEncryptionKey)
	handler := httphandlers.NewSecureCLIGrantHandler(grantStore, nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// POST to reveal endpoint.
	code, respBody := testHTTPRequest(t, mux, http.MethodPost,
		"/v1/cli-credentials/"+binaryID.String()+"/agent-grants/"+grantID.String()+"/env:reveal", nil)

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, respBody)
	}

	var revealResp map[string]any
	if err := json.Unmarshal(respBody, &revealResp); err != nil {
		t.Fatalf("unmarshal reveal response: %v", err)
	}

	envVarsRaw, ok := revealResp["env_vars"]
	if !ok {
		t.Fatalf("response missing env_vars field")
	}

	// Re-marshal to compare.
	envVarsJSON, _ := json.Marshal(envVarsRaw)
	var revealedEnv map[string]string
	if err := json.Unmarshal(envVarsJSON, &revealedEnv); err != nil {
		t.Fatalf("unmarshal env_vars: %v", err)
	}

	if len(revealedEnv) != len(expectedEnv) {
		t.Errorf("expected %d env vars, got %d", len(expectedEnv), len(revealedEnv))
	}
	for k, v := range expectedEnv {
		if revealedEnv[k] != v {
			t.Errorf("env_vars[%s]: expected %q, got %q", k, v, revealedEnv[k])
		}
	}
}

// TestSecureCLIGrantEnv_RevealCacheControlHeader verifies Cache-Control is set.
func TestSecureCLIGrantEnv_RevealCacheControlHeader(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	binaryID := envBinaryFixture(t, db, tenantID)

	// Seed grant with env_vars.
	grantID := uuid.New()
	envJSON, _ := json.Marshal(map[string]string{"KEY": "value"})
	encryptedStr, _ := crypto.Encrypt(string(envJSON), testEncryptionKey)

	_, err := db.Exec(
		`INSERT INTO secure_cli_agent_grants
			(id, binary_id, agent_id, tenant_id, encrypted_env, enabled)
		 VALUES ($1, $2, $3, $4, $5, true)`,
		grantID, binaryID, agentID, tenantID, []byte(encryptedStr),
	)
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	grantStore := pg.NewPGSecureCLIAgentGrantStore(db, testEncryptionKey)
	handler := httphandlers.NewSecureCLIGrantHandler(grantStore, nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Make request and check headers directly.
	req := httptest.NewRequest(http.MethodPost,
		"/v1/cli-credentials/"+binaryID.String()+"/agent-grants/"+grantID.String()+"/env:reveal", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	cacheControl := rr.Header().Get("Cache-Control")
	if !strings.Contains(cacheControl, "no-store") {
		t.Errorf("expected Cache-Control to contain 'no-store', got %q", cacheControl)
	}
}

// TestSecureCLIGrantEnv_RevealRequiresPOST verifies GET method returns 405.
func TestSecureCLIGrantEnv_RevealRequiresPOST(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID, agentID := seedTenantAgent(t, db)
	binaryID := envBinaryFixture(t, db, tenantID)

	// Seed grant with env_vars.
	grantID := uuid.New()
	envJSON, _ := json.Marshal(map[string]string{"KEY": "value"})
	encryptedStr, _ := crypto.Encrypt(string(envJSON), testEncryptionKey)

	_, err := db.Exec(
		`INSERT INTO secure_cli_agent_grants
			(id, binary_id, agent_id, tenant_id, encrypted_env, enabled)
		 VALUES ($1, $2, $3, $4, $5, true)`,
		grantID, binaryID, agentID, tenantID, []byte(encryptedStr),
	)
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	grantStore := pg.NewPGSecureCLIAgentGrantStore(db, testEncryptionKey)
	handler := httphandlers.NewSecureCLIGrantHandler(grantStore, nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Try GET method.
	req := httptest.NewRequest(http.MethodGet,
		"/v1/cli-credentials/"+binaryID.String()+"/agent-grants/"+grantID.String()+"/env:reveal", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

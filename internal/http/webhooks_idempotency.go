package http

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// checkIdempotency inspects the Idempotency-Key header and resolves prior calls.
//
// Returns:
//   - (true, nil)    — no key present; proceed normally.
//   - (true, nil)    — key present, no prior call; caller should record the call
//     after handler success (phases 05/06).
//   - (false, nil)   — key matches prior call with same body → response already
//     written (HTTP 200 replay). Handler must not write again.
//   - (false, error) — 409 Conflict written (body hash mismatch). Handler must
//     not write again.
//
// Body hash is SHA-256 of the raw request body bytes (already buffered by
// readLimitedBody at this point).
func checkIdempotency(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	webhookID uuid.UUID,
	calls store.WebhookCallStore,
) (proceed bool, err error) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return true, nil
	}

	bodyHash := sha256Hex(body)
	ctx := r.Context()
	locale := store.LocaleFromContext(ctx)

	existing, err := calls.GetByIdempotency(ctx, webhookID, key)
	if errors.Is(err, sql.ErrNoRows) {
		// First time this key is seen — caller proceeds; let handler record call.
		return true, nil
	}
	if err != nil {
		// Store error — fail open (don't block on idempotency store errors).
		return true, nil
	}

	// Prior call found — check body hash stored in request_payload prefix.
	// We store body hash as the first 64 bytes of request_payload (hex SHA-256).
	// If request_payload is nil or shorter, treat as no-match (edge case for
	// very old records or non-body calls).
	storedHash := extractBodyHash(existing.RequestPayload)
	if storedHash != "" && storedHash != bodyHash {
		// Same key, different body → 409 Conflict.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": i18n.T(locale, i18n.MsgWebhookIdempotencyConflict),
		})
		return false, errors.New("idempotency conflict")
	}

	// Same key + matching body → replay last stored response.
	if len(existing.Response) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Idempotency-Replayed", "true")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(existing.Response)
		return false, nil
	}

	// Call exists but response not yet written (still queued/running).
	// Return 202 Accepted so the caller knows to poll.
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  existing.Status,
		"call_id": existing.ID.String(),
	})
	return false, nil
}

// sha256Hex returns the lowercase hex SHA-256 digest of b.
func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// extractBodyHash reads the first 64 bytes of payload as a hex SHA-256 hash.
// Returns "" when payload is absent or shorter than 64 bytes (not a hash prefix).
func extractBodyHash(payload []byte) string {
	if len(payload) < 64 {
		return ""
	}
	candidate := string(payload[:64])
	// Validate it looks like a hex string before returning.
	for _, c := range candidate {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ""
		}
	}
	return candidate
}

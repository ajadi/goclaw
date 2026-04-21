package http

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

const (
	// webhookBearerPrefix is the well-known prefix for raw webhook secrets.
	// Presence allows fast rejection of non-webhook bearer tokens.
	webhookBearerPrefix = "wh_"

	// webhookHMACSkewSeconds is the maximum |now - t| allowed for HMAC timestamps.
	webhookHMACSkewSeconds = 300

	// webhookMaxBodyMessage is the body cap for /v1/webhooks/message endpoints.
	WebhookMaxBodyMessage = 256 * 1024 // 256 KB

	// webhookMaxBodyLLM is the body cap for /v1/webhooks/llm endpoints.
	WebhookMaxBodyLLM = 1024 * 1024 // 1 MB
)

// WebhookAuthMiddleware is the composed middleware chain for all /v1/webhooks/*
// runtime endpoints. Order: body cap → bearer/HMAC auth → localhost gate →
// rate limit → idempotency guard → inject context → next.
//
// Parameters:
//   - ws:       WebhookStore for secret + row lookup.
//   - calls:    WebhookCallStore for idempotency checks.
//   - limiter:  shared process-lifetime rate limiter (never nil).
//   - kind:     expected webhook kind ("llm" or "message") — enforced vs row.
//   - maxBody:  body size cap in bytes (use WebhookMaxBodyMessage/LLM constants).
func WebhookAuthMiddleware(
	ws store.WebhookStore,
	calls store.WebhookCallStore,
	limiter *webhookLimiter,
	kind string,
	maxBody int64,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			locale := store.LocaleFromContext(ctx)

			// 1. Read and cap body — HMAC needs raw bytes, so we buffer once and
			//    restore r.Body so downstream JSON decoders see correct content.
			body, err := readLimitedBody(r, maxBody)
			if err != nil {
				slog.Warn("security.webhook.body_too_large",
					"path", r.URL.Path,
					"remote_addr", r.RemoteAddr,
				)
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
					"error": i18n.T(locale, i18n.MsgWebhookBodyTooLarge),
				})
				return
			}

			// 2. Resolve webhook row via bearer or HMAC.
			webhook, err := resolveWebhook(r, body, ws)
			if err != nil {
				slog.Warn("security.webhook.auth_failed",
					"reason", err.Error(),
					"path", r.URL.Path,
					"remote_addr", r.RemoteAddr,
				)
				status := http.StatusUnauthorized
				msg := i18n.T(locale, i18n.MsgWebhookAuthFailed)
				// Surface specific reasons for well-defined failure modes.
				switch {
				case errors.Is(err, errWebhookRevoked):
					msg = i18n.T(locale, i18n.MsgWebhookRevoked)
				case errors.Is(err, errWebhookHMACInvalid):
					msg = i18n.T(locale, i18n.MsgWebhookHMACInvalid)
				case errors.Is(err, errWebhookTimestampSkew):
					msg = i18n.T(locale, i18n.MsgWebhookHMACTimestampSkew)
				case errors.Is(err, errWebhookBearerRequiresHMAC):
					msg = i18n.T(locale, i18n.MsgWebhookBearerRequiredHMAC)
				}
				writeJSON(w, status, map[string]string{"error": msg})
				return
			}

			// 3. Localhost-only gate (checked after auth to avoid timing oracle on
			//    the existence of localhost-only webhooks).
			if webhook.LocalhostOnly {
				if !isLoopback(r.RemoteAddr) {
					slog.Warn("security.webhook.localhost_only_violation",
						"webhook_id_hint", webhook.SecretPrefix,
						"remote_addr", r.RemoteAddr,
					)
					writeJSON(w, http.StatusForbidden, map[string]string{
						"error": i18n.T(locale, i18n.MsgWebhookLocalhostOnlyViolation),
					})
					return
				}
			}

			// 4. Kind match — reject if caller path targets wrong kind.
			if webhook.Kind != kind {
				slog.Warn("security.webhook.kind_mismatch",
					"webhook_id_hint", webhook.SecretPrefix,
					"expected_kind", webhook.Kind,
					"requested_kind", kind,
				)
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": i18n.T(locale, i18n.MsgWebhookKindMismatch),
				})
				return
			}

			// 5. Rate limits — per-webhook then per-tenant (both must pass).
			tenantID := webhook.TenantID.String()
			webhookID := webhook.ID.String()

			if !limiter.AllowWebhook(webhookID, webhook.RateLimitPerMin) {
				slog.Warn("security.webhook.rate_limited",
					"webhook_id_hint", webhook.SecretPrefix,
					"tier", "webhook",
				)
				w.Header().Set("Retry-After", "60")
				writeJSON(w, http.StatusTooManyRequests, map[string]string{
					"error": i18n.T(locale, i18n.MsgWebhookRateLimited),
				})
				return
			}
			if !limiter.AllowTenant(tenantID) {
				slog.Warn("security.webhook.rate_limited",
					"webhook_id_hint", webhook.SecretPrefix,
					"tier", "tenant",
				)
				w.Header().Set("Retry-After", "60")
				writeJSON(w, http.StatusTooManyRequests, map[string]string{
					"error": i18n.T(locale, i18n.MsgWebhookRateLimited),
				})
				return
			}

			// 6. Idempotency check.
			proceed, _ := checkIdempotency(w, r, body, webhook.ID, calls)
			if !proceed {
				return
			}

			// 7. Inject webhook + tenant into context; propagate to stores.
			ctx = WithWebhookData(ctx, webhook)
			ctx = store.WithTenantID(ctx, webhook.TenantID)
			if webhook.AgentID != nil {
				ctx = store.WithAgentID(ctx, *webhook.AgentID)
			}

			// Best-effort touch — don't block on failure.
			go func() { _ = ws.TouchLastUsed(r.Context(), webhook.ID) }()

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ---- sentinel errors (unexported; tested via errors.Is) ----

var (
	errWebhookRevoked           = errors.New("webhook_revoked")
	errWebhookHMACInvalid       = errors.New("hmac_invalid")
	errWebhookTimestampSkew     = errors.New("hmac_timestamp_skew")
	errWebhookBearerRequiresHMAC = errors.New("bearer_requires_hmac")
	errWebhookNotFound          = errors.New("webhook_not_found")
)

// resolveWebhook determines auth mode from headers and delegates to the
// appropriate resolver. Returns a non-nil *WebhookData on success.
//
// Auth mode detection:
//   - HMAC mode: X-GoClaw-Signature header present → resolveByHMAC.
//   - Bearer mode: Authorization: Bearer wh_* → resolveByBearer.
//   - Neither → 401 (errWebhookNotFound used as catch-all).
func resolveWebhook(r *http.Request, body []byte, ws store.WebhookStore) (*store.WebhookData, error) {
	sigHeader := r.Header.Get("X-GoClaw-Signature")
	authHeader := r.Header.Get("Authorization")

	if sigHeader != "" {
		// HMAC mode: need X-Webhook-Id to look up the row.
		webhookIDStr := r.Header.Get("X-Webhook-Id")
		return resolveByHMAC(r, body, ws, webhookIDStr, sigHeader)
	}

	if strings.HasPrefix(authHeader, "Bearer ") {
		raw := strings.TrimPrefix(authHeader, "Bearer ")
		if strings.HasPrefix(raw, webhookBearerPrefix) {
			return resolveByBearer(r, raw, ws)
		}
	}

	return nil, errWebhookNotFound
}

// resolveByBearer performs SHA-256 of the raw secret, then looks up the webhook
// by hash. Rejects revoked rows and rows that require HMAC.
func resolveByBearer(r *http.Request, rawSecret string, ws store.WebhookStore) (*store.WebhookData, error) {
	// Always compute hash — constant-time mitigation against timing oracle on
	// "does this prefix exist" (hash computation is fixed cost).
	h := sha256.Sum256([]byte(rawSecret))
	hashHex := hex.EncodeToString(h[:])

	webhook, err := ws.GetByHash(r.Context(), hashHex)
	if errors.Is(err, sql.ErrNoRows) || webhook == nil {
		return nil, errWebhookNotFound
	}
	if err != nil {
		return nil, errWebhookNotFound
	}
	if webhook.Revoked {
		return nil, errWebhookRevoked
	}
	if webhook.RequireHMAC {
		return nil, errWebhookBearerRequiresHMAC
	}
	return webhook, nil
}

// resolveByHMAC parses the X-GoClaw-Signature header, validates clock skew,
// looks up the webhook row by UUID, then verifies the HMAC.
//
// Signature format: "t=<unix_seconds>,v1=<hex_hmac_sha256>"
// Signed payload:   "<unix_seconds>.<raw_body>"
func resolveByHMAC(r *http.Request, body []byte, ws store.WebhookStore, webhookIDStr, sigHeader string) (*store.WebhookData, error) {
	// Parse t= and v1= from header.
	ts, sig, err := parseHMACHeader(sigHeader)
	if err != nil {
		return nil, errWebhookHMACInvalid
	}

	// Clock-skew check before any DB lookup (cheap).
	now := time.Now().Unix()
	if abs64(now-ts) > webhookHMACSkewSeconds {
		return nil, errWebhookTimestampSkew
	}

	// Look up webhook by UUID.
	webhookID, uuidErr := uuid.Parse(webhookIDStr)
	if uuidErr != nil {
		return nil, errWebhookNotFound
	}

	webhook, err := ws.GetByID(r.Context(), webhookID)
	if errors.Is(err, sql.ErrNoRows) || webhook == nil {
		return nil, errWebhookNotFound
	}
	if err != nil {
		return nil, errWebhookNotFound
	}
	if webhook.Revoked {
		return nil, errWebhookRevoked
	}

	// Recompute HMAC: HMAC_SHA256(secret_raw, "<ts>.<body>")
	// We store the SHA-256 hash of the secret, not the raw secret, so we
	// use the hash as the HMAC key (consistent with registration flow).
	//
	// NOTE: The HMAC key is the stored secret_hash (hex string bytes).
	// This means the caller must sign with the same value. Documented in
	// phase-08 webhooks.md. This is intentional — we never store raw secrets.
	secretKeyBytes, hexErr := hex.DecodeString(webhook.SecretHash)
	if hexErr != nil {
		// Malformed stored hash — treat as auth failure, never expose reason.
		return nil, errWebhookHMACInvalid
	}

	tsStr := strconv.FormatInt(ts, 10)
	signed := append([]byte(tsStr+"."), body...)
	mac := hmac.New(sha256.New, secretKeyBytes)
	_, _ = mac.Write(signed)
	expected := mac.Sum(nil)

	// Decode caller-provided hex signature.
	callerSig, decErr := hex.DecodeString(sig)
	if decErr != nil || len(callerSig) == 0 {
		return nil, errWebhookHMACInvalid
	}

	// Constant-time comparison — no early exit on mismatch.
	if subtle.ConstantTimeCompare(expected, callerSig) != 1 {
		return nil, errWebhookHMACInvalid
	}

	return webhook, nil
}

// readLimitedBody reads at most maxBytes from r.Body using http.MaxBytesReader.
// On success it replaces r.Body with a fresh NopCloser over the buffer so
// downstream JSON decoders see the same bytes. r.ContentLength is also updated.
func readLimitedBody(r *http.Request, maxBytes int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBytes)
	buf, err := io.ReadAll(r.Body)
	if err != nil {
		// http.MaxBytesReader returns an error when the limit is exceeded.
		return nil, err
	}
	// Restore body so downstream handlers can decode it.
	r.Body = io.NopCloser(bytes.NewReader(buf))
	r.ContentLength = int64(len(buf))
	return buf, nil
}

// parseHMACHeader splits "t=<unix>,v1=<hex>" into (timestamp, hexSig, error).
func parseHMACHeader(header string) (int64, string, error) {
	var ts int64
	var sig string
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "t="):
			v, err := strconv.ParseInt(strings.TrimPrefix(part, "t="), 10, 64)
			if err != nil {
				return 0, "", errors.New("invalid t= field")
			}
			ts = v
		case strings.HasPrefix(part, "v1="):
			sig = strings.TrimPrefix(part, "v1=")
		}
	}
	if ts == 0 || sig == "" {
		return 0, "", errors.New("missing t= or v1= field")
	}
	return ts, sig, nil
}

// isLoopback reports whether the RemoteAddr is a loopback address.
// Uses netip.ParseAddrPort for correct IPv4/IPv6 handling (not string prefix).
func isLoopback(remoteAddr string) bool {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		// Fall back: try parsing as bare address (no port).
		a, err2 := netip.ParseAddr(remoteAddr)
		if err2 != nil {
			return false
		}
		return a.IsLoopback()
	}
	return ap.Addr().IsLoopback()
}

// abs64 returns the absolute value of x.
func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}


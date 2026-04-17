package store

import (
	"context"
	"time"
)

// PancakePrivateReplyStore tracks which commenters have received the one-time
// private reply DM per Pancake page. Dedup persists across channel restarts.
//
// Scoped by tenant via TenantIDFromContext. Callers MUST set tenant context
// via WithTenantID; otherwise store returns ErrMissingTenantID.
type PancakePrivateReplyStore interface {
	// WasSent returns true if a private reply was sent to (pageID, senderID)
	// within the last ttl (based on sent_at column). Returns false for both
	// missing rows and expired rows (callers don't distinguish).
	WasSent(ctx context.Context, pageID, senderID string, ttl time.Duration) (bool, error)

	// MarkSent upserts the (tenant, page, sender) row with sent_at = NOW().
	// Idempotent: safe to call multiple times (overwrites sent_at).
	MarkSent(ctx context.Context, pageID, senderID string) error

	// TryClaim atomically checks-and-marks the send slot in a single round trip.
	// Returns claimed=true if the caller now owns the (tenant,page,sender) slot
	// (either freshly inserted or replaced an expired row) and should proceed
	// with sending the DM. Returns claimed=false if a fresh row already exists
	// within ttl — caller MUST skip.
	//
	// Pair with Unclaim on send failure so the slot can be re-won on the next
	// comment. Prevents the WasSent→Send→MarkSent TOCTOU where two concurrent
	// comments would both see "not sent" and both fire the API.
	TryClaim(ctx context.Context, pageID, senderID string, ttl time.Duration) (bool, error)

	// Unclaim removes the (tenant,page,sender) row unconditionally. Callers use
	// this to release a TryClaim that failed its follow-up API call so the next
	// comment from the same sender can retry. Returns nil if row is absent.
	Unclaim(ctx context.Context, pageID, senderID string) error

	// DeleteExpired removes rows where sent_at <= olderThan. Lazy cleanup.
	// Returns number of rows deleted.
	DeleteExpired(ctx context.Context, olderThan time.Time) (int64, error)
}

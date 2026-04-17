package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// PancakePrivateReplyStore persists the one-time DM dedup marker per
// (tenant, page, sender). Replaces the in-memory sync.Map so dedup survives
// channel restarts.
type PancakePrivateReplyStore struct {
	db *sql.DB
}

func NewPancakePrivateReplyStore(db *sql.DB) *PancakePrivateReplyStore {
	return &PancakePrivateReplyStore{db: db}
}

func (s *PancakePrivateReplyStore) WasSent(ctx context.Context, pageID, senderID string, ttl time.Duration) (bool, error) {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return false, fmt.Errorf("pancake_private_reply: missing tenant_id in context")
	}
	var sentAt time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT sent_at FROM pancake_private_reply_sent
		 WHERE tenant_id = $1 AND page_id = $2 AND sender_id = $3`,
		tenantID, pageID, senderID,
	).Scan(&sentAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pancake_private_reply: query: %w", err)
	}
	if ttl > 0 && time.Since(sentAt) > ttl {
		return false, nil
	}
	return true, nil
}

func (s *PancakePrivateReplyStore) MarkSent(ctx context.Context, pageID, senderID string) error {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return fmt.Errorf("pancake_private_reply: missing tenant_id in context")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pancake_private_reply_sent (tenant_id, page_id, sender_id, sent_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (tenant_id, page_id, sender_id)
		 DO UPDATE SET sent_at = NOW()`,
		tenantID, pageID, senderID,
	)
	if err != nil {
		return fmt.Errorf("pancake_private_reply: upsert: %w", err)
	}
	return nil
}

func (s *PancakePrivateReplyStore) TryClaim(ctx context.Context, pageID, senderID string, ttl time.Duration) (bool, error) {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return false, fmt.Errorf("pancake_private_reply: missing tenant_id in context")
	}
	cutoff := time.Now().Add(-ttl)

	// INSERT always attempts — on conflict, only UPDATE if existing row is
	// older than cutoff. RETURNING sent_at fires for both INSERT and successful
	// conflict-update paths; sql.ErrNoRows means the row exists but is fresh
	// (WHERE blocked the update) — claim denied.
	var claimedAt time.Time
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO pancake_private_reply_sent (tenant_id, page_id, sender_id, sent_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (tenant_id, page_id, sender_id)
		 DO UPDATE SET sent_at = NOW()
		 WHERE pancake_private_reply_sent.sent_at <= $4
		 RETURNING sent_at`,
		tenantID, pageID, senderID, cutoff,
	).Scan(&claimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pancake_private_reply: try_claim: %w", err)
	}
	return true, nil
}

func (s *PancakePrivateReplyStore) Unclaim(ctx context.Context, pageID, senderID string) error {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return fmt.Errorf("pancake_private_reply: missing tenant_id in context")
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM pancake_private_reply_sent
		 WHERE tenant_id = $1 AND page_id = $2 AND sender_id = $3`,
		tenantID, pageID, senderID,
	)
	if err != nil {
		return fmt.Errorf("pancake_private_reply: unclaim: %w", err)
	}
	return nil
}

func (s *PancakePrivateReplyStore) DeleteExpired(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM pancake_private_reply_sent WHERE sent_at <= $1`,
		olderThan,
	)
	if err != nil {
		return 0, fmt.Errorf("pancake_private_reply: delete expired: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

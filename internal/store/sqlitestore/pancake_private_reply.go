//go:build sqlite || sqliteonly

package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// SQLitePancakePrivateReplyStore implements store.PancakePrivateReplyStore
// backed by SQLite. Mirrors pg.PancakePrivateReplyStore — same interface,
// ? param placeholders, strftime timestamps.
type SQLitePancakePrivateReplyStore struct {
	db *sql.DB
}

func NewSQLitePancakePrivateReplyStore(db *sql.DB) *SQLitePancakePrivateReplyStore {
	return &SQLitePancakePrivateReplyStore{db: db}
}

// sqliteTimeFormat matches strftime('%Y-%m-%dT%H:%M:%fZ', 'now'): ISO8601 UTC
// with millisecond precision and trailing Z.
const sqliteTimeFormat = "2006-01-02T15:04:05.000Z"

func (s *SQLitePancakePrivateReplyStore) WasSent(ctx context.Context, pageID, senderID string, ttl time.Duration) (bool, error) {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return false, fmt.Errorf("pancake_private_reply: missing tenant_id in context")
	}
	var sentAtStr string
	err := s.db.QueryRowContext(ctx,
		`SELECT sent_at FROM pancake_private_reply_sent
		 WHERE tenant_id = ? AND page_id = ? AND sender_id = ?`,
		tenantID.String(), pageID, senderID,
	).Scan(&sentAtStr)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pancake_private_reply: query: %w", err)
	}
	sentAt, perr := time.Parse(sqliteTimeFormat, sentAtStr)
	if perr != nil {
		// Tolerate plain RFC3339 format if driver returned different layout.
		if sentAt, perr = time.Parse(time.RFC3339Nano, sentAtStr); perr != nil {
			return false, fmt.Errorf("pancake_private_reply: parse sent_at %q: %w", sentAtStr, perr)
		}
	}
	if ttl > 0 && time.Since(sentAt) > ttl {
		return false, nil
	}
	return true, nil
}

func (s *SQLitePancakePrivateReplyStore) MarkSent(ctx context.Context, pageID, senderID string) error {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return fmt.Errorf("pancake_private_reply: missing tenant_id in context")
	}
	now := time.Now().UTC().Format(sqliteTimeFormat)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pancake_private_reply_sent (tenant_id, page_id, sender_id, sent_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(tenant_id, page_id, sender_id)
		 DO UPDATE SET sent_at = excluded.sent_at`,
		tenantID.String(), pageID, senderID, now,
	)
	if err != nil {
		return fmt.Errorf("pancake_private_reply: upsert: %w", err)
	}
	return nil
}

func (s *SQLitePancakePrivateReplyStore) TryClaim(ctx context.Context, pageID, senderID string, ttl time.Duration) (bool, error) {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return false, fmt.Errorf("pancake_private_reply: missing tenant_id in context")
	}
	now := time.Now().UTC()
	nowStr := now.Format(sqliteTimeFormat)
	cutoffStr := now.Add(-ttl).Format(sqliteTimeFormat)

	// ON CONFLICT...DO UPDATE WHERE is SQLite-specific (>=3.24); current
	// modernc.org/sqlite ships SQLite >=3.40 so this is safe. RETURNING is
	// supported from 3.35.
	var claimedAt string
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO pancake_private_reply_sent (tenant_id, page_id, sender_id, sent_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(tenant_id, page_id, sender_id)
		 DO UPDATE SET sent_at = excluded.sent_at
		 WHERE pancake_private_reply_sent.sent_at <= ?
		 RETURNING sent_at`,
		tenantID.String(), pageID, senderID, nowStr, cutoffStr,
	).Scan(&claimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pancake_private_reply: try_claim: %w", err)
	}
	return true, nil
}

func (s *SQLitePancakePrivateReplyStore) Unclaim(ctx context.Context, pageID, senderID string) error {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return fmt.Errorf("pancake_private_reply: missing tenant_id in context")
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM pancake_private_reply_sent
		 WHERE tenant_id = ? AND page_id = ? AND sender_id = ?`,
		tenantID.String(), pageID, senderID,
	)
	if err != nil {
		return fmt.Errorf("pancake_private_reply: unclaim: %w", err)
	}
	return nil
}

func (s *SQLitePancakePrivateReplyStore) DeleteExpired(ctx context.Context, olderThan time.Time) (int64, error) {
	cutoff := olderThan.UTC().Format(sqliteTimeFormat)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM pancake_private_reply_sent WHERE sent_at <= ?`,
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("pancake_private_reply: delete expired: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

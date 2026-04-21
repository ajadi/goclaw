package pg

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// compile-time interface assertion
var _ store.WebhookStore = (*PGWebhookStore)(nil)

// PGWebhookStore implements store.WebhookStore using PostgreSQL.
type PGWebhookStore struct {
	db *sql.DB
}

// NewPGWebhookStore creates a new PostgreSQL-backed webhook store.
func NewPGWebhookStore(db *sql.DB) *PGWebhookStore {
	return &PGWebhookStore{db: db}
}

// webhookColumns is the canonical SELECT column list for webhooks.
const webhookColumns = `id, tenant_id, agent_id, name, kind, secret_prefix, secret_hash,
	scopes, channel_id, rate_limit_per_min, ip_allowlist,
	require_hmac, localhost_only, revoked, created_by,
	created_at, updated_at, last_used_at`

// scanWebhookRow scans a single webhooks row into WebhookData.
// scopes and ip_allowlist are scanned as raw bytes from PostgreSQL text[] columns.
func scanWebhookRow(row interface {
	Scan(dest ...any) error
}) (*store.WebhookData, error) {
	var w store.WebhookData
	var scopesRaw, ipAllowlistRaw []byte
	var agentID, channelID *uuid.UUID
	// secret_prefix and created_by are nullable TEXT columns.
	var secretPrefix, createdBy *string

	err := row.Scan(
		&w.ID, &w.TenantID, &agentID,
		&w.Name, &w.Kind, &secretPrefix, &w.SecretHash,
		&scopesRaw, &channelID, &w.RateLimitPerMin, &ipAllowlistRaw,
		&w.RequireHMAC, &w.LocalhostOnly, &w.Revoked, &createdBy,
		&w.CreatedAt, &w.UpdatedAt, &w.LastUsedAt,
	)
	if err != nil {
		return nil, err
	}
	w.AgentID = agentID
	w.ChannelID = channelID
	if secretPrefix != nil {
		w.SecretPrefix = *secretPrefix
	}
	if createdBy != nil {
		w.CreatedBy = *createdBy
	}
	scanStringArray(scopesRaw, &w.Scopes)
	scanStringArray(ipAllowlistRaw, &w.IPAllowlist)
	return &w, nil
}

func (s *PGWebhookStore) Create(ctx context.Context, w *store.WebhookData) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO webhooks
		 (id, tenant_id, agent_id, name, kind, secret_prefix, secret_hash,
		  scopes, channel_id, rate_limit_per_min, ip_allowlist,
		  require_hmac, localhost_only, revoked, created_by, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		w.ID, w.TenantID, nilUUID(w.AgentID),
		w.Name, w.Kind, nilStr(w.SecretPrefix), w.SecretHash,
		pqStringArray(w.Scopes), nilUUID(w.ChannelID), w.RateLimitPerMin, pqStringArray(w.IPAllowlist),
		w.RequireHMAC, w.LocalhostOnly, w.Revoked,
		nilStr(w.CreatedBy), w.CreatedAt, w.UpdatedAt,
	)
	return err
}

func (s *PGWebhookStore) GetByID(ctx context.Context, id uuid.UUID) (*store.WebhookData, error) {
	tid, err := requireTenantID(ctx)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+webhookColumns+`
		 FROM webhooks
		 WHERE id = $1 AND tenant_id = $2`,
		id, tid,
	)
	return scanWebhookRow(row)
}

func (s *PGWebhookStore) GetByHash(ctx context.Context, secretHash string) (*store.WebhookData, error) {
	tid, err := requireTenantID(ctx)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+webhookColumns+`
		 FROM webhooks
		 WHERE secret_hash = $1 AND tenant_id = $2 AND NOT revoked`,
		secretHash, tid,
	)
	return scanWebhookRow(row)
}

func (s *PGWebhookStore) List(ctx context.Context, f store.WebhookListFilter) ([]store.WebhookData, error) {
	tid, err := requireTenantID(ctx)
	if err != nil {
		return nil, err
	}

	q := `SELECT ` + webhookColumns + ` FROM webhooks WHERE tenant_id = $1`
	args := []any{tid}
	n := 2

	if f.AgentID != nil {
		q += fmt.Sprintf(` AND agent_id = $%d`, n)
		args = append(args, *f.AgentID)
		n++
	}
	q += ` ORDER BY created_at DESC`

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	q += fmt.Sprintf(` LIMIT $%d OFFSET $%d`, n, n+1)
	args = append(args, limit, f.Offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.WebhookData
	for rows.Next() {
		w, scanErr := scanWebhookRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

func (s *PGWebhookStore) Update(ctx context.Context, id uuid.UUID, updates map[string]any) error {
	tid, err := requireTenantID(ctx)
	if err != nil {
		return err
	}
	return execMapUpdateWhereTenant(ctx, s.db, "webhooks", updates, id, tid)
}

func (s *PGWebhookStore) RotateSecret(ctx context.Context, id uuid.UUID, newSecretHash, newPrefix string) error {
	tid, err := requireTenantID(ctx)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE webhooks SET secret_hash = $1, secret_prefix = $2, updated_at = $3
		 WHERE id = $4 AND tenant_id = $5`,
		newSecretHash, newPrefix, time.Now(), id, tid,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *PGWebhookStore) Revoke(ctx context.Context, id uuid.UUID) error {
	tid, err := requireTenantID(ctx)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE webhooks SET revoked = true, updated_at = $1
		 WHERE id = $2 AND tenant_id = $3`,
		time.Now(), id, tid,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *PGWebhookStore) TouchLastUsed(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webhooks SET last_used_at = $1 WHERE id = $2`,
		time.Now(), id,
	)
	return err
}

-- Tracks one-time private reply DM sent per (tenant, page, sender). Dedup
-- persists across Pancake channel restarts. Expired rows cleaned lazily by
-- channel runtime based on private_reply_ttl_days config.
CREATE TABLE pancake_private_reply_sent (
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    page_id     TEXT NOT NULL,
    sender_id   TEXT NOT NULL,
    sent_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, page_id, sender_id)
);

CREATE INDEX idx_pancake_private_reply_sent_at ON pancake_private_reply_sent (sent_at);

COMMENT ON TABLE pancake_private_reply_sent IS
    'Tracks first-DM sent to commenter per Pancake page. Dedup persists across restarts. Expired rows cleaned lazily based on config TTL.';

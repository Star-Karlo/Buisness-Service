-- Public tracking links: a customer follows one order through a link the
-- transporter copies from Control Tower, without an account.
--
-- The token is the whole secret: 32 random bytes, base64url. Nothing on the
-- public page is writable and nothing beyond that one order is readable, so a
-- leaked link exposes exactly what the transporter chose to share. A link is
-- revoked by stamping revoked_at rather than deleting the row, so the audit
-- of what was shared survives.
CREATE TABLE IF NOT EXISTS order_tracking_links (
    token       TEXT PRIMARY KEY,
    order_id    UUID NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    company_id  UUID NOT NULL,
    created_by  UUID,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_order_tracking_links_order ON order_tracking_links(order_id) WHERE revoked_at IS NULL;

-- Cold storage: what left the hot database, and where it went.
--
-- Orders, agreements and invoices are kept for years but read for weeks. Past
-- the retention window the archiver bundles each one with everything that
-- hangs off it, writes the bundle to S3 (Glacier Deep Archive), records it
-- here, and deletes the hot rows. This table is the only trace left behind,
-- and it is what a restore reads. Small: one row per archived aggregate.
CREATE TABLE IF NOT EXISTS archive_catalog (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entity          VARCHAR(32) NOT NULL,            -- order | agreement | invoice
    entity_id       UUID NOT NULL,
    -- Denormalised so "what did company X archive" is answerable without S3.
    company_id      UUID,
    reference       TEXT,                            -- order number, agreement number, invoice number
    s3_key          TEXT NOT NULL,
    storage_class   VARCHAR(32) NOT NULL,
    size_bytes      BIGINT NOT NULL,
    sha256          CHAR(64) NOT NULL,
    row_counts      JSONB NOT NULL DEFAULT '{}',     -- per table, for a sanity check on restore
    source_updated_at TIMESTAMPTZ,                   -- the aggregate's last change when archived
    archived_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    restored_at     TIMESTAMPTZ,                     -- set when the rows were put back
    UNIQUE (entity, entity_id)
);

CREATE INDEX IF NOT EXISTS idx_archive_catalog_company ON archive_catalog (company_id, entity, archived_at DESC);
CREATE INDEX IF NOT EXISTS idx_archive_catalog_reference ON archive_catalog (reference);

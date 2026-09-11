BEGIN;
DROP VIEW IF EXISTS agreement_price_history;
DROP INDEX IF EXISTS idx_orders_agreement_root;
ALTER TABLE orders DROP COLUMN IF EXISTS agreement_root_id;
DROP INDEX IF EXISTS idx_agreements_pending_approval;
DROP INDEX IF EXISTS idx_agreements_lineage;
DROP INDEX IF EXISTS idx_agreements_lineage_version;
ALTER TABLE agreements
    DROP CONSTRAINT IF EXISTS chk_agreement_version_lineage,
    DROP CONSTRAINT IF EXISTS chk_agreement_revision_kind,
    DROP COLUMN IF EXISTS superseded_at,
    DROP COLUMN IF EXISTS decision_note,
    DROP COLUMN IF EXISTS rejected_at,
    DROP COLUMN IF EXISTS rejected_by_user_id,
    DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS approved_by_user_id,
    DROP COLUMN IF EXISTS requested_at,
    DROP COLUMN IF EXISTS requested_by_user_id,
    DROP COLUMN IF EXISTS revision_note,
    DROP COLUMN IF EXISTS revision_kind,
    DROP COLUMN IF EXISTS supersedes_agreement_id,
    DROP COLUMN IF EXISTS root_agreement_id,
    DROP COLUMN IF EXISTS version;
COMMIT;

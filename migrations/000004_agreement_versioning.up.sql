-- Agreement versioning and the approval workflow.
--
-- An agreement is a priced contract, and until now it was one row updated in
-- place. That is the shape the legacy system had and it loses the only thing a
-- commercial dispute needs: what the price WAS when the work was done. An
-- amendment silently restated completed shipments, and "why does this invoice
-- disagree with the agreement" had no answer.
--
-- The PRD asks for two scenarios and they are the same mechanism:
--
--   Renewal — the term has ended and a new one is agreed. A new version
--             referring back to the old.
--   Update  — the price or terms change mid-term. Also a new version, and one
--             that a Sales Manager has to approve before it takes effect.
--
-- So versions are rows, not a column, and an order pins the version it was
-- priced against.

BEGIN;

-- ---------------------------------------------------------------------------
-- 1. Versions
-- ---------------------------------------------------------------------------

-- Every agreement row becomes version 1 of a lineage.
--
-- root_agreement_id is what makes "show me this contract's history" one query
-- instead of a recursive walk up parent links. The parent is kept as well,
-- because "what did this version change" needs the immediate predecessor and
-- the root cannot answer it.
ALTER TABLE agreements
    ADD COLUMN version INTEGER NOT NULL DEFAULT 1,

    -- The first version of this lineage. Self-referential on version 1, which
    -- is set by the backfill below and by the service on create — NOT NULL
    -- after the backfill, so there is no "lineage unknown" state to handle.
    ADD COLUMN root_agreement_id UUID REFERENCES agreements (id) ON DELETE RESTRICT,

    -- The version this one replaced. NULL on version 1.
    ADD COLUMN supersedes_agreement_id UUID REFERENCES agreements (id) ON DELETE RESTRICT,

    -- Why this version exists. NULL on version 1: the first version of a
    -- contract is not a change to anything.
    ADD COLUMN revision_kind VARCHAR(16),
    ADD COLUMN revision_note TEXT,

    -- Who asked, who decided. Separate from created_by_user_id, which stays
    -- the author: on an update requested by Sales and approved by a Sales
    -- Manager these are three different people, and collapsing any two of them
    -- loses the separation the approval exists to create.
    ADD COLUMN requested_by_user_id UUID,
    ADD COLUMN requested_at        TIMESTAMPTZ,
    ADD COLUMN approved_by_user_id UUID,
    ADD COLUMN approved_at         TIMESTAMPTZ,
    ADD COLUMN rejected_by_user_id UUID,
    ADD COLUMN rejected_at         TIMESTAMPTZ,
    ADD COLUMN decision_note       TEXT,

    -- Set when a later version takes over. The pair of nullable timestamps —
    -- this and approved_at — is what makes "which version was live on the 3rd
    -- of March" answerable, which is the question an invoice dispute asks.
    ADD COLUMN superseded_at TIMESTAMPTZ,

    ADD CONSTRAINT chk_agreement_revision_kind
        CHECK (revision_kind IS NULL OR revision_kind IN ('renewal', 'update')),

    -- Version 1 has no predecessor and no reason; every later version has both.
    -- Stated as a constraint rather than left to the service because a lineage
    -- with a gap cannot be repaired after the fact.
    ADD CONSTRAINT chk_agreement_version_lineage
        CHECK (
            (version = 1 AND supersedes_agreement_id IS NULL AND revision_kind IS NULL)
            OR
            (version > 1 AND supersedes_agreement_id IS NOT NULL AND revision_kind IS NOT NULL)
        );

-- Existing rows are version 1 of their own lineage.
UPDATE agreements SET root_agreement_id = id WHERE root_agreement_id IS NULL;

ALTER TABLE agreements
    ALTER COLUMN root_agreement_id SET NOT NULL;

-- One version number per lineage. Two people amending the same contract at the
-- same moment would otherwise both write version 3, and the history would show
-- a fork that the model has no way to express.
CREATE UNIQUE INDEX idx_agreements_lineage_version
    ON agreements (root_agreement_id, version)
    WHERE deleted_at IS NULL;

-- "Show me this contract's history", the detail screen's query.
CREATE INDEX idx_agreements_lineage
    ON agreements (root_agreement_id, version DESC)
    WHERE deleted_at IS NULL;

-- The approval queue: what is waiting on a Sales Manager right now.
CREATE INDEX idx_agreements_pending_approval
    ON agreements (transporter_company_id, requested_at)
    WHERE deleted_at IS NULL AND status_code = 'pendingApproval';

COMMENT ON COLUMN agreements.root_agreement_id IS
    'The first version of this lineage. Every version of one contract shares it, which makes history a single indexed read rather than a recursive walk.';

COMMENT ON COLUMN agreements.superseded_at IS
    'When a later version took over. With approved_at this answers "which version was live on date X" — the question an invoice dispute asks.';

-- ---------------------------------------------------------------------------
-- 2. Orders pin the version they were priced against
-- ---------------------------------------------------------------------------

-- Without this, an amendment restates the price of work already done.
--
-- orders.agreement_id already points at a specific version row — versions are
-- rows — so what is added here is the LINEAGE, so that "every order under this
-- contract" stays answerable across amendments. The two together are what let
-- an invoice show the price that applied and the contract it belonged to.
ALTER TABLE orders
    ADD COLUMN agreement_root_id UUID;

CREATE INDEX idx_orders_agreement_root
    ON orders (agreement_root_id)
    WHERE deleted_at IS NULL AND agreement_root_id IS NOT NULL;

-- Backfill: today every agreement is its own root.
UPDATE orders o
SET agreement_root_id = a.root_agreement_id
FROM agreements a
WHERE o.agreement_id = a.id AND o.agreement_root_id IS NULL;

COMMENT ON COLUMN orders.agreement_root_id IS
    'The agreement LINEAGE this order was placed under. agreement_id pins the exact version it was priced against; this survives amendments so "every order under this contract" stays answerable.';

-- ---------------------------------------------------------------------------
-- 3. Price history, as a queryable view
-- ---------------------------------------------------------------------------

-- "View price history" is a PRD requirement for all three personas, and it is a
-- read that would otherwise be assembled by hand at every call site: join the
-- rate lines to their version, order by version, and show what changed.
--
-- A view rather than a table because it is derived — the rates and versions
-- already hold every fact, and a second copy would be one more thing to keep
-- in step with them.
CREATE VIEW agreement_price_history AS
SELECT
    a.root_agreement_id,
    a.id            AS agreement_id,
    a.agreement_number,
    a.version,
    a.revision_kind,
    a.revision_note,
    a.status_code,
    a.valid_from,
    a.valid_until,
    a.approved_at,
    a.approved_by_user_id,
    a.superseded_at,
    r.id            AS rate_id,
    r.origin_city_id,
    r.destination_city_id,
    r.origin_district_id,
    r.destination_district_id,
    r.truck_type_id,
    r.pricing_type_id,
    r.price,
    r.currency_id
FROM agreements a
LEFT JOIN agreement_rates r ON r.agreement_id = a.id
WHERE a.deleted_at IS NULL;

COMMENT ON VIEW agreement_price_history IS
    'Every priced line of every version of every agreement, for the PRD''s "view price history". Derived, not stored: the rates and versions already hold the facts.';

COMMIT;

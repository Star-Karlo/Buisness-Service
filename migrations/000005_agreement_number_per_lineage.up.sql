-- Let the versions of one contract share its number.
--
-- Migration 000001 made agreement_number globally UNIQUE, which was right when
-- an agreement was one row. Versioning breaks it: AGR-JYA-ASB-000001 version 2
-- IS the same contract as version 1 and deliberately carries the same number —
-- minting a new one per amendment would make the history unrecognisable to
-- anyone holding the paper.
--
-- What must still hold is that two DIFFERENT contracts never share a number.
-- Since every lineage has exactly one version 1, and every version of a lineage
-- carries the lineage's number, uniqueness among version-1 rows gives exactly
-- that with no extra column to keep in step.
--
-- The partial index also excludes soft-deleted rows, which the old constraint
-- did not: a deleted agreement should not reserve its number forever. That
-- matches how every other unique index in this system is written.

BEGIN;

ALTER TABLE agreements
    DROP CONSTRAINT IF EXISTS agreements_agreement_number_key;

CREATE UNIQUE INDEX idx_agreements_number_per_lineage
    ON agreements (agreement_number)
    WHERE version = 1 AND deleted_at IS NULL;

COMMENT ON INDEX idx_agreements_number_per_lineage IS
    'One lineage per agreement number. Scoped to version 1 because every version of a contract shares its number, and each lineage has exactly one version 1.';

COMMIT;

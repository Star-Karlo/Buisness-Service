BEGIN;
DROP INDEX IF EXISTS idx_agreements_number_per_lineage;
-- Restoring the global constraint fails if any lineage has more than one
-- version, which is correct: those rows cannot exist under the old rule.
ALTER TABLE agreements ADD CONSTRAINT agreements_agreement_number_key UNIQUE (agreement_number);
COMMIT;

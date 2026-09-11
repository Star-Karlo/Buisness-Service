BEGIN;
DROP INDEX IF EXISTS idx_agreement_rates_unrouted;
DROP INDEX IF EXISTS idx_agreement_rates_customer;
ALTER TABLE agreement_rates
    DROP CONSTRAINT IF EXISTS chk_agreement_rate_route_status,
    DROP COLUMN IF EXISTS lane_level,
    DROP COLUMN IF EXISTS route_status,
    DROP COLUMN IF EXISTS routed_at,
    DROP COLUMN IF EXISTS route_cache_key,
    DROP COLUMN IF EXISTS distance_meters,
    DROP COLUMN IF EXISTS customer_company_id;
DROP TABLE IF EXISTS agreement_customers;
COMMIT;

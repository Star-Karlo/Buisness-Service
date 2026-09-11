BEGIN;
DROP INDEX IF EXISTS idx_orders_expiring;
ALTER TABLE orders DROP COLUMN IF EXISTS expires_at;
DROP TABLE IF EXISTS order_items;
DROP INDEX IF EXISTS idx_agreement_rates_lane_district;
ALTER TABLE agreement_rates
    DROP COLUMN IF EXISTS origin_district_id,
    DROP COLUMN IF EXISTS destination_district_id;
DROP TABLE IF EXISTS company_field_config;
DROP TABLE IF EXISTS field_definitions;
COMMIT;

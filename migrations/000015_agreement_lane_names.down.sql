-- Narrowing back fails on any row already storing a longer name, which is the
-- point of the widening; truncating here would silently corrupt a lane. The
-- view, the generated column and its index stand aside the same way.
DROP VIEW IF EXISTS agreement_price_history;
DROP INDEX IF EXISTS idx_agreement_rates_unrouted;
ALTER TABLE agreement_rates DROP COLUMN IF EXISTS lane_level;

ALTER TABLE agreement_rates
    ALTER COLUMN origin_city_id          TYPE VARCHAR(24),
    ALTER COLUMN destination_city_id     TYPE VARCHAR(24),
    ALTER COLUMN origin_district_id      TYPE VARCHAR(24),
    ALTER COLUMN destination_district_id TYPE VARCHAR(24);

ALTER TABLE agreement_rates
    ADD COLUMN lane_level VARCHAR(12)
    GENERATED ALWAYS AS (
        CASE
            WHEN origin_warehouse_id IS NOT NULL AND destination_warehouse_id IS NOT NULL
                THEN 'warehouse'
            WHEN origin_district_id IS NOT NULL AND destination_district_id IS NOT NULL
                THEN 'district'
            WHEN origin_city_id IS NOT NULL OR destination_city_id IS NOT NULL
                THEN 'city'
            ELSE 'any'
        END
    ) STORED;

CREATE INDEX idx_agreement_rates_unrouted
    ON agreement_rates (agreement_id)
    WHERE distance_meters IS NULL AND lane_level = 'warehouse';

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

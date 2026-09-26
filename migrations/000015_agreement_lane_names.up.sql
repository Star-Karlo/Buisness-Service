-- Lane columns hold NAMES, and 24 characters is not enough for them.
--
-- origin_city_id and its siblings were sized as identifiers. What the console
-- actually stores in them is the place's name — "KABUPATEN LABUHANBATU
-- SELATAN" is 29 characters — because there is no region table to hold an id
-- that would reference anything. 73 of the 513 places in the picker are longer
-- than 24, so an agreement on any of those lanes was refused by Postgres with
-- a 22001, which reached the planner as a bare 500 "Request failed". Multi
-- shipment failed every time, because the console joined its routes into one
-- string before sending them.
--
-- Widening a varchar upward does not rewrite the table, but two things read
-- these columns and both must stand aside for it: the price-history view, and
-- lane_level, a STORED generated column that classifies the lane. Postgres
-- refuses to alter a type either depends on. Both are dropped and recreated
-- verbatim; lane_level recomputes for every row on the way back, which is the
-- one real cost here and is proportional to the rate count, not the order
-- count.
DROP VIEW IF EXISTS agreement_price_history;
DROP INDEX IF EXISTS idx_agreement_rates_unrouted;
ALTER TABLE agreement_rates DROP COLUMN IF EXISTS lane_level;

ALTER TABLE agreement_rates
    ALTER COLUMN origin_city_id          TYPE VARCHAR(64),
    ALTER COLUMN destination_city_id     TYPE VARCHAR(64),
    ALTER COLUMN origin_district_id      TYPE VARCHAR(64),
    ALTER COLUMN destination_district_id TYPE VARCHAR(64);

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

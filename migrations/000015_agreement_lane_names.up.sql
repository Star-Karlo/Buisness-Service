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
-- Widening a varchar upward does not rewrite the table. The view has to be
-- dropped and rebuilt around it, though: Postgres refuses to alter the type of
-- a column a view selects. It is recreated verbatim below.
DROP VIEW IF EXISTS agreement_price_history;

ALTER TABLE agreement_rates
    ALTER COLUMN origin_city_id          TYPE VARCHAR(64),
    ALTER COLUMN destination_city_id     TYPE VARCHAR(64),
    ALTER COLUMN origin_district_id      TYPE VARCHAR(64),
    ALTER COLUMN destination_district_id TYPE VARCHAR(64);

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

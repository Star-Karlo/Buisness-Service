DROP INDEX IF EXISTS uq_shipment_handovers_field_token;
ALTER TABLE shipment_handovers DROP COLUMN IF EXISTS field_token;
DROP TABLE IF EXISTS shipment_pods;
ALTER TABLE shipments
    DROP COLUMN IF EXISTS accepted_at,
    DROP COLUMN IF EXISTS accepted_latitude,
    DROP COLUMN IF EXISTS accepted_longitude,
    DROP COLUMN IF EXISTS loading_cargo_matches,
    DROP COLUMN IF EXISTS loading_cargo_note,
    DROP COLUMN IF EXISTS loading_cargo_checked_at,
    DROP COLUMN IF EXISTS loading_cargo_checked_by,
    DROP COLUMN IF EXISTS unloading_cargo_matches,
    DROP COLUMN IF EXISTS unloading_cargo_note,
    DROP COLUMN IF EXISTS unloading_cargo_checked_at,
    DROP COLUMN IF EXISTS unloading_cargo_checked_by,
    DROP COLUMN IF EXISTS unloading_cargo_checked_via;

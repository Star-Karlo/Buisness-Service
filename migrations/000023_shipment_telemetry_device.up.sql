-- Which GPS device was on the truck when this work was assigned.
--
-- Positions from a hardware tracker are keyed by the device's IMEI, and the
-- truck a device belongs to is read from master data at the moment somebody
-- asks. That is true enough for "where is this truck now" and wrong for every
-- question about the past: move a box from one truck to another and the old
-- truck's history becomes unreachable through it, while the new truck's
-- appears to include trips it never made.
--
-- Recording the device here does not fix that in general — it is master data
-- that needs to remember which device was on which truck and when. What it
-- does fix is THIS order: whatever is swapped later, the shipment still names
-- the device whose track is its own, so an order's journey stays reproducible.
--
-- Null for a truck with no tracker, and for every shipment assigned before
-- this column existed. Both mean "not recorded", not "no device".
ALTER TABLE shipments
    ADD COLUMN IF NOT EXISTS telemetry_imei VARCHAR(32);

COMMENT ON COLUMN shipments.telemetry_imei IS
    'The IMEI of the tracker on the assigned truck, as it was at assignment. '
    'Read-time truck-to-device lookups cannot answer for the past; this can.';

CREATE INDEX IF NOT EXISTS idx_shipments_telemetry_imei
    ON shipments (telemetry_imei) WHERE telemetry_imei IS NOT NULL;

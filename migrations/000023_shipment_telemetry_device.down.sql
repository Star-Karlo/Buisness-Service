DROP INDEX IF EXISTS idx_shipments_telemetry_imei;
ALTER TABLE shipments DROP COLUMN IF EXISTS telemetry_imei;

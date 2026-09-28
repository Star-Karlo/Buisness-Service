DROP INDEX IF EXISTS uq_shipment_pods_open_stop;
CREATE UNIQUE INDEX IF NOT EXISTS uq_shipment_pods_open ON shipment_pods (shipment_id, stage) WHERE status = 'submitted';
ALTER TABLE shipment_pods DROP COLUMN IF EXISTS stop_id;

DROP INDEX IF EXISTS idx_shipment_handovers_live;
CREATE UNIQUE INDEX idx_shipment_handovers_live
    ON shipment_handovers (shipment_id, stage)
    WHERE verified_at IS NULL;
DROP INDEX IF EXISTS idx_shipment_handovers_stop;
ALTER TABLE shipment_handovers DROP COLUMN IF EXISTS stop_id;

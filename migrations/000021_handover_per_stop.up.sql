-- A code per unloading point, not one per delivery.
--
-- A journey that unloads at Semarang and then at Priok hands the goods over
-- twice, to two different people, and the status cycle says so: "OTP bongkar
-- terverifikasi #1 (Shipment 1)" and "#2 (Shipment 2)" are two steps. With one
-- handover per shipment the second point had no code at all — the PIC named on
-- that stop was never messaged and the driver walked straight to the POD.
ALTER TABLE shipment_handovers
    ADD COLUMN IF NOT EXISTS stop_id UUID REFERENCES order_stops(id) ON DELETE SET NULL;

COMMENT ON COLUMN shipment_handovers.stop_id IS
    'The visit this code hands over. NULL is the whole stage, which is what a '
    'two-ended journey has and what every row written before per-stop codes is.';

CREATE INDEX IF NOT EXISTS idx_shipment_handovers_stop
    ON shipment_handovers (stop_id) WHERE stop_id IS NOT NULL;

-- One live code per handover, where a handover is now a stop and not a stage.
--
-- The old index was (shipment_id, stage) WHERE verified_at IS NULL, so issuing
-- Priok's code would collide with Semarang's. Adding stop_id to it plainly
-- would not work either: NULL never equals NULL in a unique index, so two
-- stage-level rows would both be allowed and the shipment could hold two live
-- codes at once. Coalescing to the nil UUID gives NULL rows one shared value,
-- which restores the old guarantee for two-ended journeys exactly.
DROP INDEX IF EXISTS idx_shipment_handovers_live;
CREATE UNIQUE INDEX idx_shipment_handovers_live
    ON shipment_handovers (shipment_id, stage, COALESCE(stop_id, '00000000-0000-0000-0000-000000000000'::uuid))
    WHERE verified_at IS NULL;

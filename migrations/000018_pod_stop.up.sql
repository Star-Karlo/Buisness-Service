-- A POD belongs to a stop, when the journey has more than two.
--
-- shipment_pods carried a stage — loading or unloading — which is enough for a
-- trip with one of each. A delivery that unloads at Semarang and then at Priok
-- files two unloading PODs, and without knowing which stop each belongs to,
-- approving the first would finish the whole delivery: the review path
-- advances the shipment to unloaded and finished on the first approval it
-- sees.
--
-- Nullable, because a two-ended journey needs no stop and every POD already
-- filed has none.
ALTER TABLE shipment_pods
    ADD COLUMN IF NOT EXISTS stop_id UUID REFERENCES order_stops (id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_shipment_pods_stop ON shipment_pods (stop_id) WHERE stop_id IS NOT NULL;

-- The open-submission rule becomes per stop: one pending POD per stop, rather
-- than one per stage, or the second stop cannot file while the first is
-- waiting for review.
DROP INDEX IF EXISTS uq_shipment_pods_open;
CREATE UNIQUE INDEX IF NOT EXISTS uq_shipment_pods_open_stop
    ON shipment_pods (shipment_id, stage, COALESCE(stop_id, '00000000-0000-0000-0000-000000000000'::uuid))
    WHERE status = 'submitted';

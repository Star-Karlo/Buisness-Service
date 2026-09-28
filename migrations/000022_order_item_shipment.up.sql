-- Which shipment's goods a line of cargo is.
--
-- On a multi-shipment order the plan shown at a stop — tonnage, quantity,
-- volume — is that shipment's own items, not the order's. Without the number
-- every stop is checked against the whole order's weight, so unloading 2 000
-- of 4 500 kg at Semarang reads as 2 500 kg missing. Items are paired to a
-- shipment exactly as stops are: shipment k+1 is loadingPoints[k] with
-- unloadingPoints[k].
ALTER TABLE order_items
    ADD COLUMN IF NOT EXISTS shipment_no SMALLINT NOT NULL DEFAULT 1;

COMMENT ON COLUMN order_items.shipment_no IS
    'Which shipment of the order these goods belong to, 1-based. Every item '
    'of a single-shipment order is 1, which is also what an order written '
    'before shipments were numbered has.';

CREATE INDEX IF NOT EXISTS idx_order_items_shipment
    ON order_items (order_id, shipment_no);

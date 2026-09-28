-- A shipment is a PAIR of points, and the journey has a visit order.
--
-- The first cut of stops (000016) recorded every point of a journey but not
-- which unloading point belonged to which loading point: loads were numbered
-- first, then unloads, with nothing joining them. The product model is a pair
-- — loadingPoints[k] with unloadingPoints[k] is Shipment k+1, the arrays are
-- the same length, and each shipment carries its own items, weight and volume
-- (PRD Flow Order Multi Shipment, 4.2 and 4.3). Without the pairing there is
-- no Shipment 2 to report a plan against, and no way to state the one rule
-- the order of visits must obey.
ALTER TABLE order_stops
    ADD COLUMN IF NOT EXISTS shipment_no SMALLINT;

COMMENT ON COLUMN order_stops.shipment_no IS
    'Which shipment of the order this point belongs to, 1-based. A load and '
    'an unload sharing a number are the two ends of one shipment.';

-- Visit order is the planner's, and it lives on the order's detail beside the
-- point lists it indexes (orders.detail->'stopSequence', written by
-- PUT /orders/:id/stop-sequence), so the choice travels with the thing it
-- describes. The default is every Muat then every Bongkar, which is what the
-- journey does when nobody says otherwise; a planner may reorder in Allocate
-- to M1 -> B1 -> M2 -> B2 and the route, distance, ETA and toll follow that
-- order (PRD ALC-08).

-- Existing stops: loads and unloads were written in list order, so the k-th
-- load and the k-th unload are the pair. Numbering them is arithmetic on what
-- is already there rather than a guess.
WITH ranked AS (
    SELECT id,
           ROW_NUMBER() OVER (PARTITION BY order_id, kind ORDER BY seq) AS n
    FROM order_stops
    WHERE shipment_no IS NULL
)
UPDATE order_stops s
SET shipment_no = ranked.n
FROM ranked
WHERE s.id = ranked.id;

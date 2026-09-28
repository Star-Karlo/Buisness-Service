-- A journey has stops, not two ends.
--
-- An order carried origin_warehouse_id and destination_warehouse_id, and the
-- console sent the FIRST loading point and the LAST unloading point. Every
-- point in between existed only as text in detail.loadingPoints /
-- detail.unloadingPoints, which nothing on the server read. So an order for
-- Jakarta → Semarang → Priok was routed as Jakarta → Priok, priced for that
-- distance, and produced exactly one unloading event — at Priok. Semarang
-- never happened as far as the system was concerned.
--
-- Stops are rows now: what the driver must visit, in order, each with its own
-- arrival, cargo check and POD. The two order columns stay as the first and
-- last stop, because everything from pricing to the geofence watcher reads
-- them, and a change that large is not a prerequisite for recording the
-- middle of a journey.
CREATE TABLE IF NOT EXISTS order_stops (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id      UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,

    -- Visit order, 1-based, across the whole journey: loading stops first,
    -- then unloading. Not per-kind, so "which stop is next" is one comparison.
    seq           SMALLINT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('load', 'unload')),
    warehouse_id  VARCHAR(64) NOT NULL,

    -- Who is expected at this gate. Copied from the order at creation rather
    -- than read from master data later: the person the planner named is the
    -- person the driver was told to meet, even if the site's list changes.
    pic_name      TEXT,
    pic_phone     TEXT,

    -- The visit itself.
    arrived_at        TIMESTAMPTZ,
    arrived_latitude  NUMERIC(10,7),
    arrived_longitude NUMERIC(10,7),
    within_geofence   BOOLEAN,
    started_at        TIMESTAMPTZ,
    finished_at       TIMESTAMPTZ,

    -- The cargo check taken at this stop, and the POD that closed it.
    cargo_matches    BOOLEAN,
    cargo_note       TEXT,
    cargo_checked_at TIMESTAMPTZ,
    pod_id           UUID REFERENCES shipment_pods (id) ON DELETE SET NULL,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One row per position in the journey; re-running the builder replaces rather
-- than duplicates.
CREATE UNIQUE INDEX IF NOT EXISTS uq_order_stops_seq ON order_stops (order_id, seq);
CREATE INDEX IF NOT EXISTS idx_order_stops_order ON order_stops (order_id, seq);

COMMENT ON TABLE order_stops IS
    'Every point a journey must visit, in order. The order''s own origin and '
    'destination columns remain the first and last of these.';

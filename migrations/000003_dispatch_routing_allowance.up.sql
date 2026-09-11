-- Dispatch, routing and driver allowance.
--
-- The three things that happen between "sales finished the order" and "the
-- driver arrives": the planner picks a truck, the system works out the roads,
-- and somebody decides what the driver is paid up front.

BEGIN;

-- ---------------------------------------------------------------------------
-- 1. Route cache
-- ---------------------------------------------------------------------------

-- Routes MAPID has already computed, keyed by the request rather than by the
-- order that asked for it.
--
-- This is the point of the cache: the same warehouse pair is routed over and
-- over — every order on a lane, every re-open of a planner screen — and the
-- roads between two fixed points do not change between Tuesday and Wednesday.
-- Keying by order id instead would make the cache useless, because each order
-- is new. Keying by the geometry of the request means the second order on a
-- lane is free.
--
-- The key is a hash rather than the coordinates themselves because a route may
-- carry waypoints, so the natural key is variable-length; hashing gives a fixed
-- primary key that indexes properly.
CREATE TABLE route_cache (
    -- SHA-256 of profile + toll preference + every coordinate, rounded. See
    -- routing.CacheKey: the rounding is deliberate and is what makes two
    -- requests for the same warehouse agree despite float noise.
    cache_key       CHAR(64) PRIMARY KEY,

    profile         VARCHAR(16) NOT NULL,
    avoid_tolls     BOOLEAN NOT NULL DEFAULT FALSE,

    -- The request, kept so a cache entry can be explained and re-issued
    -- without the caller having to still hold the inputs.
    points          JSONB NOT NULL,

    distance_meters INTEGER NOT NULL,
    duration_seconds INTEGER NOT NULL,

    -- The road line and toll detail. Geometry is the bulk of the row — a
    -- 350km route is around 2,500 coordinate pairs — which is the other reason
    -- to cache rather than re-fetch.
    geometry        JSONB,
    bbox            JSONB,
    toll_segments   JSONB,

    -- Whether any part of the route is known to be tolled. Derived from
    -- toll_segments at write time so that "does this lane cross a toll road"
    -- is an indexed boolean rather than a JSONB scan.
    has_toll        BOOLEAN NOT NULL DEFAULT FALSE,

    -- Distance travelled on tolled stretches, which is what a toll estimate is
    -- computed from.
    toll_distance_meters INTEGER NOT NULL DEFAULT 0,

    hit_count       INTEGER NOT NULL DEFAULT 0,
    last_used_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Entries expire so that a road that genuinely changed — a new toll road,
    -- a closure — is eventually picked up. Long, because the failure mode of a
    -- slightly stale route is a distance a few percent out, not a wrong answer.
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '90 days'
);

-- Supports the eviction sweep.
CREATE INDEX idx_route_cache_expiry ON route_cache (expires_at);

COMMENT ON TABLE route_cache IS
    'MAPID routes keyed by the request, so the second order on a lane costs nothing. Not keyed by order: that would never hit.';

-- ---------------------------------------------------------------------------
-- 2. The routes an order actually has
-- ---------------------------------------------------------------------------

-- An order has two distinct journeys and they are not interchangeable.
--
--   haul     — origin warehouse to destination warehouse. Known as soon as the
--              order exists, priced against the agreement, and the same for
--              every candidate truck.
--   approach — the assigned driver's last known position to the origin
--              warehouse. Cannot exist before assignment, is different for
--              every candidate, and is what makes one truck a better choice
--              than another.
--
-- They are separate rows rather than two column sets because reassigning a
-- driver replaces the approach and must leave the haul untouched, and because
-- the allowance quotes both distances separately.
CREATE TABLE order_routes (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id        UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,

    leg             VARCHAR(12) NOT NULL,

    -- The cached computation this leg points at. A route is never copied into
    -- this table: two orders on the same lane share one cache row, and a leg
    -- that outlived its cache entry is recomputed rather than half-remembered.
    cache_key       CHAR(64) NOT NULL REFERENCES route_cache (cache_key) ON DELETE RESTRICT,

    -- The endpoints as resolved at the time, so the leg still means something
    -- after a warehouse is moved or a truck has driven on.
    from_lat        NUMERIC(10,7) NOT NULL,
    from_lon        NUMERIC(10,7) NOT NULL,
    to_lat          NUMERIC(10,7) NOT NULL,
    to_lon          NUMERIC(10,7) NOT NULL,

    -- Set when a planner asked for this leg to be planned again — the paid
    -- reroute. Recorded rather than overwritten silently, because "the distance
    -- changed after assignment" is a question the allowance reconciliation asks.
    rerouted_at         TIMESTAMPTZ,
    rerouted_by_user_id UUID,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_order_route_leg CHECK (leg IN ('haul', 'approach'))
);

-- One current route per leg per order. Replacing an approach on reassignment
-- is an upsert, and this is what makes that safe rather than accumulating
-- stale legs that later queries have to guess between.
CREATE UNIQUE INDEX idx_order_routes_leg ON order_routes (order_id, leg);

-- ---------------------------------------------------------------------------
-- 3. Uang sangu
-- ---------------------------------------------------------------------------

-- The driver's cash advance, attached to the order.
--
-- Entered manually, by whoever the company says may enter it — sales or
-- finance — which is why there is no calculator here. What the system supplies
-- is the evidence: both distances and a toll estimate, so the number is typed
-- against something rather than guessed.
--
-- A formula per company is where this is heading, but not yet. The shape below
-- is what makes that migration cheap when it comes: the components are already
-- itemised rather than being one total, so a calculator can later populate the
-- same rows a person types today, and the history stays comparable across the
-- change.
CREATE TABLE order_allowances (
    order_id        UUID PRIMARY KEY REFERENCES orders (id) ON DELETE CASCADE,

    -- The evidence, snapshotted at the time the advance was set rather than
    -- read live. A reroute after finalisation changes the live distance; it
    -- must not silently restate what somebody was paid against.
    haul_distance_meters     INTEGER,
    approach_distance_meters INTEGER,
    toll_estimate            NUMERIC(18,2),

    -- The itemised advance: [{code, label, amount, note}]. JSONB because the
    -- components a company uses are its own — BBM, tol, uang makan, ferry —
    -- and enumerating them as columns would mean a migration per customer.
    components      JSONB NOT NULL DEFAULT '[]'::jsonb,

    total           NUMERIC(18,2) NOT NULL DEFAULT 0,
    currency_id     VARCHAR(24),

    note            TEXT,

    -- Who set it, which matters because the company decides whether that is
    -- sales or finance and the answer is auditable rather than assumed.
    entered_by_user_id UUID,
    entered_at         TIMESTAMPTZ,

    -- Finalised means committed to the driver. After this the row is read-only
    -- to the normal path; a change is a new revision in the history below.
    finalised_at         TIMESTAMPTZ,
    finalised_by_user_id UUID,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_order_allowance_total CHECK (total >= 0)
);

-- Every version of the advance, because it is money and it gets revised.
--
-- The legacy system overwrote the figure in place, so "why was this driver paid
-- more than the sheet says" had no answer. Keeping revisions costs one insert.
CREATE TABLE order_allowance_history (
    id              BIGSERIAL PRIMARY KEY,
    order_id        UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,

    components      JSONB NOT NULL,
    total           NUMERIC(18,2) NOT NULL,

    reason          TEXT,
    changed_by_user_id UUID,
    changed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_order_allowance_history_order
    ON order_allowance_history (order_id, changed_at DESC);

-- ---------------------------------------------------------------------------
-- 4. Unloading handover
-- ---------------------------------------------------------------------------

-- The one-time code the receiving PIC gives the driver at the unloading point.
--
-- This is the proof that a real person at the destination accepted the goods.
-- The driver enters the PIC's WhatsApp number, the code goes to that number,
-- and the driver types back what the PIC reads out — so possession of the phone
-- at the delivery address is what is being demonstrated.
--
-- The code is stored hashed. It is short-lived and low-value, but it is still a
-- credential, and a plaintext column would mean anyone with database read
-- access could complete a delivery from their desk.
CREATE TABLE shipment_handovers (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    shipment_id     UUID NOT NULL REFERENCES shipments (id) ON DELETE CASCADE,

    stage           VARCHAR(16) NOT NULL DEFAULT 'unloading',

    pic_name        VARCHAR(128),
    pic_whatsapp    VARCHAR(32) NOT NULL,

    code_hash       BYTEA NOT NULL,

    -- Attempts are counted and capped. Without a cap a six-digit code is
    -- guessable by a bored driver in an afternoon.
    attempts        SMALLINT NOT NULL DEFAULT 0,
    max_attempts    SMALLINT NOT NULL DEFAULT 5,

    sent_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL,
    verified_at     TIMESTAMPTZ,

    -- Where the driver was when the code was verified, and whether that was
    -- inside the warehouse geofence. Recorded rather than enforced, matching
    -- how shipments already treat geofencing: a company can turn enforcement
    -- on later and still have the history to look back at.
    verified_lat    NUMERIC(10,7),
    verified_lon    NUMERIC(10,7),
    verified_within_geofence BOOLEAN,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_handover_stage CHECK (stage IN ('loading', 'unloading'))
);

-- The lookup is always "the live code for this shipment at this stage".
-- Partial, so superseded codes do not collide with the current one.
CREATE UNIQUE INDEX idx_shipment_handovers_live
    ON shipment_handovers (shipment_id, stage)
    WHERE verified_at IS NULL;

CREATE INDEX idx_shipment_handovers_shipment
    ON shipment_handovers (shipment_id, created_at DESC);

COMMIT;

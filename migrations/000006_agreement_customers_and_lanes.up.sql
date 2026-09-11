-- One agreement, several customers, and lanes at whatever granularity was
-- actually negotiated.
--
-- Until now an agreement named ONE shipper and its rates were city-to-city. Two
-- things were wrong with that:
--
--   * A transporter signs one contract covering several of its clients. Forcing
--     one agreement each meant the same terms typed repeatedly, and a price
--     change touched N documents instead of one.
--   * A lane is agreed at whatever level the parties actually said: sometimes a
--     city pair, sometimes a kecamatan, sometimes two named warehouses. The
--     columns for all three already existed; nothing recorded WHICH was meant,
--     so a reader could not tell a deliberate city-level lane from a
--     warehouse-level one somebody had not filled in.

BEGIN;

-- ---------------------------------------------------------------------------
-- 1. The customers an agreement covers
-- ---------------------------------------------------------------------------

-- A join table rather than more columns, because the count is genuinely
-- unbounded — a 3PL's framework agreement can cover a dozen clients.
--
-- agreements.shipper_company_id stays: it is the counterparty the agreement was
-- struck WITH, and it is what the agreement number is built from. This table is
-- who it COVERS, which is a superset — and for the ordinary single-customer
-- agreement, exactly the same one company.
CREATE TABLE agreement_customers (
    agreement_id        UUID NOT NULL REFERENCES agreements (id) ON DELETE CASCADE,
    customer_company_id UUID NOT NULL,

    -- What this client is called on this contract, when it differs from the
    -- company's own name. Framework agreements often name a division.
    label       VARCHAR(128),

    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (agreement_id, customer_company_id)
);

CREATE INDEX idx_agreement_customers_customer
    ON agreement_customers (customer_company_id);

COMMENT ON TABLE agreement_customers IS
    'Which client companies an agreement covers. agreements.shipper_company_id remains the counterparty it was struck with and the one its number is built from; this is who it applies to.';

-- Every existing agreement covers exactly the shipper it names.
INSERT INTO agreement_customers (agreement_id, customer_company_id)
SELECT id, shipper_company_id FROM agreements WHERE deleted_at IS NULL
ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------
-- 2. Lanes: whose, at what level, and how far
-- ---------------------------------------------------------------------------

ALTER TABLE agreement_rates
    -- Which customer this price is for. NULL means EVERY customer on the
    -- agreement, which is the common case — one set of lanes at one price,
    -- covering all of them. A value narrows the lane to one client, for the
    -- contract that prices the same route differently per division.
    ADD COLUMN customer_company_id UUID,

    -- The road distance, once MAPID has been asked.
    --
    -- Stored rather than computed on read: a rate is priced against the
    -- distance agreed at signing, and a road that changes later must not
    -- silently restate what a contract says. It is also what the allowance and
    -- the invoice read, so recomputing per read would be a routing call per row.
    ADD COLUMN distance_meters INTEGER,

    -- The cached computation behind that number, so the geometry can be drawn
    -- and the figure explained without asking MAPID again.
    ADD COLUMN route_cache_key CHAR(64) REFERENCES route_cache (cache_key) ON DELETE SET NULL,

    ADD COLUMN routed_at TIMESTAMPTZ,

    -- Why a lane has no distance. Not every lane can have one: a city-to-city
    -- lane has no coordinates to route between, and saying so is better than
    -- an empty column a reader has to guess about.
    ADD COLUMN route_status VARCHAR(24),

    ADD CONSTRAINT chk_agreement_rate_route_status
        CHECK (route_status IS NULL OR route_status IN
               ('routed', 'noCoordinates', 'unroutable', 'pending'));

-- The granularity, GENERATED rather than stored by the writer.
--
-- Derivable from which columns are filled, and a generated column is how that
-- stays true: a writer that sets a warehouse and forgets to update a hand-kept
-- level column would produce a row claiming to be one thing and behaving as
-- another. Postgres recomputes it on every write instead.
--
-- Warehouse beats district beats city, because the most specific pair named is
-- what the parties meant.
ALTER TABLE agreement_rates
    ADD COLUMN lane_level VARCHAR(12)
    GENERATED ALWAYS AS (
        CASE
            WHEN origin_warehouse_id IS NOT NULL AND destination_warehouse_id IS NOT NULL
                THEN 'warehouse'
            WHEN origin_district_id IS NOT NULL AND destination_district_id IS NOT NULL
                THEN 'district'
            WHEN origin_city_id IS NOT NULL OR destination_city_id IS NOT NULL
                THEN 'city'
            ELSE 'any'
        END
    ) STORED;

CREATE INDEX idx_agreement_rates_customer
    ON agreement_rates (agreement_id, customer_company_id);

-- Supports "which lanes still need routing", which the sweep asks.
CREATE INDEX idx_agreement_rates_unrouted
    ON agreement_rates (agreement_id)
    WHERE distance_meters IS NULL AND lane_level = 'warehouse';

COMMENT ON COLUMN agreement_rates.customer_company_id IS
    'Which customer this lane prices for. NULL means every customer the agreement covers, which is the ordinary case.';

COMMENT ON COLUMN agreement_rates.lane_level IS
    'How specific the lane is: warehouse, district, city or any. Generated, so it cannot disagree with the columns it describes.';

COMMENT ON COLUMN agreement_rates.route_status IS
    'Why distance_meters is what it is. noCoordinates means the lane is city or district level and has no points to route between.';

COMMIT;

-- Business service schema: agreements, orders, the shipment lifecycle, invoices.
--
-- Cross-service references (companies, users, trucks, warehouses, catalogue
-- entries) are stored as bare ids with no foreign key, because they live in
-- other databases. Auth and company ids are UUIDs from Postgres; truck,
-- warehouse and catalogue ids are 24-character Mongo ObjectId hex strings.
-- The column types record that difference so it stays visible.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ---------------------------------------------------------------------------
-- Agreements
-- ---------------------------------------------------------------------------

CREATE TABLE agreements (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id               VARCHAR(24) UNIQUE,
    agreement_number        VARCHAR(64) NOT NULL UNIQUE,

    shipper_company_id      UUID NOT NULL,
    transporter_company_id  UUID NOT NULL,
    created_by_user_id      UUID NOT NULL,

    status_code             VARCHAR(32) NOT NULL DEFAULT 'draft',
    status                  VARCHAR(128),

    valid_from              DATE NOT NULL,
    valid_until             DATE NOT NULL,

    -- verified marks an agreement a shipper has explicitly approved. Companies
    -- with active_agreement_verified_only refuse to order against an
    -- unverified one.
    verified                BOOLEAN NOT NULL DEFAULT FALSE,
    verified_by_user_id     UUID,
    verified_at             TIMESTAMPTZ,

    payment_type_id         VARCHAR(24),
    currency_id             VARCHAR(24),

    -- detail holds the negotiated terms: the route/price matrix, requirements
    -- and cargo restrictions. JSONB because the shape varies by pricing model.
    detail                  JSONB NOT NULL DEFAULT '{}'::jsonb,

    deleted_at              TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_agreement_validity CHECK (valid_until >= valid_from)
);

CREATE INDEX idx_agreements_shipper     ON agreements (shipper_company_id, status_code) WHERE deleted_at IS NULL;
CREATE INDEX idx_agreements_transporter ON agreements (transporter_company_id, status_code) WHERE deleted_at IS NULL;
-- Supports the nightly expiry sweep.
CREATE INDEX idx_agreements_expiry      ON agreements (valid_until) WHERE deleted_at IS NULL AND status_code NOT IN ('expired', 'cancelled');

-- Agreement price lines: one row per origin-destination-trucktype combination.
-- The legacy system buried these in an array inside the agreement document,
-- which made "what did we charge on this lane" unanswerable without scanning
-- every agreement.
CREATE TABLE agreement_rates (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agreement_id            UUID NOT NULL REFERENCES agreements (id) ON DELETE CASCADE,

    origin_warehouse_id     VARCHAR(24),
    destination_warehouse_id VARCHAR(24),
    origin_city_id          VARCHAR(24),
    destination_city_id     VARCHAR(24),
    truck_type_id           VARCHAR(24),

    pricing_type_id         VARCHAR(24),
    price                   NUMERIC(18,2) NOT NULL,
    min_quantity            NUMERIC(12,2),
    currency_id             VARCHAR(24),

    lead_time_hours         INTEGER,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_agreement_rate_price CHECK (price >= 0)
);

CREATE INDEX idx_agreement_rates_agreement ON agreement_rates (agreement_id);
CREATE INDEX idx_agreement_rates_lane      ON agreement_rates (origin_city_id, destination_city_id, truck_type_id);

-- ---------------------------------------------------------------------------
-- Orders
-- ---------------------------------------------------------------------------

CREATE TABLE orders (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id               VARCHAR(24) UNIQUE,
    order_number            VARCHAR(64) NOT NULL UNIQUE,

    agreement_id            UUID REFERENCES agreements (id) ON DELETE RESTRICT,

    shipper_company_id      UUID NOT NULL,
    transporter_company_id  UUID,
    created_by_user_id      UUID NOT NULL,

    -- parent_order_id links a 3PL child order to the order it was spawned from.
    parent_order_id         UUID REFERENCES orders (id) ON DELETE SET NULL,

    -- order_kind separates the flows the legacy system kept in one collection
    -- with divergent branches: normal freight, an empty repositioning trip
    -- ("ngosong"), and a 3PL sub-contract.
    order_kind              VARCHAR(16) NOT NULL DEFAULT 'standard',

    status_code             VARCHAR(32) NOT NULL DEFAULT 'draft',
    status                  VARCHAR(128),
    status_alias            VARCHAR(128),

    driver_user_id          UUID,
    truck_id                VARCHAR(24),

    origin_warehouse_id     VARCHAR(24),
    destination_warehouse_id VARCHAR(24),

    cargo_type_id           VARCHAR(24),
    item_type_id            VARCHAR(24),
    quantity                NUMERIC(12,2),
    weight_kg               NUMERIC(12,2),
    volume_m3               NUMERIC(12,2),

    pickup_at               TIMESTAMPTZ,
    delivery_at             TIMESTAMPTZ,

    price                   NUMERIC(18,2),
    currency_id             VARCHAR(24),

    customer_id             VARCHAR(24),
    reference_number        VARCHAR(64),

    detail                  JSONB NOT NULL DEFAULT '{}'::jsonb,

    cancelled_at            TIMESTAMPTZ,
    cancelled_by_user_id    UUID,
    cancel_reason           TEXT,

    deleted_at              TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_order_kind CHECK (order_kind IN ('standard', 'empty', 'threepl')),
    CONSTRAINT chk_order_no_self_parent CHECK (parent_order_id IS NULL OR parent_order_id <> id)
);

-- The listing indexes. Every order query is scoped to a company and usually to
-- a status, so these two carry most of the read load.
CREATE INDEX idx_orders_shipper      ON orders (shipper_company_id, status_code, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_orders_transporter  ON orders (transporter_company_id, status_code, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_orders_driver       ON orders (driver_user_id, status_code) WHERE deleted_at IS NULL AND driver_user_id IS NOT NULL;
CREATE INDEX idx_orders_truck        ON orders (truck_id) WHERE deleted_at IS NULL AND truck_id IS NOT NULL;
CREATE INDEX idx_orders_agreement    ON orders (agreement_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_orders_parent       ON orders (parent_order_id) WHERE parent_order_id IS NOT NULL;
CREATE INDEX idx_orders_pickup       ON orders (pickup_at) WHERE deleted_at IS NULL;

-- Every status change, so "when did this order become late" is answerable.
-- The legacy system overwrote status in place and kept no history.
CREATE TABLE order_status_history (
    id                      BIGSERIAL PRIMARY KEY,
    order_id                UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    from_status_code        VARCHAR(32),
    to_status_code          VARCHAR(32) NOT NULL,
    changed_by_user_id      UUID,
    -- source records what drove the change: a user action, a cron sweep, or a
    -- geofence event reported by the telemetry service.
    source                  VARCHAR(32) NOT NULL DEFAULT 'user',
    note                    TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_order_status_history_order ON order_status_history (order_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- Shipments
-- ---------------------------------------------------------------------------

-- A shipment is one physical execution of an order. It is separate from the
-- order because an order can be re-run after a failed attempt, and because the
-- lifecycle timestamps below are what the POD and the invoice are built from.
CREATE TABLE shipments (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id                UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,

    driver_user_id          UUID,
    truck_id                VARCHAR(24),

    status_code             VARCHAR(32) NOT NULL DEFAULT 'assigned',

    -- The lifecycle. Each column is one step of the driver app's flow, in the
    -- order the legacy ShipmentController advanced them.
    started_to_loading_at   TIMESTAMPTZ,
    arrived_loading_at      TIMESTAMPTZ,
    loading_started_at      TIMESTAMPTZ,
    loading_finished_at     TIMESTAMPTZ,
    started_to_unloading_at TIMESTAMPTZ,
    arrived_unloading_at    TIMESTAMPTZ,
    unloading_started_at    TIMESTAMPTZ,
    unloading_finished_at   TIMESTAMPTZ,
    finished_at             TIMESTAMPTZ,

    -- Positions captured at the moments that matter for dispute resolution.
    loading_latitude        NUMERIC(10,7),
    loading_longitude       NUMERIC(10,7),
    unloading_latitude      NUMERIC(10,7),
    unloading_longitude     NUMERIC(10,7),

    -- Whether each arrival was inside the warehouse geofence. Recorded rather
    -- than enforced, so a company can turn enforcement on later and still have
    -- the history.
    loading_within_geofence   BOOLEAN,
    unloading_within_geofence BOOLEAN,

    distance_meters         INTEGER,
    toll_cost               NUMERIC(18,2),

    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_shipments_order  ON shipments (order_id);
-- Supports the telemetry service asking which shipment a driver's fix belongs to.
CREATE INDEX idx_shipments_driver_active ON shipments (driver_user_id)
    WHERE finished_at IS NULL AND driver_user_id IS NOT NULL;

-- Proof of delivery documents and loading/unloading checklists.
CREATE TABLE shipment_documents (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    shipment_id             UUID NOT NULL REFERENCES shipments (id) ON DELETE CASCADE,
    doc_type                VARCHAR(32) NOT NULL,
    stage                   VARCHAR(16),
    file_url                TEXT NOT NULL,
    uploaded_by_user_id     UUID,
    -- verified_at is set when a warehouse PIC signs the document off.
    verified_at             TIMESTAMPTZ,
    verified_by_user_id     UUID,
    metadata                JSONB,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_shipment_doc_stage CHECK (stage IS NULL OR stage IN ('loading', 'unloading'))
);

CREATE INDEX idx_shipment_documents_shipment ON shipment_documents (shipment_id);

-- ---------------------------------------------------------------------------
-- Invoices
-- ---------------------------------------------------------------------------

-- Invoices here are the freight billing record. The separate accounting service
-- owns the ledger; this table is what it reads from.
CREATE TABLE invoices (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id               VARCHAR(24) UNIQUE,
    invoice_number          VARCHAR(64) NOT NULL UNIQUE,

    shipper_company_id      UUID NOT NULL,
    transporter_company_id  UUID NOT NULL,

    status_code             VARCHAR(32) NOT NULL DEFAULT 'draft',

    -- Amounts are NUMERIC, never floating point: money that does not add up is
    -- worse than money that is slow to compute.
    subtotal                NUMERIC(18,2) NOT NULL DEFAULT 0,
    ppn_percentage          NUMERIC(6,4) NOT NULL DEFAULT 0,
    ppn_amount              NUMERIC(18,2) NOT NULL DEFAULT 0,
    pph23_percentage        NUMERIC(6,4) NOT NULL DEFAULT 0,
    pph23_amount            NUMERIC(18,2) NOT NULL DEFAULT 0,
    adjustment              NUMERIC(18,2) NOT NULL DEFAULT 0,
    total                   NUMERIC(18,2) NOT NULL DEFAULT 0,
    currency_id             VARCHAR(24),

    issued_at               TIMESTAMPTZ,
    due_at                  TIMESTAMPTZ,
    submitted_at            TIMESTAMPTZ,
    verified_at             TIMESTAMPTZ,
    paid_at                 TIMESTAMPTZ,
    cancelled_at            TIMESTAMPTZ,

    notes                   TEXT,
    detail                  JSONB NOT NULL DEFAULT '{}'::jsonb,

    deleted_at              TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_invoices_shipper     ON invoices (shipper_company_id, status_code) WHERE deleted_at IS NULL;
CREATE INDEX idx_invoices_transporter ON invoices (transporter_company_id, status_code) WHERE deleted_at IS NULL;
CREATE INDEX idx_invoices_due         ON invoices (due_at) WHERE deleted_at IS NULL AND paid_at IS NULL;

-- One invoice covers many orders, and re-invoicing must not silently detach the
-- old lines, so the link is its own table.
CREATE TABLE invoice_lines (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    invoice_id              UUID NOT NULL REFERENCES invoices (id) ON DELETE CASCADE,
    order_id                UUID NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,

    description             TEXT,
    quantity                NUMERIC(12,2) NOT NULL DEFAULT 1,
    unit_price              NUMERIC(18,2) NOT NULL DEFAULT 0,
    amount                  NUMERIC(18,2) NOT NULL DEFAULT 0,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- An order must not be billed twice on the same invoice.
    UNIQUE (invoice_id, order_id)
);

CREATE INDEX idx_invoice_lines_invoice ON invoice_lines (invoice_id);
CREATE INDEX idx_invoice_lines_order   ON invoice_lines (order_id);

-- ---------------------------------------------------------------------------
-- Ratings
-- ---------------------------------------------------------------------------

CREATE TABLE ratings (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id                UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    rated_user_id           UUID NOT NULL,
    rated_by_user_id        UUID NOT NULL,
    score                   SMALLINT NOT NULL,
    comment                 TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_rating_score CHECK (score BETWEEN 1 AND 5),
    -- One rating per rater per order.
    UNIQUE (order_id, rated_by_user_id)
);

CREATE INDEX idx_ratings_rated_user ON ratings (rated_user_id);

-- ---------------------------------------------------------------------------
-- Number sequences
-- ---------------------------------------------------------------------------

-- Human-facing document numbers reset per year and per company. A dedicated
-- table with an atomic increment replaces the legacy "uniqid" collection, which
-- read-then-wrote and could hand the same number to two concurrent requests.
CREATE TABLE number_sequences (
    scope                   VARCHAR(64) NOT NULL,
    period                  VARCHAR(16) NOT NULL,
    current_value           BIGINT NOT NULL DEFAULT 0,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (scope, period)
);

-- next_sequence_value atomically reserves the next number in a scope.
-- INSERT ... ON CONFLICT DO UPDATE holds a row lock for the duration, so two
-- concurrent callers cannot receive the same value.
CREATE OR REPLACE FUNCTION next_sequence_value(p_scope VARCHAR, p_period VARCHAR)
RETURNS BIGINT AS $$
DECLARE
    v_next BIGINT;
BEGIN
    INSERT INTO number_sequences (scope, period, current_value)
    VALUES (p_scope, p_period, 1)
    ON CONFLICT (scope, period)
    DO UPDATE SET current_value = number_sequences.current_value + 1,
                  updated_at    = NOW()
    RETURNING current_value INTO v_next;

    RETURN v_next;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------------
-- updated_at maintenance
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_agreements_updated_at BEFORE UPDATE ON agreements FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_orders_updated_at     BEFORE UPDATE ON orders     FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_shipments_updated_at  BEFORE UPDATE ON shipments  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_invoices_updated_at   BEFORE UPDATE ON invoices   FOR EACH ROW EXECUTE FUNCTION set_updated_at();

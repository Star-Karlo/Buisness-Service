-- Configurable detail, dispatch, routing and allowances.
--
-- The flow itself is fixed and is not modelled here: agreement -> order ->
-- dispatch -> driver -> loading -> unloading is the same for every company, so
-- it stays in the state machines where it can be read in one place. What
-- varies between companies is the DETAIL each step captures, and that is what
-- this migration makes configurable.
--
-- Two examples drive the design, and both turn out to be the same thing:
--   * one company's routes stop at the city, another needs the kecamatan
--   * one company's orders name a cargo category, another itemises components
-- Neither is a different flow. Both are "is this field required, optional, or
-- absent for this company", which is one mechanism rather than two.

BEGIN;

-- ---------------------------------------------------------------------------
-- 1. Configurable fields
-- ---------------------------------------------------------------------------

-- The catalogue of fields a company may configure.
--
-- Deliberately the same shape as authentication's permission_catalog, and for
-- the same reason: the CODE declares which fields exist, the DATABASE decides
-- how they are presented and whether they are demanded. Inserting a row here
-- enables nothing, because a field means something only where a handler reads
-- it and a form renders it. A row for a field the code does not declare would
-- otherwise appear in the configurator, be switched on, and do nothing — with
-- no error anywhere to find.
--
--   code --declares--> field_definitions --read by--> configurator + forms
CREATE TABLE field_definitions (
    -- Which form the field belongs to. Not an enum: adding a third
    -- configurable entity should not need a type migration.
    entity      VARCHAR(24) NOT NULL,

    -- Dotted path into the entity's payload, e.g. 'route.originDistrictId'.
    -- The path is what the form binds to and what validation walks, so it is
    -- the identity of the field rather than a label for it.
    key         VARCHAR(64) NOT NULL,

    data_type   VARCHAR(16) NOT NULL,

    -- FALSE for fields that exist but must not be switched off — an order with
    -- no origin warehouse is not a configuration choice, it is a broken order.
    -- Declaring it here means the configurator cannot offer the toggle at all,
    -- rather than the save failing later with a message nobody expected.
    configurable BOOLEAN NOT NULL DEFAULT TRUE,

    -- What a company gets if it has never configured this field. Stated per
    -- field rather than assumed, because the sensible default differs: a
    -- kecamatan is 'hidden' until asked for, an origin warehouse is 'required'
    -- always.
    default_requirement VARCHAR(12) NOT NULL DEFAULT 'optional',

    -- Presentation, supplied by the code and refreshable at startup.
    group_name  VARCHAR(64) NOT NULL,
    label       VARCHAR(128) NOT NULL,
    help_text   TEXT,
    sort_order  INTEGER NOT NULL DEFAULT 0,

    -- FALSE once the code stops declaring it. Marked rather than deleted so
    -- that "which companies still configure something that no longer exists"
    -- stays an answerable question.
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,

    synced_at   TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (entity, key),

    CONSTRAINT chk_field_default_requirement
        CHECK (default_requirement IN ('required', 'optional', 'hidden')),
    CONSTRAINT chk_field_data_type
        CHECK (data_type IN ('string', 'number', 'boolean', 'date', 'datetime', 'ref', 'list'))
);

CREATE INDEX idx_field_definitions_active
    ON field_definitions (entity, group_name, sort_order) WHERE is_active;

COMMENT ON TABLE field_definitions IS
    'Queryable copy of the configurable fields the CODE declares, synced at startup. Inserting a row enables nothing.';

-- What one company decided about one field.
--
-- Only rows that DIFFER from the declared default are stored. A company with no
-- rows behaves exactly as the code intends, which means shipping a new field
-- does not require writing 6,000 rows, and reading the effective configuration
-- is a left join rather than a demand that every company be backfilled.
CREATE TABLE company_field_config (
    company_id  UUID NOT NULL,
    entity      VARCHAR(24) NOT NULL,
    key         VARCHAR(64) NOT NULL,

    requirement VARCHAR(12) NOT NULL,

    -- A company's own wording for the field. Presentation only; it never
    -- changes what is stored or validated.
    label_override VARCHAR(128),

    updated_by_user_id UUID,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (company_id, entity, key),

    -- The FK is what stops a configuration row outliving its field. Without it
    -- a renamed field key would leave rows that match nothing, and the company
    -- would silently revert to the default with no trace of why.
    CONSTRAINT fk_company_field_config_definition
        FOREIGN KEY (entity, key) REFERENCES field_definitions (entity, key) ON DELETE CASCADE,

    CONSTRAINT chk_company_field_requirement
        CHECK (requirement IN ('required', 'optional', 'hidden'))
);

CREATE INDEX idx_company_field_config_lookup
    ON company_field_config (company_id, entity);

-- ---------------------------------------------------------------------------
-- 2. The detail those configurable fields need somewhere to go
-- ---------------------------------------------------------------------------

-- Kecamatan-level lanes. A company routing at city level leaves these NULL and
-- the existing city columns carry the lane, so enabling district granularity
-- is additive and does not invalidate agreements written before it.
ALTER TABLE agreement_rates
    ADD COLUMN origin_district_id      VARCHAR(24),
    ADD COLUMN destination_district_id VARCHAR(24);

CREATE INDEX idx_agreement_rates_lane_district
    ON agreement_rates (origin_district_id, destination_district_id)
    WHERE origin_district_id IS NOT NULL;

-- Component-level cargo, for the companies that itemise.
--
-- A separate table rather than a JSONB array on the order, because these are
-- reconciled against what was actually loaded and unloaded: plan-versus-actual
-- per line is the number a dispute turns on, and an array cannot be joined,
-- indexed or partially updated. Companies that only name a cargo category
-- simply have no rows here.
CREATE TABLE order_items (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id    UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,

    -- The master data item this line instantiates, when it came from the
    -- catalogue. NULL for a one-off line typed at order time, which is allowed
    -- because refusing it would push people back to writing it in the notes.
    catalog_item_id VARCHAR(24),

    name        VARCHAR(255) NOT NULL,
    quantity    NUMERIC(12,2),
    unit        VARCHAR(24),
    packaging   VARCHAR(32),
    weight_kg   NUMERIC(12,2),

    length_cm   NUMERIC(10,2),
    width_cm    NUMERIC(10,2),
    height_cm   NUMERIC(10,2),

    -- Volume is stored, not computed on read, because it is derived from the
    -- dimensions AT ORDER TIME. Recomputing it later from a catalogue item
    -- whose dimensions have since been corrected would silently restate a
    -- shipped order.
    volume_m3   NUMERIC(12,4),

    handling_notes TEXT,

    -- Actuals, filled from the loading and unloading proof. NULL until then,
    -- which is how "not yet measured" stays distinct from "measured as zero".
    loaded_quantity   NUMERIC(12,2),
    unloaded_quantity NUMERIC(12,2),

    sort_order  INTEGER NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_order_item_quantity CHECK (quantity IS NULL OR quantity >= 0)
);

CREATE INDEX idx_order_items_order ON order_items (order_id, sort_order);

-- When an order must be actioned by, from the PRD. Distinct from pickup_at:
-- pickup is when the truck is due, expiry is when the planner has run out of
-- time to find one.
ALTER TABLE orders
    ADD COLUMN expires_at TIMESTAMPTZ;

CREATE INDEX idx_orders_expiring ON orders (expires_at)
    WHERE deleted_at IS NULL AND expires_at IS NOT NULL
      AND status_code NOT IN ('completed', 'cancelled', 'rejected');

COMMIT;

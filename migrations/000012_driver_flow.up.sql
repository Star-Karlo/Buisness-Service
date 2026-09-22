-- The K-Trip driver flow: order acceptance, cargo checks, POD submissions
-- with a review, and the PIC's field link.
--
-- A shipment used to move loading → loaded and unloading → unloaded on the
-- driver's say-so, with the POD checked afterwards in the console. The driver
-- flow gates those two steps on a reviewed POD instead: the driver submits
-- photos, somebody at the company (or the warehouse PIC) approves or rejects,
-- and approval is what moves the shipment on. A rejected POD is re-submitted;
-- every submission is kept, so the record shows what was refused and why.

ALTER TABLE shipments
    ADD COLUMN IF NOT EXISTS accepted_at                TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS accepted_latitude          NUMERIC(10,7),
    ADD COLUMN IF NOT EXISTS accepted_longitude         NUMERIC(10,7),
    -- The driver's own check at the loading point: does what was loaded match
    -- the order? Recorded before the loading POD is submitted.
    ADD COLUMN IF NOT EXISTS loading_cargo_matches      BOOLEAN,
    ADD COLUMN IF NOT EXISTS loading_cargo_note         TEXT,
    ADD COLUMN IF NOT EXISTS loading_cargo_checked_at   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS loading_cargo_checked_by   UUID,
    -- The receiving PIC's check at the unloading point, taken on the field
    -- page (by handover token) or in the console. The unloading POD cannot be
    -- submitted until it is recorded.
    ADD COLUMN IF NOT EXISTS unloading_cargo_matches    BOOLEAN,
    ADD COLUMN IF NOT EXISTS unloading_cargo_note       TEXT,
    ADD COLUMN IF NOT EXISTS unloading_cargo_checked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS unloading_cargo_checked_by UUID,
    ADD COLUMN IF NOT EXISTS unloading_cargo_checked_via TEXT;

CREATE TABLE IF NOT EXISTS shipment_pods (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    shipment_id           UUID NOT NULL REFERENCES shipments(id) ON DELETE CASCADE,
    stage                 TEXT NOT NULL CHECK (stage IN ('loading', 'unloading')),
    -- [{docType, fileUrl}] — surat jalan, muatan, pendukung photos, as
    -- storage keys resolved through /uploads/download-url.
    photos                JSONB NOT NULL DEFAULT '[]'::jsonb,
    note                  TEXT,
    status                TEXT NOT NULL DEFAULT 'submitted' CHECK (status IN ('submitted', 'approved', 'rejected')),
    rejection_reason      TEXT,
    submitted_by_user_id  UUID,
    submitted_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    reviewed_by_user_id   UUID,
    reviewed_at           TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_shipment_pods_shipment ON shipment_pods(shipment_id, stage, submitted_at DESC);
-- One open submission per stage: a re-upload replaces the pending one.
CREATE UNIQUE INDEX IF NOT EXISTS uq_shipment_pods_open ON shipment_pods(shipment_id, stage) WHERE status = 'submitted';

-- The field link: the PIC opens it from the handover message (or the driver's
-- screen) to record the cargo check without an account. Same shape as the
-- tracking link — the token is the whole secret.
ALTER TABLE shipment_handovers ADD COLUMN IF NOT EXISTS field_token TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS uq_shipment_handovers_field_token ON shipment_handovers(field_token) WHERE field_token IS NOT NULL;

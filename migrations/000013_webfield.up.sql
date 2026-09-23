-- Web-Field: the receiving PIC audits the load and finalises the manifest.
--
-- Two things change from the first cut of the driver flow. The unloading code
-- now goes to the DRIVER rather than to the PIC: the driver enters it in
-- K-Trip to begin unloading and reads it out to the PIC, who enters the same
-- code on Web-Field. Holding the code is then evidence that the two people
-- are at the gate together, which a code sent only to the warehouse never
-- showed. And the PIC's answer is no longer a bare sesuai / tidak sesuai: the
-- figures actually received are recorded beside the ordered ones, and
-- finalising closes the manifest.

ALTER TABLE shipment_handovers
    -- Who the code was sent to. 'pic' is how rows written before Web-Field
    -- were issued; everything new is 'driver'.
    ADD COLUMN IF NOT EXISTS code_recipient    TEXT NOT NULL DEFAULT 'pic',
    -- The PIC's own use of the code, on Web-Field. Separate from verified_at,
    -- which is the driver's use of it in K-Trip: both happen, in that order.
    ADD COLUMN IF NOT EXISTS field_verified_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS field_attempts    SMALLINT NOT NULL DEFAULT 0,
    -- Set when the PIC opened Web-Field from a signed-in account rather than
    -- from the link; the audit is then attributable to a user, not a name
    -- typed into a box.
    ADD COLUMN IF NOT EXISTS field_pic_user_id UUID;

ALTER TABLE shipments
    -- What the PIC actually counted at the gate, against the ordered figures
    -- the order carries. Nullable: a PIC who only answers sesuai leaves them
    -- empty, and an empty column is honest where a copied figure would not be.
    ADD COLUMN IF NOT EXISTS unloading_audit_weight_kg NUMERIC(12,2),
    ADD COLUMN IF NOT EXISTS unloading_audit_volume_m3 NUMERIC(12,2),
    ADD COLUMN IF NOT EXISTS unloading_audit_quantity  NUMERIC(12,2),
    -- Finalising is the PIC's closing act and is not reversible from the
    -- field page. The console can still reject a POD afterwards; what is
    -- closed is the count, not the paperwork.
    ADD COLUMN IF NOT EXISTS manifest_finalized_at   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS manifest_finalized_by   TEXT,
    ADD COLUMN IF NOT EXISTS manifest_finalized_note TEXT;

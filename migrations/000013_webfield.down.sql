ALTER TABLE shipments
    DROP COLUMN IF EXISTS manifest_finalized_note,
    DROP COLUMN IF EXISTS manifest_finalized_by,
    DROP COLUMN IF EXISTS manifest_finalized_at,
    DROP COLUMN IF EXISTS unloading_audit_quantity,
    DROP COLUMN IF EXISTS unloading_audit_volume_m3,
    DROP COLUMN IF EXISTS unloading_audit_weight_kg;

ALTER TABLE shipment_handovers
    DROP COLUMN IF EXISTS field_pic_user_id,
    DROP COLUMN IF EXISTS field_attempts,
    DROP COLUMN IF EXISTS field_verified_at,
    DROP COLUMN IF EXISTS code_recipient;

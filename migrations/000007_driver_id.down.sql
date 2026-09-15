DROP INDEX IF EXISTS idx_shipments_driver_id_active;
DROP INDEX IF EXISTS idx_orders_driver_id;
ALTER TABLE shipments DROP COLUMN IF EXISTS driver_id;
ALTER TABLE orders    DROP COLUMN IF EXISTS driver_id;

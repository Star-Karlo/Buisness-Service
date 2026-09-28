DROP INDEX IF EXISTS idx_order_items_shipment;
ALTER TABLE order_items DROP COLUMN IF EXISTS shipment_no;

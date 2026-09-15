-- Drivers are master data now: a name, a phone and a licence, usually with no
-- login. An order is assigned to THAT record, identified by its master-data id
-- (a Mongo ObjectId hex), and the login-user column is filled in only when
-- the driver happens to have one — it is what the driver app authenticates
-- as, and what the handover checks against.
ALTER TABLE orders    ADD COLUMN IF NOT EXISTS driver_id TEXT;
ALTER TABLE shipments ADD COLUMN IF NOT EXISTS driver_id TEXT;

CREATE INDEX IF NOT EXISTS idx_orders_driver_id ON orders (driver_id, status_code)
    WHERE deleted_at IS NULL AND driver_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_shipments_driver_id_active ON shipments (driver_id)
    WHERE finished_at IS NULL AND driver_id IS NOT NULL;

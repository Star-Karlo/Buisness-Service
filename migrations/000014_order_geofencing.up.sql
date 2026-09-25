-- Geofencing becomes a per-order decision.
--
-- It was a company switch: on, every arrival outside a warehouse's radius was
-- refused; off, none were. That is the wrong grain for the way these trips
-- actually differ — a fenced distribution yard and a roadside drop on the same
-- day want different answers, and a planner who has to turn the company
-- setting off for one awkward delivery has turned it off for every other
-- truck on the road.
--
-- NULL means nobody has decided, and an undecided order is not enforced —
-- which is what every existing order does today, so nothing changes until a
-- planner turns it on for a delivery that needs it.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS geofencing_enabled BOOLEAN;

COMMENT ON COLUMN orders.geofencing_enabled IS
    'Per-order geofence enforcement: TRUE demands the driver be inside the '
    'warehouse radius to report arrival, FALSE or NULL records the distance '
    'and lets them through. The company-wide switch it replaced is gone.';

-- Toll fares per route, from MAPID's route_n_toll API: prices per golongan
-- (I–V) and the gate-by-gate breakdown. Stored with the route it was
-- computed for, so an order's planned toll is fixed at planning time.
ALTER TABLE route_cache ADD COLUMN IF NOT EXISTS toll JSONB;

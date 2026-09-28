-- One home for the visit order, not two.
--
-- 000019 added orders.stop_sequence as a column, but the value is written and
-- read as orders.detail->'stopSequence' — beside loadingPoints and
-- unloadingPoints, the lists it indexes, so the choice travels with the thing
-- it describes. Two homes for one value is how the order comes to say one
-- thing and the driver gets sent somewhere else, so the one nothing reads
-- goes. Never populated, so nothing is lost.
ALTER TABLE orders DROP COLUMN IF EXISTS stop_sequence;

-- Give the orders that already exist their stops.
--
-- 000016 created the table and the service fills it when an order is placed
-- or its points are edited. Every order placed before that has its points in
-- detail.loadingPoints / detail.unloadingPoints and no rows, so the driver
-- app would still show two pins for a three-point trip and the console would
-- still show a journey with no middle. This derives them once, from the same
-- lists the service reads.
--
-- Positional PICs: detail.loadingPics / unloadingPics are arrays written
-- alongside the points, one entry per point, so they join on ordinality.
-- Orders that already have stops are skipped rather than rebuilt — their rows
-- may carry a recorded arrival, and a backfill must not erase one.
INSERT INTO order_stops (order_id, seq, kind, warehouse_id, pic_name, pic_phone)
SELECT
    o.id,
    (ROW_NUMBER() OVER (PARTITION BY o.id ORDER BY p.kind_rank, p.ord))::SMALLINT,
    p.kind,
    p.warehouse_id,
    NULLIF(p.pic ->> 'name', ''),
    NULLIF(p.pic ->> 'phone', '')
FROM orders o
CROSS JOIN LATERAL (
    SELECT 0 AS kind_rank, 'load' AS kind, wh.value AS warehouse_id, wh.ordinality AS ord,
           COALESCE(pic.value, '{}'::jsonb) AS pic
    FROM jsonb_array_elements_text(COALESCE(o.detail -> 'loadingPoints', '[]'::jsonb)) WITH ORDINALITY AS wh(value, ordinality)
    LEFT JOIN LATERAL (
        SELECT value FROM jsonb_array_elements(COALESCE(o.detail -> 'loadingPics', '[]'::jsonb))
        WITH ORDINALITY AS pics(value, ordinality)
        WHERE pics.ordinality = wh.ordinality
    ) AS pic ON TRUE
    UNION ALL
    SELECT 1, 'unload', wh.value, wh.ordinality, COALESCE(pic.value, '{}'::jsonb)
    FROM jsonb_array_elements_text(COALESCE(o.detail -> 'unloadingPoints', '[]'::jsonb)) WITH ORDINALITY AS wh(value, ordinality)
    LEFT JOIN LATERAL (
        SELECT value FROM jsonb_array_elements(COALESCE(o.detail -> 'unloadingPics', '[]'::jsonb))
        WITH ORDINALITY AS pics(value, ordinality)
        WHERE pics.ordinality = wh.ordinality
    ) AS pic ON TRUE
) AS p
WHERE o.deleted_at IS NULL
  AND p.warehouse_id <> ''
  AND NOT EXISTS (SELECT 1 FROM order_stops s WHERE s.order_id = o.id)
  -- Only journeys the lists actually describe: an order with points on one
  -- side only is not a trip, and half a visit list is worse than none.
  AND jsonb_array_length(COALESCE(o.detail -> 'loadingPoints', '[]'::jsonb)) > 0
  AND jsonb_array_length(COALESCE(o.detail -> 'unloadingPoints', '[]'::jsonb)) > 0;

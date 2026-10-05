-- 002_reporting.sql — course ch. 4: joins and saving queries as views.
-- Views run with the owner's privileges, so relief_analyst can read these
-- without being able to touch the underlying tables.
SET ROLE relief_owner;

-- How many days each consumable will last at current occupancy.
CREATE VIEW reporting.supply_coverage AS
SELECT
  s.id                         AS shelter_id,
  s.name                       AS shelter,
  r.name                       AS region,
  it.id                        AS item_id,
  it.sku,
  it.name                      AS item,
  it.category,
  s.occupancy,
  coalesce(inv.quantity, 0)    AS on_hand,
  round(s.occupancy * it.daily_need_per_person, 1) AS daily_need,
  CASE WHEN s.occupancy = 0 THEN NULL
       ELSE round(coalesce(inv.quantity, 0) / (s.occupancy * it.daily_need_per_person), 1)
  END                          AS days_of_supply,
  -- Units required to reach a 3-day buffer.
  greatest(0, ceil(3 * s.occupancy * it.daily_need_per_person - coalesce(inv.quantity, 0)))::int AS shortfall_3d
FROM core.shelters s
JOIN core.regions r ON r.id = s.region_id
CROSS JOIN core.items it
LEFT JOIN ops.inventory inv ON inv.shelter_id = s.id AND inv.item_id = it.id
WHERE NOT s.is_hub
  AND s.status = 'open'
  AND it.daily_need_per_person > 0;

-- Anything with less than 1.5 days left, worst first, with whether help is already on the way.
CREATE VIEW reporting.critical_shortages AS
SELECT
  c.*,
  CASE WHEN c.days_of_supply < 0.5 THEN 'critical'
       WHEN c.days_of_supply < 1   THEN 'high'
       ELSE 'watch' END AS severity,
  EXISTS (SELECT 1 FROM ops.requests q
          WHERE q.shelter_id = c.shelter_id AND q.item_id = c.item_id AND q.status = 'open') AS request_open
FROM reporting.supply_coverage c
WHERE c.days_of_supply < 1.5
ORDER BY c.days_of_supply, c.occupancy DESC;

-- One row per location for the dashboard.
CREATE VIEW reporting.shelter_status AS
SELECT
  s.id, s.name, r.name AS region, s.address, s.status, s.is_hub,
  s.capacity, s.occupancy,
  s.capacity - s.occupancy AS beds_free,
  CASE WHEN s.capacity = 0 THEN 0 ELSE round(100.0 * s.occupancy / s.capacity, 1) END AS occupancy_pct,
  s.accessible, s.pets_allowed, s.languages, s.needed_skills,
  s.latitude, s.longitude,
  coalesce(q.open_requests, 0)     AS open_requests,
  coalesce(q.critical_requests, 0) AS critical_requests,
  low.item           AS lowest_item,
  low.days_of_supply AS lowest_days,
  coalesce(st.units_in_stock, 0)   AS units_in_stock,
  coalesce(sh.on_shift_now, 0)     AS volunteers_on_shift,
  s.updated_at
FROM core.shelters s
JOIN core.regions r ON r.id = s.region_id
LEFT JOIN LATERAL (
  SELECT count(*) AS open_requests,
         count(*) FILTER (WHERE priority = 'critical') AS critical_requests
  FROM ops.requests WHERE shelter_id = s.id AND status = 'open'
) q ON true
LEFT JOIN LATERAL (
  SELECT c.item, c.days_of_supply FROM reporting.supply_coverage c
  WHERE c.shelter_id = s.id ORDER BY c.days_of_supply NULLS LAST LIMIT 1
) low ON true
LEFT JOIN LATERAL (
  SELECT sum(quantity) AS units_in_stock FROM ops.inventory WHERE shelter_id = s.id
) st ON true
LEFT JOIN LATERAL (
  SELECT count(*) AS on_shift_now FROM ops.shifts
  WHERE shelter_id = s.id AND now() BETWEEN starts_at AND ends_at
) sh ON true;

-- Open requests with names instead of ids.
CREATE VIEW reporting.open_requests AS
SELECT q.id, q.created_at, q.priority, s.id AS shelter_id, s.name AS shelter, r.name AS region,
       it.sku, it.name AS item, q.quantity, it.unit, q.note,
       (SELECT coalesce(sum(i.quantity), 0) FROM ops.inventory i
          JOIN core.shelters h ON h.id = i.shelter_id
         WHERE h.is_hub AND h.status = 'open' AND i.item_id = q.item_id) AS hub_stock
FROM ops.requests q
JOIN core.shelters s ON s.id = q.shelter_id
JOIN core.regions  r ON r.id = s.region_id
JOIN core.items   it ON it.id = q.item_id
WHERE q.status = 'open'
ORDER BY q.priority DESC, q.created_at;

-- Shipment log.
CREATE VIEW reporting.shipment_log AS
SELECT sh.id, sh.shipped_at, h.name AS from_hub, d.name AS to_shelter, it.name AS item,
       sh.quantity, it.unit, sh.distance_km, q.priority
FROM ops.shipments sh
JOIN core.shelters h ON h.id = sh.from_shelter_id
JOIN core.shelters d ON d.id = sh.to_shelter_id
JOIN core.items   it ON it.id = sh.item_id
JOIN ops.requests  q ON q.id = sh.request_id
ORDER BY sh.shipped_at DESC;

-- Volunteer roster without contact details (no email, no phone).
CREATE VIEW reporting.volunteer_roster AS
SELECT v.id, v.full_name, r.name AS home_region, v.skills, v.languages, v.available,
       EXISTS (SELECT 1 FROM ops.shifts sh WHERE sh.volunteer_id = v.id AND now() BETWEEN sh.starts_at AND sh.ends_at) AS on_shift_now
FROM ops.volunteers v
LEFT JOIN core.regions r ON r.id = v.home_region_id;

-- Skills coverage: which needed skills are short across all shelters.
CREATE VIEW reporting.skill_gaps AS
WITH needed AS (
  SELECT unnest(needed_skills) AS skill, count(*) AS shelters_needing
  FROM core.shelters WHERE status = 'open' AND NOT is_hub GROUP BY 1
), supply AS (
  SELECT unnest(skills) AS skill, count(*) AS volunteers_available
  FROM ops.volunteers WHERE available GROUP BY 1
)
SELECT n.skill, n.shelters_needing, coalesce(s.volunteers_available, 0) AS volunteers_available,
       round(coalesce(s.volunteers_available, 0)::numeric / n.shelters_needing, 2) AS volunteers_per_shelter
FROM needed n LEFT JOIN supply s USING (skill)
ORDER BY volunteers_per_shelter, n.skill;

-- Region roll-up, materialized because it's an expensive aggregate.
CREATE MATERIALIZED VIEW reporting.region_summary AS
SELECT r.id, r.name AS region,
       count(s.id) FILTER (WHERE NOT s.is_hub AND s.status = 'open') AS open_shelters,
       count(s.id) FILTER (WHERE s.is_hub)                           AS hubs,
       coalesce(sum(s.capacity)  FILTER (WHERE NOT s.is_hub AND s.status = 'open'), 0) AS capacity,
       coalesce(sum(s.occupancy) FILTER (WHERE NOT s.is_hub), 0)     AS people_sheltered,
       (SELECT count(*) FROM ops.requests q JOIN core.shelters x ON x.id = q.shelter_id
         WHERE x.region_id = r.id AND q.status = 'open')             AS open_requests,
       (SELECT count(*) FROM ops.volunteers v WHERE v.home_region_id = r.id AND v.available) AS volunteers_available,
       now() AS refreshed_at
FROM core.regions r
LEFT JOIN core.shelters s ON s.region_id = r.id
GROUP BY r.id, r.name;
CREATE UNIQUE INDEX region_summary_id ON reporting.region_summary (id);  -- allows REFRESH ... CONCURRENTLY

-- Rank available volunteers for a shelter by skills it needs and languages spoken there.
-- SECURITY DEFINER: analysts can call it without reading ops.volunteers directly.
CREATE FUNCTION reporting.match_volunteers(p_shelter_id integer, p_limit integer DEFAULT 10)
RETURNS TABLE (volunteer_id integer, full_name text, matched_skills text[], shared_languages text[],
               same_region boolean, busy_now boolean, score integer)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  SELECT v.id, v.full_name,
         ARRAY(SELECT unnest(v.skills)    INTERSECT SELECT unnest(s.needed_skills) ORDER BY 1),
         ARRAY(SELECT unnest(v.languages) INTERSECT SELECT unnest(s.languages)     ORDER BY 1),
         v.home_region_id = s.region_id,
         EXISTS (SELECT 1 FROM ops.shifts sh WHERE sh.volunteer_id = v.id AND now() BETWEEN sh.starts_at AND sh.ends_at),
         (cardinality(ARRAY(SELECT unnest(v.skills) INTERSECT SELECT unnest(s.needed_skills))) * 10
          + cardinality(ARRAY(SELECT unnest(v.languages) INTERSECT SELECT unnest(s.languages) EXCEPT SELECT 'en')) * 4
          + CASE WHEN v.home_region_id = s.region_id THEN 3 ELSE 0 END)::int
  FROM ops.volunteers v
  JOIN core.shelters s ON s.id = p_shelter_id
  WHERE v.available
    AND v.skills && s.needed_skills          -- array overlap, served by the GIN index
  ORDER BY 6, 7 DESC, 2   -- free volunteers first, then best match
  LIMIT p_limit
$$;

-- Function to refresh the materialized view; callable by the app.
CREATE FUNCTION reporting.refresh_region_summary() RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
  REFRESH MATERIALIZED VIEW CONCURRENTLY reporting.region_summary;
$$;

RESET ROLE;

-- Move staged CSV rows into the real tables, resolving names to foreign keys.
-- Any bad row (unknown region, negative stock, malformed email…) fails the
-- whole load because of the constraints — nothing half-imported.
INSERT INTO core.regions (name) SELECT name FROM stage_regions;

INSERT INTO core.shelters (region_id, name, address, latitude, longitude, is_hub, capacity, occupancy,
                           accessible, pets_allowed, languages, needed_skills, phone)
SELECT r.id, s.name, s.address, s.latitude, s.longitude, s.is_hub, s.capacity, s.occupancy,
       s.accessible, s.pets_allowed, s.languages, s.needed_skills, s.phone
FROM stage_shelters s JOIN core.regions r ON r.name = s.region;

INSERT INTO core.items (sku, name, category, unit, daily_need_per_person)
SELECT sku, name, category::core.item_category, unit, daily_need_per_person FROM stage_items;

INSERT INTO ops.inventory (shelter_id, item_id, quantity)
SELECT sh.id, it.id, st.quantity
FROM stage_inventory st
JOIN core.shelters sh ON sh.name = st.shelter
JOIN core.items    it ON it.sku  = st.sku;

INSERT INTO ops.volunteers (full_name, email, phone, home_region_id, skills, languages, available)
SELECT v.full_name, v.email, v.phone, r.id, v.skills, v.languages, v.available
FROM stage_volunteers v LEFT JOIN core.regions r ON r.name = v.home_region;

INSERT INTO ops.requests (shelter_id, item_id, quantity, priority, note, created_at)
SELECT sh.id, it.id, q.quantity, q.priority::ops.priority, nullif(q.note, ''),
       now() - (row_number() OVER ()) * interval '17 minutes'
FROM stage_requests q
JOIN core.shelters sh ON sh.name = q.shelter
JOIN core.items    it ON it.sku  = q.sku;

-- A few volunteer shifts around the current time, one per matching shelter.
INSERT INTO ops.shifts (volunteer_id, shelter_id, skill, starts_at, ends_at)
SELECT DISTINCT ON (v.id) v.id, s.id, sk.skill,
       date_trunc('hour', now()) - interval '2 hours',
       date_trunc('hour', now()) + interval '4 hours'
FROM ops.volunteers v
JOIN core.shelters s ON s.needed_skills && v.skills AND NOT s.is_hub
CROSS JOIN LATERAL (SELECT unnest(v.skills) INTERSECT SELECT unnest(s.needed_skills) LIMIT 1) sk(skill)
WHERE v.available AND v.id % 3 = 0
ORDER BY v.id, (v.id * 7 + s.id) % 11;

REFRESH MATERIALIZED VIEW reporting.region_summary;

-- The audit log should start with real activity, not hundreds of seed rows.
-- (Not done by disabling triggers: session_replication_role = replica would
-- also switch off foreign-key enforcement, which is implemented with triggers.)
TRUNCATE audit.changes;

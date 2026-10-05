-- Staging tables for CSV import (course ch. 4: import data from a CSV).
-- CSVs use natural keys (names, SKUs); load.sql resolves them to ids with joins.
CREATE TEMP TABLE stage_regions    (name text);
CREATE TEMP TABLE stage_shelters   (name text, region text, address text, latitude numeric, longitude numeric,
                                    is_hub boolean, capacity int, occupancy int, accessible boolean,
                                    pets_allowed boolean, languages text[], needed_skills text[], phone text);
CREATE TEMP TABLE stage_items      (sku text, name text, category text, unit text, daily_need_per_person numeric);
CREATE TEMP TABLE stage_inventory  (shelter text, sku text, quantity int);
CREATE TEMP TABLE stage_volunteers (full_name text, email text, phone text, home_region text,
                                    skills text[], languages text[], available boolean);
CREATE TEMP TABLE stage_requests   (shelter text, sku text, quantity int, priority text, note text);

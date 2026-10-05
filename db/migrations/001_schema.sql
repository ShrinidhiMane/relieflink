-- 001_schema.sql — tables, types, keys, constraints, defaults, indexes, triggers.
-- Everything is created by (and owned by) relief_owner.
SET ROLE relief_owner;

-- Course ch. 3: organize tables with schemas.
CREATE SCHEMA core;       -- reference data: regions, shelters, catalog of items
CREATE SCHEMA ops;        -- day-to-day operations: stock, requests, volunteers, shifts
CREATE SCHEMA audit;      -- append-only change history
CREATE SCHEMA reporting;  -- read-only views for analysts and dashboards

COMMENT ON SCHEMA core      IS 'Reference data that changes rarely';
COMMENT ON SCHEMA ops       IS 'Operational data written by the application';
COMMENT ON SCHEMA audit     IS 'Who changed what, when';
COMMENT ON SCHEMA reporting IS 'Views exposed to the relief_analyst role';

-- Course ch. 2: native data types, including enumerated types.
CREATE TYPE core.item_category  AS ENUM ('water', 'food', 'medical', 'hygiene', 'bedding', 'baby', 'power');
CREATE TYPE core.shelter_status AS ENUM ('open', 'closed');
CREATE TYPE ops.priority        AS ENUM ('low', 'normal', 'high', 'critical');
CREATE TYPE ops.request_status  AS ENUM ('open', 'fulfilled', 'cancelled');

-- A reusable rule for phone numbers.
CREATE DOMAIN core.phone AS text
  CHECK (VALUE ~ '^\+?[0-9 ()-]{7,20}$');

-------------------------------------------------------------------------------
-- core
-------------------------------------------------------------------------------
CREATE TABLE core.regions (
  id         smallint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name       text NOT NULL UNIQUE,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE core.shelters (
  id             integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  region_id      smallint NOT NULL REFERENCES core.regions (id) ON DELETE RESTRICT,
  name           text NOT NULL UNIQUE,
  address        text NOT NULL,
  latitude       numeric(8,5) NOT NULL CHECK (latitude  BETWEEN -90  AND 90),
  longitude      numeric(8,5) NOT NULL CHECK (longitude BETWEEN -180 AND 180),
  is_hub         boolean NOT NULL DEFAULT false,      -- distribution hub (warehouse), not a shelter for people
  capacity       integer NOT NULL CHECK (capacity >= 0),
  occupancy      integer NOT NULL DEFAULT 0 CHECK (occupancy >= 0),
  status         core.shelter_status NOT NULL DEFAULT 'open',
  accessible     boolean NOT NULL DEFAULT false,
  pets_allowed   boolean NOT NULL DEFAULT false,
  languages      text[] NOT NULL DEFAULT '{en}',      -- course ch. 2: array data types
  needed_skills  text[] NOT NULL DEFAULT '{}',
  phone          core.phone,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  -- Course ch. 5: constrain acceptable input values.
  CONSTRAINT occupancy_within_capacity CHECK (occupancy <= capacity),
  CONSTRAINT hubs_do_not_house_people  CHECK (NOT is_hub OR occupancy = 0),
  CONSTRAINT closed_shelters_are_empty CHECK (status = 'open' OR occupancy = 0)
);
COMMENT ON COLUMN core.shelters.is_hub IS 'Supply warehouse: stock is shipped from hubs to shelters';

CREATE TABLE core.items (
  id                   integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  sku                  text NOT NULL UNIQUE CHECK (sku ~ '^[A-Z]{3}-[0-9]{3}$'),
  name                 text NOT NULL,
  category             core.item_category NOT NULL,
  unit                 text NOT NULL DEFAULT 'each',
  daily_need_per_person numeric(6,3) NOT NULL DEFAULT 0 CHECK (daily_need_per_person >= 0)
);
COMMENT ON COLUMN core.items.daily_need_per_person IS 'Units one person consumes per day; 0 = not a daily consumable';

-------------------------------------------------------------------------------
-- ops
-------------------------------------------------------------------------------
-- Stock on hand: one row per (location, item). Composite primary key.
CREATE TABLE ops.inventory (
  shelter_id integer NOT NULL REFERENCES core.shelters (id) ON DELETE CASCADE,
  item_id    integer NOT NULL REFERENCES core.items (id)    ON DELETE RESTRICT,
  quantity   integer NOT NULL DEFAULT 0 CHECK (quantity >= 0),  -- stock can never go negative
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (shelter_id, item_id)
);

CREATE TABLE ops.volunteers (
  id             integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  full_name      text NOT NULL CHECK (length(trim(full_name)) > 1),
  email          citext NOT NULL UNIQUE CHECK (email ~ '^[^@\s]+@[^@\s]+\.[^@\s]+$'),
  phone          core.phone,
  home_region_id smallint REFERENCES core.regions (id) ON DELETE SET NULL,
  skills         text[] NOT NULL DEFAULT '{}',
  languages      text[] NOT NULL DEFAULT '{en}',
  available      boolean NOT NULL DEFAULT true,
  created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ops.requests (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  shelter_id   integer NOT NULL REFERENCES core.shelters (id) ON DELETE CASCADE,
  item_id      integer NOT NULL REFERENCES core.items (id)    ON DELETE RESTRICT,
  quantity     integer NOT NULL CHECK (quantity > 0),
  priority     ops.priority NOT NULL DEFAULT 'normal',
  status       ops.request_status NOT NULL DEFAULT 'open',
  note         text,
  created_at   timestamptz NOT NULL DEFAULT now(),
  closed_at    timestamptz,
  CONSTRAINT closed_requests_have_timestamp CHECK ((status = 'open') = (closed_at IS NULL))
);

CREATE TABLE ops.shipments (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  request_id      bigint NOT NULL UNIQUE REFERENCES ops.requests (id) ON DELETE RESTRICT,
  from_shelter_id integer NOT NULL REFERENCES core.shelters (id),
  to_shelter_id   integer NOT NULL REFERENCES core.shelters (id),
  item_id         integer NOT NULL REFERENCES core.items (id),
  quantity        integer NOT NULL CHECK (quantity > 0),
  distance_km     numeric(7,1) NOT NULL,
  shipped_at      timestamptz NOT NULL DEFAULT now(),
  CHECK (from_shelter_id <> to_shelter_id)
);

CREATE TABLE ops.shifts (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  volunteer_id integer NOT NULL REFERENCES ops.volunteers (id) ON DELETE CASCADE,
  shelter_id   integer NOT NULL REFERENCES core.shelters (id)  ON DELETE CASCADE,
  skill        text NOT NULL,
  starts_at    timestamptz NOT NULL,
  ends_at      timestamptz NOT NULL,
  CHECK (ends_at > starts_at),
  CHECK (ends_at - starts_at <= interval '12 hours'),
  -- A volunteer can't be in two places at once: no overlapping shifts.
  CONSTRAINT no_double_booking EXCLUDE USING gist (
    volunteer_id WITH =,
    tstzrange(starts_at, ends_at) WITH &&
  )
);

-------------------------------------------------------------------------------
-- Course ch. 5: indexes
-------------------------------------------------------------------------------
-- Foreign keys aren't indexed automatically; index the ones we join or filter on.
CREATE INDEX shelters_region_idx   ON core.shelters (region_id);
CREATE INDEX inventory_item_idx    ON ops.inventory (item_id);
CREATE INDEX shipments_to_idx      ON ops.shipments (to_shelter_id);
CREATE INDEX shifts_shelter_idx    ON ops.shifts (shelter_id, starts_at);
-- Partial index: the dashboard only ever looks at open requests.
CREATE INDEX requests_open_idx     ON ops.requests (shelter_id, priority) WHERE status = 'open';
-- GIN indexes make array containment/overlap (@>, &&) fast.
CREATE INDEX volunteers_skills_gin ON ops.volunteers USING gin (skills);
CREATE INDEX volunteers_langs_gin  ON ops.volunteers USING gin (languages);
-- Expression index for the nearest-hub search.
CREATE INDEX shelters_hub_geo_idx  ON core.shelters USING gist (ll_to_earth(latitude::float8, longitude::float8)) WHERE is_hub;

-------------------------------------------------------------------------------
-- audit + triggers
-------------------------------------------------------------------------------
CREATE TABLE audit.changes (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  changed_at timestamptz NOT NULL DEFAULT now(),
  actor      text NOT NULL DEFAULT session_user,
  table_name text NOT NULL,
  operation  text NOT NULL CHECK (operation IN ('INSERT', 'UPDATE', 'DELETE')),
  old_row    jsonb,
  new_row    jsonb
);
CREATE INDEX changes_table_time_idx ON audit.changes (table_name, changed_at DESC);

-- Keep updated_at honest without trusting the application.
CREATE FUNCTION core.touch_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END $$;

-- Record every change and broadcast it on the relief_events channel.
-- SECURITY DEFINER so writers don't need privileges on the audit schema.
CREATE FUNCTION audit.record_change() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  old_j jsonb := CASE WHEN TG_OP <> 'INSERT' THEN to_jsonb(OLD) END;
  new_j jsonb := CASE WHEN TG_OP <> 'DELETE' THEN to_jsonb(NEW) END;
BEGIN
  INSERT INTO audit.changes (table_name, operation, old_row, new_row)
  VALUES (TG_TABLE_SCHEMA || '.' || TG_TABLE_NAME, TG_OP, old_j, new_j);

  PERFORM pg_notify('relief_events', json_build_object(
    'table', TG_TABLE_SCHEMA || '.' || TG_TABLE_NAME,
    'op',    TG_OP,
    'row',   coalesce(new_j, old_j)
  )::text);
  RETURN NULL;
END $$;

CREATE TRIGGER shelters_touch  BEFORE UPDATE ON core.shelters  FOR EACH ROW EXECUTE FUNCTION core.touch_updated_at();
CREATE TRIGGER inventory_touch BEFORE UPDATE ON ops.inventory  FOR EACH ROW EXECUTE FUNCTION core.touch_updated_at();

CREATE TRIGGER shelters_audit  AFTER INSERT OR UPDATE OR DELETE ON core.shelters  FOR EACH ROW EXECUTE FUNCTION audit.record_change();
CREATE TRIGGER inventory_audit AFTER INSERT OR UPDATE OR DELETE ON ops.inventory  FOR EACH ROW EXECUTE FUNCTION audit.record_change();
CREATE TRIGGER requests_audit  AFTER INSERT OR UPDATE OR DELETE ON ops.requests   FOR EACH ROW EXECUTE FUNCTION audit.record_change();
CREATE TRIGGER shipments_audit AFTER INSERT ON ops.shipments                       FOR EACH ROW EXECUTE FUNCTION audit.record_change();
CREATE TRIGGER shifts_audit    AFTER INSERT OR DELETE ON ops.shifts                FOR EACH ROW EXECUTE FUNCTION audit.record_change();

-------------------------------------------------------------------------------
-- Business operation: fulfil a request from the nearest hub with enough stock.
-- Runs as one transaction: lock request → pick hub → move stock → record shipment.
-------------------------------------------------------------------------------
CREATE FUNCTION ops.fulfill_request(p_request_id bigint, p_hub_id integer DEFAULT NULL)
RETURNS ops.shipments
LANGUAGE plpgsql AS $$
DECLARE
  req  ops.requests;
  dest core.shelters;
  hub  core.shelters;
  ship ops.shipments;
  dispatched boolean := false;
BEGIN
  SELECT * INTO req FROM ops.requests WHERE id = p_request_id FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'request % does not exist', p_request_id USING ERRCODE = 'RL404';
  END IF;
  IF req.status <> 'open' THEN
    RAISE EXCEPTION 'request % is already %', p_request_id, req.status USING ERRCODE = 'RL409';
  END IF;

  SELECT * INTO dest FROM core.shelters WHERE id = req.shelter_id;

  -- Try hubs nearest-first. The conditional UPDATE takes a row lock and
  -- re-checks the stock, so if another dispatch drained a hub a moment ago we
  -- simply move on to the next-nearest one instead of failing or overselling.
  FOR hub IN
    SELECT s.*
    FROM core.shelters s
    JOIN ops.inventory i ON i.shelter_id = s.id AND i.item_id = req.item_id
    WHERE s.is_hub
      AND s.status = 'open'
      AND s.id <> req.shelter_id
      AND i.quantity >= req.quantity
      AND (p_hub_id IS NULL OR s.id = p_hub_id)
    ORDER BY earth_distance(ll_to_earth(s.latitude::float8, s.longitude::float8),
                            ll_to_earth(dest.latitude::float8, dest.longitude::float8))
  LOOP
    UPDATE ops.inventory SET quantity = quantity - req.quantity
    WHERE shelter_id = hub.id AND item_id = req.item_id AND quantity >= req.quantity;
    IF FOUND THEN
      dispatched := true;
      EXIT;
    END IF;
  END LOOP;

  IF NOT dispatched THEN
    RAISE EXCEPTION 'no open hub has % units of the requested item', req.quantity USING ERRCODE = 'RL422';
  END IF;

  INSERT INTO ops.inventory AS inv (shelter_id, item_id, quantity)
  VALUES (req.shelter_id, req.item_id, req.quantity)
  ON CONFLICT (shelter_id, item_id) DO UPDATE SET quantity = inv.quantity + EXCLUDED.quantity;

  INSERT INTO ops.shipments (request_id, from_shelter_id, to_shelter_id, item_id, quantity, distance_km)
  VALUES (req.id, hub.id, dest.id, req.item_id, req.quantity,
          round((earth_distance(ll_to_earth(hub.latitude::float8,  hub.longitude::float8),
                                ll_to_earth(dest.latitude::float8, dest.longitude::float8)) / 1000)::numeric, 1))
  RETURNING * INTO ship;

  UPDATE ops.requests SET status = 'fulfilled', closed_at = now() WHERE id = req.id;
  RETURN ship;
END $$;

RESET ROLE;

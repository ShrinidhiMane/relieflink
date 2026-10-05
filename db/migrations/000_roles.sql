-- 000_roles.sql — run as a superuser.
-- Course ch. 6: user accounts and roles.
--
-- relief_owner    NOLOGIN  owns every object (nobody logs in as the owner)
-- relief_app      LOGIN    the web application: reads and writes operational data
-- relief_analyst  LOGIN    reporting only: can read the reporting schema, nothing else
--                          (no access to volunteers' emails or phone numbers)
--
-- Passwords come from session settings so they never live in this file:
--   SET relieflink.app_password = '...';  SET relieflink.analyst_password = '...';

DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'relief_owner') THEN
    CREATE ROLE relief_owner NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'relief_app') THEN
    CREATE ROLE relief_app LOGIN;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'relief_analyst') THEN
    CREATE ROLE relief_analyst LOGIN;
  END IF;

  EXECUTE format('ALTER ROLE relief_app PASSWORD %L',
    coalesce(nullif(current_setting('relieflink.app_password', true), ''), 'relief_app_dev'));
  EXECUTE format('ALTER ROLE relief_analyst PASSWORD %L',
    coalesce(nullif(current_setting('relieflink.analyst_password', true), ''), 'relief_analyst_dev'));
END $$;

-- Guard rails on the login roles.
ALTER ROLE relief_app     CONNECTION LIMIT 50;
ALTER ROLE relief_analyst CONNECTION LIMIT 10;
ALTER ROLE relief_analyst SET statement_timeout = '5s';
ALTER ROLE relief_analyst SET default_transaction_read_only = on;

-- The owner needs to create schemas in this database.
DO $$ BEGIN EXECUTE format('GRANT CREATE, CONNECT ON DATABASE %I TO relief_owner', current_database()); END $$;
DO $$ BEGIN EXECUTE format('GRANT CONNECT ON DATABASE %I TO relief_app, relief_analyst', current_database()); END $$;

-- Extensions are installed by the superuser before ownership switches.
CREATE EXTENSION IF NOT EXISTS citext;        -- case-insensitive email addresses
CREATE EXTENSION IF NOT EXISTS btree_gist;    -- exclusion constraint on volunteer shifts
CREATE EXTENSION IF NOT EXISTS cube;
CREATE EXTENSION IF NOT EXISTS earthdistance; -- great-circle distance to the nearest supply hub

-- Nobody but the owner should create objects in public.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

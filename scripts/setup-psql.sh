#!/usr/bin/env bash
# Set up ReliefLink using only psql (no Go needed) — the same SQL the binary embeds.
#
#   PGHOST=localhost PGUSER=postgres scripts/setup-psql.sh
set -euo pipefail
cd "$(dirname "$0")/.."
DB="${RELIEF_DB:-relieflink}"

psql -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '$DB'" | grep -q 1 || createdb "$DB"

for f in db/migrations/*.sql; do
  echo "applying $f"
  psql -d "$DB" -q -v ON_ERROR_STOP=1 --single-transaction \
       -c "SET relieflink.app_password = '${RELIEF_APP_PASSWORD:-relief_app_dev}'" \
       -c "SET relieflink.analyst_password = '${RELIEF_ANALYST_PASSWORD:-relief_analyst_dev}'" \
       -f "$f"
done

echo "loading CSV seed data"
cd db/seed
psql -d "$DB" -q -v ON_ERROR_STOP=1 <<'SQL'
BEGIN;
\i stage.sql
\copy stage_regions    FROM 'regions.csv'    WITH (FORMAT csv, HEADER true)
\copy stage_shelters   FROM 'shelters.csv'   WITH (FORMAT csv, HEADER true)
\copy stage_items      FROM 'items.csv'      WITH (FORMAT csv, HEADER true)
\copy stage_inventory  FROM 'inventory.csv'  WITH (FORMAT csv, HEADER true)
\copy stage_volunteers FROM 'volunteers.csv' WITH (FORMAT csv, HEADER true)
\copy stage_requests   FROM 'requests.csv'   WITH (FORMAT csv, HEADER true)
\i load.sql
COMMIT;
SQL
echo "done: psql -d $DB"

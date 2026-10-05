#!/usr/bin/env bash
# Restore a ReliefLink backup into a database.
#
#   scripts/restore.sh backups/relieflink-20261004-120000.dump              # into relieflink_restore
#   scripts/restore.sh backups/relieflink-20261004-120000.dump relieflink   # replace the live database
#
# Restoring into a new database first is the safe default: check it, then swap.
# Roles must already exist (run `relieflink setup` or the roles-*.sql file first).
set -euo pipefail

file="${1:?usage: restore.sh <file.dump> [target-db]}"
target="${2:-${RELIEF_DB:-relieflink}_restore}"

[ -f "$file" ] || { echo "no such file: $file" >&2; exit 1; }
pg_restore --list "$file" > /dev/null   # fail fast on a corrupt archive

if psql -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '$target'" | grep -q 1; then
  read -r -p "Database '$target' exists and will be dropped. Continue? [y/N] " ok
  [ "$ok" = "y" ] || exit 1
  psql -d postgres -c "DROP DATABASE \"$target\" WITH (FORCE)"
fi

createdb "$target"
echo "Restoring $file into $target"
pg_restore --dbname="$target" --exit-on-error --jobs=4 "$file"

# Re-grant database-level privileges (they belong to the database, not the dump).
psql -d "$target" -q -c "GRANT CREATE, CONNECT ON DATABASE \"$target\" TO relief_owner;
                         GRANT CONNECT ON DATABASE \"$target\" TO relief_app, relief_analyst;"

echo "Restored. Sanity check:"
psql -d "$target" -c "SELECT (SELECT count(*) FROM core.shelters) AS shelters,
                             (SELECT count(*) FROM ops.requests)   AS requests,
                             (SELECT count(*) FROM audit.changes)  AS audit_rows;"

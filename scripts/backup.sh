#!/usr/bin/env bash
# Back up the ReliefLink database (course ch. 6: backup and restore).
#
#   scripts/backup.sh                 # → backups/relieflink-YYYYmmdd-HHMMSS.dump
#   KEEP=14 scripts/backup.sh         # keep only the newest 14 dumps
#
# Uses pg_dump's custom format: compressed, and pg_restore can restore single
# tables or run in parallel. Connection comes from the usual PG* variables.
set -euo pipefail

DB="${RELIEF_DB:-relieflink}"
DIR="${BACKUP_DIR:-backups}"
KEEP="${KEEP:-30}"
mkdir -p "$DIR"

stamp="$(date +%Y%m%d-%H%M%S)"
out="$DIR/$DB-$stamp.dump"

echo "Backing up $DB to $out"
pg_dump --format=custom --compress=9 --file="$out.partial" "$DB"
mv "$out.partial" "$out"   # never leave a half-written file with the real name

# Verify the archive is readable before trusting it.
pg_restore --list "$out" > /dev/null
echo "OK: $(du -h "$out" | cut -f1), $(pg_restore --list "$out" | grep -c 'TABLE DATA') tables"

# Also dump global objects (roles), which pg_dump doesn't include.
pg_dumpall --roles-only --no-role-passwords 2>/dev/null | grep -E 'relief_' > "$DIR/roles-$stamp.sql" || true

# Rotate: keep the newest $KEEP dumps.
ls -1t "$DIR"/"$DB"-*.dump 2>/dev/null | tail -n +"$((KEEP + 1))" | xargs -r rm --

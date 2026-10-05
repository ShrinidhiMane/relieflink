# ReliefLink

**Disaster-relief coordination on PostgreSQL.** When a wildfire or heatwave
forces thousands of people into emergency shelters, coordinators need answers
fast: which shelter runs out of water first, which hub can send supplies and
how far away it is, which volunteers speak the right languages, and who changed
what. ReliefLink puts those answers in a live dashboard, and does almost all of
the work inside PostgreSQL itself.

Built with Go and PostgreSQL 16. The data is a fictional evacuation across four
regions: 12 shelters, 3 supply hubs, 60 volunteers, 12 kinds of supplies.

## Why it matters

- **Days of supply, not raw counts.** "1,200 bottles" means nothing; "0.4 days of
  water for 312 people" means send a truck now. A view computes this from
  occupancy and per-person daily need.
- **The database refuses bad data.** A shelter can't exceed capacity, stock
  can't go negative, a volunteer can't be booked in two places at once, and
  two coordinators dispatching at the same moment can't oversell a hub. These
  are constraints and row locks, not app code that someone can bypass.
- **Privacy by role.** Analysts get a SQL console, but their PostgreSQL role can
  only see reporting views. Volunteers' emails and phone numbers are
  unreachable, enforced by the server rather than the UI.
- **Nothing is silent.** Every change lands in an audit table with who did it,
  and is pushed to every open dashboard instantly, even if it was made from
  psql or pgAdmin.

## Run it

**Docker (everything included, with pgAdmin):**

```bash
docker compose up --build
# dashboard http://localhost:8080 · pgAdmin http://localhost:5050
```

**Your own PostgreSQL (13+):**

```bash
export PGHOST=localhost PGUSER=postgres PGPASSWORD=...   # a superuser, for setup only
go run ./cmd/relieflink setup     # creates the database, roles, schema, demo data
go run ./cmd/relieflink serve     # http://localhost:8080
```

`setup` is safe to re-run; `setup -reset` drops everything and starts over.
Prefer plain psql? `scripts/setup-psql.sh` applies the same SQL files.

Set `RELIEF_APP_PASSWORD` and `RELIEF_ANALYST_PASSWORD` for real deployments;
the defaults are for local development only.

## Course topics → where they are

Built on *PostgreSQL Essential Training*. Every chapter shows up somewhere:

| Course chapter | In ReliefLink |
|---|---|
| **1. Getting started** — install, Docker, psql, pgAdmin | `docker-compose.yml` runs PostgreSQL 16 + pgAdmin with both connections pre-registered; `scripts/setup-psql.sh` is a psql-only install |
| **2. Relational basics** — data types, relationships, arrays | Enums (`item_category`, `priority`), a `phone` domain, `numeric` coordinates, `citext` emails, `timestamptz`, `jsonb`; `text[]` for skills and languages, matched with `&&` and `@>` |
| **3. Building a database** — schemas, PK/FK, referential integrity | Four schemas (`core`, `ops`, `audit`, `reporting`); composite primary key on inventory; foreign keys with deliberate `RESTRICT` / `CASCADE` / `SET NULL` choices |
| **4. Queries** — CSV import, SELECT, joins, views | Seed data is imported from CSV with `COPY` into staging tables, then joined to resolve names into keys. Seven reporting views, one materialized view |
| **5. Managing data** — indexes, defaults, constraints | B-tree, partial (`WHERE status = 'open'`), GIN (arrays) and GiST (geo + exclusion) indexes; named `CHECK` constraints; an `EXCLUDE` constraint against double-booked shifts |
| **6. Administration** — roles, privileges, backup/restore | Owner / app / analyst roles with least privilege, including column-level `GRANT UPDATE (occupancy, status)`; `scripts/backup.sh` and `scripts/restore.sh` with pg_dump custom format |

Beyond the course: PL/pgSQL functions, triggers, `SECURITY DEFINER`,
`LISTEN/NOTIFY`, `earthdistance` for nearest-hub routing, and concurrency-safe
stock movement with row locks.

## The database

```
core.regions ─┬─< core.shelters ─┬─< ops.inventory >── core.items
              │                  ├─< ops.requests  >── core.items
              │                  │        └── ops.shipments (from hub → to shelter)
              │                  └─< ops.shifts    >── ops.volunteers
              └─< ops.volunteers (home region)

audit.changes   ← every insert/update/delete, via trigger
reporting.*     ← views over core + ops (the only thing analysts can see)
```

### Rules enforced by PostgreSQL

| Rule | Mechanism |
|---|---|
| Occupancy never exceeds capacity, never negative | `CHECK (occupancy <= capacity)`, `CHECK (occupancy >= 0)` |
| Supply hubs don't house people; closed shelters are empty | `CHECK` constraints |
| Stock never goes negative | `CHECK (quantity >= 0)` |
| A volunteer can't have overlapping shifts | `EXCLUDE USING gist (volunteer_id WITH =, tstzrange(starts_at, ends_at) WITH &&)` |
| A closed request always has a close time | `CHECK ((status = 'open') = (closed_at IS NULL))` |
| SKUs, emails, phones are well-formed | regex `CHECK`s and a `DOMAIN` |
| The app can change occupancy but not rename a shelter | column-level `GRANT UPDATE (occupancy, status)` |

The API returns the constraint name and SQLSTATE with every rejection, and the
dashboard shows them, so you can see exactly which rule said no.

### Dispatching supplies

`ops.fulfill_request(request_id)` runs as one transaction:

1. Lock the request row (`FOR UPDATE`) and check it's still open.
2. Walk hubs nearest-first (`earth_distance` on lat/long) that have enough stock.
3. Decrement the hub with a conditional `UPDATE … WHERE quantity >= n`. If a
   concurrent dispatch just drained that hub, move on to the next one.
4. Add stock to the shelter (`INSERT … ON CONFLICT DO UPDATE`), record the
   shipment with its distance, close the request.

A test fires 10 simultaneous dispatches at hubs that can only cover 3 and checks
that exactly 3 succeed and no stock goes negative.

### Roles

| Role | Login | Can |
|---|---|---|
| `relief_owner` | no | owns every object; nobody uses it day to day |
| `relief_app` | yes | read `core`; write `ops`; update only `occupancy`/`status` on shelters; read the audit log; call `fulfill_request` |
| `relief_analyst` | yes | read `reporting` only, read-only by default, 5 s statement timeout |

Views and `SECURITY DEFINER` functions run as the owner, so analysts can use
`reporting.volunteer_roster` and `reporting.match_volunteers()` without ever
reading `ops.volunteers` (and its contact details) directly.

## The dashboard

- **KPIs**: people sheltered, beds free, critical shortages, open requests, volunteers on shift.
- **Shelter board** sorted by the most urgent supply, with check-in/out buttons.
  Push a shelter past capacity and PostgreSQL's `CHECK` rejects it.
- **Needs attention**: everything under 1.5 days of supply, with a one-click
  "request 3-day supply" that pre-fills the shortfall.
- **Open requests** with **Dispatch**, which reports the hub and distance.
- **Volunteer matching** per shelter, ranked by needed skills, shared non-English
  languages, and home region; book a 4-hour shift in one click.
- **Analyst console** connected as `relief_analyst`, with example queries
  including two that PostgreSQL blocks.
- **Live activity** from the audit log, pushed via `LISTEN/NOTIFY` → Server-Sent Events.
- **Regions** (materialized view) and **skill coverage**.

## API

| Method | Path | |
|---|---|---|
| GET | `/api/totals`, `/api/shelters`, `/api/shortages`, `/api/requests`, `/api/shipments`, `/api/regions`, `/api/skill-gaps`, `/api/activity`, `/api/items` | reads |
| GET | `/api/shelters/{id}/matches` | ranked volunteers |
| POST | `/api/shelters/{id}/occupancy` `{"delta": 10}` | check people in/out |
| POST | `/api/requests` `{"shelter_id","item_id","quantity","priority","note"}` | open a request |
| POST | `/api/requests/{id}/fulfill`, `/api/requests/{id}/cancel` | dispatch / cancel |
| POST | `/api/shifts` `{"volunteer_id","shelter_id","skill","starts_at","hours"}` | book a shift |
| POST | `/api/sql` `{"query": "..."}` | analyst console (one statement, 500-row cap) |
| GET | `/api/stream` | live changes (SSE) |
| GET | `/api/health` | liveness + server version |

## Backup and restore

```bash
scripts/backup.sh                         # backups/relieflink-<timestamp>.dump, verified, rotated (KEEP=30)
scripts/restore.sh backups/relieflink-….dump              # into relieflink_restore (safe default)
scripts/restore.sh backups/relieflink-….dump relieflink   # replace the live database (asks first)
```

Backups use pg_dump's custom format (compressed, selective, parallel restore).
Roles are cluster-wide and not in pg_dump output, so the script saves them
separately.

## Tests

```bash
go test ./...   # without a database, integration tests are skipped
RELIEFLINK_TEST_DSN='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' go test -race ./...
```

Each run creates a throwaway database, installs everything, and drops it
afterwards. The tests cover CSV import, every constraint, column privileges,
the exclusion constraint, nearest-hub dispatch, concurrent dispatch, the
analyst role's limits, and the audit trigger + NOTIFY.

## Useful psql commands

```
\c relieflink              connect
\dn+                       schemas with owners and privileges
\dt core.*  \dt ops.*      tables per schema
\d+ core.shelters          columns, constraints, indexes, triggers
\dv reporting.*            views
\df ops.*                  functions
\du                        roles
\dp ops.inventory          privileges on a table
\x auto                    expanded display for wide rows
SELECT * FROM reporting.critical_shortages;
SELECT * FROM reporting.match_volunteers(4);
LISTEN relief_events;      then change something in another session
```

## Project layout

```
cmd/relieflink/       setup and serve commands
db/migrations/        000 roles · 001 schema · 002 reporting views · 003 grants
db/seed/              CSV files + staging/load SQL
internal/store/       data access, setup, integration tests
internal/live/        LISTEN/NOTIFY → Server-Sent Events
internal/api/         HTTP handlers + dashboard (static/index.html)
scripts/              backup, restore, psql-only setup, pgAdmin servers
```

## Ideas for next steps

- Map view of shelters and hubs (the coordinates are already there).
- Row-level security so each region's coordinators see only their shelters.
- Partition `audit.changes` by month and expire old partitions.
- A `pg_cron` job to refresh `region_summary` and alert on new critical shortages.

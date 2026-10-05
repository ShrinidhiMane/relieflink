// Package store is the data-access layer. Most reads let PostgreSQL build the
// JSON directly (json_agg), so the database's views are the API's shape.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Store holds two pools: one per database role.
type Store struct {
	App     *sql.DB // relief_app: reads and writes operational data
	Analyst *sql.DB // relief_analyst: reporting views only, read-only
}

// Open connects both roles and verifies the connections.
func Open(ctx context.Context, appDSN, analystDSN string) (*Store, error) {
	app, err := sql.Open("postgres", appDSN)
	if err != nil {
		return nil, err
	}
	app.SetMaxOpenConns(20)
	app.SetConnMaxIdleTime(5 * time.Minute)
	an, err := sql.Open("postgres", analystDSN)
	if err != nil {
		return nil, err
	}
	an.SetMaxOpenConns(4)
	for name, d := range map[string]*sql.DB{"relief_app": app, "relief_analyst": an} {
		if err := d.PingContext(ctx); err != nil {
			return nil, fmt.Errorf("connect as %s: %w", name, err)
		}
	}
	return &Store{App: app, Analyst: an}, nil
}

func (s *Store) Close() { s.App.Close(); s.Analyst.Close() }

// JSON runs a query and returns its rows as a JSON array built by Postgres.
func (s *Store) JSON(ctx context.Context, query string, args ...any) (json.RawMessage, error) {
	var out []byte
	err := s.App.QueryRowContext(ctx, `SELECT coalesce(json_agg(t), '[]'::json) FROM (`+query+`) t`, args...).Scan(&out)
	return out, err
}

// JSONOne returns a single row as a JSON object, or ErrNotFound.
func (s *Store) JSONOne(ctx context.Context, query string, args ...any) (json.RawMessage, error) {
	var out []byte
	err := s.App.QueryRowContext(ctx, `SELECT row_to_json(t) FROM (`+query+`) t`, args...).Scan(&out)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return out, err
}

// writeJSON runs a data-modifying statement whose final SELECT returns one json
// column. (Data-modifying CTEs must be top-level, so JSONOne can't wrap them.)
func (s *Store) writeJSON(ctx context.Context, query string, args ...any) (json.RawMessage, error) {
	var out []byte
	err := s.App.QueryRowContext(ctx, query, args...).Scan(&out)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return out, err
}

// ErrNotFound is returned when a row doesn't exist.
var ErrNotFound = errors.New("not found")

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

func (s *Store) Shelters(ctx context.Context) (json.RawMessage, error) {
	return s.JSON(ctx, `SELECT * FROM reporting.shelter_status ORDER BY is_hub, lowest_days NULLS LAST, name`)
}

func (s *Store) Shortages(ctx context.Context) (json.RawMessage, error) {
	return s.JSON(ctx, `SELECT * FROM reporting.critical_shortages`)
}

func (s *Store) OpenRequests(ctx context.Context) (json.RawMessage, error) {
	return s.JSON(ctx, `SELECT * FROM reporting.open_requests`)
}

func (s *Store) Shipments(ctx context.Context, limit int) (json.RawMessage, error) {
	return s.JSON(ctx, `SELECT * FROM reporting.shipment_log LIMIT $1`, limit)
}

func (s *Store) Items(ctx context.Context) (json.RawMessage, error) {
	return s.JSON(ctx, `SELECT id, sku, name, category, unit FROM core.items ORDER BY category, name`)
}

func (s *Store) SkillGaps(ctx context.Context) (json.RawMessage, error) {
	return s.JSON(ctx, `SELECT * FROM reporting.skill_gaps`)
}

// Regions refreshes the materialized view first so the numbers are current.
func (s *Store) Regions(ctx context.Context) (json.RawMessage, error) {
	if _, err := s.App.ExecContext(ctx, `SELECT reporting.refresh_region_summary()`); err != nil {
		return nil, err
	}
	return s.JSON(ctx, `SELECT * FROM reporting.region_summary ORDER BY region`)
}

func (s *Store) Matches(ctx context.Context, shelterID, limit int) (json.RawMessage, error) {
	return s.JSON(ctx, `SELECT * FROM reporting.match_volunteers($1, $2)`, shelterID, limit)
}

// Totals is the KPI strip.
func (s *Store) Totals(ctx context.Context) (json.RawMessage, error) {
	return s.JSONOne(ctx, `
		SELECT
		  (SELECT coalesce(sum(occupancy), 0) FROM core.shelters WHERE NOT is_hub)                          AS people_sheltered,
		  (SELECT coalesce(sum(capacity - occupancy), 0) FROM core.shelters WHERE NOT is_hub AND status = 'open') AS beds_free,
		  (SELECT count(*) FROM reporting.critical_shortages WHERE severity = 'critical')                   AS critical_shortages,
		  (SELECT count(*) FROM ops.requests WHERE status = 'open')                                         AS open_requests,
		  (SELECT count(DISTINCT volunteer_id) FROM ops.shifts WHERE now() BETWEEN starts_at AND ends_at)   AS volunteers_on_shift,
		  (SELECT count(*) FROM ops.shipments WHERE shipped_at > now() - interval '24 hours')               AS shipments_24h`)
}

// Activity returns recent audit entries in a compact, human-readable form.
func (s *Store) Activity(ctx context.Context, limit int) (json.RawMessage, error) {
	return s.JSON(ctx, `
		SELECT id, changed_at, actor, table_name, operation, `+describeChangeSQL+` AS summary
		FROM audit.changes c ORDER BY id DESC LIMIT $1`, limit)
}

// describeChangeSQL turns an audit row into a sentence, entirely in SQL.
const describeChangeSQL = `
  CASE c.table_name
    WHEN 'core.shelters' THEN
      (SELECT name FROM core.shelters WHERE id = (coalesce(c.new_row, c.old_row)->>'id')::int) ||
      CASE WHEN c.old_row->>'occupancy' IS DISTINCT FROM c.new_row->>'occupancy'
           THEN ': occupancy ' || (c.old_row->>'occupancy') || ' → ' || (c.new_row->>'occupancy')
           ELSE ' updated' END
    WHEN 'ops.requests' THEN
      'Request #' || (coalesce(c.new_row, c.old_row)->>'id') || ' ' ||
      CASE c.operation WHEN 'INSERT' THEN 'opened (' || (c.new_row->>'priority') || ')'
                       ELSE coalesce(c.new_row->>'status', 'changed') END
    WHEN 'ops.shipments' THEN
      'Shipped ' || (c.new_row->>'quantity') || ' × ' ||
      (SELECT name FROM core.items WHERE id = (c.new_row->>'item_id')::int) ||
      ' (' || (c.new_row->>'distance_km') || ' km)'
    WHEN 'ops.inventory' THEN
      (SELECT name FROM core.shelters WHERE id = (coalesce(c.new_row, c.old_row)->>'shelter_id')::int) || ': ' ||
      (SELECT name FROM core.items    WHERE id = (coalesce(c.new_row, c.old_row)->>'item_id')::int) || ' ' ||
      coalesce(c.old_row->>'quantity', '0') || ' → ' || coalesce(c.new_row->>'quantity', '0')
    WHEN 'ops.shifts' THEN
      'Shift ' || CASE c.operation WHEN 'INSERT' THEN 'booked' ELSE 'cancelled' END || ': ' ||
      (SELECT full_name FROM ops.volunteers WHERE id = (coalesce(c.new_row, c.old_row)->>'volunteer_id')::int) || ' at ' ||
      (SELECT name FROM core.shelters WHERE id = (coalesce(c.new_row, c.old_row)->>'shelter_id')::int)
    ELSE c.operation
  END`

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

// NewRequest is a supply request from a shelter.
type NewRequest struct {
	ShelterID int    `json:"shelter_id"`
	ItemID    int    `json:"item_id"`
	Quantity  int    `json:"quantity"`
	Priority  string `json:"priority"`
	Note      string `json:"note"`
}

func (s *Store) CreateRequest(ctx context.Context, r NewRequest) (json.RawMessage, error) {
	if r.Priority == "" {
		r.Priority = "normal"
	}
	return s.writeJSON(ctx, `
		WITH ins AS (
		  INSERT INTO ops.requests (shelter_id, item_id, quantity, priority, note)
		  SELECT $1, $2, $3, $4::ops.priority, nullif($5, '')
		  WHERE EXISTS (SELECT 1 FROM core.shelters WHERE id = $1 AND NOT is_hub)
		  RETURNING *
		)
		-- A data-modifying CTE's rows aren't visible to other parts of the same
		-- statement, so build the response from RETURNING, not from the view.
		SELECT row_to_json(r) FROM (
		  SELECT ins.id, ins.created_at, ins.priority, ins.quantity, ins.note,
		         s.name AS shelter, it.name AS item, it.unit
		  FROM ins JOIN core.shelters s ON s.id = ins.shelter_id JOIN core.items it ON it.id = ins.item_id
		) r`,
		r.ShelterID, r.ItemID, r.Quantity, r.Priority, r.Note)
}

// Fulfill dispatches a request from the nearest hub with stock (ops.fulfill_request).
func (s *Store) Fulfill(ctx context.Context, requestID int64) (json.RawMessage, error) {
	return s.JSONOne(ctx, `
		SELECT f.id, f.quantity, f.distance_km, h.name AS from_hub, d.name AS to_shelter, it.name AS item, it.unit
		FROM ops.fulfill_request($1) f
		JOIN core.shelters h ON h.id = f.from_shelter_id
		JOIN core.shelters d ON d.id = f.to_shelter_id
		JOIN core.items   it ON it.id = f.item_id`, requestID)
}

func (s *Store) CancelRequest(ctx context.Context, requestID int64) error {
	res, err := s.App.ExecContext(ctx, `UPDATE ops.requests SET status = 'cancelled', closed_at = now() WHERE id = $1 AND status = 'open'`, requestID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AdjustOccupancy checks people in (delta > 0) or out (delta < 0).
// The CHECK constraints decide what's allowed, not the Go code.
func (s *Store) AdjustOccupancy(ctx context.Context, shelterID, delta int) (json.RawMessage, error) {
	return s.writeJSON(ctx, `
		WITH upd AS (
		  UPDATE core.shelters SET occupancy = occupancy + $2 WHERE id = $1
		  RETURNING id, name, occupancy, capacity
		) SELECT row_to_json(upd) FROM upd`, shelterID, delta)
}

// NewShift books a volunteer.
type NewShift struct {
	VolunteerID int       `json:"volunteer_id"`
	ShelterID   int       `json:"shelter_id"`
	Skill       string    `json:"skill"`
	StartsAt    time.Time `json:"starts_at"`
	Hours       float64   `json:"hours"`
}

func (s *Store) BookShift(ctx context.Context, n NewShift) (json.RawMessage, error) {
	if n.StartsAt.IsZero() {
		n.StartsAt = time.Now().Truncate(time.Hour).Add(time.Hour)
	}
	if n.Hours == 0 {
		n.Hours = 4
	}
	return s.writeJSON(ctx, `
		WITH ins AS (
		  INSERT INTO ops.shifts (volunteer_id, shelter_id, skill, starts_at, ends_at)
		  VALUES ($1, $2, $3, $4::timestamptz, $4::timestamptz + make_interval(secs => $5::float8 * 3600))
		  RETURNING id, volunteer_id, shelter_id, skill, starts_at, ends_at
		) SELECT row_to_json(ins) FROM ins`,
		n.VolunteerID, n.ShelterID, n.Skill, n.StartsAt, n.Hours)
}

// ---------------------------------------------------------------------------
// Analyst SQL console
// ---------------------------------------------------------------------------

// QueryResult is a tabular result for the console.
type QueryResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"`
	Millis    float64  `json:"ms"`
}

// AnalystQuery runs one statement as relief_analyst in a read-only transaction.
// Privileges are enforced by PostgreSQL itself; this function adds a row cap,
// a timeout and single-statement execution (prepared statements can't batch).
func (s *Store) AnalystQuery(ctx context.Context, query string, maxRows int) (*QueryResult, error) {
	query = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(query), ";"))
	if query == "" {
		return nil, errors.New("empty query")
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	tx, err := s.Analyst.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '3s'`); err != nil {
		return nil, err
	}
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	start := time.Now()
	rows, err := stmt.QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	res := &QueryResult{Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		if len(res.Rows) >= maxRows {
			res.Truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			switch x := v.(type) {
			case []byte:
				vals[i] = string(x)
			case time.Time:
				vals[i] = x.Format("2006-01-02 15:04:05")
			}
		}
		res.Rows = append(res.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.Millis = float64(time.Since(start).Microseconds()) / 1000
	return res, nil
}

// PGError extracts the Postgres error, if any.
func PGError(err error) (*pq.Error, bool) {
	var pe *pq.Error
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

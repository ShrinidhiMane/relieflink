// Package api is the HTTP layer: JSON endpoints, the live event stream, the
// analyst SQL console and the embedded dashboard.
package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ShrinidhiMane/relieflink/internal/live"
	"github.com/ShrinidhiMane/relieflink/internal/store"
)

//go:embed static/index.html
var static embed.FS

// Server wires handlers to the store.
type Server struct {
	Store *store.Store
	Hub   *live.Hub
	Log   *slog.Logger
}

// Routes returns the HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	get := func(path string, fn func(context.Context) (json.RawMessage, error)) {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) { s.raw(w, r, fn) })
	}

	get("/api/totals", s.Store.Totals)
	get("/api/shelters", s.Store.Shelters)
	get("/api/shortages", s.Store.Shortages)
	get("/api/requests", s.Store.OpenRequests)
	get("/api/items", s.Store.Items)
	get("/api/regions", s.Store.Regions)
	get("/api/skill-gaps", s.Store.SkillGaps)
	get("/api/shipments", func(ctx context.Context) (json.RawMessage, error) { return s.Store.Shipments(ctx, 25) })
	get("/api/activity", func(ctx context.Context) (json.RawMessage, error) { return s.Store.Activity(ctx, 40) })

	mux.HandleFunc("GET /api/shelters/{id}/matches", func(w http.ResponseWriter, r *http.Request) {
		id, ok := intParam(w, r, "id")
		if !ok {
			return
		}
		s.raw(w, r, func(ctx context.Context) (json.RawMessage, error) { return s.Store.Matches(ctx, id, 8) })
	})
	mux.HandleFunc("POST /api/shelters/{id}/occupancy", func(w http.ResponseWriter, r *http.Request) {
		id, ok := intParam(w, r, "id")
		if !ok {
			return
		}
		var body struct {
			Delta int `json:"delta"`
		}
		if !decode(w, r, &body) {
			return
		}
		s.raw(w, r, func(ctx context.Context) (json.RawMessage, error) {
			return s.Store.AdjustOccupancy(ctx, id, body.Delta)
		})
	})
	mux.HandleFunc("POST /api/requests", func(w http.ResponseWriter, r *http.Request) {
		var body store.NewRequest
		if !decode(w, r, &body) {
			return
		}
		s.raw(w, r, func(ctx context.Context) (json.RawMessage, error) { return s.Store.CreateRequest(ctx, body) }, http.StatusCreated)
	})
	mux.HandleFunc("POST /api/requests/{id}/fulfill", func(w http.ResponseWriter, r *http.Request) {
		id, ok := intParam(w, r, "id")
		if !ok {
			return
		}
		s.raw(w, r, func(ctx context.Context) (json.RawMessage, error) { return s.Store.Fulfill(ctx, int64(id)) })
	})
	mux.HandleFunc("POST /api/requests/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		id, ok := intParam(w, r, "id")
		if !ok {
			return
		}
		if err := s.Store.CancelRequest(r.Context(), int64(id)); err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"cancelled": id})
	})
	mux.HandleFunc("POST /api/shifts", func(w http.ResponseWriter, r *http.Request) {
		var body store.NewShift
		if !decode(w, r, &body) {
			return
		}
		s.raw(w, r, func(ctx context.Context) (json.RawMessage, error) { return s.Store.BookShift(ctx, body) }, http.StatusCreated)
	})

	// Analyst console: runs as relief_analyst, so PostgreSQL enforces what's visible.
	mux.HandleFunc("POST /api/sql", s.sql)
	mux.HandleFunc("GET /api/sql/examples", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, examples) })

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		var version string
		if err := s.Store.App.QueryRowContext(r.Context(), `SELECT current_setting('server_version')`).Scan(&version); err != nil {
			writeJSON(w, 503, map[string]string{"status": "down", "error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok", "postgres": version})
	})
	mux.Handle("GET /api/stream", s.Hub)

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := static.ReadFile("static/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
	return s.logging(mux)
}

func (s *Server) raw(w http.ResponseWriter, r *http.Request, fn func(context.Context) (json.RawMessage, error), status ...int) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	out, err := fn(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	code := http.StatusOK
	if len(status) > 0 {
		code = status[0]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(out)
}

func (s *Server) sql(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query string `json:"query"`
	}
	if !decode(w, r, &body) {
		return
	}
	res, err := s.Store.AnalystQuery(r.Context(), body.Query, 500)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, res)
}

// APIError is the error body. Constraint and SQLState show which database
// rule rejected the request.
type APIError struct {
	Error      string `json:"error"`
	SQLState   string `json:"sqlstate,omitempty"`
	Constraint string `json:"constraint,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

// friendly explains named constraints in plain language.
var friendly = map[string]string{
	"occupancy_within_capacity": "The shelter is full: that would exceed its capacity.",
	"shelters_occupancy_check":  "Occupancy can't go below zero.",
	"hubs_do_not_house_people":  "Supply hubs don't house people.",
	"closed_shelters_are_empty": "A closed shelter can't have occupants.",
	"inventory_quantity_check":  "Stock can't go below zero.",
	"requests_quantity_check":   "Quantity must be at least 1.",
	"no_double_booking":         "That volunteer already has an overlapping shift.",
	"shifts_check":              "A shift must end after it starts.",
	"shifts_check1":             "Shifts can be at most 12 hours.",
	"requests_shelter_id_fkey":  "That shelter doesn't exist.",
	"requests_item_id_fkey":     "That item doesn't exist.",
	"shifts_volunteer_id_fkey":  "That volunteer doesn't exist.",
	"shifts_shelter_id_fkey":    "That shelter doesn't exist.",
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, 404, APIError{Error: "not found"})
		return
	}
	if pe, ok := store.PGError(err); ok {
		e := APIError{Error: pe.Message, SQLState: string(pe.Code), Constraint: pe.Constraint, Detail: pe.Detail, Hint: pe.Hint}
		if msg, ok := friendly[pe.Constraint]; ok {
			e.Error = msg
		}
		if pe.Code.Class() == "23" {
			// Integrity errors echo the whole failing row (possibly personal data). Don't return it.
			e.Detail = ""
		}
		status := 400
		switch {
		case pe.Code == "RL404":
			status = 404
		case pe.Code == "RL409", pe.Code == "23505", pe.Code == "23514", pe.Code == "23P01":
			status = 409
		case pe.Code == "RL422", pe.Code == "23503":
			status = 422
		case pe.Code == "42501":
			status = 403
		case pe.Code == "57014":
			status = 408
		case pe.Code.Class() == "08" || pe.Code.Class() == "53" || pe.Code.Class() == "XX":
			status = 503
		}
		writeJSON(w, status, e)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeJSON(w, 504, APIError{Error: "timed out"})
		return
	}
	s.Log.Error("internal error", "err", err)
	writeJSON(w, 500, APIError{Error: err.Error()})
}

func intParam(w http.ResponseWriter, r *http.Request, name string) (int, bool) {
	v, err := strconv.Atoi(r.PathValue(name))
	if err != nil || v <= 0 {
		writeJSON(w, 400, APIError{Error: name + " must be a positive integer"})
		return 0, false
	}
	return v, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, 400, APIError{Error: "invalid JSON: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(c int) { r.status = c; r.ResponseWriter.WriteHeader(c) }
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if r.Method != http.MethodGet || rec.status >= 500 {
			s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "dur", time.Since(start).Round(time.Microsecond))
		}
	})
}

// Example queries for the console, including ones the analyst is NOT allowed to run.
type example struct {
	Label string `json:"label"`
	Query string `json:"query"`
}

var examples = []example{
	{"Worst shortages", "SELECT shelter, item, days_of_supply, shortfall_3d, severity\nFROM reporting.critical_shortages\nLIMIT 10;"},
	{"Regions", "SELECT region, open_shelters, people_sheltered, capacity,\n       round(100.0 * people_sheltered / nullif(capacity, 0), 1) AS pct_full\nFROM reporting.region_summary\nORDER BY pct_full DESC;"},
	{"Who speaks Spanish & does first aid?", "SELECT full_name, home_region, skills, languages\nFROM reporting.volunteer_roster\nWHERE languages @> '{es}' AND skills @> '{first_aid}' AND available;"},
	{"Best volunteers for shelter #4", "SELECT * FROM reporting.match_volunteers(4, 5);"},
	{"Skill gaps", "SELECT * FROM reporting.skill_gaps;"},
	{"Distance hub → shelter", "SELECT from_hub, to_shelter, item, quantity, distance_km\nFROM reporting.shipment_log;"},
	{"Blocked: contact details", "-- Analysts can't read ops.* — PostgreSQL rejects this.\nSELECT full_name, email, phone FROM ops.volunteers;"},
	{"Blocked: writes", "-- The analyst role is read-only.\nUPDATE reporting.region_summary SET capacity = 0;"},
}

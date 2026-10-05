package store

// Integration tests against a real PostgreSQL server. They are skipped unless
// RELIEFLINK_TEST_DSN points at a superuser connection, for example:
//
//	RELIEFLINK_TEST_DSN='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' go test ./...
//
// Each run creates a throwaway database and drops it afterwards.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
)

type env struct {
	store    *Store
	admin    *sql.DB
	appDSN   string
	adminDSN string
}

var shared *env

func TestMain(m *testing.M) {
	base := os.Getenv("RELIEFLINK_TEST_DSN")
	if base == "" {
		fmt.Println("RELIEFLINK_TEST_DSN not set: skipping PostgreSQL integration tests")
		os.Exit(m.Run())
	}
	code, err := withDatabase(base, m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "test setup:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func withDatabase(base string, m *testing.M) (int, error) {
	ctx := context.Background()
	u, err := url.Parse(base)
	if err != nil {
		return 0, err
	}
	maint, err := sql.Open("postgres", base)
	if err != nil {
		return 0, err
	}
	defer maint.Close()
	name := fmt.Sprintf("relieflink_test_%d", time.Now().UnixNano())
	if _, err := maint.Exec(`CREATE DATABASE ` + pq.QuoteIdentifier(name)); err != nil {
		return 0, err
	}
	defer func() {
		maint.Exec(`DROP DATABASE ` + pq.QuoteIdentifier(name) + ` WITH (FORCE)`)
	}()

	dsn := func(user, pw string) string {
		c := *u
		c.Path = "/" + name
		if user != "" {
			c.User = url.UserPassword(user, pw)
		}
		return c.String()
	}
	adminDSN := dsn("", "")
	if err := Setup(ctx, SetupOptions{AdminDSN: adminDSN, AppPassword: "test_app", AnalystPassword: "test_analyst", Seed: true}); err != nil {
		return 0, err
	}
	st, err := Open(ctx, dsn("relief_app", "test_app"), dsn("relief_analyst", "test_analyst"))
	if err != nil {
		return 0, err
	}
	defer st.Close()
	admin, _ := sql.Open("postgres", adminDSN)
	defer admin.Close()
	shared = &env{store: st, admin: admin, appDSN: dsn("relief_app", "test_app"), adminDSN: adminDSN}
	return m.Run(), nil
}

func need(t *testing.T) *env {
	t.Helper()
	if shared == nil {
		t.Skip("no database")
	}
	return shared
}

func pgCode(t *testing.T, err error) string {
	t.Helper()
	pe, ok := PGError(err)
	if !ok {
		t.Fatalf("expected a PostgreSQL error, got %v", err)
	}
	return string(pe.Code)
}

func scanInt(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSetupIsIdempotent(t *testing.T) {
	e := need(t)
	if err := Setup(context.Background(), SetupOptions{AdminDSN: e.adminDSN, AppPassword: "test_app", AnalystPassword: "test_analyst", Seed: true}); err != nil {
		t.Fatalf("second setup: %v", err)
	}
	if n := scanInt(t, e.admin, `SELECT count(*) FROM core.shelters`); n != 15 {
		t.Fatalf("seed ran twice? %d shelters", n)
	}
}

func TestSeedImportedEverything(t *testing.T) {
	e := need(t)
	for table, want := range map[string]int{"core.regions": 4, "core.shelters": 15, "core.items": 12, "ops.inventory": 180, "ops.volunteers": 60, "ops.requests": 8} {
		if got := scanInt(t, e.admin, `SELECT count(*) FROM `+table); got != want {
			t.Errorf("%s: %d rows, want %d", table, got, want)
		}
	}
}

func TestCheckConstraints(t *testing.T) {
	e := need(t)
	ctx := context.Background()
	if _, err := e.store.AdjustOccupancy(ctx, 1, 10_000); pgCode(t, err) != "23514" {
		t.Fatal("over capacity should violate a CHECK constraint")
	}
	if _, err := e.store.CreateRequest(ctx, NewRequest{ShelterID: 2, ItemID: 1, Quantity: -1}); pgCode(t, err) != "23514" {
		t.Fatal("negative quantity should be rejected")
	}
	if _, err := e.store.CreateRequest(ctx, NewRequest{ShelterID: 2, ItemID: 1, Quantity: 1, Priority: "urgent"}); pgCode(t, err) != "22P02" {
		t.Fatal("unknown enum value should be rejected")
	}
	if _, err := e.store.CreateRequest(ctx, NewRequest{ShelterID: 2, ItemID: 99999, Quantity: 1}); pgCode(t, err) != "23503" {
		t.Fatal("unknown item should violate the foreign key")
	}
}

func TestColumnLevelPrivileges(t *testing.T) {
	e := need(t)
	_, err := e.store.App.Exec(`UPDATE core.shelters SET name = 'renamed' WHERE id = 1`)
	if pgCode(t, err) != "42501" {
		t.Fatal("relief_app may only update occupancy and status")
	}
	_, err = e.store.App.Exec(`DELETE FROM ops.requests WHERE id = 1`)
	if pgCode(t, err) != "42501" {
		t.Fatal("relief_app may not delete requests")
	}
}

func TestNoDoubleBooking(t *testing.T) {
	e := need(t)
	ctx := context.Background()
	start := time.Now().Add(48 * time.Hour).Truncate(time.Hour)
	if _, err := e.store.BookShift(ctx, NewShift{VolunteerID: 5, ShelterID: 4, Skill: "first_aid", StartsAt: start, Hours: 4}); err != nil {
		t.Fatal(err)
	}
	_, err := e.store.BookShift(ctx, NewShift{VolunteerID: 5, ShelterID: 7, Skill: "first_aid", StartsAt: start.Add(2 * time.Hour), Hours: 4})
	if pgCode(t, err) != "23P01" {
		t.Fatal("overlapping shift should violate the exclusion constraint")
	}
	// Back-to-back is fine: ranges are [start, end).
	if _, err := e.store.BookShift(ctx, NewShift{VolunteerID: 5, ShelterID: 7, Skill: "first_aid", StartsAt: start.Add(4 * time.Hour), Hours: 4}); err != nil {
		t.Fatalf("adjacent shift: %v", err)
	}
}

func TestFulfillMovesStockFromNearestHub(t *testing.T) {
	e := need(t)
	ctx := context.Background()
	// Request #1: 1500 bottles of water for Lincoln High School Gym.
	before := scanInt(t, e.admin, `SELECT quantity FROM ops.inventory WHERE shelter_id = 1 AND item_id = 1`)
	raw, err := e.store.Fulfill(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var ship struct {
		FromHub  string  `json:"from_hub"`
		Quantity int     `json:"quantity"`
		Distance float64 `json:"distance_km"`
	}
	json.Unmarshal(raw, &ship)
	if ship.FromHub != "North Distribution Hub" || ship.Quantity != 1500 || ship.Distance <= 0 {
		t.Fatalf("unexpected shipment: %s", raw)
	}
	after := scanInt(t, e.admin, `SELECT quantity FROM ops.inventory WHERE shelter_id = 1 AND item_id = 1`)
	if after-before != 1500 {
		t.Fatalf("shelter stock moved by %d", after-before)
	}
	if _, err := e.store.Fulfill(ctx, 1); pgCode(t, err) != "RL409" {
		t.Fatal("fulfilling twice should fail")
	}
	// North hub has no electrolytes, so request #2 must come from another hub.
	raw, err = e.store.Fulfill(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &ship)
	if ship.FromHub == "North Distribution Hub" {
		t.Fatal("picked a hub with no stock")
	}
}

func TestConcurrentDispatchNeverOversells(t *testing.T) {
	e := need(t)
	ctx := context.Background()
	// Every hub gets exactly 10 insulin packs (item MED-002).
	if _, err := e.admin.Exec(`UPDATE ops.inventory i SET quantity = 10 FROM core.shelters s, core.items it
		WHERE s.id = i.shelter_id AND s.is_hub AND it.id = i.item_id AND it.sku = 'MED-002'`); err != nil {
		t.Fatal(err)
	}
	var item int
	e.admin.QueryRow(`SELECT id FROM core.items WHERE sku = 'MED-002'`).Scan(&item)

	// 10 requests of 6 packs, dispatched at once. Each hub can serve only one.
	ids := make([]int64, 10)
	for i := range ids {
		raw, err := e.store.CreateRequest(ctx, NewRequest{ShelterID: 4 + i%3, ItemID: item, Quantity: 6, Priority: "high"})
		if err != nil {
			t.Fatal(err)
		}
		var r struct{ ID int64 }
		json.Unmarshal(raw, &r)
		ids[i] = r.ID
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, rejected := 0, 0
	for _, id := range ids {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			_, err := e.store.Fulfill(ctx, id)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if pe, isPG := PGError(err); isPG && pe.Code == "RL422" {
				rejected++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}(id)
	}
	wg.Wait()
	if ok != 3 || rejected != 7 {
		t.Fatalf("ok=%d rejected=%d, want 3 and 7", ok, rejected)
	}
	if neg := scanInt(t, e.admin, `SELECT count(*) FROM ops.inventory WHERE quantity < 0`); neg != 0 {
		t.Fatal("stock went negative")
	}
}

func TestAnalystIsReadOnlyAndScoped(t *testing.T) {
	e := need(t)
	ctx := context.Background()
	if _, err := e.store.AnalystQuery(ctx, `SELECT email FROM ops.volunteers`, 10); pgCode(t, err) != "42501" {
		t.Fatal("analyst must not read volunteer contact details")
	}
	if _, err := e.store.AnalystQuery(ctx, `SELECT * FROM audit.changes`, 10); pgCode(t, err) != "42501" {
		t.Fatal("analyst must not read the audit log")
	}
	if _, err := e.store.AnalystQuery(ctx, `UPDATE reporting.region_summary SET capacity = 0`, 10); err == nil {
		t.Fatal("analyst must not write")
	}
	if _, err := e.store.AnalystQuery(ctx, `SELECT 1; SELECT 2`, 10); err == nil {
		t.Fatal("multiple statements must be rejected")
	}
	res, err := e.store.AnalystQuery(ctx, `SELECT * FROM reporting.match_volunteers(4, 3)`, 10)
	if err != nil || len(res.Rows) == 0 {
		t.Fatalf("analyst should run match_volunteers: %v", err)
	}
	res, err = e.store.AnalystQuery(ctx, `SELECT generate_series(1, 1000)`, 50)
	if err != nil || len(res.Rows) != 50 || !res.Truncated {
		t.Fatalf("row cap not applied: %v", err)
	}
}

func TestAuditTrailAndNotify(t *testing.T) {
	e := need(t)
	l := pq.NewListener(e.appDSN, time.Second, time.Second, nil)
	defer l.Close()
	if err := l.Listen("relief_events"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AdjustOccupancy(context.Background(), 8, 3); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-l.Notify:
		var ev struct{ Table, Op string }
		json.Unmarshal([]byte(n.Extra), &ev)
		if ev.Table != "core.shelters" || ev.Op != "UPDATE" {
			t.Fatalf("unexpected notification: %s", n.Extra)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no NOTIFY received")
	}
	var actor, oldOcc, newOcc string
	err := e.admin.QueryRow(`SELECT actor, old_row->>'occupancy', new_row->>'occupancy' FROM audit.changes
		WHERE table_name = 'core.shelters' ORDER BY id DESC LIMIT 1`).Scan(&actor, &oldOcc, &newOcc)
	if err != nil || actor != "relief_app" || oldOcc == newOcc {
		t.Fatalf("audit row: actor=%s %s→%s err=%v", actor, oldOcc, newOcc, err)
	}
}

func TestReportingViews(t *testing.T) {
	e := need(t)
	ctx := context.Background()
	for name, fn := range map[string]func(context.Context) (json.RawMessage, error){
		"shelters": e.store.Shelters, "shortages": e.store.Shortages, "requests": e.store.OpenRequests,
		"regions": e.store.Regions, "gaps": e.store.SkillGaps, "totals": e.store.Totals,
		"activity": func(c context.Context) (json.RawMessage, error) { return e.store.Activity(c, 10) },
	} {
		raw, err := fn(ctx)
		if err != nil || !json.Valid(raw) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var days float64
	if err := e.store.App.QueryRow(`SELECT days_of_supply FROM reporting.critical_shortages ORDER BY days_of_supply LIMIT 1`).Scan(&days); err != nil || days >= 0.5 {
		t.Fatalf("seed should contain a critical shortage, got %v (%v)", days, err)
	}
}

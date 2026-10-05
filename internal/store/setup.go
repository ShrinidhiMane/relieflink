package store

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/lib/pq"

	"github.com/ShrinidhiMane/relieflink/db"
)

// SetupOptions configures database installation.
type SetupOptions struct {
	AdminDSN        string // superuser connection
	AppPassword     string
	AnalystPassword string
	Reset           bool // drop everything first
	Seed            bool // load the CSV demo data
	Log             func(format string, args ...any)
}

// Setup applies pending migrations (and optionally seeds) as the superuser.
func Setup(ctx context.Context, o SetupOptions) error {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	admin, err := sql.Open("postgres", o.AdminDSN)
	if err != nil {
		return err
	}
	defer admin.Close()
	if err := admin.PingContext(ctx); err != nil {
		return fmt.Errorf("connect as admin: %w", err)
	}

	if o.Reset {
		logf("dropping schemas core, ops, audit, reporting")
		if _, err := admin.ExecContext(ctx, `
			DROP SCHEMA IF EXISTS reporting, audit, ops, core CASCADE;
			DROP TABLE IF EXISTS public.relieflink_migrations;`); err != nil {
			return err
		}
	}

	if _, err := admin.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS public.relieflink_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	files, err := fs.Glob(db.Migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	fresh := false
	for _, f := range files {
		version := path.Base(f)
		var done bool
		if err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM public.relieflink_migrations WHERE version = $1)`, version).Scan(&done); err != nil {
			return err
		}
		if done && version != "000_roles.sql" { // roles always re-run so passwords can be rotated
			continue
		}
		body, _ := db.Migrations.ReadFile(f)
		if err := inTx(ctx, admin, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `SELECT set_config('relieflink.app_password', $1, true),
			                                         set_config('relieflink.analyst_password', $2, true)`,
				o.AppPassword, o.AnalystPassword); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO public.relieflink_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`, version)
			return err
		}); err != nil {
			return fmt.Errorf("migration %s: %w", version, describe(err))
		}
		if !done {
			fresh = true
			logf("applied %s", version)
		}
	}
	if !fresh {
		logf("schema up to date")
	}

	if o.Seed {
		var n int
		if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM core.regions`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			logf("seed skipped: data already present (use -reset to start over)")
			return nil
		}
		if err := seed(ctx, admin, logf); err != nil {
			return fmt.Errorf("seed: %w", describe(err))
		}
	}
	return nil
}

// seed imports the CSV files with COPY inside one transaction.
func seed(ctx context.Context, admin *sql.DB, logf func(string, ...any)) error {
	stage, _ := db.Seed.ReadFile("seed/stage.sql")
	load, _ := db.Seed.ReadFile("seed/load.sql")
	return inTx(ctx, admin, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, string(stage)); err != nil {
			return err
		}
		for _, name := range []string{"regions", "shelters", "items", "inventory", "volunteers", "requests"} {
			n, err := copyCSV(ctx, tx, "seed/"+name+".csv", "stage_"+name)
			if err != nil {
				return fmt.Errorf("%s.csv: %w", name, err)
			}
			logf("copied %d rows from %s.csv", n, name)
		}
		_, err := tx.ExecContext(ctx, string(load))
		return err
	})
}

// copyCSV streams a CSV into a table using the COPY protocol.
func copyCSV(ctx context.Context, tx *sql.Tx, file, table string) (int, error) {
	f, err := db.Seed.Open(file)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, pq.CopyIn(table, header...))
	if err != nil {
		return 0, err
	}
	n := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, err
		}
		args := make([]any, len(rec))
		for i, v := range rec {
			if v == "" {
				args[i] = nil // empty CSV field = NULL, as COPY ... CSV would do
			} else {
				args[i] = v
			}
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			return n, err
		}
		n++
	}
	if _, err := stmt.ExecContext(ctx); err != nil { // flush
		return n, err
	}
	return n, stmt.Close()
}

func inTx(ctx context.Context, dbh *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := dbh.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// describe adds Postgres detail/hint/position to an error message.
func describe(err error) error {
	if pe, ok := err.(*pq.Error); ok {
		var b strings.Builder
		b.WriteString(pe.Message)
		if pe.Detail != "" {
			b.WriteString(" — " + pe.Detail)
		}
		if pe.Where != "" {
			b.WriteString(" (" + pe.Where + ")")
		}
		return fmt.Errorf("%s [SQLSTATE %s]", b.String(), pe.Code)
	}
	return err
}

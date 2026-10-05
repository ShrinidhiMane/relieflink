// Command relieflink coordinates shelters, supplies and volunteers during a
// disaster, on top of PostgreSQL.
//
//	relieflink setup [-reset] [-seed]   create the database, roles, schema and demo data
//	relieflink serve                    run the web dashboard and API
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lib/pq"

	"github.com/ShrinidhiMane/relieflink/internal/api"
	"github.com/ShrinidhiMane/relieflink/internal/live"
	"github.com/ShrinidhiMane/relieflink/internal/store"
)

type conn struct {
	host, port, dbname, sslmode string
}

func (c conn) dsn(user, password string) string {
	u := url.URL{Scheme: "postgres", Host: c.host + ":" + c.port, Path: "/" + c.dbname}
	if password != "" {
		u.User = url.UserPassword(user, password)
	} else {
		u.User = url.User(user)
	}
	q := url.Values{"sslmode": {c.sslmode}, "application_name": {"relieflink"}}
	u.RawQuery = q.Encode()
	return u.String()
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	c := conn{}
	fs.StringVar(&c.host, "host", env("PGHOST", "localhost"), "PostgreSQL host")
	fs.StringVar(&c.port, "port", env("PGPORT", "5432"), "PostgreSQL port")
	fs.StringVar(&c.dbname, "db", env("RELIEF_DB", "relieflink"), "database name")
	fs.StringVar(&c.sslmode, "sslmode", env("PGSSLMODE", "disable"), "sslmode (disable, require, verify-full)")
	appPW := env("RELIEF_APP_PASSWORD", "relief_app_dev")
	analystPW := env("RELIEF_ANALYST_PASSWORD", "relief_analyst_dev")

	var err error
	switch cmd {
	case "setup":
		adminUser := fs.String("admin-user", env("PGUSER", "postgres"), "superuser for setup")
		reset := fs.Bool("reset", false, "drop and recreate all ReliefLink schemas")
		seed := fs.Bool("seed", true, "load the demo CSV data into an empty database")
		fs.Parse(args)
		err = setup(c, *adminUser, env("PGPASSWORD", ""), appPW, analystPW, *reset, *seed, log)
	case "serve":
		addr := fs.String("addr", env("RELIEF_ADDR", ":8080"), "HTTP listen address")
		fs.Parse(args)
		err = serve(c, appPW, analystPW, *addr, log)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error(cmd+" failed", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  relieflink setup [-reset] [-seed=true] [-host H -port P -db NAME -admin-user postgres]
  relieflink serve [-addr :8080] [-host H -port P -db NAME]

environment: PGHOST PGPORT PGUSER PGPASSWORD RELIEF_DB
             RELIEF_APP_PASSWORD RELIEF_ANALYST_PASSWORD RELIEF_ADDR`)
}

func setup(c conn, adminUser, adminPW, appPW, analystPW string, reset, seed bool, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Create the database if it doesn't exist (connect to the maintenance db).
	maint := c
	maint.dbname = "postgres"
	m, err := sql.Open("postgres", maint.dsn(adminUser, adminPW))
	if err != nil {
		return err
	}
	var exists bool
	if err := m.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, c.dbname).Scan(&exists); err != nil {
		m.Close()
		return fmt.Errorf("connect to %s:%s as %s: %w", c.host, c.port, adminUser, err)
	}
	if !exists {
		if _, err := m.ExecContext(ctx, `CREATE DATABASE `+pq.QuoteIdentifier(c.dbname)); err != nil {
			m.Close()
			return err
		}
		log.Info("created database", "name", c.dbname)
	}
	m.Close()

	return store.Setup(ctx, store.SetupOptions{
		AdminDSN:        c.dsn(adminUser, adminPW),
		AppPassword:     appPW,
		AnalystPassword: analystPW,
		Reset:           reset,
		Seed:            seed,
		Log:             func(f string, a ...any) { log.Info(fmt.Sprintf(f, a...)) },
	})
}

func serve(c conn, appPW, analystPW, addr string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	appDSN := c.dsn("relief_app", appPW)
	st, err := store.Open(ctx, appDSN, c.dsn("relief_analyst", analystPW))
	if err != nil {
		return fmt.Errorf("%w (did you run `relieflink setup`?)", err)
	}
	defer st.Close()

	hub := live.NewHub(log)
	if err := hub.Listen(ctx, appDSN); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           (&api.Server{Store: st, Hub: hub, Log: log}).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("ReliefLink is running", "url", "http://localhost"+addr, "db", c.dbname)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// memory-plugin-postgres is the built-in implementation of the memory
// plugin contract (RFC #2728). Operators run it next to workspace-
// server; workspace-server points MEMORY_PLUGIN_URL at it.
//
// Owns its own postgres tables (see migrations/). When an operator
// swaps in a different plugin, this binary's tables become orphaned
// — not auto-dropped. Document this in the plugin docs (PR-10).
package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/memory/pgplugin"
)

// migrationsFS bundles the .up.sql files into the binary at build time
// so the prebuilt image doesn't need the source tree at runtime. The
// prior `os.ReadDir("cmd/memory-plugin-postgres/migrations")` path
// only resolved during `go test` from the repo root — in the published
// image the path didn't exist and boot failed after the 30s health gate
// (caught on staging redeploy 2026-05-05 after PR #2906).
//
//go:embed migrations/*.up.sql
var migrationsFS embed.FS

const (
	envDatabaseURL = "MEMORY_PLUGIN_DATABASE_URL"
	envListenAddr  = "MEMORY_PLUGIN_LISTEN_ADDR"
	envSkipMigrate = "MEMORY_PLUGIN_SKIP_MIGRATE"

	// Loopback-only by default (defense in depth). The platform talks to
	// the plugin over `http://localhost:9100` from the same container, so
	// binding to all interfaces would only widen the reachable surface
	// without enabling any in-design caller. Operators running the plugin
	// on a separate host override via MEMORY_PLUGIN_LISTEN_ADDR=:9100 (or
	// some other interface).
	defaultListenAddr = "127.0.0.1:9100"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("memory-plugin-postgres: %v", err)
	}
}

// run is the boot path. Extracted from main() so tests can drive it
// with synthesized env. Returns nil on graceful shutdown, an error on
// failure to bring up.
func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	db, err := openDBWithRetry(cfg.DatabaseURL, openDB, dbConnectWait, dbConnectInterval, time.Now, time.Sleep)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	if !cfg.SkipMigrate {
		if err := runMigrations(db); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	store := pgplugin.NewStore(db)
	handler := pgplugin.NewHandler(store, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return db.PingContext(ctx)
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Listen separately so we can log the bound port (handy when
	// :0 is used in tests).
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.ListenAddr, err)
	}
	log.Printf("memory-plugin-postgres listening on %s", ln.Addr())

	// Run server in a goroutine; main waits on signal.
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigCh:
		log.Println("shutdown signal received")
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// How long the boot keeps trying to reach Postgres, and how often.
//
// The tenant entrypoint (entrypoint-tenant.sh) gives the sidecar about 30 s
// to answer /v1/health, then fails the boot (MEMORY_PLUGIN_REQUIRED) or
// serves without memory. A new tenant pod's first connection to its own
// Postgres can be refused for a moment after a roll (molecule-core#5303:
// every roll on node robot-1), and the same database accepts a connection
// seconds later. Exiting on that first refusal cost one tenant its memory
// until the next roll, and crashed the others once. So the boot keeps
// trying for most of the entrypoint's window. A database that stays
// unreachable still fails the boot, inside that window. Variables only so
// tests can shorten them.
var (
	dbConnectWait     = 25 * time.Second
	dbConnectInterval = time.Second
)

// openDBWithRetry calls open until it succeeds, pausing interval between
// attempts, and gives up when the next attempt would start at or after
// wait from the first one. The error then wraps the last attempt's error
// and says how many attempts were made. now and sleep are injected for tests.
func openDBWithRetry(databaseURL string, open func(string) (*sql.DB, error), wait, interval time.Duration, now func() time.Time, sleep func(time.Duration)) (*sql.DB, error) {
	deadline := now().Add(wait)
	for attempt := 1; ; attempt++ {
		db, err := open(databaseURL)
		if err == nil {
			if attempt > 1 {
				log.Printf("memory-plugin-postgres: database reachable on attempt %d", attempt)
			}
			return db, nil
		}
		if !now().Add(interval).Before(deadline) {
			return nil, fmt.Errorf("after %d attempt(s) over %s: %w", attempt, wait, err)
		}
		log.Printf("memory-plugin-postgres: database not reachable yet (attempt %d), retrying in %s: %v", attempt, interval, err)
		sleep(interval)
	}
}

type config struct {
	DatabaseURL string
	ListenAddr  string
	SkipMigrate bool
}

func loadConfig() (*config, error) {
	dbURL := strings.TrimSpace(os.Getenv(envDatabaseURL))
	if dbURL == "" {
		return nil, fmt.Errorf("%s is required", envDatabaseURL)
	}
	addr := strings.TrimSpace(os.Getenv(envListenAddr))
	if addr == "" {
		addr = defaultListenAddr
	}
	return &config{
		DatabaseURL: dbURL,
		ListenAddr:  addr,
		SkipMigrate: os.Getenv(envSkipMigrate) == "1",
	}, nil
}

func openDB(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close() // a failed attempt must not leave its pool behind; the boot may try again
		return nil, fmt.Errorf("ping: %w", err)
	}
	return db, nil
}

// runMigrations applies the schema migrations bundled into the binary
// via go:embed (see migrationsFS at the top of this file). Idempotent
// on repeat boot — every migration file uses CREATE … IF NOT EXISTS.
//
// The down migrations are deliberately NOT applied here — that's a
// manual operator action. This keeps the binary tiny and avoids
// dragging in golang-migrate's drivers.
//
// MEMORY_PLUGIN_MIGRATIONS_DIR (filesystem path) is honored as an
// override for operators who need to ship custom migrations alongside
// the binary without rebuilding. When unset (the common case) we read
// from the embedded FS.
func runMigrations(db *sql.DB) error {
	if dir := strings.TrimSpace(os.Getenv("MEMORY_PLUGIN_MIGRATIONS_DIR")); dir != "" {
		return runMigrationsFromDisk(db, dir)
	}
	return runMigrationsFromEmbed(db)
}

// runMigrationsFromEmbed applies the *.up.sql files bundled into the
// binary at build time. Order is alphabetical (matches the on-disk
// behavior of os.ReadDir on Linux for the same set of names).
func runMigrationsFromEmbed(db *sql.DB) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read embedded %q: %w", name, err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			return fmt.Errorf("apply %q: %w", name, err)
		}
		log.Printf("applied embedded migration %s", name)
	}
	return nil
}

// runMigrationsFromDisk preserves the legacy filesystem-path mode for
// operator-supplied custom migrations.
func runMigrationsFromDisk(db *sql.DB, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir %q: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		path := dir + "/" + name
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %q: %w", path, err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			return fmt.Errorf("apply %q: %w", path, err)
		}
		log.Printf("applied disk migration %s (from %s)", name, dir)
	}
	return nil
}

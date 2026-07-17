package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

func main() {
	modeFlag := flag.String("mode", "", "api")
	configPathFlag := flag.String("config", "", "path to a config file or directory; when empty, searches ./configs, ./, the binary's directory, and <binary-dir>/configs, falling back to the embedded default")
	flag.Parse()

	mode := *modeFlag
	switch mode {
	case "api":
	default:
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s --mode=api [--config=<path>]\n", flag.CommandLine.Name())
		os.Exit(1)
	}

	cfg, err := config.Load(*configPathFlag)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Open every registered database. The registry applies the
	// role-appropriate DDL (system, repository, or both) on
	// construction, so the per-pool schema work happens here in
	// `New`. After this call, every pool's search_path is set and
	// the right tables exist.
	registry, err := dbpool.New(ctx, cfg)
	if err != nil {
		log.Fatalf("opening database pools: %v", err)
	}
	defer registry.Close()

	// Re-affirm the schema was applied by checking that the
	// system tables we expect exist on the system pool. This is
	// belt-and-braces: if `dbpool.New` ran cleanly the tables
	// are there, but a boot-time SELECT against `users` catches
	// a misconfigured search_path (which would otherwise
	// surface as a confusing 500 on the first login).
	defaultPool := registry.Default()
	var hasUsers int
	if err := defaultPool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&hasUsers); err != nil {
		// Surfacing the error verbatim is enough — the
		// search_path is set by the registry's AfterConnect
		// hook, so a `relation "users" does not exist` here
		// means the migration didn't run on this database.
		log.Fatalf("verifying system schema on default pool: %v", err)
	}
	log.Println("database schema applied")

	// Build the default-pool Queries. Handlers that need to
	// route to a non-default pool build a per-request one
	// from `registry.Get(repo.DatabaseName)`.
	queries := store.New(defaultPool.Pool)

	switch mode {
	case "api":
		runAPI(ctx, cfg, queries, registry)
	}
}

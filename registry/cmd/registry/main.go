package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	migrate "github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openktree/knowledge-registry/internal/api"
	"github.com/openktree/knowledge-registry/internal/config"
	"github.com/openktree/knowledge-registry/internal/service"
	"github.com/openktree/knowledge-registry/internal/storage"
	"github.com/openktree/knowledge-registry/internal/store"
	knowledgeregistry "github.com/openktree/knowledge-registry"
)

func main() {
	cfgPath := ""
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	var mstore store.MetadataStore
	switch cfg.Database.Driver {
	case "sqlite", "":
		mstore, err = store.NewSQLiteStore(cfg.Database.URL)
		if err != nil {
			log.Fatalf("opening sqlite store: %v", err)
		}
	case "postgres":
		if err := migratePostgres(context.Background(), cfg.Database.URL); err != nil {
			log.Fatalf("migrating postgres: %v", err)
		}
		pool, err := pgxpool.New(context.Background(), cfg.Database.URL)
		if err != nil {
			log.Fatalf("connecting to postgres: %v", err)
		}
		mstore = store.NewPostgresStore(pool)
	default:
		log.Fatalf("unknown database driver: %s", cfg.Database.Driver)
	}
	defer mstore.Close()

	log.Printf("database: %s (%s)", cfg.Database.Driver, cfg.Database.URL)

	var fstore service.Storage
	switch cfg.Storage.Backend {
	case "filesystem":
		fstore, err = storage.NewLocalStore(cfg.Storage.FilesystemRoot)
		if err != nil {
			log.Fatalf("setting up filesystem storage: %v", err)
		}
	case "s3", "":
		fstore, err = storage.NewS3Store(storage.S3Config{
			Endpoint:       cfg.S3.Endpoint,
			Region:         cfg.S3.Region,
			Bucket:         cfg.S3.Bucket,
			AccessKey:       cfg.S3.AccessKey,
			SecretKey:      cfg.S3.SecretKey,
			PathStyle:      cfg.S3.PathStyle,
			PresignTTL:     cfg.S3.PresignTTL,
			PresignBaseURL: cfg.S3.PresignBaseURL,
		})
		if err != nil {
			log.Fatalf("setting up s3 storage: %v", err)
		}
	default:
		log.Fatalf("unknown storage backend: %s", cfg.Storage.Backend)
	}

	svc := service.New(mstore, fstore, cfg.S3.PresignTTL)
	router := api.NewRouter(svc, mstore, cfg)

	if err := svc.EnsureDefaultRepo(context.Background()); err != nil {
		log.Printf("warning: seeding default repo: %v", err)
	}

	if n, err := svc.SeedContexts(context.Background()); err != nil {
		log.Printf("warning: seeding contexts: %v", err)
	} else {
		log.Printf("seeded %d canonical contexts", n)
	}

	log.Printf("starting knowledge-registry on :%d", cfg.Port)
	log.Printf("  storage: %s", cfg.Storage.Backend)
	if cfg.Storage.Backend == "s3" {
		log.Printf("    bucket=%s, endpoint=%s", cfg.S3.Bucket, cfg.S3.Endpoint)
		if cfg.S3.PresignBaseURL != "" {
			log.Printf("    presign: enabled (base_url=%s)", cfg.S3.PresignBaseURL)
		} else {
			log.Printf("    presign: disabled (no presign_base_url; clients fall back to proxy)")
		}
	}
	log.Printf("  auth mode: %s", cfg.Auth.AuthMode)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
}

func migratePostgres(ctx context.Context, dsn string) error {
	src, err := iofs.New(knowledgeregistry.MigrationsFS, "db/migrations")
	if err != nil {
		return fmt.Errorf("loading embedded migrations: %w", err)
	}

	sqlDB, err := sql.Open("pgx/v5", dsn)
	if err != nil {
		return fmt.Errorf("opening migration connection: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("pinging database: %w", err)
	}

	driver, err := migratepgx.WithInstance(sqlDB, &migratepgx.Config{})
	if err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("initializing migrate driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("initializing migrate: %w", err)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		_ = sqlDB.Close()
		return fmt.Errorf("applying migrations: %w", err)
	}

	return sqlDB.Close()
}

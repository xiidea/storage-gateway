package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"storage-gateway/config"
	"storage-gateway/internal/api/admin"
	"storage-gateway/internal/api/health"
	apigw "storage-gateway/internal/api/s3"
	"storage-gateway/internal/auth"
	"storage-gateway/internal/backend"
	"storage-gateway/internal/db"
	"storage-gateway/internal/registry"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Error("loading config", "err", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// --- Database ---
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("opening database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		log.Error("running migrations", "err", err)
		os.Exit(1)
	}
	log.Info("migrations applied")

	// --- Redis ---
	rdbOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		log.Error("parsing redis url", "err", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(rdbOpts)
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Error("pinging redis", "err", err)
		os.Exit(1)
	}
	defer rdb.Close()

	// --- Crypto key ---
	cryptoKey, err := auth.DeriveKey(cfg.MasterKey)
	if err != nil {
		log.Error("deriving crypto key", "err", err)
		os.Exit(1)
	}

	// --- Registry (Postgres + Redis cache) ---
	mgr := registry.NewCached(registry.NewPostgres(pool), rdb, cfg.CacheTTL)

	// --- Backend pool ---
	backendPool := backend.NewPool(cryptoKey)

	// --- Admin token ---
	adminToken := os.Getenv("ADMIN_TOKEN")
	if adminToken == "" {
		log.Error("ADMIN_TOKEN environment variable is required")
		os.Exit(1)
	}

	// --- Health handler (shared by both servers, no auth) ---
	healthHandler := health.New(pool, rdb)

	// --- Admin server ---
	// /healthz is mounted outside bearerAuth so probes can reach it freely.
	adminMux := http.NewServeMux()
	adminMux.Handle("/healthz", healthHandler)
	adminMux.Handle("/", admin.New(mgr, cryptoKey, backendPool, adminToken, cfg.AdminBasePath))
	adminSrv := &http.Server{
		Addr:         cfg.AdminAddr,
		Handler:      adminMux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// --- S3 gateway server ---
	// /healthz also lives here so k8s readiness probes on the data-plane port work.
	gatewayMux := http.NewServeMux()
	gatewayMux.Handle("/healthz", healthHandler)
	gatewayMux.Handle("/", apigw.New(mgr, cryptoKey, backendPool, cfg.GatewayRegion))
	gatewaySrv := &http.Server{
		Addr:         cfg.GatewayAddr,
		Handler:      gatewayMux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // disabled: streaming downloads have no fixed deadline
		IdleTimeout:  60 * time.Second,
	}

	// Start both servers.
	go func() {
		log.Info("admin server listening", "addr", cfg.AdminAddr)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("admin server error", "err", err)
			os.Exit(1)
		}
	}()

	go func() {
		log.Info("gateway server listening", "addr", cfg.GatewayAddr)
		if err := gatewaySrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("gateway server error", "err", err)
			os.Exit(1)
		}
	}()

	// Block until SIGINT or SIGTERM.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down gracefully")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_ = adminSrv.Shutdown(shutdownCtx)
	_ = gatewaySrv.Shutdown(shutdownCtx)
	log.Info("shutdown complete")
}

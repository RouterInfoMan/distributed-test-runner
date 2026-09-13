// Command dtp-master is the control plane: it accepts regression submissions,
// schedules suites onto pool slots via Nomad, ingests results, and serves the
// dashboard.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/andrei/distributed-test-platform/internal/api"
	"github.com/andrei/distributed-test-platform/internal/backend"
	"github.com/andrei/distributed-test-platform/internal/config"
	"github.com/andrei/distributed-test-platform/internal/pg"
	"github.com/andrei/distributed-test-platform/internal/s3"
	"github.com/andrei/distributed-test-platform/internal/sched"
	"github.com/andrei/distributed-test-platform/internal/store"
)

func main() {
	cfgPath := flag.String("config", os.Getenv("DTP_CONFIG"), "path to master config JSON")
	debug := flag.Bool("debug", os.Getenv("DTP_DEBUG") != "", "verbose logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	st, err := openStore(cfg, log)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// Pools and quotas live in the store and nowhere else. Until it holds
	// some, the master runs with none: submissions are refused with "unknown
	// pool" and the dashboard's Config editor (or dtp config apply, or SQL)
	// is where they get added.
	switch cat, err := st.LoadCatalog(); {
	case err != nil:
		log.Error("load catalog", "err", err)
		os.Exit(1)
	case cat == nil:
		log.Warn("catalog: the store has no pools yet; add them with `dtp config apply <catalog.json>`, the dashboard or SQL")
	default:
		if err := cfg.SetCatalog(cat); err != nil {
			log.Error("catalog: stored pools/quotas are invalid; running with none until fixed", "err", err)
		} else {
			log.Info("catalog: loaded from the store", "pools", len(cat.Pools), "nodes", len(cat.Nodes), "groups", len(cat.Groups), "users", len(cat.Users), "rules", len(cat.Quotas))
		}
	}

	var s3c *s3.Client
	if cfg.S3.Endpoint != "" {
		s3c = s3.New(cfg.S3.Endpoint, cfg.S3.Region, cfg.S3.AccessKey, cfg.S3.SecretKey)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := s3c.EnsureBucket(ctx, cfg.S3.Bucket); err != nil {
			// Not fatal: the object store may still be starting up.
			log.Warn("object store not ready", "bucket", cfg.S3.Bucket, "err", err)
		} else {
			log.Info("object store ready", "endpoint", cfg.S3.Endpoint, "bucket", cfg.S3.Bucket)
		}
		cancel()
	} else {
		log.Warn("no object store configured; artifacts will stay on the nodes")
	}

	var be backend.Backend
	switch cfg.Backend {
	case "nomad":
		nb := backend.NewNomad(cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := nb.Healthy(ctx); err != nil {
			log.Warn("nomad not reachable yet", "addr", cfg.Nomad.Address, "err", err)
		} else {
			log.Info("nomad connected", "addr", cfg.Nomad.Address)
		}
		cancel()
		be = nb
	default:
		be = backend.NewLocal(cfg)
		log.Info("using local backend", "nodes", len(cfg.LocalNodes), "runner", cfg.LocalRunnerPath)
	}

	scheduler := sched.New(cfg, st, be, s3c, log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go scheduler.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(cfg, st, scheduler, s3c, log).Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// Generous: the dashboard long-polls /api/v1/overview.
		WriteTimeout: 60 * time.Second,
	}

	go func() {
		log.Info("master listening", "addr", cfg.Listen, "backend", be.Name(),
			"pools", len(cfg.Catalog().Pools), "dashboard", fmt.Sprintf("http://localhost%s/", cfg.Listen))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("listen", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
}

// openStore picks the persistence backend: PostgreSQL when configured, else
// JSON files under the state dir.
func openStore(cfg *config.Config, log *slog.Logger) (*store.Store, error) {
	var be store.Backend
	var err error
	switch cfg.Store.Driver {
	case "postgres":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		be, err = store.NewPostgresBackend(ctx, cfg.Store.DSN)
		if err != nil {
			return nil, err
		}
		pc, _ := pg.ParseDSN(cfg.Store.DSN)
		log.Info("store: postgres", "host", pc.Host, "database", pc.Database)
	default:
		be, err = store.NewFileBackend(cfg.StateDir + "/regressions")
		if err != nil {
			return nil, err
		}
		log.Info("store: json files", "dir", cfg.StateDir+"/regressions")
	}
	return store.OpenWith(be, log)
}

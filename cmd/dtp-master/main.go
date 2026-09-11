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

	st, err := store.Open(cfg.StateDir + "/regressions")
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
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
			"pools", len(cfg.Pools), "dashboard", fmt.Sprintf("http://localhost%s/", cfg.Listen))
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

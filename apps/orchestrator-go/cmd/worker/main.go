// Package main is the entrypoint for the Go Temporal worker that runs all
// migration-platform workflows and IO activities.
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

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/migration-platform/orchestrator/internal/activities"
	"github.com/migration-platform/orchestrator/internal/bridge"
	"github.com/migration-platform/orchestrator/internal/config"
	"github.com/migration-platform/orchestrator/internal/connectors"
	"github.com/migration-platform/orchestrator/internal/db"
	"github.com/migration-platform/orchestrator/internal/metrics"
	"github.com/migration-platform/orchestrator/internal/redisbus"
	"github.com/migration-platform/orchestrator/internal/security"
	"github.com/migration-platform/orchestrator/internal/telemetry"
	"github.com/migration-platform/orchestrator/internal/workflows"
)

const (
	taskQueueMigration = "migration"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "err", err)
		os.Exit(1)
	}

	shutdownOtel, err := telemetry.Init(context.Background(), "migration-orchestrator", cfg.OTLPEndpoint)
	if err != nil {
		logger.Warn("otel init failed", "err", err)
	}
	defer func() {
		if shutdownOtel != nil {
			_ = shutdownOtel(context.Background())
		}
	}()

	metricsSrv := metrics.ServeHTTP(cfg.MetricsAddr)
	defer func() { _ = metricsSrv.Shutdown(context.Background()) }()

	pg, err := db.NewPool(context.Background(), cfg.DatabaseURL)
	if err != nil {
		logger.Error("postgres connect failed", "err", err)
		os.Exit(1)
	}
	defer pg.Close()

	rds := redisbus.NewClient(cfg.RedisURL)
	defer func() { _ = rds.Close() }()

	if err := connectors.InitObjectStore(); err != nil {
		logger.Error("minio init failed", "err", err)
		os.Exit(1)
	}

	c, err := client.Dial(client.Options{HostPort: cfg.TemporalHostPort, Namespace: cfg.TemporalNamespace})
	if err != nil {
		logger.Error("temporal dial failed", "err", err)
		os.Exit(1)
	}
	defer c.Close()

	deps := activities.Deps{
		DB:               pg,
		Redis:            rds,
		HTTP:             security.NewEgressHTTPClient(30*time.Second, cfg.AllowPrivateDestinations),
		SecretsMasterKey: cfg.SecretsMasterKey,
		RateLimiter:      security.NewHostLimiter(cfg.DestinationRPS, cfg.DestinationBurst),
		MaxResponseBytes: cfg.MaxResponseBytes,
	}
	acts := activities.NewActivities(deps)
	if cfg.AllowPrivateDestinations {
		logger.Warn("ALLOW_PRIVATE_DESTINATIONS is enabled; SSRF egress guard is OFF (dev only)")
	}

	w := worker.New(c, taskQueueMigration, worker.Options{
		MaxConcurrentActivityExecutionSize: cfg.ActivityConcurrency,
		WorkerStopTimeout:                  30 * time.Second,
	})

	w.RegisterWorkflow(workflows.MigrationWorkflow)
	w.RegisterWorkflow(workflows.ProcessShardWorkflow)
	w.RegisterWorkflow(workflows.RetryRowWorkflow)
	w.RegisterWorkflow(workflows.RetryFailedRowsWorkflow)
	w.RegisterWorkflow(workflows.WatchPrefixWorkflow)
	w.RegisterWorkflow(workflows.BatchWorkflow)

	w.RegisterActivityWithOptions(acts.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	w.RegisterActivityWithOptions(acts.Ingest, activities.RegisterOptions("Ingest"))
	w.RegisterActivityWithOptions(acts.BootstrapScheduledJob, activities.RegisterOptions("BootstrapScheduledJob"))
	w.RegisterActivityWithOptions(acts.LoadConnector, activities.RegisterOptions("LoadConnector"))
	w.RegisterActivityWithOptions(acts.ListPrefixObjects, activities.RegisterOptions("ListPrefixObjects"))
	w.RegisterActivityWithOptions(acts.ListSftpObjects, activities.RegisterOptions("ListSftpObjects"))
	w.RegisterActivityWithOptions(acts.StageSftpObject, activities.RegisterOptions("StageSftpObject"))
	w.RegisterActivityWithOptions(acts.AdvanceConnectorCursor, activities.RegisterOptions("AdvanceConnectorCursor"))
	w.RegisterActivityWithOptions(acts.StartWatchObjectJob, activities.RegisterOptions("StartWatchObjectJob"))
	w.RegisterActivityWithOptions(acts.MarkJobRunning, activities.RegisterOptions("MarkJobRunning"))
	w.RegisterActivityWithOptions(acts.StartBatch, activities.RegisterOptions("StartBatch"))
	w.RegisterActivityWithOptions(acts.MarkBatchRunning, activities.RegisterOptions("MarkBatchRunning"))
	w.RegisterActivityWithOptions(acts.UnpackAndStageArchive, activities.RegisterOptions("UnpackAndStageArchive"))
	w.RegisterActivityWithOptions(acts.QuarantineArchive, activities.RegisterOptions("QuarantineArchive"))
	w.RegisterActivityWithOptions(acts.MaterializeBatchStages, activities.RegisterOptions("MaterializeBatchStages"))
	w.RegisterActivityWithOptions(acts.StartBatchStageJob, activities.RegisterOptions("StartBatchStageJob"))
	w.RegisterActivityWithOptions(acts.FinalizeBatchStage, activities.RegisterOptions("FinalizeBatchStage"))
	w.RegisterActivityWithOptions(acts.FinalizeBatch, activities.RegisterOptions("FinalizeBatch"))
	w.RegisterActivityWithOptions(acts.CallEndpoint, activities.RegisterOptions("CallEndpoint"))
	w.RegisterActivityWithOptions(acts.PersistOutcome, activities.RegisterOptions("PersistOutcome"))
	w.RegisterActivityWithOptions(acts.PersistStepOutcome, activities.RegisterOptions("PersistStepOutcome"))
	w.RegisterActivityWithOptions(acts.LoadRowState, activities.RegisterOptions("LoadRowState"))
	w.RegisterActivityWithOptions(acts.ListFailedRows, activities.RegisterOptions("ListFailedRows"))
	w.RegisterActivityWithOptions(acts.PublishProgress, activities.RegisterOptions("PublishProgress"))
	w.RegisterActivityWithOptions(acts.FinalizeJob, activities.RegisterOptions("FinalizeJob"))

	// Internal Temporal bridge for the Rust API (start/signal/cancel/schedules).
	bridgeSrv := &http.Server{
		Addr:              cfg.BridgeAddr,
		Handler:           bridge.NewServer(c, cfg.BridgeToken, logger).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("temporal bridge listening", "addr", cfg.BridgeAddr)
		if err := bridgeSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("bridge server failed", "err", err)
		}
	}()
	defer func() { _ = bridgeSrv.Shutdown(context.Background()) }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		logger.Info("shutdown signal received; stopping worker")
		w.Stop()
	}()

	logger.Info("worker starting", "taskQueue", taskQueueMigration)
	if err := w.Run(worker.InterruptCh()); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("worker exited with error", "err", err)
		os.Exit(1)
	}
	logger.Info("worker stopped cleanly")
}

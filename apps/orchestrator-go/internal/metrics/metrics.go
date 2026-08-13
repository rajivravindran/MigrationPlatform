// Package metrics exposes Prometheus counters and a /metrics server.
package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	RowsProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rows_processed_total",
		Help: "Total rows successfully processed.",
	}, []string{"job_id"})

	RowsFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rows_failed_total",
		Help: "Total rows that failed permanently.",
	}, []string{"job_id"})

	EndpointLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "endpoint_call_seconds",
		Help:    "Latency of destination endpoint calls.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 14),
	}, []string{"status"})

	LicenseDenied = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "license_work_denied_total",
		Help: "Work-producing workflow attempts denied by the runtime license gate.",
	})
	SFTPFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sftp_failures_total",
		Help: "SFTP operations that failed.",
	}, []string{"operation"})
	QuarantinedBatches = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "batch_quarantine_total",
		Help: "Batch archives moved to quarantine.",
	})
	WatchLag = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "watch_discovery_lag_seconds",
		Help:    "Age of the oldest object returned by a bounded watch listing.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 18),
	}, []string{"connector"})
	DAGBatches = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "batch_dag_total",
		Help: "Batch manifests materialized with dependency edges.",
	})
)

func init() {
	prometheus.MustRegister(RowsProcessed, RowsFailed, EndpointLatency, LicenseDenied, SFTPFailures, QuarantinedBatches, WatchLag, DAGBatches)
}

func ServeHTTP(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		_ = srv.ListenAndServe()
	}()
	return srv
}

// Context-safe background ticker utility.
func After(ctx context.Context, d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		select {
		case <-time.After(d):
			close(ch)
		case <-ctx.Done():
			close(ch)
		}
	}()
	return ch
}

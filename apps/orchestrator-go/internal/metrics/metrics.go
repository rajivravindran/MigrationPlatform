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
)

func init() {
	prometheus.MustRegister(RowsProcessed, RowsFailed, EndpointLatency)
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

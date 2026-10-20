package config

import (
	"errors"
	"os"
	"strconv"
	"time"
)

type Config struct {
	DatabaseURL         string
	RedisURL            string
	TemporalHostPort    string
	TemporalNamespace   string
	OTLPEndpoint        string
	MetricsAddr         string
	SecretsMasterKey    string
	ActivityConcurrency int

	// Egress hardening. AllowPrivateDestinations must only be true in local
	// development (compose networks resolve to private IPs).
	AllowPrivateDestinations bool
	// Destination rate limit per host. DestinationRPS/DestinationBurst are
	// the deployment-wide budget as configured; the limiter in each worker
	// process enforces only its share (budget / DestinationRateWorkers),
	// because token buckets are per process and the deployment runs several
	// (architecture review A6). The aggregate stays within budget only while
	// at most DestinationRateWorkers orchestrator processes run; set it to
	// the maximum replica count. <= 0 RPS disables limiting.
	DestinationRPS         float64
	DestinationBurst       int
	DestinationRateWorkers int
	// Cap on stored/parsed response body bytes.
	MaxResponseBytes int64

	// Internal HTTP bridge that lets the Rust API drive Temporal.
	BridgeAddr     string
	BridgeToken    string
	APIInternalURL string

	// Job reconciler: how often it runs, and how long a non-terminal job must
	// be unchanged before its workflow is checked.
	ReconcileInterval   time.Duration
	ReconcileStaleAfter time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:       getenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/migration"),
		RedisURL:          getenv("REDIS_URL", "redis://localhost:6379/0"),
		TemporalHostPort:  getenv("TEMPORAL_ADDRESS", "localhost:7233"),
		TemporalNamespace: getenv("TEMPORAL_NAMESPACE", "default"),
		OTLPEndpoint:      os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		MetricsAddr:       getenv("METRICS_ADDR", ":9464"),
		SecretsMasterKey:  os.Getenv("MASTER_KEY"),

		AllowPrivateDestinations: getenv("ALLOW_PRIVATE_DESTINATIONS", "false") == "true",
	}
	if cfg.SecretsMasterKey == "" {
		return cfg, errors.New("MASTER_KEY is required for secret decryption")
	}
	concStr := getenv("ACTIVITY_CONCURRENCY", "64")
	c, err := strconv.Atoi(concStr)
	if err != nil || c <= 0 {
		return cfg, errors.New("ACTIVITY_CONCURRENCY must be a positive integer")
	}
	cfg.ActivityConcurrency = c

	rps, err := strconv.ParseFloat(getenv("DESTINATION_RPS", "50"), 64)
	if err != nil {
		return cfg, errors.New("DESTINATION_RPS must be a number")
	}
	cfg.DestinationRPS = rps
	burst, err := strconv.Atoi(getenv("DESTINATION_BURST", "100"))
	if err != nil || burst <= 0 {
		return cfg, errors.New("DESTINATION_BURST must be a positive integer")
	}
	cfg.DestinationBurst = burst
	workers, err := strconv.Atoi(getenv("DESTINATION_RATE_WORKERS", "1"))
	if err != nil || workers <= 0 {
		return cfg, errors.New("DESTINATION_RATE_WORKERS must be a positive integer (the maximum number of orchestrator replicas sharing DESTINATION_RPS)")
	}
	cfg.DestinationRateWorkers = workers
	maxResp, err := strconv.ParseInt(getenv("MAX_RESPONSE_BYTES", "1048576"), 10, 64)
	if err != nil || maxResp <= 0 {
		return cfg, errors.New("MAX_RESPONSE_BYTES must be a positive integer")
	}
	cfg.MaxResponseBytes = maxResp

	cfg.BridgeAddr = getenv("BRIDGE_ADDR", ":7070")
	cfg.BridgeToken = os.Getenv("BRIDGE_TOKEN")
	if cfg.BridgeToken == "" {
		return cfg, errors.New("BRIDGE_TOKEN is required (shared secret between API and orchestrator)")
	}
	cfg.APIInternalURL = getenv("API_INTERNAL_URL", "http://api:8080")

	if cfg.ReconcileInterval, err = time.ParseDuration(getenv("RECONCILE_INTERVAL", "1m")); err != nil || cfg.ReconcileInterval <= 0 {
		return cfg, errors.New("RECONCILE_INTERVAL must be a positive duration")
	}
	if cfg.ReconcileStaleAfter, err = time.ParseDuration(getenv("RECONCILE_STALE_AFTER", "5m")); err != nil || cfg.ReconcileStaleAfter <= 0 {
		return cfg, errors.New("RECONCILE_STALE_AFTER must be a positive duration")
	}
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// WorkerRateShare returns the per-process share of the destination budget:
// the configured RPS and burst divided across DestinationRateWorkers
// processes. The burst share is at least 1 so a single request can always
// proceed. RPS <= 0 stays disabled.
func (c Config) WorkerRateShare() (rps float64, burst int) {
	workers := c.DestinationRateWorkers
	if workers <= 0 {
		workers = 1
	}
	if c.DestinationRPS <= 0 {
		return c.DestinationRPS, c.DestinationBurst
	}
	rps = c.DestinationRPS / float64(workers)
	burst = (c.DestinationBurst + workers - 1) / workers
	if burst < 1 {
		burst = 1
	}
	return rps, burst
}

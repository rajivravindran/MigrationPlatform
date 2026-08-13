package config

import (
	"errors"
	"os"
	"strconv"
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
	// Per-destination-host rate limit; <= 0 disables limiting.
	DestinationRPS   float64
	DestinationBurst int
	// Cap on stored/parsed response body bytes.
	MaxResponseBytes int64

	// Internal HTTP bridge that lets the Rust API drive Temporal.
	BridgeAddr     string
	BridgeToken    string
	APIInternalURL string
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
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

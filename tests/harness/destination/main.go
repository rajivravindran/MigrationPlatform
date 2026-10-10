// Command destination is a deterministic HTTP destination fixture for the
// migration harness. It is test-only: it must never be deployed or pointed at
// real data.
//
// Every accepted write is appended to a durable receipt ledger (JSONL) so the
// harness can establish what the destination actually received independently
// of the platform's own counters. Fault rules are deterministic: they match on
// tenant/entity/business key and fire a fixed number of times.
package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := envOr("DEST_ADDR", ":8099")
	ledgerPath := envOr("DEST_LEDGER_PATH", "/data/receipts.jsonl")

	cfg := Config{
		Idempotency:       envOr("DEST_IDEMPOTENCY", "on") == "on",
		UniqueBusinessKey: envOr("DEST_UNIQUE_BUSINESS_KEY", "off") == "on",
		RequiredToken:     os.Getenv("DEST_REQUIRED_TOKEN"),
		IdempotencyHeader: envOr("DEST_IDEMPOTENCY_HEADER", "Idempotency-Key"),
	}

	srv, err := NewServer(ledgerPath, cfg)
	if err != nil {
		log.Fatalf("destination fixture: %v", err)
	}
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("destination fixture listening on %s (ledger %s, idempotency=%v)", addr, ledgerPath, cfg.Idempotency)
	log.Fatal(httpSrv.ListenAndServe())
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

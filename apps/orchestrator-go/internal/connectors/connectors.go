// Package connectors provides a common SourceConnector interface plus the
// implementations for every supported ingest type: CSV, JSON array, streaming
// XML, Salesforce (OAuth2 + Bulk API 2.0), and watched_prefix (MinIO/S3 glob).
package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/migration-platform/orchestrator/internal/security"
)

// Row is one ingested record plus its stable index (0-based). The index is
// critical for idempotent row writes and retry targeting.
type Row struct {
	Index int64
	Data  map[string]any
}

// Offset is the resume token the orchestrator persists between activity
// invocations. Items is always the number of rows already delivered, which is
// also the absolute index of the next row. Salesforce additionally pins the
// Bulk query job (the snapshot being read), the locator of the result page
// being consumed ("" is the first page) and how many rows of that page were
// already delivered.
type Offset struct {
	Bytes             int64  `json:"bytes,omitempty"`
	Items             int64  `json:"items,omitempty"`
	SalesforceJobID   string `json:"sf_job,omitempty"`
	SalesforceLocator string `json:"sf_locator,omitempty"`
	SalesforceSkip    int64  `json:"sf_skip,omitempty"`
	S3Continuation    string `json:"s3_cont,omitempty"`
}

func (o Offset) Marshal() ([]byte, error) { return json.Marshal(o) }
func UnmarshalOffset(b []byte) (Offset, error) {
	var o Offset
	if len(b) == 0 {
		return o, nil
	}
	err := json.Unmarshal(b, &o)
	return o, err
}

// SourceConnector streams rows, optionally resuming from an offset. Implementations
// MUST be safe to cancel via context. The caller is responsible for closing the
// iterator.
type SourceConnector interface {
	Kind() string
	Open(ctx context.Context, offset Offset) (RowIterator, error)
}

type RowIterator interface {
	Next(ctx context.Context) (Row, Offset, bool, error)
	io.Closer
}

// Common errors.
var (
	ErrUnsupportedKind = errors.New("unsupported connector kind")
	// ErrSourceNotReady means the source accepted the request but has no
	// rows yet (a Salesforce Bulk query still running). The offset returned
	// alongside it must be passed back on the next attempt.
	ErrSourceNotReady = errors.New("source not ready")
	// ErrInvalidOffset means a resume offset cannot belong to this source.
	// Resuming from it would skip or repeat rows, so callers must not retry.
	ErrInvalidOffset = errors.New("invalid resume offset")
)

// Factory builds a connector by kind with the config/secret from the DB.
type Factory struct {
	// InjectHTTPClient etc can be attached here.
}

type Config struct {
	Kind   string
	Config map[string]any
	Secret string
	// HTTP serves url sources and Salesforce calls; nil uses the
	// SSRF-guarded egress client.
	HTTP *http.Client
	// Version identifies the content being read (an object ETag) when the
	// source has one; resumed reads must see the same value.
	Version string
}

var defaultEgressClient = sync.OnceValue(func() *http.Client {
	return security.NewEgressHTTPClient(60*time.Second, os.Getenv("ALLOW_PRIVATE_DESTINATIONS") == "true")
})

func (c Config) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultEgressClient()
}

// Build dispatches on Config.Kind.
func (Factory) Build(cfg Config) (SourceConnector, error) {
	switch cfg.Kind {
	case "csv":
		return NewCSV(cfg)
	case "json":
		return NewJSON(cfg)
	case "xml":
		return NewXML(cfg)
	case "salesforce":
		return NewSalesforce(cfg)
	case "watched_prefix":
		return NewWatchedPrefix(cfg)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedKind, cfg.Kind)
	}
}

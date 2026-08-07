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
)

// Row is one ingested record plus its stable index (0-based). The index is
// critical for idempotent row writes and retry targeting.
type Row struct {
	Index int64
	Data  map[string]any
}

// Offset is an opaque resume token that the orchestrator persists between
// activity invocations. CSV/JSON pass a byte offset; XML passes an element
// count; Salesforce/S3 use the native cursor (jobId+locator / continuation).
type Offset struct {
	Bytes     int64  `json:"bytes,omitempty"`
	Items     int64  `json:"items,omitempty"`
	SalesforceLocator string `json:"sf_locator,omitempty"`
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
)

// Factory builds a connector by kind with the config/secret from the DB.
type Factory struct {
	// InjectHTTPClient etc can be attached here.
}

type Config struct {
	Kind   string
	Config map[string]any
	Secret string
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

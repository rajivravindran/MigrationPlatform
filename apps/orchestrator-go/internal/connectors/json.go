package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"io"
)

type jsonBuilder struct{ cfg Config }

func NewJSON(cfg Config) (SourceConnector, error) { return &jsonBuilder{cfg: cfg}, nil }

func (b *jsonBuilder) Kind() string { return "json" }

func (b *jsonBuilder) Open(ctx context.Context, offset Offset) (RowIterator, error) {
	src, err := openSource(ctx, b.cfg.Config)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(src)
	// Expect a top-level array; skip the opening bracket.
	tok, err := dec.Token()
	if err != nil {
		_ = src.Close()
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		_ = src.Close()
		return nil, errors.New("json source must be a top-level array")
	}
	it := &jsonIter{closer: src, dec: dec}
	for i := int64(0); i < offset.Items; i++ {
		var skip map[string]any
		if err := dec.Decode(&skip); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			_ = src.Close()
			return nil, err
		}
		it.idx++
	}
	return it, nil
}

type jsonIter struct {
	closer io.Closer
	dec    *json.Decoder
	idx    int64
}

func (it *jsonIter) Next(ctx context.Context) (Row, Offset, bool, error) {
	select {
	case <-ctx.Done():
		return Row{}, Offset{Items: it.idx}, false, ctx.Err()
	default:
	}
	if !it.dec.More() {
		return Row{}, Offset{Items: it.idx}, false, nil
	}
	var m map[string]any
	if err := it.dec.Decode(&m); err != nil {
		return Row{}, Offset{Items: it.idx}, false, err
	}
	row := Row{Index: it.idx, Data: m}
	it.idx++
	return row, Offset{Items: it.idx}, true, nil
}

func (it *jsonIter) Close() error { return it.closer.Close() }

package connectors

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"unicode"
)

type jsonBuilder struct{ cfg Config }

func NewJSON(cfg Config) (SourceConnector, error) { return &jsonBuilder{cfg: cfg}, nil }

func (b *jsonBuilder) Kind() string { return "json" }

func (b *jsonBuilder) Open(ctx context.Context, offset Offset) (RowIterator, error) {
	src, err := openSource(ctx, b.cfg)
	if err != nil {
		return nil, err
	}
	// Two layouts: a top-level array of records, or JSON Lines / NDJSON
	// (one record object after another). The first non-space byte decides.
	br := bufio.NewReader(src)
	first, err := firstNonSpace(br)
	if err != nil {
		_ = src.Close()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("json source is empty")
		}
		return nil, err
	}
	dec := json.NewDecoder(br)
	switch first {
	case '[':
		if _, err := dec.Token(); err != nil {
			_ = src.Close()
			return nil, err
		}
	case '{':
	default:
		_ = src.Close()
		return nil, errors.New("json source must be a top-level array of records or JSON Lines (one object per line)")
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

// firstNonSpace returns the first non-whitespace byte without consuming it,
// skipping a leading UTF-8 byte-order mark.
func firstNonSpace(br *bufio.Reader) (byte, error) {
	if bom, err := br.Peek(3); err == nil && bom[0] == 0xEF && bom[1] == 0xBB && bom[2] == 0xBF {
		_, _ = br.Discard(3)
	}
	for {
		b, err := br.Peek(1)
		if err != nil {
			return 0, err
		}
		if !unicode.IsSpace(rune(b[0])) {
			return b[0], nil
		}
		if _, err := br.ReadByte(); err != nil {
			return 0, err
		}
	}
}

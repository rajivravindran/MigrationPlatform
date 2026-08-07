package connectors

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"io"
)

// watchedPrefix streams rows out of every object under a bucket+prefix. For the
// initial implementation we only support CSV objects inside the prefix; each
// object is treated as a distinct stream and its rows flattened in order.
// Production deployments typically swap in the S3 SDK here.
type watchedPrefixBuilder struct{ cfg Config }

func NewWatchedPrefix(cfg Config) (SourceConnector, error) {
	return &watchedPrefixBuilder{cfg: cfg}, nil
}

func (b *watchedPrefixBuilder) Kind() string { return "watched_prefix" }

func (b *watchedPrefixBuilder) Open(ctx context.Context, offset Offset) (RowIterator, error) {
	// The Rust API is responsible for listing objects and passing them here as
	// cfg["objects"] - a []string of fully-qualified URLs. This keeps the
	// connector free of S3 SDK dependencies at the orchestrator layer.
	objsAny, ok := b.cfg.Config["objects"].([]any)
	if !ok {
		return nil, errors.New("watched_prefix requires cfg.objects as an array of URLs")
	}
	urls := make([]string, 0, len(objsAny))
	for _, v := range objsAny {
		if s, ok := v.(string); ok {
			urls = append(urls, s)
		}
	}
	return &watchedIter{urls: urls, offset: offset}, nil
}

type watchedIter struct {
	urls    []string
	offset  Offset
	current *csvObjIter
	cursor  int
	idx     int64
}

func (it *watchedIter) advance(ctx context.Context) error {
	if it.cursor >= len(it.urls) {
		return io.EOF
	}
	next := &csvObjIter{}
	if err := next.open(ctx, it.urls[it.cursor]); err != nil {
		return err
	}
	it.current = next
	it.cursor++
	return nil
}

func (it *watchedIter) Next(ctx context.Context) (Row, Offset, bool, error) {
	for {
		if it.current == nil {
			if err := it.advance(ctx); err != nil {
				if errors.Is(err, io.EOF) {
					return Row{}, Offset{Items: it.idx}, false, nil
				}
				return Row{}, Offset{Items: it.idx}, false, err
			}
		}
		rec, err := it.current.reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				_ = it.current.closer.Close()
				it.current = nil
				continue
			}
			return Row{}, Offset{Items: it.idx}, false, err
		}
		row := Row{Index: it.idx, Data: recordToMap(it.current.header, rec)}
		it.idx++
		return row, Offset{Items: it.idx}, true, nil
	}
}

func (it *watchedIter) Close() error {
	if it.current != nil {
		return it.current.closer.Close()
	}
	return nil
}

type csvObjIter struct {
	closer io.Closer
	reader *csv.Reader
	header []string
}

func (c *csvObjIter) open(ctx context.Context, url string) error {
	rc, err := openSource(ctx, map[string]any{"url": url})
	if err != nil {
		return err
	}
	c.closer = rc
	c.reader = csv.NewReader(bufio.NewReader(rc))
	c.reader.FieldsPerRecord = -1
	c.reader.ReuseRecord = true
	head, err := c.reader.Read()
	if err == nil {
		c.header = append([]string{}, head...)
	}
	return err
}

// parseCSVtoMaps is a shared helper used by the Salesforce connector too.
func parseCSVtoMaps(r io.Reader) ([]map[string]any, error) {
	csvR := csv.NewReader(bufio.NewReader(r))
	csvR.FieldsPerRecord = -1
	header, err := csvR.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]map[string]any, 0, 1024)
	for {
		rec, err := csvR.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
		out = append(out, recordToMap(header, rec))
	}
}

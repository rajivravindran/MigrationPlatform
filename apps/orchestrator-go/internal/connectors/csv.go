package connectors

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
)

func NewCSV(cfg Config) (SourceConnector, error) {
	return &csvBuilder{cfg: cfg}, nil
}

type csvBuilder struct {
	cfg Config
}

func (b *csvBuilder) Kind() string { return "csv" }

func (b *csvBuilder) Open(ctx context.Context, offset Offset) (RowIterator, error) {
	src, err := openSource(ctx, b.cfg.Config)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(src, 1<<20)
	delim := ','
	if s, ok := b.cfg.Config["delimiter"].(string); ok && len(s) == 1 {
		delim = rune(s[0])
	}
	r := csv.NewReader(br)
	r.Comma = delim
	r.FieldsPerRecord = -1
	r.ReuseRecord = true

	it := &csvIter{
		closer: src,
		reader: r,
		delim:  delim,
		bytes:  offset.Bytes,
	}
	if hasHeader(b.cfg.Config) {
		rec, err := r.Read()
		if err != nil {
			_ = src.Close()
			return nil, err
		}
		it.header = append(it.header, rec...)
	}
	// Skip forward to offset.Bytes is infeasible in csv.Reader once we've read;
	// we instead interpret offset.Items as a count of already-delivered rows.
	for i := int64(0); i < offset.Items; i++ {
		if _, err := r.Read(); err != nil {
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

type csvIter struct {
	closer io.Closer
	reader *csv.Reader
	header []string
	delim  rune
	idx    int64
	bytes  int64
}

func (it *csvIter) Next(ctx context.Context) (Row, Offset, bool, error) {
	select {
	case <-ctx.Done():
		return Row{}, Offset{Items: it.idx}, false, ctx.Err()
	default:
	}
	rec, err := it.reader.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Row{}, Offset{Items: it.idx}, false, nil
		}
		return Row{}, Offset{Items: it.idx}, false, err
	}
	row := Row{Index: it.idx, Data: recordToMap(it.header, rec)}
	it.idx++
	return row, Offset{Items: it.idx}, true, nil
}

func (it *csvIter) Close() error { return it.closer.Close() }

func recordToMap(header []string, rec []string) map[string]any {
	m := make(map[string]any, len(rec))
	for i, v := range rec {
		key := "col" + itoa(i)
		if i < len(header) {
			key = header[i]
		}
		m[key] = v
	}
	return m
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string('0'+rune(i%10)) + s
		i /= 10
	}
	return s
}

func hasHeader(cfg map[string]any) bool {
	if v, ok := cfg["header"].(bool); ok {
		return v
	}
	return true
}

func openSource(ctx context.Context, cfg map[string]any) (io.ReadCloser, error) {
	if bucket, ok := cfg["s3_bucket"].(string); ok && bucket != "" {
		if key, ok := cfg["s3_key"].(string); ok && key != "" {
			return openObjectStore(ctx, bucket, key)
		}
	}
	if url, ok := cfg["url"].(string); ok && strings.HasPrefix(url, "http") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			_ = resp.Body.Close()
			return nil, errors.New("source fetch failed: " + resp.Status)
		}
		return resp.Body, nil
	}
	if path, ok := cfg["path"].(string); ok {
		return os.Open(path)
	}
	return nil, errors.New("csv source missing path/url")
}

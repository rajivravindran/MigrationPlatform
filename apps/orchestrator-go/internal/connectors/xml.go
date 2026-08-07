package connectors

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
)

type xmlBuilder struct{ cfg Config }

func NewXML(cfg Config) (SourceConnector, error) { return &xmlBuilder{cfg: cfg}, nil }

func (b *xmlBuilder) Kind() string { return "xml" }

func (b *xmlBuilder) Open(ctx context.Context, offset Offset) (RowIterator, error) {
	src, err := openSource(ctx, b.cfg.Config)
	if err != nil {
		return nil, err
	}
	recordTag, _ := b.cfg.Config["recordTag"].(string)
	if recordTag == "" {
		recordTag = "record"
	}
	dec := xml.NewDecoder(src)
	it := &xmlIter{closer: src, dec: dec, tag: recordTag}
	for i := int64(0); i < offset.Items; i++ {
		_, _, _, err := it.Next(ctx)
		if err != nil {
			_ = src.Close()
			return nil, err
		}
	}
	return it, nil
}

type xmlIter struct {
	closer io.Closer
	dec    *xml.Decoder
	tag    string
	idx    int64
}

func (it *xmlIter) Next(ctx context.Context) (Row, Offset, bool, error) {
	for {
		select {
		case <-ctx.Done():
			return Row{}, Offset{Items: it.idx}, false, ctx.Err()
		default:
		}
		tok, err := it.dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return Row{}, Offset{Items: it.idx}, false, nil
			}
			return Row{}, Offset{Items: it.idx}, false, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != it.tag {
			continue
		}
		m, err := decodeElement(it.dec, start)
		if err != nil {
			return Row{}, Offset{Items: it.idx}, false, err
		}
		row := Row{Index: it.idx, Data: m}
		it.idx++
		return row, Offset{Items: it.idx}, true, nil
	}
}

func (it *xmlIter) Close() error { return it.closer.Close() }

// decodeElement walks one element and builds a nested map. Attribute values are
// stored under "@attr" keys; text content under "$text".
func decodeElement(dec *xml.Decoder, start xml.StartElement) (map[string]any, error) {
	out := make(map[string]any)
	for _, a := range start.Attr {
		out["@"+a.Name.Local] = a.Value
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return out, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			child, err := decodeElement(dec, t)
			if err != nil {
				return out, err
			}
			addMulti(out, t.Name.Local, simplify(child))
		case xml.CharData:
			text := string(t)
			if trim(text) != "" {
				out["$text"] = text
			}
		case xml.EndElement:
			return out, nil
		}
	}
}

func simplify(m map[string]any) any {
	if len(m) == 1 {
		if v, ok := m["$text"]; ok {
			return v
		}
	}
	return m
}

func addMulti(m map[string]any, k string, v any) {
	if existing, ok := m[k]; ok {
		if arr, isArr := existing.([]any); isArr {
			m[k] = append(arr, v)
			return
		}
		m[k] = []any{existing, v}
		return
	}
	m[k] = v
}

func trim(s string) string {
	start := 0
	end := len(s)
	for start < end {
		c := s[start]
		if c != ' ' && c != '\n' && c != '\t' && c != '\r' {
			break
		}
		start++
	}
	for end > start {
		c := s[end-1]
		if c != ' ' && c != '\n' && c != '\t' && c != '\r' {
			break
		}
		end--
	}
	return s[start:end]
}

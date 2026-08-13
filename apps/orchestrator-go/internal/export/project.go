// Package export projects job_rows into a results CSV for BA download.
package export

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/ohler55/ojg/jp"
	"github.com/ohler55/ojg/oj"

	"github.com/migration-platform/orchestrator/internal/template"
)

const (
	ColRowIndex = "row_index"
	ColStatus   = "status"
	ColError    = "error"
)

// Row is one persisted job_rows record plus optional per-step responses.
type Row struct {
	Index     int64
	Status    string
	Error     string
	Source    map[string]any
	Responses map[string]json.RawMessage
}

func Headers(cols []template.ExportColumn) []string {
	out := []string{ColRowIndex, ColStatus, ColError}
	for _, c := range cols {
		if c.Name == "" {
			continue
		}
		out = append(out, c.Name)
	}
	return out
}

func Project(row Row, cols []template.ExportColumn) []string {
	out := []string{
		strconv.FormatInt(row.Index, 10),
		row.Status,
		row.Error,
	}
	for _, c := range cols {
		if c.Name == "" {
			continue
		}
		out = append(out, cell(row, c))
	}
	return out
}

func cell(row Row, c template.ExportColumn) string {
	if c.From != "" {
		if row.Source == nil {
			return ""
		}
		return stringify(row.Source[c.From])
	}
	if c.Path == "" {
		return ""
	}
	step := c.FromResponse
	if step == "" {
		step = "main"
	}
	raw, ok := row.Responses[step]
	if !ok || len(raw) == 0 {
		return ""
	}
	doc, err := oj.Parse(raw)
	if err != nil {
		return ""
	}
	expr, err := jp.ParseString(c.Path)
	if err != nil {
		return ""
	}
	hits := expr.Get(doc)
	if len(hits) == 0 {
		return ""
	}
	if len(hits) == 1 {
		return stringify(hits[0])
	}
	return stringify(hits)
}

func stringify(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case json.RawMessage:
		return string(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

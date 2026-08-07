package template

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGoldenRoundtrip(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "packages", "rule-schema", "fixtures", "golden.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var tpl RuleTemplate
	if err := json.Unmarshal(raw, &tpl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tpl.ID != "rt_golden" {
		t.Fatalf("unexpected id %q", tpl.ID)
	}
	if tpl.Source.Type != "csv" || len(tpl.Preprocess) != 2 {
		t.Fatalf("unexpected template contents: %+v", tpl)
	}
	back, err := json.Marshal(tpl)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var original, roundtripped any
	_ = json.Unmarshal(raw, &original)
	_ = json.Unmarshal(back, &roundtripped)
	if !reflect.DeepEqual(normalize(original), normalize(roundtripped)) {
		t.Fatalf("golden round-trip diverged")
	}
}

func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v := range t {
			out[k] = normalize(v)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			out[i] = normalize(v)
		}
		return out
	default:
		return v
	}
}

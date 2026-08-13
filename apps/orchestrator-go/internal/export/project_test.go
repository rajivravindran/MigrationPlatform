package export

import (
	"encoding/json"
	"testing"

	"github.com/migration-platform/orchestrator/internal/template"
)

func TestProjectOpsColumnsOnly(t *testing.T) {
	got := Project(Row{Index: 3, Status: "failed", Error: "status 404: missing"}, nil)
	want := []string{"3", "failed", "status 404: missing"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("got %v want %v", got, want)
	}
	h := Headers(nil)
	if len(h) != 3 || h[0] != ColRowIndex {
		t.Fatalf("headers %v", h)
	}
}

func TestProjectSourceAndResponse(t *testing.T) {
	cols := []template.ExportColumn{
		{Name: "email", From: "email"},
		{Name: "family", FromResponse: "fetchPatient", Path: "$.name[0].family"},
		{Name: "id", Path: "$.id"},
	}
	row := Row{
		Index:  1,
		Status: "succeeded",
		Source: map[string]any{"email": "a@b.com", "country": "us"},
		Responses: map[string]json.RawMessage{
			"fetchPatient": json.RawMessage(`{"name":[{"family":"Smith"}]}`),
			"main":         json.RawMessage(`{"id":"c-9"}`),
		},
	}
	got := Project(row, cols)
	if got[3] != "a@b.com" || got[4] != "Smith" || got[5] != "c-9" {
		t.Fatalf("got %v", got)
	}
}

func TestProjectMissingPathIsEmpty(t *testing.T) {
	got := Project(Row{
		Index:     0,
		Status:    "succeeded",
		Responses: map[string]json.RawMessage{"main": json.RawMessage(`{"ok":true}`)},
	}, []template.ExportColumn{{Name: "id", Path: "$.missing"}})
	if got[3] != "" {
		t.Fatalf("expected empty cell, got %q", got[3])
	}
}

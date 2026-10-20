package connectors

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeJSON(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in.json")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestJSONArrayHappyPath(t *testing.T) {
	path := writeJSON(t, `[{"id":1,"name":"a"},{"id":2,"name":"b"}]`)
	c, _ := NewJSON(Config{Kind: "json", Config: map[string]any{"path": path}})
	it, err := c.Open(context.Background(), Offset{})
	require.NoError(t, err)
	defer it.Close()
	r1, _, ok, err := it.Next(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	require.EqualValues(t, 1, r1.Data["id"].(float64))
	r2, _, ok, err := it.Next(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "b", r2.Data["name"])
	_, _, ok, err = it.Next(context.Background())
	require.NoError(t, err)
	require.False(t, ok)
}

func TestJSONResumeFromOffset(t *testing.T) {
	path := writeJSON(t, `[{"i":0},{"i":1},{"i":2}]`)
	c, _ := NewJSON(Config{Kind: "json", Config: map[string]any{"path": path}})
	it, err := c.Open(context.Background(), Offset{Items: 2})
	require.NoError(t, err)
	defer it.Close()
	r, _, ok, err := it.Next(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	require.EqualValues(t, 2, r.Data["i"].(float64))
}

func TestJSONSingleObjectIsOneJSONLinesRecord(t *testing.T) {
	// A lone object is a one-line JSON Lines file, not an error.
	rows, err := readAllJSON(t, `{"not":"array"}`, 0)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"not": "array"}}, rows)
}

func readAllJSON(t *testing.T, body string, offset int64) ([]map[string]any, error) {
	t.Helper()
	c, _ := NewJSON(Config{Kind: "json", Config: map[string]any{"path": writeJSON(t, body)}})
	it, err := c.Open(context.Background(), Offset{Items: offset})
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var out []map[string]any
	for {
		r, _, ok, err := it.Next(context.Background())
		if err != nil {
			return out, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, r.Data)
	}
}

func TestJSONLinesAndBOM(t *testing.T) {
	rows, err := readAllJSON(t, "{\"i\":0}\n{\"i\":1}\n\n{\"i\":2}\n", 0)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	require.EqualValues(t, 2, rows[2]["i"])

	rows, err = readAllJSON(t, "{\"i\":0}\n{\"i\":1}\n{\"i\":2}", 2)
	require.NoError(t, err)
	require.Len(t, rows, 1, "JSON Lines resume from an offset like arrays do")

	rows, err = readAllJSON(t, "\xEF\xBB\xBF  [{\"i\":0}]", 0)
	require.NoError(t, err)
	require.Len(t, rows, 1, "a UTF-8 byte-order mark is skipped")
}

func TestJSONRejectsScalarsAndEmpty(t *testing.T) {
	_, err := readAllJSON(t, `"just a string"`, 0)
	require.ErrorContains(t, err, "JSON Lines")
	_, err = readAllJSON(t, "  \n", 0)
	require.ErrorContains(t, err, "empty")
}

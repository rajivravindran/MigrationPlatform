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

func TestJSONNonArrayFails(t *testing.T) {
	path := writeJSON(t, `{"not":"array"}`)
	c, _ := NewJSON(Config{Kind: "json", Config: map[string]any{"path": path}})
	_, err := c.Open(context.Background(), Offset{})
	require.Error(t, err)
}

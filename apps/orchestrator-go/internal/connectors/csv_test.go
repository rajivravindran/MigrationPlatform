package connectors

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "input.csv")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestCSVHeaderedHappyPath(t *testing.T) {
	path := writeTemp(t, "id,name,country\n1,alice,usa\n2,bob,canada\n")
	c, err := NewCSV(Config{Kind: "csv", Config: map[string]any{"path": path, "header": true}})
	require.NoError(t, err)
	it, err := c.Open(context.Background(), Offset{})
	require.NoError(t, err)
	defer it.Close()

	row, off, ok, err := it.Next(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(0), row.Index)
	require.Equal(t, "alice", row.Data["name"])
	require.EqualValues(t, 1, off.Items)

	row2, _, ok, err := it.Next(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "canada", row2.Data["country"])

	_, _, ok, err = it.Next(context.Background())
	require.NoError(t, err)
	require.False(t, ok)
}

func TestCSVResumesFromOffset(t *testing.T) {
	path := writeTemp(t, "id,name\n1,a\n2,b\n3,c\n")
	c, _ := NewCSV(Config{Kind: "csv", Config: map[string]any{"path": path, "header": true}})
	it, err := c.Open(context.Background(), Offset{Items: 2})
	require.NoError(t, err)
	defer it.Close()
	row, _, ok, err := it.Next(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "c", row.Data["name"])
}

func TestCSVCustomDelimiter(t *testing.T) {
	path := writeTemp(t, "id;amount\n1;100\n2;200\n")
	c, _ := NewCSV(Config{Kind: "csv", Config: map[string]any{"path": path, "header": true, "delimiter": ";"}})
	it, err := c.Open(context.Background(), Offset{})
	require.NoError(t, err)
	defer it.Close()
	row, _, ok, err := it.Next(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "100", row.Data["amount"])
}

func TestCSVHeaderless(t *testing.T) {
	path := writeTemp(t, "1,a\n2,b\n")
	c, _ := NewCSV(Config{Kind: "csv", Config: map[string]any{"path": path, "header": false}})
	it, err := c.Open(context.Background(), Offset{})
	require.NoError(t, err)
	defer it.Close()
	row, _, ok, err := it.Next(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "1", row.Data["col0"])
	require.Equal(t, "a", row.Data["col1"])
}

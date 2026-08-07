package connectors

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestXMLStreamRecords(t *testing.T) {
	p := filepath.Join(t.TempDir(), "in.xml")
	body := `<?xml version="1.0"?>
<records>
  <record id="1"><name>alice</name><country>usa</country></record>
  <record id="2"><name>bob</name><country>canada</country></record>
</records>`
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	c, _ := NewXML(Config{Kind: "xml", Config: map[string]any{"path": p, "recordTag": "record"}})
	it, err := c.Open(context.Background(), Offset{})
	require.NoError(t, err)
	defer it.Close()

	r1, _, ok, err := it.Next(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "1", r1.Data["@id"])
	require.Equal(t, "alice", r1.Data["name"])

	r2, _, ok, err := it.Next(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "canada", r2.Data["country"])

	_, _, ok, err = it.Next(context.Background())
	require.NoError(t, err)
	require.False(t, ok)
}

func TestXMLOffsetSkips(t *testing.T) {
	p := filepath.Join(t.TempDir(), "in.xml")
	body := `<root><record n="0"/><record n="1"/><record n="2"/></root>`
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	c, _ := NewXML(Config{Kind: "xml", Config: map[string]any{"path": p}})
	it, err := c.Open(context.Background(), Offset{Items: 2})
	require.NoError(t, err)
	defer it.Close()
	r, _, ok, err := it.Next(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "2", r.Data["@n"])
}

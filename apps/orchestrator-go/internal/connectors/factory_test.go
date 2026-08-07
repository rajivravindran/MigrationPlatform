package connectors

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFactoryDispatchesByKind(t *testing.T) {
	f := Factory{}
	cases := []string{"csv", "json", "xml", "salesforce", "watched_prefix"}
	for _, kind := range cases {
		c, err := f.Build(Config{Kind: kind})
		require.NoErrorf(t, err, "kind=%s", kind)
		require.Equal(t, kind, c.Kind())
	}
}

func TestFactoryRejectsUnknownKind(t *testing.T) {
	_, err := Factory{}.Build(Config{Kind: "bogus"})
	require.ErrorIs(t, err, ErrUnsupportedKind)
}

func TestOffsetMarshalRoundtrip(t *testing.T) {
	o := Offset{Items: 42, Bytes: 9000, SalesforceLocator: "loc-1"}
	b, err := o.Marshal()
	require.NoError(t, err)
	got, err := UnmarshalOffset(b)
	require.NoError(t, err)
	require.Equal(t, o, got)
}

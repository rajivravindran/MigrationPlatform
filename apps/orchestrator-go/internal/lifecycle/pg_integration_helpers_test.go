//go:build integration

package lifecycle

import "encoding/json"

// encoded is a minimal converter.EncodedValue over a Go value.
type encoded struct{ v any }

func (e encoded) HasValue() bool { return e.v != nil }

func (e encoded) Get(ptr interface{}) error {
	b, err := json.Marshal(e.v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, ptr)
}

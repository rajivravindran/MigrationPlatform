package template

import (
	"encoding/json"
	"fmt"

	"github.com/ohler55/ojg/jp"
	"github.com/ohler55/ojg/oj"
)

// Context carries everything a value expression may reference while rendering
// one row: the (preprocessed) row fields plus the parsed response body of
// every previously-executed step in the chain, keyed by step name.
type Context struct {
	Row       map[string]any
	Responses map[string]any
}

func NewContext(row map[string]any) *Context {
	return &Context{Row: row, Responses: map[string]any{}}
}

// AddResponse parses a step's raw response body and makes it addressable via
// {"$fromResponse": name, "path": "$..."}. Unparseable/empty bodies are stored
// as nil so later references fail with a clear "no match" error instead of a
// panic.
func (c *Context) AddResponse(name string, body []byte) {
	if len(body) == 0 {
		c.Responses[name] = nil
		return
	}
	parsed, err := oj.Parse(body)
	if err != nil {
		c.Responses[name] = nil
		return
	}
	c.Responses[name] = parsed
}

// AddResponseJSON is AddResponse for pre-parsed json.RawMessage (used when
// seeding a retry from persisted step outcomes).
func (c *Context) AddResponseJSON(name string, raw json.RawMessage) {
	c.AddResponse(name, []byte(raw))
}

// ResolveResponsePath evaluates a JSONPath against a named step response.
// A path that matches nothing is a hard error by design: silently emitting
// null would make chained requests non-deterministic and hide mapping bugs.
func (c *Context) ResolveResponsePath(step, path string) (any, error) {
	doc, ok := c.Responses[step]
	if !ok {
		return nil, fmt.Errorf("$fromResponse references step %q which has not executed", step)
	}
	expr, err := jp.ParseString(path)
	if err != nil {
		return nil, fmt.Errorf("invalid JSONPath %q: %w", path, err)
	}
	results := expr.Get(doc)
	if len(results) == 0 {
		return nil, fmt.Errorf("JSONPath %q matched nothing in response of step %q", path, step)
	}
	if len(results) == 1 {
		return results[0], nil
	}
	return results, nil
}

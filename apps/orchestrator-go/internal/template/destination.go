// Package template renderer: resolves per-row URL/header/payload templating
// for a Step against a Context (row fields + prior step responses).
package template

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Rendered is the per-row, resolved destination ready to be turned into an
// http.Request. URL is fully composed (path-substituted + query-encoded).
type Rendered struct {
	URL     string
	Headers map[string]string
}

var placeholderRe = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// RenderDestination resolves PathParams, QueryParams and Headers against the
// render context. It returns an error for any unresolved {placeholder} in the
// URL after substitution, for any path param whose value cannot be resolved,
// and for any $fromResponse expression whose JSONPath matches nothing.
func RenderDestination(d Destination, ctx *Context) (Rendered, error) {
	rendered := Rendered{Headers: map[string]string{}}

	rawURL := d.URL
	if len(d.PathParams) > 0 {
		// Iterate sorted for determinism in tests / idempotency keys.
		keys := make([]string, 0, len(d.PathParams))
		for k := range d.PathParams {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v, ok, err := resolveValueExpr(d.PathParams[k], ctx)
			if err != nil {
				return rendered, fmt.Errorf("pathParam %q: %w", k, err)
			}
			if !ok {
				return rendered, fmt.Errorf("pathParam %q has no value in row", k)
			}
			rawURL = strings.ReplaceAll(rawURL, "{"+k+"}", url.PathEscape(v))
		}
	}
	if leftover := placeholderRe.FindAllStringSubmatch(rawURL, -1); len(leftover) > 0 {
		names := make([]string, 0, len(leftover))
		for _, m := range leftover {
			names = append(names, m[1])
		}
		return rendered, fmt.Errorf("unresolved url placeholder(s): %s", strings.Join(names, ","))
	}

	if len(d.QueryParams) > 0 {
		u, err := url.Parse(rawURL)
		if err != nil {
			return rendered, fmt.Errorf("parse url for query append: %w", err)
		}
		q := u.Query()
		// Sort outer keys for determinism. For repeated values we preserve the
		// caller's slice ordering.
		keys := make([]string, 0, len(d.QueryParams))
		for k := range d.QueryParams {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch arr := d.QueryParams[k].(type) {
			case []any:
				for _, item := range arr {
					v, ok, err := resolveValueExpr(item, ctx)
					if err != nil {
						return rendered, fmt.Errorf("queryParam %q: %w", k, err)
					}
					if ok {
						q.Add(k, v)
					}
				}
			default:
				v, ok, err := resolveValueExpr(arr, ctx)
				if err != nil {
					return rendered, fmt.Errorf("queryParam %q: %w", k, err)
				}
				if ok {
					q.Set(k, v)
				}
			}
		}
		u.RawQuery = q.Encode()
		rawURL = u.String()
	}
	rendered.URL = rawURL

	for k, expr := range d.Headers {
		v, ok, err := resolveValueExpr(expr, ctx)
		if err != nil {
			return rendered, fmt.Errorf("header %q: %w", k, err)
		}
		if ok {
			rendered.Headers[k] = v
		}
	}

	return rendered, nil
}

// RenderPayload materializes a payload mapping template against the render
// context. $from pulls row fields, $fromResponse pulls JSONPath matches from
// prior step responses (hard error on no match), $py sigils are passed
// through untouched (they are evaluated by the Python transform tier).
func RenderPayload(tpl map[string]any, ctx *Context) (map[string]any, error) {
	if tpl == nil {
		return map[string]any{}, nil
	}
	out := make(map[string]any, len(tpl))
	for k, v := range tpl {
		rv, err := renderValue(v, ctx)
		if err != nil {
			return nil, fmt.Errorf("payload key %q: %w", k, err)
		}
		out[k] = rv
	}
	return out, nil
}

func renderValue(tpl any, ctx *Context) (any, error) {
	switch v := tpl.(type) {
	case map[string]any:
		if from, ok := v["$from"].(string); ok {
			if x, ok := ctx.Row[from]; ok {
				return x, nil
			}
			return nil, nil
		}
		if step, ok := v["$fromResponse"].(string); ok {
			path, _ := v["path"].(string)
			return ctx.ResolveResponsePath(step, path)
		}
		if lit, ok := v["$literal"]; ok {
			return lit, nil
		}
		if _, ok := v["$py"].(string); ok {
			// Python evaluation happens inside the transform-worker activity.
			// The pure-Go path passes the sigil through unchanged.
			return v, nil
		}
		out := make(map[string]any, len(v))
		for k, vv := range v {
			rv, err := renderValue(vv, ctx)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, vv := range v {
			rv, err := renderValue(vv, ctx)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	default:
		return v, nil
	}
}

// resolveValueExpr maps a schema "ValueExpr" (string | {$from} | {$literal} |
// {$fromResponse,path}) to a concrete string.
// Returns (value, true, nil) on success, ("", false, nil) when a $from
// reference resolves to a missing/nil row value, and a non-nil error for
// $fromResponse expressions that cannot be resolved (hard failure).
func resolveValueExpr(expr any, ctx *Context) (string, bool, error) {
	switch v := expr.(type) {
	case string:
		return v, true, nil
	case map[string]any:
		if from, ok := v["$from"].(string); ok {
			rv, present := ctx.Row[from]
			if !present || rv == nil {
				return "", false, nil
			}
			return coerceToString(rv), true, nil
		}
		if lit, ok := v["$literal"].(string); ok {
			return lit, true, nil
		}
		if step, ok := v["$fromResponse"].(string); ok {
			path, _ := v["path"].(string)
			rv, err := ctx.ResolveResponsePath(step, path)
			if err != nil {
				return "", false, err
			}
			if rv == nil {
				return "", false, fmt.Errorf("JSONPath %q resolved to null in response of step %q", path, step)
			}
			return coerceToString(rv), true, nil
		}
	}
	return "", false, nil
}

func coerceToString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	default:
		return fmt.Sprint(v)
	}
}

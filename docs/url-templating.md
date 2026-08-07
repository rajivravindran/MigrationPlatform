# URL templating

The destination URL, query string, and HTTP headers can all be templated
per-row using the same value-expression grammar as the JSON payload mapping.
This lets a single rule template fan out to row-specific endpoints without
writing custom Python.

> Path/query/header values are rendered **deterministically** in the workflow
> before the HTTP activity runs. The fully-rendered URL is included in the
> idempotency key, so retries always reach the same target.

## Grammar

A `valueExpr` is one of:

| Form                       | Meaning                                          |
| -------------------------- | ------------------------------------------------ |
| `"some-string"`            | Literal string used as-is.                       |
| `{ "$from": "fieldName" }` | Pulled from the (preprocessed) row field.        |
| `{ "$literal": "abc" }`    | Explicit literal — handy when a string would otherwise be ambiguous. |

`pathParams` and `headers` accept a single `valueExpr` per key.
`queryParams` additionally accepts an array of `valueExpr` to repeat the same
key (`?tag=a&tag=b`).

## Destination shape

```jsonc
{
  "destination": {
    "type": "http",
    "method": "GET",
    "url": "https://api.example.com/v1/users/{userId}/orders",
    "pathParams": {
      "userId": { "$from": "id" }
    },
    "queryParams": {
      "region": { "$from": "country" },
      "since":  { "$literal": "2024-01-01" },
      "tag":    [{ "$from": "tag1" }, { "$from": "tag2" }, "static"]
    },
    "headers": {
      "X-Tenant-Id": { "$from": "tenant" },
      "X-Trace":     "static-value"
    }
  }
}
```

For a row `{"id":"u1","country":"usa","tenant":"acme","tag1":"a","tag2":"b"}`
the orchestrator calls:

```
GET https://api.example.com/v1/users/u1/orders?region=usa&since=2024-01-01&tag=a&tag=b&tag=static
X-Tenant-Id: acme
X-Trace: static-value
```

## Rendering rules

- **Path params** use `url.PathEscape` (RFC 3986 path segment) — slashes are
  escaped. Use multiple placeholders if you need to span path segments.
- **Query params** use Go's `url.Values.Encode` (`application/x-www-form-urlencoded`).
- **Outer key order** for path/query is sorted to keep the rendered URL —
  and therefore the idempotency key — stable across runs.
- **Missing `$from`**:
  - In `pathParams` → row is **failed deterministically** (no retry,
    `last_error: pathParam "X" has no value in row`).
  - In `queryParams` and `headers` → that single value is **omitted**.
- **Unresolved `{placeholder}`** left in the URL after substitution → row is
  failed deterministically.
- **Coercion**: non-string row values are stringified with Go's `fmt.Sprint`
  (numbers and booleans render as you'd expect).

## Backwards compatibility

- Pre-existing templates with no `pathParams` / `queryParams` / `headers` keep
  working unchanged.
- The previous `headers: {string -> string}` shape is still valid: a bare
  string is a valid `valueExpr`.

## UI

The Designer (`/templates/{id}`) gains three buttons next to **+ Payload key**:

- **+ Path param** — creates a node labelled `{name}`. Drag a source field
  onto it to bind.
- **+ Query param** — creates a node labelled `?name`. Multiple incoming edges
  produce repeated query keys.
- **+ Header** — creates a node for the header name. Bind a source field for
  per-row values, or leave unbound to keep an existing literal value from the
  template JSON.

The live JSON preview on the right reflects the assembled `destination`
object.

## Schema reference

The canonical JSON Schema is at
`packages/rule-schema/schema/rule-template.schema.json`; the matching Zod
types live in `packages/rule-schema/src/index.ts`. The Go renderer is
`apps/orchestrator-go/internal/template/destination.go` and is exhaustively
unit-tested in `destination_test.go`.

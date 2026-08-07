package template

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenderDestinationLiteralURL(t *testing.T) {
	d := Destination{
		Type: "http", Method: "POST",
		URL: "https://api.example.com/v1/users",
	}
	got, err := RenderDestination(d, NewContext(map[string]any{}))
	require.NoError(t, err)
	require.Equal(t, "https://api.example.com/v1/users", got.URL)
	require.Empty(t, got.Headers)
}

func TestRenderDestinationPathAndQuery(t *testing.T) {
	d := Destination{
		Type: "http", Method: "GET",
		URL: "https://api.example.com/users/{userId}/orders",
		PathParams: map[string]any{
			"userId": map[string]any{"$from": "id"},
		},
		QueryParams: map[string]any{
			"region": map[string]any{"$from": "country"},
			"since":  map[string]any{"$literal": "2024-01-01"},
		},
		Headers: map[string]any{
			"X-Tenant-Id": map[string]any{"$from": "tenant"},
			"X-Trace":     "static",
		},
	}
	got, err := RenderDestination(d, NewContext(map[string]any{
		"id": "abc 42", "country": "usa", "tenant": "acme",
	}))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got.URL, "https://api.example.com/users/abc%2042/orders?"),
		"unexpected url: %s", got.URL)
	require.Contains(t, got.URL, "region=usa")
	require.Contains(t, got.URL, "since=2024-01-01")
	require.Equal(t, "acme", got.Headers["X-Tenant-Id"])
	require.Equal(t, "static", got.Headers["X-Trace"])
}

func TestRenderDestinationRepeatedQueryKeys(t *testing.T) {
	d := Destination{
		Type: "http", Method: "GET",
		URL: "https://api.example.com/things",
		QueryParams: map[string]any{
			"tag": []any{
				map[string]any{"$from": "t1"},
				map[string]any{"$from": "t2"},
				"static",
			},
		},
	}
	got, err := RenderDestination(d, NewContext(map[string]any{"t1": "a", "t2": "b"}))
	require.NoError(t, err)
	require.Equal(t, "https://api.example.com/things?tag=a&tag=b&tag=static", got.URL)
}

func TestRenderDestinationCoercesNonStringFromValues(t *testing.T) {
	d := Destination{
		Type: "http", Method: "GET",
		URL: "https://api.example.com/users/{id}",
		PathParams: map[string]any{
			"id": map[string]any{"$from": "id"},
		},
	}
	got, err := RenderDestination(d, NewContext(map[string]any{"id": 42}))
	require.NoError(t, err)
	require.Equal(t, "https://api.example.com/users/42", got.URL)
}

func TestRenderDestinationFailsOnMissingPathParam(t *testing.T) {
	d := Destination{
		Type: "http", Method: "GET",
		URL: "https://api.example.com/users/{userId}",
		PathParams: map[string]any{
			"userId": map[string]any{"$from": "id"},
		},
	}
	_, err := RenderDestination(d, NewContext(map[string]any{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "userId")
}

func TestRenderDestinationFailsOnUnresolvedPlaceholder(t *testing.T) {
	d := Destination{
		Type: "http", Method: "GET",
		URL: "https://api.example.com/users/{userId}/orders/{orderId}",
		PathParams: map[string]any{
			"userId": map[string]any{"$from": "id"},
		},
	}
	_, err := RenderDestination(d, NewContext(map[string]any{"id": "x"}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "orderId")
}

func TestRenderDestinationOmitsMissingQueryParam(t *testing.T) {
	d := Destination{
		Type: "http", Method: "GET",
		URL: "https://api.example.com/things",
		QueryParams: map[string]any{
			"region": map[string]any{"$from": "missing"},
			"since":  map[string]any{"$literal": "yesterday"},
		},
	}
	got, err := RenderDestination(d, NewContext(map[string]any{}))
	require.NoError(t, err)
	require.Contains(t, got.URL, "since=yesterday")
	require.NotContains(t, got.URL, "region=")
}

func TestRenderDestinationFromResponsePathParam(t *testing.T) {
	ctx := NewContext(map[string]any{"email": "a@b.com"})
	ctx.AddResponse("createContact", []byte(`{"data":{"id":"c-123","tags":["x","y"]}}`))

	d := Destination{
		Type: "http", Method: "POST",
		URL: "https://api.example.com/contacts/{contactId}/addresses",
		PathParams: map[string]any{
			"contactId": map[string]any{"$fromResponse": "createContact", "path": "$.data.id"},
		},
	}
	got, err := RenderDestination(d, ctx)
	require.NoError(t, err)
	require.Equal(t, "https://api.example.com/contacts/c-123/addresses", got.URL)
}

func TestRenderDestinationFromResponseNoMatchIsHardError(t *testing.T) {
	ctx := NewContext(map[string]any{})
	ctx.AddResponse("createContact", []byte(`{"data":{}}`))

	d := Destination{
		Type: "http", Method: "POST",
		URL: "https://api.example.com/contacts/{contactId}",
		PathParams: map[string]any{
			"contactId": map[string]any{"$fromResponse": "createContact", "path": "$.data.id"},
		},
	}
	_, err := RenderDestination(d, ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "matched nothing")
}

func TestRenderDestinationFromResponseUnknownStep(t *testing.T) {
	ctx := NewContext(map[string]any{})
	d := Destination{
		Type: "http", Method: "POST",
		URL: "https://api.example.com/x",
		Headers: map[string]any{
			"X-Ref": map[string]any{"$fromResponse": "nope", "path": "$.id"},
		},
	}
	_, err := RenderDestination(d, ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "has not executed")
}

func TestRenderPayloadFromRowAndResponse(t *testing.T) {
	ctx := NewContext(map[string]any{"street": "1 Main St", "city": "Springfield"})
	ctx.AddResponse("createContact", []byte(`{"data":{"id":"c-9"}}`))

	payload, err := RenderPayload(map[string]any{
		"street":     map[string]any{"$from": "street"},
		"city":       map[string]any{"$from": "city"},
		"contactRef": map[string]any{"$fromResponse": "createContact", "path": "$.data.id"},
		"nested": map[string]any{
			"static": "v",
			"ref":    map[string]any{"$fromResponse": "createContact", "path": "$.data.id"},
		},
	}, ctx)
	require.NoError(t, err)
	require.Equal(t, "1 Main St", payload["street"])
	require.Equal(t, "c-9", payload["contactRef"])
	require.Equal(t, "c-9", payload["nested"].(map[string]any)["ref"])
}

func TestRenderPayloadFromResponseNoMatchFails(t *testing.T) {
	ctx := NewContext(map[string]any{})
	ctx.AddResponse("s1", []byte(`{"a":1}`))
	_, err := RenderPayload(map[string]any{
		"x": map[string]any{"$fromResponse": "s1", "path": "$.missing.deep"},
	}, ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "matched nothing")
}

func TestRenderPayloadPassesPyThrough(t *testing.T) {
	ctx := NewContext(map[string]any{})
	payload, err := RenderPayload(map[string]any{
		"expr": map[string]any{"$py": "row['a'][:2]"},
	}, ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"$py": "row['a'][:2]"}, payload["expr"])
}

func TestContextResolveResponsePathMultiMatch(t *testing.T) {
	ctx := NewContext(map[string]any{})
	ctx.AddResponse("list", []byte(`{"items":[{"id":1},{"id":2}]}`))
	v, err := ctx.ResolveResponsePath("list", "$.items[*].id")
	require.NoError(t, err)
	require.Len(t, v, 2)
}

func TestRenderDestinationDeterministicOrder(t *testing.T) {
	d := Destination{
		Type: "http", Method: "GET",
		URL: "https://api.example.com/x",
		QueryParams: map[string]any{
			"a": "1", "b": "2", "c": "3",
		},
	}
	ctx := NewContext(map[string]any{})
	first, err := RenderDestination(d, ctx)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		again, err := RenderDestination(d, ctx)
		require.NoError(t, err)
		require.Equal(t, first.URL, again.URL, "url must be deterministic for stable idempotency keys")
	}
}

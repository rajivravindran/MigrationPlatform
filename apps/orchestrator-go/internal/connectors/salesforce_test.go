package connectors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func fakeSalesforce(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		require.Equal(t, "stored-secret", r.PostForm.Get("client_secret"))
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
	})
	mux.HandleFunc("POST /services/data/v58.0/jobs/query", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "J1"})
	})
	mux.HandleFunc("GET /services/data/v58.0/jobs/query/J1", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"state": "JobComplete"})
	})
	mux.HandleFunc("GET /services/data/v58.0/jobs/query/J1/results", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Sforce-Locator", "null")
		_, _ = w.Write([]byte("Id,Name\n001,Acme\n002,Globex\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func sfConfig(srv *httptest.Server, client *http.Client) Config {
	return Config{
		Kind:   "salesforce",
		Secret: "stored-secret",
		HTTP:   client,
		Config: map[string]any{
			"soql":        "SELECT Id, Name FROM Account",
			"tokenUrl":    srv.URL + "/token",
			"instanceUrl": srv.URL,
			"clientId":    "cid",
		},
	}
}

func TestSalesforceUsesInjectedClient(t *testing.T) {
	srv := fakeSalesforce(t)
	conn, err := NewSalesforce(sfConfig(srv, srv.Client()))
	require.NoError(t, err)
	it, err := conn.Open(context.Background(), Offset{})
	require.NoError(t, err)
	defer it.Close()

	var names []any
	for {
		row, _, more, err := it.Next(context.Background())
		require.NoError(t, err)
		if !more {
			break
		}
		names = append(names, row.Data["Name"])
	}
	require.Equal(t, []any{"Acme", "Globex"}, names)
}

func TestSalesforceDefaultClientRefusesPrivateAddresses(t *testing.T) {
	t.Setenv("ALLOW_PRIVATE_DESTINATIONS", "")
	srv := fakeSalesforce(t) // listens on 127.0.0.1
	conn, err := NewSalesforce(sfConfig(srv, nil))
	require.NoError(t, err)
	_, err = conn.Open(context.Background(), Offset{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked")
}

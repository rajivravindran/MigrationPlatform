package security

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIPBlocked(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "::1", "fc00::1", "fe80::1",
	}
	for _, s := range blocked {
		require.True(t, IPBlocked(net.ParseIP(s)), "%s must be blocked", s)
	}
	allowed := []string{"8.8.8.8", "93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"}
	for _, s := range allowed {
		require.False(t, IPBlocked(net.ParseIP(s)), "%s must be allowed", s)
	}
	require.True(t, IPBlocked(nil), "unparseable IPs must be blocked")
}

func TestEgressClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	strict := NewEgressHTTPClient(5*time.Second, false)
	_, err := strict.Get(srv.URL)
	require.Error(t, err, "loopback must be rejected when private destinations are disallowed")
	require.Contains(t, err.Error(), "blocked")

	permissive := NewEgressHTTPClient(5*time.Second, true)
	resp, err := permissive.Get(srv.URL)
	require.NoError(t, err)
	resp.Body.Close()
}

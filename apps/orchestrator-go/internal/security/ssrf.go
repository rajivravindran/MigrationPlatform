// Package security contains network-egress hardening for the orchestrator.
//
// Destination URLs are user-supplied, so every outbound request must be
// prevented from reaching internal infrastructure (SSRF). The check runs at
// dial time on the *resolved* IP, which also defeats DNS-rebinding tricks
// where a hostname resolves to a public IP during validation and a private
// one moments later.
package security

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// blockedNets covers loopback, RFC1918, link-local, CGNAT, unique-local IPv6
// and the metadata endpoint ranges used by cloud providers.
var blockedNets = mustParseCIDRs(
	"127.0.0.0/8",
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16", // link-local incl. cloud metadata 169.254.169.254
	"100.64.0.0/10",  // CGNAT
	"0.0.0.0/8",
	"::1/128",
	"fc00::/7",  // unique-local
	"fe80::/10", // link-local
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(fmt.Sprintf("bad builtin CIDR %s: %v", c, err))
		}
		out = append(out, n)
	}
	return out
}

// IPBlocked reports whether the address falls inside a blocked range.
func IPBlocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// NewEgressHTTPClient builds an http.Client whose dialer rejects connections
// to private/internal address space unless allowPrivate is set (needed for
// local development where mock endpoints run on the compose network).
func NewEgressHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("egress dial %q: %w", address, err)
			}
			ip := net.ParseIP(host)
			if IPBlocked(ip) {
				return fmt.Errorf("destination %s is in a blocked (private/internal) address range", host)
			}
			return nil
		}
	}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		// Re-validate on redirect: a permitted host could 302 to an internal
		// address; the dialer control covers that too, but cap the hop count.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
}

var _ = context.Background

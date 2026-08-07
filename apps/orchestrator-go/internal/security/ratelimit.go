package security

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

// HostLimiter enforces a per-destination-host request rate so one large job
// cannot hammer a target API. Hosts are limited independently; the limiter
// map is unbounded in theory but in practice holds one entry per distinct
// destination host, which is small.
type HostLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	rps      rate.Limit
	burst    int
}

// NewHostLimiter creates a limiter allowing `rps` requests per second with
// the given burst per destination host. rps <= 0 disables limiting.
func NewHostLimiter(rps float64, burst int) *HostLimiter {
	return &HostLimiter{
		limiters: map[string]*rate.Limiter{},
		rps:      rate.Limit(rps),
		burst:    burst,
	}
}

// Wait blocks until a token is available for the host (or ctx is done).
func (h *HostLimiter) Wait(ctx context.Context, host string) error {
	if h == nil || h.rps <= 0 {
		return nil
	}
	h.mu.Lock()
	lim, ok := h.limiters[host]
	if !ok {
		lim = rate.NewLimiter(h.rps, h.burst)
		h.limiters[host] = lim
	}
	h.mu.Unlock()
	return lim.Wait(ctx)
}

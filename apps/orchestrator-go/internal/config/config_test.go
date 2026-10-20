package config

import "testing"

func TestWorkerRateShare(t *testing.T) {
	cases := []struct {
		name           string
		rps            float64
		burst, workers int
		wantRPS        float64
		wantBurst      int
	}{
		{"single worker keeps the budget", 50, 100, 1, 50, 100},
		{"budget split across replicas", 50, 100, 4, 12.5, 25},
		{"burst rounds up and never drops below one", 50, 3, 40, 1.25, 1},
		{"unset workers behaves as one", 50, 100, 0, 50, 100},
		{"disabled limiter stays disabled", 0, 100, 40, 0, 100},
	}
	for _, c := range cases {
		cfg := Config{DestinationRPS: c.rps, DestinationBurst: c.burst, DestinationRateWorkers: c.workers}
		rps, burst := cfg.WorkerRateShare()
		if rps != c.wantRPS || burst != c.wantBurst {
			t.Errorf("%s: got rps=%v burst=%d, want rps=%v burst=%d", c.name, rps, burst, c.wantRPS, c.wantBurst)
		}
	}
}

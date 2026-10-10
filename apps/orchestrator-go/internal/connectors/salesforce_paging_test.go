package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// bulkFixture is a Bulk API 2.0 stand-in holding a fixed result set. Locators
// are opaque to the client; here they encode the start position of a page.
type bulkFixture struct {
	mu          sync.Mutex
	records     int
	pollsBefore int // job-state polls answered InProgress before JobComplete
	jobsCreated int
	statePolls  int
	pageFetches int
	maxRecords  []int
}

func (f *bulkFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
	})
	mux.HandleFunc("POST /services/data/v58.0/jobs/query", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.jobsCreated++
		id := fmt.Sprintf("J%d", f.jobsCreated)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	})
	mux.HandleFunc("GET /services/data/v58.0/jobs/query/{id}", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.statePolls++
		state := "JobComplete"
		if f.statePolls <= f.pollsBefore {
			state = "InProgress"
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"state": state})
	})
	mux.HandleFunc("GET /services/data/v58.0/jobs/query/{id}/results", func(w http.ResponseWriter, r *http.Request) {
		limit, err := strconv.Atoi(r.URL.Query().Get("maxRecords"))
		require.NoError(t, err)
		start := 0
		if loc := r.URL.Query().Get("locator"); loc != "" {
			start, err = strconv.Atoi(strings.TrimPrefix(loc, "L"))
			require.NoError(t, err)
		}
		f.mu.Lock()
		f.pageFetches++
		f.maxRecords = append(f.maxRecords, limit)
		f.mu.Unlock()
		end := min(start+limit, f.records)
		if end < f.records {
			w.Header().Set("Sforce-Locator", fmt.Sprintf("L%d", end))
		} else {
			w.Header().Set("Sforce-Locator", "null")
		}
		var b strings.Builder
		b.WriteString("Id,Name\n")
		for i := start; i < end; i++ {
			fmt.Fprintf(&b, "A%05d,Account %d\n", i, i)
		}
		_, _ = w.Write([]byte(b.String()))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// ingestBatch mirrors one Ingest activity call: a fresh connector opened at
// the persisted offset, at most batchSize rows, offset round-tripped as JSON.
func ingestBatch(t *testing.T, cfg Config, raw []byte, batchSize int) ([]Row, []byte, bool, error) {
	t.Helper()
	off, err := UnmarshalOffset(raw)
	require.NoError(t, err)
	conn, err := NewSalesforce(cfg)
	require.NoError(t, err)
	it, err := conn.Open(context.Background(), off)
	if err != nil {
		return nil, raw, false, err
	}
	defer it.Close()
	var rows []Row
	for len(rows) < batchSize {
		row, next, more, err := it.Next(context.Background())
		off = next
		if err != nil {
			b, _ := off.Marshal()
			return rows, b, false, err
		}
		if !more {
			b, _ := off.Marshal()
			return rows, b, true, nil
		}
		rows = append(rows, row)
	}
	b, _ := off.Marshal()
	return rows, b, false, nil
}

func TestSalesforceResumesAcrossBatchesWithoutLossOrDuplicates(t *testing.T) {
	const records, batchSize = 4501, 500
	fx := &bulkFixture{records: records, pollsBefore: 2}
	srv := fx.server(t)
	cfg := sfConfig(srv, srv.Client())

	var (
		raw      []byte
		all      []Row
		notReady int
		calls    int
	)
	for done := false; !done; {
		calls++
		require.Less(t, calls, 100, "ingest loop did not terminate")
		rows, next, d, err := ingestBatch(t, cfg, raw, batchSize)
		if errors.Is(err, ErrSourceNotReady) {
			notReady++
			raw = next
			continue
		}
		require.NoError(t, err)
		all = append(all, rows...)
		raw, done = next, d
	}

	require.Equal(t, 1, fx.jobsCreated, "the Bulk query must be created once and reused")
	require.Equal(t, 2, notReady, "InProgress polls surface as not-ready, not as rows")
	require.Len(t, all, records)
	for i, r := range all {
		require.EqualValues(t, i, r.Index, "row index must be absolute")
		require.Equal(t, fmt.Sprintf("A%05d", i), r.Data["Id"])
	}
	for _, n := range fx.maxRecords {
		require.Equal(t, 2000, n)
	}
}

func TestSalesforceResumesMidPage(t *testing.T) {
	fx := &bulkFixture{records: 4501}
	srv := fx.server(t)
	cfg := sfConfig(srv, srv.Client())

	raw, err := Offset{Items: 2250, SalesforceJobID: "J9", SalesforceLocator: "L2000", SalesforceSkip: 250}.Marshal()
	require.NoError(t, err)
	rows, _, _, err := ingestBatch(t, cfg, raw, 10)
	require.NoError(t, err)
	require.Equal(t, 0, fx.jobsCreated, "a resumed read must not start a new query")
	require.Len(t, rows, 10)
	require.EqualValues(t, 2250, rows[0].Index)
	require.Equal(t, "A02250", rows[0].Data["Id"])
}

func TestSalesforceRefusesOffsetsItCannotResume(t *testing.T) {
	fx := &bulkFixture{records: 10}
	srv := fx.server(t)
	cfg := sfConfig(srv, srv.Client())

	legacy, _ := Offset{Items: 500, SalesforceLocator: "L2000"}.Marshal()
	_, _, _, err := ingestBatch(t, cfg, legacy, 10)
	require.ErrorIs(t, err, ErrInvalidOffset)
	require.Equal(t, 0, fx.jobsCreated)

	pastPage, _ := Offset{Items: 50, SalesforceJobID: "J1", SalesforceSkip: 50}.Marshal()
	_, _, _, err = ingestBatch(t, cfg, pastPage, 10)
	require.ErrorIs(t, err, ErrInvalidOffset)
}

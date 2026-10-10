package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func newTestServer(t *testing.T, cfg Config) (*Server, *httptest.Server, string) {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), "receipts.jsonl")
	s, err := NewServer(ledger, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts, ledger
}

func post(t *testing.T, client *http.Client, url, tenant, idem string, body map[string]any) (*http.Response, error) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if tenant != "" {
		req.Header.Set("X-Tenant", tenant)
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	return client.Do(req)
}

func putJSON(t *testing.T, url string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(b))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT %s: %d", url, resp.StatusCode)
	}
}

func receipts(t *testing.T, base string) []Receipt {
	t.Helper()
	resp, err := http.Get(base + "/_admin/receipts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []Receipt
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCreateWritesDurableReceipt(t *testing.T) {
	_, ts, ledger := newTestServer(t, Config{Idempotency: true})
	resp, err := post(t, http.DefaultClient, ts.URL+"/v1/customers", "org-a", "k1", map[string]any{"external_id": "C-1"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}

	reloaded, err := NewServer(ledger, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.receipts) != 1 || reloaded.receipts[0].ExternalID != "C-1" || reloaded.receipts[0].Tenant != "org-a" {
		t.Fatalf("ledger not durable: %+v", reloaded.receipts)
	}
}

func TestIdempotencyOnReplaysAndOffDuplicates(t *testing.T) {
	_, ts, _ := newTestServer(t, Config{Idempotency: true})
	for i := 0; i < 2; i++ {
		resp, _ := post(t, http.DefaultClient, ts.URL+"/v1/customers", "org-a", "k1", map[string]any{"external_id": "C-1"})
		resp.Body.Close()
	}
	if n := len(receipts(t, ts.URL)); n != 1 {
		t.Fatalf("idempotency on: want 1 receipt, got %d", n)
	}

	_, ts2, _ := newTestServer(t, Config{Idempotency: false})
	for i := 0; i < 2; i++ {
		resp, _ := post(t, http.DefaultClient, ts2.URL+"/v1/customers", "org-a", "k1", map[string]any{"external_id": "C-1"})
		resp.Body.Close()
	}
	if n := len(receipts(t, ts2.URL)); n != 2 {
		t.Fatalf("idempotency off: want 2 receipts, got %d", n)
	}
}

func TestIdempotencyKeyScopedToTenantAndPayload(t *testing.T) {
	_, ts, _ := newTestServer(t, Config{Idempotency: true})
	r1, _ := post(t, http.DefaultClient, ts.URL+"/v1/customers", "org-a", "k1", map[string]any{"external_id": "C-1"})
	r1.Body.Close()
	r2, _ := post(t, http.DefaultClient, ts.URL+"/v1/customers", "org-b", "k1", map[string]any{"external_id": "C-1"})
	r2.Body.Close()
	if r2.StatusCode != http.StatusCreated {
		t.Fatalf("other tenant must not replay: %d", r2.StatusCode)
	}
	r3, _ := post(t, http.DefaultClient, ts.URL+"/v1/customers", "org-a", "k1", map[string]any{"external_id": "C-2"})
	r3.Body.Close()
	if r3.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("reused key with different payload: want 422, got %d", r3.StatusCode)
	}
}

func TestRejectAndThrottleDoNotWrite(t *testing.T) {
	_, ts, _ := newTestServer(t, Config{Idempotency: true})
	putJSON(t, ts.URL+"/_admin/faults", []FaultRule{
		{ID: "rej", ExternalID: "C-1", Action: ActionReject, Status: 400},
		{ID: "thr", ExternalID: "C-2", Action: ActionThrottle, Times: 1},
	})
	r1, _ := post(t, http.DefaultClient, ts.URL+"/v1/customers", "org-a", "", map[string]any{"external_id": "C-1"})
	r1.Body.Close()
	r2, _ := post(t, http.DefaultClient, ts.URL+"/v1/customers", "org-a", "", map[string]any{"external_id": "C-2"})
	r2.Body.Close()
	if r1.StatusCode != 400 || r2.StatusCode != 429 || r2.Header.Get("Retry-After") == "" {
		t.Fatalf("got %d / %d", r1.StatusCode, r2.StatusCode)
	}
	if n := len(receipts(t, ts.URL)); n != 0 {
		t.Fatalf("faults must not write, got %d receipts", n)
	}
	r3, _ := post(t, http.DefaultClient, ts.URL+"/v1/customers", "org-a", "", map[string]any{"external_id": "C-2"})
	r3.Body.Close()
	if r3.StatusCode != http.StatusCreated {
		t.Fatalf("throttle times=1 should allow the second attempt, got %d", r3.StatusCode)
	}
}

func TestCommitThenDisconnectWritesButClientSeesError(t *testing.T) {
	_, ts, _ := newTestServer(t, Config{Idempotency: true})
	putJSON(t, ts.URL+"/_admin/faults", []FaultRule{{ID: "ctd", ExternalID: "C-1", Action: ActionCommitThenDisconnect, Times: 1}})

	_, err := post(t, http.DefaultClient, ts.URL+"/v1/customers", "org-a", "k1", map[string]any{"external_id": "C-1"})
	if err == nil {
		t.Fatal("client must observe a transport error")
	}
	if n := len(receipts(t, ts.URL)); n != 1 {
		t.Fatalf("write must be committed, got %d receipts", n)
	}
	// The retry with the same key is replayed: still one effect.
	resp, err := post(t, http.DefaultClient, ts.URL+"/v1/customers", "org-a", "k1", map[string]any{"external_id": "C-1"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d", resp.StatusCode)
	}
	if n := len(receipts(t, ts.URL)); n != 1 {
		t.Fatalf("replay must not write, got %d receipts", n)
	}
}

func TestTimeoutBeforeWriteDoesNotWrite(t *testing.T) {
	_, ts, _ := newTestServer(t, Config{})
	putJSON(t, ts.URL+"/_admin/faults", []FaultRule{{ID: "to", Action: ActionTimeoutBeforeWrite, DelayMs: 5000, Times: 1}})
	client := &http.Client{Timeout: 200 * time.Millisecond}
	if _, err := post(t, client, ts.URL+"/v1/customers", "org-a", "", map[string]any{"external_id": "C-1"}); err == nil {
		t.Fatal("expected client timeout")
	}
	if n := len(receipts(t, ts.URL)); n != 0 {
		t.Fatalf("timeout before write must not write, got %d", n)
	}
}

func TestUniqueBusinessKeyConflictAndLookup(t *testing.T) {
	_, ts, _ := newTestServer(t, Config{UniqueBusinessKey: true})
	r1, _ := post(t, http.DefaultClient, ts.URL+"/v1/customers", "org-a", "", map[string]any{"external_id": "C-1"})
	r1.Body.Close()
	r2, _ := post(t, http.DefaultClient, ts.URL+"/v1/customers", "org-a", "", map[string]any{"external_id": "C-1"})
	r2.Body.Close()
	if r2.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", r2.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/customers?external_id=C-1", nil)
	req.Header.Set("X-Tenant", "org-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Data) != 1 {
		t.Fatalf("lookup: %+v", body)
	}
}

func TestRequiredTokenAndMissingExternalID(t *testing.T) {
	_, ts, _ := newTestServer(t, Config{RequiredToken: "s3cret"})
	r1, _ := post(t, http.DefaultClient, ts.URL+"/v1/customers", "org-a", "", map[string]any{"external_id": "C-1"})
	r1.Body.Close()
	if r1.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", r1.StatusCode)
	}
	_, ts2, _ := newTestServer(t, Config{})
	r2, _ := post(t, http.DefaultClient, ts2.URL+"/v1/customers", "org-a", "", map[string]any{"name": "x"})
	r2.Body.Close()
	if r2.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", r2.StatusCode)
	}
}

func TestResetTruncatesLedger(t *testing.T) {
	_, ts, ledger := newTestServer(t, Config{})
	r1, _ := post(t, http.DefaultClient, ts.URL+"/v1/customers", "org-a", "", map[string]any{"external_id": "C-1"})
	r1.Body.Close()
	resp, err := http.Post(ts.URL+"/_admin/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n := len(receipts(t, ts.URL)); n != 0 {
		t.Fatalf("reset left %d receipts", n)
	}
	reloaded, _ := NewServer(ledger, Config{})
	if len(reloaded.receipts) != 0 {
		t.Fatal("ledger file not truncated")
	}
}

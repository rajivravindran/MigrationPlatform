package activities

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/migration-platform/orchestrator/internal/template"
)

// memLedger mirrors pgOperationLedger's transitions in memory.
type memLedger struct {
	mu        sync.Mutex
	ops       map[[3]int64]*opSnapshot
	finished  []finishInput
	finishErr error
}

func newMemLedger() *memLedger { return &memLedger{ops: map[[3]int64]*opSnapshot{}} }

func opKey(op operationRef) [3]int64 { return [3]int64{op.JobID, op.RowIndex, int64(op.StepIndex)} }

func (l *memLedger) begin(_ context.Context, in beginInput) (beginResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur, ok := l.ops[opKey(in.Op)]
	if !ok {
		cur = &opSnapshot{State: opPending, PayloadSHA256: in.Intent.PayloadSHA256}
		l.ops[opKey(in.Op)] = cur
	}
	snap := *cur
	snap.Exists = cur.Attempts > 0 || cur.State != opPending
	snap.Now = time.Now()
	res := beginResult{Decision: decideAttempt(snap, in.Intent), Snapshot: snap}
	switch res.Decision.Kind {
	case decideBusy, decideReplay:
	case decideUnresolved:
		cur.State, cur.LeaseAttempt = opUnresolved, ""
	default:
		cur.State, cur.LeaseAttempt, cur.LeaseUntil = opSending, in.AttemptID, snap.Now.Add(operationLease)
		cur.Attempts++
		cur.PayloadSHA256 = in.Intent.PayloadSHA256
		if cur.FirstSentAt.IsZero() {
			cur.FirstSentAt = snap.Now
		}
		if in.Intent.Generation != "" && (in.Intent.Resend || in.Intent.ConfirmNotApplied) {
			cur.DecisionGeneration = in.Intent.Generation
		}
	}
	return res, nil
}

func (l *memLedger) finish(_ context.Context, in finishInput) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.finishErr != nil {
		return l.finishErr
	}
	cur := l.ops[opKey(in.Op)]
	if cur == nil || cur.LeaseAttempt != in.AttemptID {
		return errLeaseLost
	}
	cur.State, cur.LeaseAttempt = in.State, ""
	if in.State == opSucceeded {
		cur.ReceiptStatus, cur.Receipt = in.HTTPStatus, in.Receipt
	}
	l.finished = append(l.finished, in)
	return nil
}

func (l *memLedger) state(t *testing.T) string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, op := range l.ops {
		return op.State
	}
	t.Fatal("no operation recorded")
	return ""
}

type memSecrets map[string]string

func (m memSecrets) destinationSecret(_ context.Context, _ int64, name string) (string, error) {
	if v, ok := m[name]; ok {
		return v, nil
	}
	return "", secretUnavailable("secret %q is not available to this job's organization", name)
}

type endpointFixture struct {
	t      *testing.T
	ledger *memLedger
	a      *Activities
	hits   atomic.Int32
	srv    *httptest.Server
}

func newEndpointFixture(t *testing.T, handler http.HandlerFunc) *endpointFixture {
	f := &endpointFixture{t: t, ledger: newMemLedger()}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/lookup") {
			f.hits.Add(1)
		}
		handler(w, r)
	}))
	t.Cleanup(f.srv.Close)
	f.a = &Activities{
		d:       Deps{HTTP: &http.Client{Timeout: 5 * time.Second}},
		ledger:  f.ledger,
		secrets: memSecrets{"crm_token": "s3cr3t-value", "basic_creds": "svc:pw"},
	}
	return f
}

func (f *endpointFixture) input(method string) CallEndpointInput {
	return CallEndpointInput{
		JobID: 7, RowIndex: 3, StepName: "main", Method: method, URL: f.srv.URL + "/contacts",
		Payload: map[string]any{"email": "a@example.test"}, HasBody: true, IdempotencyKey: "k1",
	}
}

func (f *endpointFixture) call(in CallEndpointInput) (CallEndpointOutput, error) {
	f.t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
	env.RegisterActivity(f.a)
	val, err := env.ExecuteActivity(f.a.CallEndpoint, in)
	var out CallEndpointOutput
	if err == nil {
		if gerr := val.Get(&out); gerr != nil {
			f.t.Fatalf("decode output: %v", gerr)
		}
	}
	return out, err
}

func errType(err error) string {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		return appErr.Type()
	}
	return ""
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, `{"code":%d}`, code)
	}
}

func TestCallEndpointSuccessIsReplayedNotResent(t *testing.T) {
	f := newEndpointFixture(t, status(201))
	out, err := f.call(f.input("POST"))
	if err != nil || !out.Succeeded || out.Outcome != CallSucceeded || out.Replayed {
		t.Fatalf("first call: out=%+v err=%v", out, err)
	}
	out, err = f.call(f.input("POST"))
	if err != nil || !out.Succeeded || !out.Replayed || out.Status != 201 {
		t.Fatalf("second call should replay the receipt: out=%+v err=%v", out, err)
	}
	if f.hits.Load() != 1 {
		t.Fatalf("destination received %d requests, want 1", f.hits.Load())
	}

	resend := f.input("POST")
	resend.Resend = true
	if _, err := f.call(resend); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 2 {
		t.Fatalf("an explicit resend must send; hits=%d", f.hits.Load())
	}
}

func TestCallEndpointRefusalIsFailedWithoutRetry(t *testing.T) {
	f := newEndpointFixture(t, status(422))
	out, err := f.call(f.input("POST"))
	if err != nil || out.Succeeded || out.Outcome != CallFailed || out.Status != 422 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if got := f.ledger.state(t); got != opFailed {
		t.Fatalf("operation state %s, want failed", got)
	}
	if _, err := f.call(f.input("POST")); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 2 {
		t.Fatalf("a refused operation may be sent again after a fix; hits=%d", f.hits.Load())
	}
}

func TestCallEndpointUnknownOutcomeOnCreateIsUnresolved(t *testing.T) {
	f := newEndpointFixture(t, status(503))
	out, err := f.call(f.input("POST"))
	if err != nil || out.Outcome != CallUnresolved || !strings.Contains(out.Error, "not re-sent") {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	out, err = f.call(f.input("POST"))
	if err != nil || out.Outcome != CallUnresolved {
		t.Fatalf("retry without confirmation: out=%+v err=%v", out, err)
	}
	if f.hits.Load() != 1 {
		t.Fatalf("an unresolved create must not be re-sent; hits=%d", f.hits.Load())
	}

	confirmed := f.input("POST")
	confirmed.ConfirmNotApplied = true
	if _, err := f.call(confirmed); err != nil && errType(err) != "" {
		t.Fatalf("confirmed retry: %v", err)
	}
	if f.hits.Load() != 2 {
		t.Fatalf("confirm_not_applied must send; hits=%d", f.hits.Load())
	}
}

func TestCallEndpointUnknownOutcomeWithIdempotencyReSends(t *testing.T) {
	var n atomic.Int32
	f := newEndpointFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != "k1" {
			t.Errorf("idempotency header = %q", r.Header.Get("Idempotency-Key"))
		}
		if n.Add(1) == 1 {
			status(502)(w, r)
			return
		}
		status(200)(w, r)
	})
	in := f.input("POST")
	in.DeliveryMode, in.IdempotencyHeader = template.DeliveryIdempotent, "Idempotency-Key"
	_, err := f.call(in)
	if errType(err) != ErrTypeOutcomeUnknown {
		t.Fatalf("want retryable %s, got %v", ErrTypeOutcomeUnknown, err)
	}
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) && appErr.NonRetryable() {
		t.Fatal("OutcomeUnknown must be retryable")
	}
	out, err := f.call(in)
	if err != nil || !out.Succeeded {
		t.Fatalf("re-send: out=%+v err=%v", out, err)
	}
}

func TestCallEndpointLookupFindsAppliedEffect(t *testing.T) {
	f := newEndpointFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/lookup") {
			if r.URL.Query().Get("email") != "a@example.test" || r.Header.Get("Authorization") != "Bearer s3cr3t-value" {
				t.Errorf("lookup request %s auth=%q", r.URL, r.Header.Get("Authorization"))
			}
			_, _ = io.WriteString(w, `{"items":[{"id":"c-1"}]}`)
			return
		}
		status(504)(w, r)
	})
	in := f.input("POST")
	in.DeliveryMode = template.DeliveryLookup
	in.Auth = &DestinationAuth{Type: "bearer", SecretRef: "crm_token"}
	in.Lookup = &RenderedLookup{URL: f.srv.URL + "/lookup?email=a@example.test", FoundPath: "$.items[0].id"}
	if _, err := f.call(in); errType(err) != ErrTypeOutcomeUnknown {
		t.Fatalf("first attempt: %v", err)
	}
	out, err := f.call(in)
	if err != nil || !out.Succeeded || !out.Replayed || !strings.Contains(string(out.Body), "c-1") {
		t.Fatalf("lookup should supply the receipt: out=%+v err=%v", out, err)
	}
	if f.hits.Load() != 1 {
		t.Fatalf("a found effect must not be re-sent; hits=%d", f.hits.Load())
	}
}

func TestCallEndpointLookupModeRequiresLookup(t *testing.T) {
	f := newEndpointFixture(t, status(200))
	in := f.input("POST")
	in.DeliveryMode = template.DeliveryLookup
	if _, err := f.call(in); errType(err) != "InvalidDelivery" {
		t.Fatalf("want InvalidDelivery, got %v", err)
	}
	if f.hits.Load() != 0 {
		t.Fatal("nothing may be sent")
	}
}

func TestCallEndpointTransportFailureClassification(t *testing.T) {
	t.Run("connection dropped after the request was written is unknown", func(t *testing.T) {
		f := newEndpointFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		})
		out, err := f.call(f.input("POST"))
		if err != nil || out.Outcome != CallUnresolved {
			t.Fatalf("out=%+v err=%v", out, err)
		}
	})
	t.Run("the HTTP client never re-sends on a reused connection", func(t *testing.T) {
		for _, tc := range []struct {
			name, method string
			hasBody      bool
		}{{"create with body", "POST", true}, {"bodyless call with an idempotency header", "DELETE", false}} {
			var drops atomic.Int32
			f := newEndpointFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/warm" {
					status(200)(w, r)
					return
				}
				drops.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			})
			warm := f.input("POST")
			warm.URL, warm.RowIndex = f.srv.URL+"/warm", 90
			if _, err := f.call(warm); err != nil {
				t.Fatal(err)
			}
			in := f.input(tc.method)
			in.RowIndex, in.HasBody = 91, tc.hasBody
			if !tc.hasBody {
				in.Payload = nil
			}
			in.DeliveryMode, in.IdempotencyHeader = template.DeliveryManual, "Idempotency-Key"
			out, err := f.call(in)
			if err != nil || out.Outcome != CallUnresolved {
				t.Fatalf("%s: out=%+v err=%v", tc.name, out, err)
			}
			if drops.Load() != 1 {
				t.Fatalf("%s: destination received the request %d times; only the ledger may re-send", tc.name, drops.Load())
			}
		}
	})
	t.Run("connection refused is not sent and retryable", func(t *testing.T) {
		f := newEndpointFixture(t, status(200))
		in := f.input("POST")
		f.srv.Close()
		_, err := f.call(in)
		if err == nil || errType(err) == ErrTypeOutcomeUnknown {
			t.Fatalf("want a plain retryable error, got %v", err)
		}
		if got := f.ledger.state(t); got != opPending {
			t.Fatalf("operation state %s, want pending", got)
		}
	})
	t.Run("429 is not sent", func(t *testing.T) {
		f := newEndpointFixture(t, status(429))
		if _, err := f.call(f.input("POST")); err == nil {
			t.Fatal("429 must be retried")
		}
		if got := f.ledger.state(t); got != opPending {
			t.Fatalf("operation state %s, want pending", got)
		}
	})
}

func TestCallEndpointBusyWhileAnotherAttemptSends(t *testing.T) {
	f := newEndpointFixture(t, status(200))
	f.ledger.ops[[3]int64{7, 3, 0}] = &opSnapshot{State: opSending, PayloadSHA256: payloadDigest(f.input("POST").Payload, true),
		Attempts: 1, LeaseAttempt: "other", LeaseUntil: time.Now().Add(time.Minute)}
	_, err := f.call(f.input("POST"))
	if errType(err) != ErrTypeOperationBusy {
		t.Fatalf("want %s, got %v", ErrTypeOperationBusy, err)
	}
	if f.hits.Load() != 0 {
		t.Fatal("nothing may be sent while another attempt holds the lease")
	}
}

// Principal review F1: one from-start retry is one generation. Its send is
// consumed on first delivery; a redelivered activity with the same generation
// replays the receipt, and only a genuinely new operator decision sends again.
func TestCallEndpointResendIsConsumedPerGeneration(t *testing.T) {
	f := newEndpointFixture(t, status(201))
	if _, err := f.call(f.input("POST")); err != nil {
		t.Fatal(err)
	}
	resend := f.input("POST")
	resend.Resend = true
	resend.DecisionGeneration = "retry-run-1"
	out, err := f.call(resend)
	if err != nil || out.Replayed || f.hits.Load() != 2 {
		t.Fatalf("the authorized resend must send once: out=%+v err=%v hits=%d", out, err, f.hits.Load())
	}
	// Lost activity acknowledgment: Temporal redelivers the same input.
	out, err = f.call(resend)
	if err != nil || !out.Replayed || !out.Succeeded || f.hits.Load() != 2 {
		t.Fatalf("redelivery of the same generation must replay, not send: out=%+v err=%v hits=%d", out, err, f.hits.Load())
	}
	// A new operator decision is a new generation and may send again.
	resend.DecisionGeneration = "retry-run-2"
	if _, err := f.call(resend); err != nil || f.hits.Load() != 3 {
		t.Fatalf("a new generation must send: err=%v hits=%d", err, f.hits.Load())
	}
}

func TestCallEndpointConfirmNotAppliedIsConsumedPerGeneration(t *testing.T) {
	f := newEndpointFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprint(w, `{"err":"upstream"}`)
	})
	// POST without a policy: a 502 is an unknown outcome and the operation
	// becomes unresolved.
	if out, err := f.call(f.input("POST")); err != nil || out.Outcome != CallUnresolved {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	confirm := f.input("POST")
	confirm.ConfirmNotApplied = true
	confirm.DecisionGeneration = "retry-run-1"
	if out, err := f.call(confirm); err != nil || out.Outcome != CallUnresolved || f.hits.Load() != 2 {
		t.Fatalf("the confirmed retry sends once and is unresolved again on 502: out=%+v err=%v hits=%d", out, err, f.hits.Load())
	}
	// Redelivery of the same decision must not treat the new ambiguity as confirmed.
	out, err := f.call(confirm)
	if err != nil || out.Outcome != CallUnresolved || f.hits.Load() != 2 {
		t.Fatalf("a spent confirmation must not authorize another send: out=%+v err=%v hits=%d", out, err, f.hits.Load())
	}
	if !strings.Contains(out.Error, "already used") {
		t.Fatalf("the reason must say the confirmation was spent: %q", out.Error)
	}
}

// Principal review F6: a receipt that cannot be saved after the destination
// accepted the request is an unknown outcome, never an ordinary failure.
func TestCallEndpointReceiptWriteFailureIsAnUnknownOutcome(t *testing.T) {
	f := newEndpointFixture(t, status(200))
	f.ledger.finishErr = errors.New("db down")
	out, err := f.call(f.input("POST"))
	if err == nil || out.Succeeded {
		t.Fatalf("an unrecorded success must fail the activity: out=%+v err=%v", out, err)
	}
	if errType(err) != ErrTypeOutcomeUnknown {
		t.Fatalf("error type %q, want %q so the row is unresolved rather than failed", errType(err), ErrTypeOutcomeUnknown)
	}
}

func TestCallEndpointReceiptWriteFailureStopsTheStep(t *testing.T) {
	f := newEndpointFixture(t, status(200))
	f.ledger.finishErr = errors.New("db down")
	f.a.d.HTTP.Timeout = 0
	out, err := f.call(f.input("POST"))
	if err == nil || out.Succeeded {
		t.Fatalf("an unrecorded success must fail the activity: out=%+v err=%v", out, err)
	}
	f.ledger.finishErr = nil
	out, err = f.call(f.input("POST"))
	if errType(err) != ErrTypeOperationBusy || f.hits.Load() != 1 {
		t.Fatalf("the next attempt must not re-send while the lease is live: out=%+v err=%v hits=%d", out, err, f.hits.Load())
	}
}

func TestCallEndpointAuth(t *testing.T) {
	var got http.Header
	f := newEndpointFixture(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		status(200)(w, r)
	})
	cases := []struct {
		auth   *DestinationAuth
		header string
		want   string
	}{
		{&DestinationAuth{Type: "bearer", SecretRef: "crm_token"}, "Authorization", "Bearer s3cr3t-value"},
		{&DestinationAuth{Type: "basic", SecretRef: "basic_creds"}, "Authorization", "Basic c3ZjOnB3"},
		{&DestinationAuth{Type: "api_key", SecretRef: "crm_token"}, "X-API-Key", "s3cr3t-value"},
		{&DestinationAuth{Type: "api_key", SecretRef: "crm_token", HeaderName: "X-Custom"}, "X-Custom", "s3cr3t-value"},
	}
	for i, tc := range cases {
		in := f.input("PUT")
		in.RowIndex = int64(100 + i)
		in.Auth = tc.auth
		out, err := f.call(in)
		if err != nil || !out.Succeeded {
			t.Fatalf("%+v: out=%+v err=%v", tc.auth, out, err)
		}
		if got.Get(tc.header) != tc.want {
			t.Errorf("%+v: %s = %q, want %q", tc.auth, tc.header, got.Get(tc.header), tc.want)
		}
	}

	for _, auth := range []*DestinationAuth{
		{Type: "bearer", SecretRef: "other_org_token"},
		{Type: "bearer"},
		{Type: "oauth2_client", SecretRef: "crm_token"},
		{Type: "basic", SecretRef: "crm_token"},
	} {
		in := f.input("POST")
		in.RowIndex = 500
		in.Auth = auth
		before := f.hits.Load()
		out, err := f.call(in)
		if errType(err) != ErrTypeSecretUnavailable {
			t.Fatalf("%+v: want %s, got %v", auth, ErrTypeSecretUnavailable, err)
		}
		var appErr *temporal.ApplicationError
		if !errors.As(err, &appErr) || !appErr.NonRetryable() {
			t.Fatalf("%+v: must be non-retryable", auth)
		}
		if f.hits.Load() != before {
			t.Fatalf("%+v: nothing may be sent without credentials", auth)
		}
		if strings.Contains(err.Error()+out.Error, "s3cr3t-value") {
			t.Fatalf("%+v: secret value leaked into the error", auth)
		}
	}
}

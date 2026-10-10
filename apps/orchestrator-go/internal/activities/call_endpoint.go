package activities

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ohler55/ojg/jp"
	"github.com/ohler55/ojg/oj"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/migration-platform/orchestrator/internal/metrics"
	"github.com/migration-platform/orchestrator/internal/security"
	"github.com/migration-platform/orchestrator/internal/template"
)

// CallEndpointInput carries the per-row, fully-rendered HTTP request. URL,
// Headers and IdempotencyKey are computed by the workflow (see
// template.RenderDestination). Credentials are never part of it: Auth names
// a secret that the activity resolves for the job's organization.
type CallEndpointInput struct {
	JobID             int64             `json:"jobId"`
	RowIndex          int64             `json:"rowIndex"`
	StepIndex         int               `json:"stepIndex,omitempty"`
	StepName          string            `json:"stepName,omitempty"`
	Method            string            `json:"method"`
	URL               string            `json:"url"`
	Headers           map[string]string `json:"headers,omitempty"`
	Payload           map[string]any    `json:"payload"`
	HasBody           bool              `json:"hasBody,omitempty"`
	IdempotencyKey    string            `json:"idempotencyKey,omitempty"`
	IdempotencyHeader string            `json:"idempotencyHeader,omitempty"`
	RetryAttempts     int               `json:"retryAttempts,omitempty"`

	// DeliveryMode is the destination's policy for unknown outcomes; empty
	// uses the default for Method (ADR 0003).
	DeliveryMode   string           `json:"deliveryMode,omitempty"`
	RetentionHours int              `json:"retentionHours,omitempty"`
	Lookup         *RenderedLookup  `json:"lookup,omitempty"`
	Auth           *DestinationAuth `json:"auth,omitempty"`
	// Resend re-applies an operation that already succeeded (from-start
	// retry); ConfirmNotApplied lets an unresolved operation be sent.
	Resend            bool `json:"resend,omitempty"`
	ConfirmNotApplied bool `json:"confirmNotApplied,omitempty"`
}

// DestinationAuth references an organization secret; it never holds one.
type DestinationAuth struct {
	Type       string `json:"type"`
	SecretRef  string `json:"secretRef,omitempty"`
	HeaderName string `json:"headerName,omitempty"`
}

// RenderedLookup is the per-row request that finds an applied effect.
type RenderedLookup struct {
	Method    string            `json:"method,omitempty"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	FoundPath string            `json:"foundPath"`
}

// Call outcomes reported to the workflow.
const (
	CallSucceeded  = "succeeded"
	CallFailed     = "failed"
	CallUnresolved = "unresolved"
)

type CallEndpointOutput struct {
	Status    int             `json:"status"`
	Body      json.RawMessage `json:"body"`
	Succeeded bool            `json:"succeeded"`
	Error     string          `json:"error,omitempty"`
	LatencyMs int64           `json:"latencyMs"`
	// Outcome is CallSucceeded, CallFailed or CallUnresolved. Replayed means
	// the receipt came from the ledger or a lookup and nothing was sent.
	Outcome  string `json:"outcome,omitempty"`
	Replayed bool   `json:"replayed,omitempty"`
}

func (a *Activities) CallEndpoint(ctx context.Context, in CallEndpointInput) (CallEndpointOutput, error) {
	started := time.Now()
	mode := in.DeliveryMode
	if mode == "" {
		mode = template.Destination{Method: in.Method}.EffectiveDeliveryMode()
	}
	retention := in.RetentionHours
	if retention <= 0 {
		retention = template.Delivery{}.RetentionHours()
	}
	if mode == template.DeliveryLookup && in.Lookup == nil {
		return CallEndpointOutput{Error: "delivery mode lookup requires delivery.lookup"},
			temporal.NewNonRetryableApplicationError("delivery mode lookup requires delivery.lookup", "InvalidDelivery", nil)
	}

	authHeaders, err := a.resolveAuth(ctx, in)
	if err != nil {
		return CallEndpointOutput{Error: err.Error()}, err
	}

	info := activity.GetInfo(ctx)
	op := operationRef{JobID: in.JobID, RowIndex: in.RowIndex, StepIndex: in.StepIndex, StepName: in.StepName}
	attemptID := fmt.Sprintf("%s:%s:%d", info.WorkflowExecution.RunID, info.ActivityID, info.Attempt)
	begun, err := a.ledger.begin(ctx, beginInput{
		Op: op, AttemptID: attemptID, OperationKey: in.IdempotencyKey,
		Intent: attemptIntent{
			PayloadSHA256: payloadDigest(in.Payload, in.HasBody), Mode: mode, RetentionHours: retention,
			Resend: in.Resend, ConfirmNotApplied: in.ConfirmNotApplied,
		},
	})
	if err != nil {
		return CallEndpointOutput{Error: "open operation attempt: " + err.Error()}, fmt.Errorf("open operation attempt: %w", err)
	}
	finish := func(f finishInput) error {
		f.Op, f.AttemptID = op, attemptID
		return a.finishWithRetry(ctx, f)
	}

	switch begun.Decision.Kind {
	case decideBusy:
		return CallEndpointOutput{Error: begun.Decision.Reason},
			temporal.NewApplicationError(begun.Decision.Reason, ErrTypeOperationBusy)
	case decideReplay:
		return CallEndpointOutput{Status: begun.Snapshot.ReceiptStatus, Body: safeJSON(begun.Snapshot.Receipt),
			Succeeded: true, Outcome: CallSucceeded, Replayed: true}, nil
	case decideUnresolved:
		activity.GetLogger(ctx).Warn("operation unresolved", "jobId", in.JobID, "rowIndex", in.RowIndex,
			"step", in.StepName, "reason", begun.Decision.Reason)
		return CallEndpointOutput{Outcome: CallUnresolved, Error: begun.Decision.Reason}, nil
	case decideLookupThenSend:
		found, status, body, err := a.lookupEffect(ctx, in.Lookup, authHeaders)
		if err != nil {
			msg := "lookup before re-send failed: " + err.Error()
			if ferr := finish(finishInput{Outcome: outcomeUnknown, State: opUnknown, Error: msg}); ferr != nil {
				return CallEndpointOutput{Error: msg}, ferr
			}
			return CallEndpointOutput{Error: msg}, temporal.NewApplicationError(msg, ErrTypeOutcomeUnknown)
		}
		if found {
			if err := finish(finishInput{Outcome: outcomeFoundByLookup, State: opSucceeded, HTTPStatus: status, Receipt: body}); err != nil {
				return CallEndpointOutput{Error: err.Error()}, err
			}
			return CallEndpointOutput{Status: status, Body: body, Succeeded: true, Outcome: CallSucceeded, Replayed: true,
				LatencyMs: time.Since(started).Milliseconds()}, nil
		}
	}

	res := a.send(ctx, in, authHeaders)
	out := CallEndpointOutput{Status: res.status, Body: res.body, LatencyMs: time.Since(started).Milliseconds()}
	switch {
	case res.err != nil && !res.wrote, res.err == nil && res.status == http.StatusTooManyRequests:
		out.Error = res.describe()
		if err := finish(finishInput{Outcome: outcomeNotSent, State: opPending, HTTPStatus: res.status, Error: out.Error}); err != nil {
			return out, err
		}
		return out, errors.New(out.Error)
	case res.err == nil && res.status >= 200 && res.status < 300:
		out.Succeeded, out.Outcome = true, CallSucceeded
		if err := finish(finishInput{Outcome: outcomeResponded, State: opSucceeded, HTTPStatus: res.status, Receipt: res.body}); err != nil {
			// The effect exists but its receipt is not recorded. Failing the
			// activity stops dependent steps; the next attempt finds the
			// operation in flight and resolves it under the delivery policy.
			return CallEndpointOutput{Error: "record receipt: " + err.Error()}, fmt.Errorf("record receipt: %w", err)
		}
		return out, nil
	case res.err == nil && res.status >= 400 && res.status < 500 && res.status != http.StatusRequestTimeout:
		out.Outcome, out.Error = CallFailed, res.describe()
		if err := finish(finishInput{Outcome: outcomeResponded, State: opFailed, HTTPStatus: res.status, Error: out.Error}); err != nil {
			return out, err
		}
		return out, nil
	}

	// The destination may have applied the request.
	out.Error = "outcome unknown: " + res.describe()
	if mode == template.DeliveryManual {
		msg := out.Error + "; the destination has no deduplication or lookup policy, so it is not re-sent"
		if err := finish(finishInput{Outcome: outcomeUnknown, State: opUnresolved, HTTPStatus: res.status, Error: msg}); err != nil {
			return out, err
		}
		out.Outcome, out.Error = CallUnresolved, msg
		return out, nil
	}
	if err := finish(finishInput{Outcome: outcomeUnknown, State: opUnknown, HTTPStatus: res.status, Error: out.Error}); err != nil {
		return out, err
	}
	return out, temporal.NewApplicationError(out.Error, ErrTypeOutcomeUnknown)
}

// finishWithRetry absorbs short database blips before giving up on
// recording an attempt.
func (a *Activities) finishWithRetry(ctx context.Context, f finishInput) error {
	var err error
	for i, wait := range []time.Duration{0, 200 * time.Millisecond, time.Second, 2 * time.Second} {
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return errors.Join(err, ctx.Err())
			}
		}
		if err = a.ledger.finish(ctx, f); err == nil || errors.Is(err, errLeaseLost) {
			return err
		}
		activity.GetLogger(ctx).Warn("record operation attempt failed", "attempt", i+1, "error", err)
	}
	return err
}

type sendResult struct {
	status int
	body   json.RawMessage
	raw    []byte
	// wrote is true once request headers reached the connection; after
	// that the destination may have acted on the request.
	wrote bool
	err   error
}

func (r sendResult) describe() string {
	if r.err != nil {
		return r.err.Error()
	}
	return fmt.Sprintf("status %d: %s", r.status, excerpt(r.raw, 300))
}

func (a *Activities) send(ctx context.Context, in CallEndpointInput, authHeaders map[string]string) sendResult {
	var res sendResult
	var bodyReader io.Reader
	// GET/DELETE steps typically carry no body; sending "{}" to strict APIs
	// causes spurious 400s, so only attach a body when the step mapped one.
	sendBody := in.HasBody || len(in.Payload) > 0
	if sendBody {
		body, err := json.Marshal(in.Payload)
		if err != nil {
			res.err = err
			return res
		}
		bodyReader = strings.NewReader(string(body))
	}
	trace := &httptrace.ClientTrace{WroteHeaders: func() { res.wrote = true }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), in.Method, in.URL, bodyReader)
	if err != nil {
		res.err = err
		return res
	}
	if sendBody {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range in.Headers {
		req.Header.Set(k, v)
	}
	if in.IdempotencyHeader != "" && in.IdempotencyKey != "" {
		req.Header.Set(in.IdempotencyHeader, in.IdempotencyKey)
	}
	for k, v := range authHeaders {
		req.Header.Set(k, v)
	}
	preventTransparentResend(req)
	if a.d.RateLimiter != nil {
		if err := a.d.RateLimiter.Wait(ctx, req.URL.Host); err != nil {
			res.err = err
			return res
		}
	}

	started := time.Now()
	resp, err := a.d.HTTP.Do(req)
	latency := time.Since(started).Seconds()
	if err != nil {
		metrics.EndpointLatency.WithLabelValues("error").Observe(latency)
		res.err = err
		return res
	}
	defer resp.Body.Close()
	res.wrote = true
	maxBytes := a.d.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	res.raw, _ = io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	res.status, res.body = resp.StatusCode, safeJSON(res.raw)
	metrics.EndpointLatency.WithLabelValues(fmt.Sprintf("%d", resp.StatusCode/100*100)).Observe(latency)
	return res
}

// preventTransparentResend stops net/http from re-sending a request on its
// own when a reused connection breaks before the response. The Transport
// does that for requests it deems replayable: a body with GetBody, or no
// body with a safe method or an Idempotency-Key / X-Idempotency-Key header
// (matched by canonical map key). Only the operation ledger may re-send.
func preventTransparentResend(req *http.Request) {
	req.GetBody = nil
	for _, k := range []string{"Idempotency-Key", "X-Idempotency-Key"} {
		if v, ok := req.Header[k]; ok {
			delete(req.Header, k)
			req.Header[strings.ToLower(k)] = v
		}
	}
}

// lookupEffect runs the destination's lookup request. found reports whether
// FoundPath matched a non-null value in a 2xx response; 404 means not found.
func (a *Activities) lookupEffect(ctx context.Context, l *RenderedLookup, authHeaders map[string]string) (bool, int, json.RawMessage, error) {
	path, err := jp.ParseString(l.FoundPath)
	if err != nil {
		return false, 0, nil, fmt.Errorf("invalid foundPath %q: %w", l.FoundPath, err)
	}
	method := l.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, l.URL, nil)
	if err != nil {
		return false, 0, nil, err
	}
	for k, v := range l.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range authHeaders {
		req.Header.Set(k, v)
	}
	resp, err := a.d.HTTP.Do(req)
	if err != nil {
		return false, 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, resp.StatusCode, nil, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return false, resp.StatusCode, nil, fmt.Errorf("lookup status %d: %s", resp.StatusCode, excerpt(raw, 200))
	}
	doc, err := oj.Parse(raw)
	if err != nil {
		return false, resp.StatusCode, nil, fmt.Errorf("lookup response is not JSON: %w", err)
	}
	for _, v := range path.Get(doc) {
		if v != nil {
			return true, resp.StatusCode, safeJSON(raw), nil
		}
	}
	return false, resp.StatusCode, nil, nil
}

// secretResolver returns a destination secret owned by the job's
// organization.
type secretResolver interface {
	destinationSecret(ctx context.Context, jobID int64, name string) (string, error)
}

type pgSecretResolver struct {
	db        *pgxpool.Pool
	masterKey string
}

func (r pgSecretResolver) destinationSecret(ctx context.Context, jobID int64, name string) (string, error) {
	var ct, nonce []byte
	err := r.db.QueryRow(ctx, `
        SELECT s.ciphertext, s.nonce FROM secrets s JOIN jobs j ON j.org_id = s.org_id
        WHERE j.id = $1 AND s.name = $2 AND s.kind = 'destination' AND s.revoked_at IS NULL`,
		jobID, name).Scan(&ct, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", secretUnavailable("secret %q is not available to this job's organization (missing, revoked, or not a destination secret)", name)
	}
	if err != nil {
		return "", fmt.Errorf("load destination secret: %w", err)
	}
	plain, err := security.DecryptSecret(r.masterKey, ct, nonce)
	if err != nil {
		return "", secretUnavailable("secret %q cannot be decrypted with the configured master key", name)
	}
	return string(plain), nil
}

func secretUnavailable(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), ErrTypeSecretUnavailable, nil)
}

// resolveAuth turns Auth into request headers. The values exist only here
// and in the outgoing request.
func (a *Activities) resolveAuth(ctx context.Context, in CallEndpointInput) (map[string]string, error) {
	if in.Auth == nil || in.Auth.Type == "" {
		return nil, nil
	}
	if in.Auth.SecretRef == "" {
		return nil, secretUnavailable("auth type %q requires secretRef", in.Auth.Type)
	}
	switch in.Auth.Type {
	case "bearer", "basic", "api_key":
	default:
		return nil, secretUnavailable("auth type %q is not supported (use bearer, basic or api_key)", in.Auth.Type)
	}
	value, err := a.secrets.destinationSecret(ctx, in.JobID, in.Auth.SecretRef)
	if err != nil {
		return nil, err
	}
	switch in.Auth.Type {
	case "bearer":
		return map[string]string{"Authorization": "Bearer " + value}, nil
	case "basic":
		if !strings.Contains(value, ":") {
			return nil, secretUnavailable("secret %q for basic auth must be user:password", in.Auth.SecretRef)
		}
		return map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(value))}, nil
	default:
		header := in.Auth.HeaderName
		if header == "" {
			header = "X-API-Key"
		}
		return map[string]string{header: value}, nil
	}
}

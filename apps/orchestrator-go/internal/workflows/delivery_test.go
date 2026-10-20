package workflows

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/migration-platform/orchestrator/internal/activities"
	"github.com/migration-platform/orchestrator/internal/connectors"
	"github.com/migration-platform/orchestrator/internal/lifecycle"
	"github.com/migration-platform/orchestrator/internal/template"
)

type deliveryHarness struct {
	env      *testsuite.TestWorkflowEnvironment
	calls    []activities.CallEndpointInput
	steps    []activities.PersistStepOutcomeInput
	rows     []activities.PersistOutcomeInput
	call     func(activities.CallEndpointInput) (activities.CallEndpointOutput, error)
	stepErr  func(activities.PersistStepOutcomeInput) error
	rowState activities.LoadRowStateOutput
}

func newDeliveryHarness(t *testing.T, tpl template.RuleTemplate, rows []connectors.Row) *deliveryHarness {
	h := &deliveryHarness{env: (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()}
	h.env.SetTestTimeout(10 * time.Second)
	stub := &stubActivities{tpl: tpl, rows: rows}
	h.env.RegisterActivityWithOptions(stub.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	h.env.RegisterActivityWithOptions(stub.Ingest, activities.RegisterOptions("Ingest"))
	h.env.RegisterActivityWithOptions(stub.RequireLicensed, activities.RegisterOptions("RequireLicensed"))
	h.env.RegisterActivityWithOptions(stub.PublishProgress, activities.RegisterOptions("PublishProgress"))
	// FinalizeJob derives its verdict from the row outcomes this harness
	// persisted, as lifecycle.Finalize does from job_rows.
	registerJobFinish(h.env).verdict = func(in activities.FinalizeJobInput) *activities.FinalizeJobOutput {
		var t lifecycle.Totals
		for _, r := range h.rows {
			t.Total++
			switch r.Status {
			case "succeeded":
				t.Processed++
			case "failed":
				t.Failed++
			case "unresolved":
				t.Unresolved++
			}
		}
		o := lifecycle.Outcome{Cancelled: in.Cancelled, Error: in.Error}
		st := lifecycle.TerminalStatus(t, o)
		return &activities.FinalizeJobOutput{Status: st, Reason: lifecycle.Explain(st, t, o, ""), Totals: t}
	}
	h.env.RegisterActivityWithOptions(func(_ context.Context, _ activities.LoadRowStateInput) (activities.LoadRowStateOutput, error) {
		return h.rowState, nil
	}, activities.RegisterOptions("LoadRowState"))
	h.env.RegisterActivityWithOptions(func(_ context.Context, in activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
		h.calls = append(h.calls, in)
		if h.call != nil {
			return h.call(in)
		}
		return activities.CallEndpointOutput{Status: 201, Succeeded: true, Outcome: activities.CallSucceeded,
			Body: []byte(`{"data":{"id":"c-1"}}`)}, nil
	}, activities.RegisterOptions("CallEndpoint"))
	h.env.RegisterActivityWithOptions(func(_ context.Context, in activities.PersistStepOutcomeInput) error {
		h.steps = append(h.steps, in)
		if h.stepErr != nil {
			return h.stepErr(in)
		}
		return nil
	}, activities.RegisterOptions("PersistStepOutcome"))
	h.env.RegisterActivityWithOptions(func(_ context.Context, in activities.PersistOutcomeInput) error {
		h.rows = append(h.rows, in)
		return nil
	}, activities.RegisterOptions("PersistOutcome"))
	t.Cleanup(func() { h.env.AssertExpectations(t) })
	return h
}

func (h *deliveryHarness) run(t *testing.T) (MigrationWorkflowResult, error) {
	t.Helper()
	h.env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{
		OrgID: 1, JobID: 2, RuleTemplateID: 3, RuleTemplateVersion: 1, SourceRef: map[string]any{"type": "csv"},
	})
	require.True(t, h.env.IsWorkflowCompleted())
	var res MigrationWorkflowResult
	if err := h.env.GetWorkflowError(); err != nil {
		return res, err
	}
	require.NoError(t, h.env.GetWorkflowResult(&res))
	return res, nil
}

func oneRow() []connectors.Row {
	return []connectors.Row{{Index: 0, Data: map[string]any{"email": "a@b.com", "city": "X"}}}
}

func TestUnresolvedStepStopsTheChain(t *testing.T) {
	h := newDeliveryHarness(t, chainTemplate(), oneRow())
	h.call = func(in activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
		return activities.CallEndpointOutput{Status: 503, Outcome: activities.CallUnresolved,
			Error: "outcome unknown: status 503"}, nil
	}
	res, err := h.run(t)
	require.NoError(t, err)
	require.EqualValues(t, MigrationWorkflowResult{Unresolved: 1, Status: "failed", Reason: "0 of 1 rows failed, 1 unresolved"}, res,
		"the persisted verdict travels with the result: an unresolved row is not success")
	require.Len(t, h.calls, 1, "dependent steps must not run after an unresolved step")
	require.Equal(t, template.DeliveryManual, h.calls[0].DeliveryMode, "POST without a policy must default to manual")
	require.Len(t, h.rows, 1)
	require.Equal(t, "unresolved", h.rows[0].Status)
	require.Contains(t, h.rows[0].LastError, "outcome unknown")
	require.Len(t, h.steps, 1)
	require.Equal(t, "unresolved", h.steps[0].Status)
}

func TestExhaustedUnknownOutcomeIsUnresolvedNotFailed(t *testing.T) {
	for _, errType := range []string{activities.ErrTypeOutcomeUnknown, activities.ErrTypeOperationBusy} {
		t.Run(errType, func(t *testing.T) {
			tpl := chainTemplate()
			tpl.Retry = &template.Retry{MaxAttempts: 2, InitialIntervalMs: 10}
			h := newDeliveryHarness(t, tpl, oneRow())
			h.call = func(activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
				return activities.CallEndpointOutput{}, temporal.NewApplicationError("may have been applied", errType)
			}
			res, err := h.run(t)
			require.NoError(t, err)
			require.EqualValues(t, 1, res.Unresolved)
			require.EqualValues(t, 0, res.Failed)
			require.Len(t, h.calls, 2, "the activity is retried up to template.retry.maxAttempts")
			require.Equal(t, "unresolved", h.rows[0].Status)
		})
	}
}

func TestNonRetryableCallErrorIsFailed(t *testing.T) {
	h := newDeliveryHarness(t, chainTemplate(), oneRow())
	h.call = func(activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
		return activities.CallEndpointOutput{}, temporal.NewNonRetryableApplicationError(
			"secret missing", activities.ErrTypeSecretUnavailable, nil)
	}
	res, err := h.run(t)
	require.NoError(t, err)
	require.EqualValues(t, 1, res.Failed)
	require.Len(t, h.calls, 1)
	require.Equal(t, "failed", h.rows[0].Status)
}

func TestStepPersistenceErrorStopsTheRow(t *testing.T) {
	tpl := chainTemplate()
	tpl.Retry = &template.Retry{MaxAttempts: 1}
	h := newDeliveryHarness(t, tpl, oneRow())
	h.stepErr = func(activities.PersistStepOutcomeInput) error {
		return temporal.NewNonRetryableApplicationError("db down", "DB", nil)
	}
	_, err := h.run(t)
	require.Error(t, err)
	require.Contains(t, err.Error(), `persist step "createContact" outcome`)
	require.Len(t, h.calls, 1, "a step whose receipt was not recorded must not feed dependent steps")
	require.Empty(t, h.rows, "the row outcome must not be reported over an unrecorded step")
}

func TestDeliveryPolicyAuthAndAttemptIDsReachTheActivities(t *testing.T) {
	tpl := chainTemplate()
	tpl.Steps[0].Destination.Auth = &template.Auth{Type: "bearer", SecretRef: "crm_token"}
	tpl.Steps[0].Destination.IdempotencyHeader = "Idempotency-Key"
	tpl.Steps[0].Destination.Delivery = &template.Delivery{Mode: template.DeliveryLookup, IdempotencyRetentionHours: 6,
		Lookup: &template.Lookup{URL: "http://api/contacts", QueryParams: map[string]any{"email": map[string]any{"$from": "email"}},
			FoundPath: "$.items[0].id"}}
	tpl.Steps[1].Destination.Method = "PUT"
	h := newDeliveryHarness(t, tpl, oneRow())
	res, err := h.run(t)
	require.NoError(t, err)
	require.EqualValues(t, 1, res.Processed)
	require.Len(t, h.calls, 2)

	first := h.calls[0]
	require.Equal(t, template.DeliveryLookup, first.DeliveryMode)
	require.Equal(t, 6, first.RetentionHours)
	require.NotNil(t, first.Lookup)
	require.Equal(t, "http://api/contacts?email=a%40b.com", first.Lookup.URL)
	require.Equal(t, "$.items[0].id", first.Lookup.FoundPath)
	require.Equal(t, &activities.DestinationAuth{Type: "bearer", SecretRef: "crm_token"}, first.Auth)
	require.Equal(t, 1, h.calls[1].StepIndex)
	require.Equal(t, template.DeliveryIdempotent, h.calls[1].DeliveryMode, "PUT defaults to idempotent")

	require.NotEmpty(t, h.rows[0].AttemptID)
	require.Len(t, h.steps, 2)
	require.NotEqual(t, h.steps[0].AttemptID, h.steps[1].AttemptID)
	require.Contains(t, h.steps[0].AttemptID, h.rows[0].AttemptID)
}

func TestRetryRowPassesOperatorDecisions(t *testing.T) {
	h := newDeliveryHarness(t, chainTemplate(), nil)
	h.rowState = activities.LoadRowStateOutput{Found: true, Row: map[string]any{"email": "a@b.com", "city": "X"}}
	h.env.ExecuteWorkflow(RetryRowWorkflow, MigrationWorkflowInput{
		OrgID: 1, JobID: 2, RuleTemplateID: 3, RuleTemplateVersion: 1,
		RetryRow: &RetryRowRef{JobID: 2, RowIndex: 0, FromStart: true, ConfirmNotApplied: true},
	})
	require.True(t, h.env.IsWorkflowCompleted())
	require.NoError(t, h.env.GetWorkflowError())
	require.Len(t, h.calls, 2)
	for _, c := range h.calls {
		require.True(t, c.Resend, "a from-start retry re-applies succeeded steps on purpose")
		require.True(t, c.ConfirmNotApplied)
	}
}

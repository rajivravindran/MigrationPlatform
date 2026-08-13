package workflows

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/migration-platform/orchestrator/internal/activities"
	"github.com/migration-platform/orchestrator/internal/connectors"
	"github.com/migration-platform/orchestrator/internal/template"
)

type stubActivities struct {
	tpl      template.RuleTemplate
	rows     []connectors.Row
	rowState activities.LoadRowStateOutput
}

func (s *stubActivities) LoadTemplate(_ any, _ activities.LoadTemplateInput) (activities.LoadTemplateOutput, error) {
	return activities.LoadTemplateOutput{Template: s.tpl}, nil
}
func (s *stubActivities) Ingest(_ any, in activities.IngestInput) (activities.IngestOutput, error) {
	return activities.IngestOutput{Rows: s.rows, Done: true}, nil
}
func (s *stubActivities) CallEndpoint(_ any, _ activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
	return activities.CallEndpointOutput{Status: 200, Succeeded: true}, nil
}
func (s *stubActivities) PersistOutcome(_ any, _ activities.PersistOutcomeInput) error { return nil }
func (s *stubActivities) PersistStepOutcome(_ any, _ activities.PersistStepOutcomeInput) error {
	return nil
}
func (s *stubActivities) LoadRowState(_ any, _ activities.LoadRowStateInput) (activities.LoadRowStateOutput, error) {
	return s.rowState, nil
}
func (s *stubActivities) PublishProgress(_ any, _ activities.PublishProgressInput) error { return nil }
func (s *stubActivities) FinalizeJob(_ any, _ activities.FinalizeJobInput) error         { return nil }
func (s *stubActivities) ExportJobResults(_ any, _ activities.ExportJobResultsInput) error {
	return nil
}
func (s *stubActivities) RequireLicensed(_ any) error { return nil }

func registerStub(env *testsuite.TestWorkflowEnvironment, stub interface {
	LoadTemplate(any, activities.LoadTemplateInput) (activities.LoadTemplateOutput, error)
	Ingest(any, activities.IngestInput) (activities.IngestOutput, error)
	CallEndpoint(any, activities.CallEndpointInput) (activities.CallEndpointOutput, error)
	PersistOutcome(any, activities.PersistOutcomeInput) error
	PersistStepOutcome(any, activities.PersistStepOutcomeInput) error
	LoadRowState(any, activities.LoadRowStateInput) (activities.LoadRowStateOutput, error)
	PublishProgress(any, activities.PublishProgressInput) error
	RequireLicensed(any) error
	FinalizeJob(any, activities.FinalizeJobInput) error
	ExportJobResults(any, activities.ExportJobResultsInput) error
}) {
	env.RegisterActivityWithOptions(stub.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	env.RegisterActivityWithOptions(stub.RequireLicensed, activities.RegisterOptions("RequireLicensed"))
	env.RegisterActivityWithOptions(stub.Ingest, activities.RegisterOptions("Ingest"))
	env.RegisterActivityWithOptions(stub.CallEndpoint, activities.RegisterOptions("CallEndpoint"))
	env.RegisterActivityWithOptions(stub.PersistOutcome, activities.RegisterOptions("PersistOutcome"))
	env.RegisterActivityWithOptions(stub.PersistStepOutcome, activities.RegisterOptions("PersistStepOutcome"))
	env.RegisterActivityWithOptions(stub.LoadRowState, activities.RegisterOptions("LoadRowState"))
	env.RegisterActivityWithOptions(stub.PublishProgress, activities.RegisterOptions("PublishProgress"))
	env.RegisterActivityWithOptions(stub.FinalizeJob, activities.RegisterOptions("FinalizeJob"))
	env.RegisterActivityWithOptions(stub.ExportJobResults, activities.RegisterOptions("ExportJobResults"))
}

type stubActivitiesFailing struct{ stubActivities }

func (s *stubActivitiesFailing) CallEndpoint(_ any, _ activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
	return activities.CallEndpointOutput{Status: 500, Succeeded: false, Body: []byte(`{"err":"boom"}`)}, nil
}

func TestMigrationWorkflowAllRowsFail(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(10 * time.Second)

	tpl := template.RuleTemplate{ID: "rt_t", Version: 1, Name: "t", Source: template.Source{Type: "csv"}, Destination: template.Destination{Type: "http", Method: "POST", URL: "http://x"}}
	stub := &stubActivitiesFailing{stubActivities: stubActivities{tpl: tpl, rows: []connectors.Row{{Index: 0}, {Index: 1}, {Index: 2}}}}
	registerStub(env, stub)

	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{OrgID: 1, JobID: 2, RuleTemplateID: 3, RuleTemplateVersion: 1, SourceRef: map[string]any{"type": "csv"}})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result MigrationWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.EqualValues(t, 0, result.Processed)
	require.EqualValues(t, 3, result.Failed)
}

func TestRetryRowWorkflowUsesPersistedRowData(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	tpl := template.RuleTemplate{
		ID: "rt_t", Version: 1, Name: "t", Source: template.Source{Type: "csv"},
		Mapping:     map[string]any{"payload": map[string]any{"email": map[string]any{"$from": "email"}}},
		Destination: template.Destination{Type: "http", Method: "POST", URL: "http://x"},
	}
	stub := &stubActivities{
		tpl: tpl,
		rowState: activities.LoadRowStateOutput{
			Found: true,
			Row:   map[string]any{"email": "persisted@row.com"},
		},
	}
	var captured []activities.CallEndpointInput
	capture := func(_ context.Context, in activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
		captured = append(captured, in)
		return activities.CallEndpointOutput{Status: 200, Succeeded: true}, nil
	}
	env.RegisterActivityWithOptions(stub.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	env.RegisterActivityWithOptions(stub.Ingest, activities.RegisterOptions("Ingest"))
	env.RegisterActivityWithOptions(capture, activities.RegisterOptions("CallEndpoint"))
	env.RegisterActivityWithOptions(stub.RequireLicensed, activities.RegisterOptions("RequireLicensed"))
	env.RegisterActivityWithOptions(stub.PersistOutcome, activities.RegisterOptions("PersistOutcome"))
	env.RegisterActivityWithOptions(stub.PersistStepOutcome, activities.RegisterOptions("PersistStepOutcome"))
	env.RegisterActivityWithOptions(stub.LoadRowState, activities.RegisterOptions("LoadRowState"))
	env.RegisterActivityWithOptions(stub.PublishProgress, activities.RegisterOptions("PublishProgress"))

	env.ExecuteWorkflow(RetryRowWorkflow, MigrationWorkflowInput{
		OrgID: 1, JobID: 2, RuleTemplateID: 3, RuleTemplateVersion: 1,
		SourceRef: map[string]any{"type": "csv"},
		RetryRow:  &RetryRowRef{JobID: 2, RowIndex: 42},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result MigrationWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.EqualValues(t, 1, result.Processed)
	require.Len(t, captured, 1)
	require.Equal(t, "persisted@row.com", captured[0].Payload["email"], "retry must send the persisted row data")
}

func TestRetryRowWorkflowFailsWithoutPersistedRow(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	tpl := template.RuleTemplate{
		ID: "rt_t", Version: 1, Name: "t", Source: template.Source{Type: "csv"},
		Destination: template.Destination{Type: "http", Method: "POST", URL: "http://x"},
	}
	stub := &stubActivities{tpl: tpl, rowState: activities.LoadRowStateOutput{Found: false}}
	registerStub(env, stub)

	env.ExecuteWorkflow(RetryRowWorkflow, MigrationWorkflowInput{
		OrgID: 1, JobID: 2, RuleTemplateID: 3, RuleTemplateVersion: 1,
		SourceRef: map[string]any{"type": "csv"},
		RetryRow:  &RetryRowRef{JobID: 2, RowIndex: 42},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result MigrationWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.EqualValues(t, 1, result.Failed, "retry without persisted data must fail explicitly")
}

func TestMigrationWorkflowRendersTemplatedDestinationPerRow(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(10 * time.Second)

	tpl := template.RuleTemplate{
		ID: "rt_t", Version: 1, Name: "t",
		Source:  template.Source{Type: "csv"},
		Mapping: map[string]any{"payload": map[string]any{"id": map[string]any{"$from": "id"}}},
		Destination: template.Destination{
			Type: "http", Method: "GET",
			URL:        "http://api/users/{userId}/orders",
			PathParams: map[string]any{"userId": map[string]any{"$from": "id"}},
			QueryParams: map[string]any{
				"region": map[string]any{"$from": "country"},
			},
			Headers: map[string]any{
				"X-Tenant": map[string]any{"$from": "tenant"},
			},
		},
	}
	stub := &stubActivities{
		tpl: tpl,
		rows: []connectors.Row{
			{Index: 0, Data: map[string]any{"id": "u1", "country": "usa", "tenant": "acme"}},
			{Index: 1, Data: map[string]any{"id": "u2", "country": "uk", "tenant": "globex"}},
		},
	}

	var (
		urls    []string
		headers []map[string]string
	)
	captureCallEndpoint := func(_ context.Context, in activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
		urls = append(urls, in.URL)
		headers = append(headers, in.Headers)
		return activities.CallEndpointOutput{Status: 200, Succeeded: true}, nil
	}

	env.RegisterActivityWithOptions(stub.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	env.RegisterActivityWithOptions(stub.Ingest, activities.RegisterOptions("Ingest"))
	env.RegisterActivityWithOptions(captureCallEndpoint, activities.RegisterOptions("CallEndpoint"))
	env.RegisterActivityWithOptions(stub.RequireLicensed, activities.RegisterOptions("RequireLicensed"))
	env.RegisterActivityWithOptions(stub.PersistOutcome, activities.RegisterOptions("PersistOutcome"))
	env.RegisterActivityWithOptions(stub.PublishProgress, activities.RegisterOptions("PublishProgress"))

	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{
		OrgID: 1, JobID: 2, RuleTemplateID: 3, RuleTemplateVersion: 1,
		SourceRef: map[string]any{"type": "csv"},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 2, len(urls), "captured: %#v", urls)
	require.Equal(t, "http://api/users/u1/orders?region=usa", urls[0])
	require.Equal(t, "http://api/users/u2/orders?region=uk", urls[1])
	require.Equal(t, "acme", headers[0]["X-Tenant"])
	require.Equal(t, "globex", headers[1]["X-Tenant"])
}

func TestMigrationWorkflowFailsRowOnUnresolvedPathParam(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(10 * time.Second)

	tpl := template.RuleTemplate{
		ID: "rt_t", Version: 1, Name: "t",
		Source: template.Source{Type: "csv"},
		Destination: template.Destination{
			Type: "http", Method: "GET",
			URL:        "http://api/users/{userId}",
			PathParams: map[string]any{"userId": map[string]any{"$from": "missing"}},
		},
	}
	stub := &stubActivities{tpl: tpl, rows: []connectors.Row{{Index: 0, Data: map[string]any{}}}}
	env.RegisterActivityWithOptions(stub.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	env.RegisterActivityWithOptions(stub.Ingest, activities.RegisterOptions("Ingest"))
	env.RegisterActivityWithOptions(stub.CallEndpoint, activities.RegisterOptions("CallEndpoint"))
	env.RegisterActivityWithOptions(stub.RequireLicensed, activities.RegisterOptions("RequireLicensed"))
	env.RegisterActivityWithOptions(stub.PersistOutcome, activities.RegisterOptions("PersistOutcome"))
	env.RegisterActivityWithOptions(stub.PublishProgress, activities.RegisterOptions("PublishProgress"))

	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{
		OrgID: 1, JobID: 2, RuleTemplateID: 3, RuleTemplateVersion: 1,
		SourceRef: map[string]any{"type": "csv"},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result MigrationWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.EqualValues(t, 0, result.Processed)
	require.EqualValues(t, 1, result.Failed)
}

func chainTemplate() template.RuleTemplate {
	return template.RuleTemplate{
		ID: "rt_chain", Version: 1, Name: "chain",
		Source: template.Source{Type: "csv"},
		Steps: []template.Step{
			{
				Name:    "createContact",
				Mapping: map[string]any{"payload": map[string]any{"email": map[string]any{"$from": "email"}}},
				Destination: template.Destination{
					Type: "http", Method: "POST", URL: "http://api/contacts",
				},
			},
			{
				Name: "attachAddress",
				Mapping: map[string]any{"payload": map[string]any{
					"city":       map[string]any{"$from": "city"},
					"contactRef": map[string]any{"$fromResponse": "createContact", "path": "$.data.id"},
				}},
				Destination: template.Destination{
					Type: "http", Method: "POST",
					URL: "http://api/contacts/{contactId}/addresses",
					PathParams: map[string]any{
						"contactId": map[string]any{"$fromResponse": "createContact", "path": "$.data.id"},
					},
				},
			},
		},
	}
}

func TestMigrationWorkflowExecutesChainPerRow(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(10 * time.Second)

	stub := &stubActivities{
		tpl:  chainTemplate(),
		rows: []connectors.Row{{Index: 0, Data: map[string]any{"email": "a@b.com", "city": "Springfield"}}},
	}

	var calls []activities.CallEndpointInput
	callEndpoint := func(_ context.Context, in activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
		calls = append(calls, in)
		if in.StepName == "createContact" {
			return activities.CallEndpointOutput{Status: 201, Succeeded: true, Body: []byte(`{"data":{"id":"c-77"}}`)}, nil
		}
		return activities.CallEndpointOutput{Status: 200, Succeeded: true, Body: []byte(`{"ok":true}`)}, nil
	}
	var stepOutcomes []activities.PersistStepOutcomeInput
	persistStep := func(_ context.Context, in activities.PersistStepOutcomeInput) error {
		stepOutcomes = append(stepOutcomes, in)
		return nil
	}

	env.RegisterActivityWithOptions(stub.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	env.RegisterActivityWithOptions(stub.Ingest, activities.RegisterOptions("Ingest"))
	env.RegisterActivityWithOptions(callEndpoint, activities.RegisterOptions("CallEndpoint"))
	env.RegisterActivityWithOptions(stub.RequireLicensed, activities.RegisterOptions("RequireLicensed"))
	env.RegisterActivityWithOptions(stub.PersistOutcome, activities.RegisterOptions("PersistOutcome"))
	env.RegisterActivityWithOptions(persistStep, activities.RegisterOptions("PersistStepOutcome"))
	env.RegisterActivityWithOptions(stub.LoadRowState, activities.RegisterOptions("LoadRowState"))
	env.RegisterActivityWithOptions(stub.PublishProgress, activities.RegisterOptions("PublishProgress"))

	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{
		OrgID: 1, JobID: 2, RuleTemplateID: 3, RuleTemplateVersion: 1,
		SourceRef: map[string]any{"type": "csv"},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result MigrationWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.EqualValues(t, 1, result.Processed)
	require.EqualValues(t, 0, result.Failed)

	require.Len(t, calls, 2)
	require.Equal(t, "createContact", calls[0].StepName)
	require.Equal(t, "attachAddress", calls[1].StepName)
	require.Equal(t, "http://api/contacts/c-77/addresses", calls[1].URL,
		"step 2 URL must be rendered from step 1's response")
	require.Equal(t, "c-77", calls[1].Payload["contactRef"])
	require.NotEqual(t, calls[0].IdempotencyKey, calls[1].IdempotencyKey,
		"each step must get its own idempotency key")

	require.Len(t, stepOutcomes, 2)
	require.Equal(t, "succeeded", stepOutcomes[0].Status)
	require.Equal(t, "succeeded", stepOutcomes[1].Status)
}

func TestMigrationWorkflowChainStopsAtFailedStep(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(10 * time.Second)

	stub := &stubActivities{
		tpl:  chainTemplate(),
		rows: []connectors.Row{{Index: 0, Data: map[string]any{"email": "a@b.com", "city": "X"}}},
	}
	var calls []activities.CallEndpointInput
	callEndpoint := func(_ context.Context, in activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
		calls = append(calls, in)
		return activities.CallEndpointOutput{
			Status: 422, Succeeded: false, Body: []byte(`{"error":"bad email"}`),
			Error: "status 422: bad email",
		}, nil
	}
	var persisted []activities.PersistOutcomeInput
	persistOutcome := func(_ context.Context, in activities.PersistOutcomeInput) error {
		persisted = append(persisted, in)
		return nil
	}

	env.RegisterActivityWithOptions(stub.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	env.RegisterActivityWithOptions(stub.Ingest, activities.RegisterOptions("Ingest"))
	env.RegisterActivityWithOptions(callEndpoint, activities.RegisterOptions("CallEndpoint"))
	env.RegisterActivityWithOptions(stub.RequireLicensed, activities.RegisterOptions("RequireLicensed"))
	env.RegisterActivityWithOptions(persistOutcome, activities.RegisterOptions("PersistOutcome"))
	env.RegisterActivityWithOptions(stub.PersistStepOutcome, activities.RegisterOptions("PersistStepOutcome"))
	env.RegisterActivityWithOptions(stub.LoadRowState, activities.RegisterOptions("LoadRowState"))
	env.RegisterActivityWithOptions(stub.PublishProgress, activities.RegisterOptions("PublishProgress"))

	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{
		OrgID: 1, JobID: 2, RuleTemplateID: 3, RuleTemplateVersion: 1,
		SourceRef: map[string]any{"type": "csv"},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, calls, 1, "step 2 must not run after step 1 fails")
	require.Len(t, persisted, 1)
	require.Equal(t, "failed", persisted[0].Status)
	require.Contains(t, persisted[0].LastError, `step "createContact"`)
	require.Contains(t, persisted[0].LastError, "422", "4xx reason must be surfaced")
}

func TestMigrationWorkflowChainContinuesOnFailure(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(10 * time.Second)

	tpl := template.RuleTemplate{
		ID: "rt_continue", Version: 1, Name: "continue",
		Source: template.Source{Type: "csv"},
		Steps: []template.Step{
			{
				Name:      "notify",
				OnFailure: "continue",
				Mapping:   map[string]any{"payload": map[string]any{"email": map[string]any{"$from": "email"}}},
				Destination: template.Destination{
					Type: "http", Method: "POST", URL: "http://api/notify",
				},
			},
			{
				Name:    "createContact",
				Mapping: map[string]any{"payload": map[string]any{"email": map[string]any{"$from": "email"}}},
				Destination: template.Destination{
					Type: "http", Method: "POST", URL: "http://api/contacts",
				},
			},
		},
	}
	stub := &stubActivities{
		tpl:  tpl,
		rows: []connectors.Row{{Index: 0, Data: map[string]any{"email": "a@b.com"}}},
	}
	var calls []activities.CallEndpointInput
	callEndpoint := func(_ context.Context, in activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
		calls = append(calls, in)
		if in.StepName == "notify" {
			return activities.CallEndpointOutput{
				Status: 422, Succeeded: false, Body: []byte(`{"error":"bad"}`),
				Error: "status 422: bad",
			}, nil
		}
		return activities.CallEndpointOutput{Status: 201, Succeeded: true, Body: []byte(`{"ok":true}`)}, nil
	}
	var persisted []activities.PersistOutcomeInput
	persistOutcome := func(_ context.Context, in activities.PersistOutcomeInput) error {
		persisted = append(persisted, in)
		return nil
	}

	env.RegisterActivityWithOptions(stub.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	env.RegisterActivityWithOptions(stub.Ingest, activities.RegisterOptions("Ingest"))
	env.RegisterActivityWithOptions(callEndpoint, activities.RegisterOptions("CallEndpoint"))
	env.RegisterActivityWithOptions(stub.RequireLicensed, activities.RegisterOptions("RequireLicensed"))
	env.RegisterActivityWithOptions(persistOutcome, activities.RegisterOptions("PersistOutcome"))
	env.RegisterActivityWithOptions(stub.PersistStepOutcome, activities.RegisterOptions("PersistStepOutcome"))
	env.RegisterActivityWithOptions(stub.LoadRowState, activities.RegisterOptions("LoadRowState"))
	env.RegisterActivityWithOptions(stub.PublishProgress, activities.RegisterOptions("PublishProgress"))
	env.RegisterActivityWithOptions(stub.FinalizeJob, activities.RegisterOptions("FinalizeJob"))
	env.RegisterActivityWithOptions(stub.ExportJobResults, activities.RegisterOptions("ExportJobResults"))

	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{
		OrgID: 1, JobID: 2, RuleTemplateID: 3, RuleTemplateVersion: 1,
		SourceRef: map[string]any{"type": "csv"},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, calls, 2, "step 2 must run when step 1 has onFailure=continue")
	require.Equal(t, "notify", calls[0].StepName)
	require.Equal(t, "createContact", calls[1].StepName)
	require.Len(t, persisted, 1)
	require.Equal(t, "failed", persisted[0].Status, "row stays failed if any step failed")
}

func TestRetryRowResumesAtFailedStep(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(10 * time.Second)

	stub := &stubActivities{
		tpl: chainTemplate(),
		rowState: activities.LoadRowStateOutput{
			Found: true,
			Row:   map[string]any{"email": "a@b.com", "city": "Shelbyville"},
			Steps: []activities.PersistedStep{
				{StepIndex: 0, StepName: "createContact", Status: "succeeded", Response: []byte(`{"data":{"id":"c-old"}}`)},
				{StepIndex: 1, StepName: "attachAddress", Status: "failed"},
			},
		},
	}
	var calls []activities.CallEndpointInput
	callEndpoint := func(_ context.Context, in activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
		calls = append(calls, in)
		return activities.CallEndpointOutput{Status: 200, Succeeded: true, Body: []byte(`{"ok":true}`)}, nil
	}
	env.RegisterActivityWithOptions(stub.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	env.RegisterActivityWithOptions(stub.Ingest, activities.RegisterOptions("Ingest"))
	env.RegisterActivityWithOptions(callEndpoint, activities.RegisterOptions("CallEndpoint"))
	env.RegisterActivityWithOptions(stub.RequireLicensed, activities.RegisterOptions("RequireLicensed"))
	env.RegisterActivityWithOptions(stub.PersistOutcome, activities.RegisterOptions("PersistOutcome"))
	env.RegisterActivityWithOptions(stub.PersistStepOutcome, activities.RegisterOptions("PersistStepOutcome"))
	env.RegisterActivityWithOptions(stub.LoadRowState, activities.RegisterOptions("LoadRowState"))
	env.RegisterActivityWithOptions(stub.PublishProgress, activities.RegisterOptions("PublishProgress"))

	env.ExecuteWorkflow(RetryRowWorkflow, MigrationWorkflowInput{
		OrgID: 1, JobID: 2, RuleTemplateID: 3, RuleTemplateVersion: 1,
		SourceRef: map[string]any{"type": "csv"},
		RetryRow:  &RetryRowRef{JobID: 2, RowIndex: 7},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result MigrationWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.EqualValues(t, 1, result.Processed)

	require.Len(t, calls, 1, "succeeded step 1 must be skipped on resume")
	require.Equal(t, "attachAddress", calls[0].StepName)
	require.Equal(t, "http://api/contacts/c-old/addresses", calls[0].URL,
		"resume must reuse the persisted step-1 response")
}

func TestExtractConnectorFromSourceRef(t *testing.T) {
	cfg, secret := extractConnector(map[string]any{
		"type":   "salesforce",
		"secret": "oauth-token",
		"extra":  map[string]any{"soql": "SELECT Id FROM Account"},
	})
	require.Equal(t, "salesforce", cfg.Kind)
	require.Equal(t, "SELECT Id FROM Account", cfg.Config["soql"])
	require.Equal(t, "oauth-token", secret)

	minioCfg, _ := extractConnector(map[string]any{
		"type":     "minio",
		"bucket":   "migration",
		"key":      "uploads/1/ab/file.csv",
		"filename": "blog_migration.csv",
	})
	require.Equal(t, "csv", minioCfg.Kind)
	require.Equal(t, "migration", minioCfg.Config["s3_bucket"])
	require.Equal(t, "uploads/1/ab/file.csv", minioCfg.Config["s3_key"])
	require.Equal(t, true, minioCfg.Config["header"])
}

func TestMigrationWorkflowHappyPath(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(10 * time.Second)

	tpl := template.RuleTemplate{
		ID:          "rt_t",
		Version:     1,
		Name:        "t",
		Source:      template.Source{Type: "csv"},
		Preprocess:  []template.PreprocessStep{},
		Mapping:     map[string]any{"payload": map[string]any{"id": map[string]any{"$from": "id"}}},
		Destination: template.Destination{Type: "http", Method: "POST", URL: "http://x/y"},
	}
	stub := &stubActivities{
		tpl: tpl,
		rows: []connectors.Row{
			{Index: 0, Data: map[string]any{"id": "1"}},
			{Index: 1, Data: map[string]any{"id": "2"}},
		},
	}

	env.RegisterActivityWithOptions(stub.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	env.RegisterActivityWithOptions(stub.Ingest, activities.RegisterOptions("Ingest"))
	env.RegisterActivityWithOptions(stub.CallEndpoint, activities.RegisterOptions("CallEndpoint"))
	env.RegisterActivityWithOptions(stub.RequireLicensed, activities.RegisterOptions("RequireLicensed"))
	env.RegisterActivityWithOptions(stub.PersistOutcome, activities.RegisterOptions("PersistOutcome"))
	env.RegisterActivityWithOptions(stub.PublishProgress, activities.RegisterOptions("PublishProgress"))

	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{
		OrgID: 1, JobID: 2, RuleTemplateID: 3, RuleTemplateVersion: 1,
		SourceRef: map[string]any{"type": "csv"},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result MigrationWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.EqualValues(t, 2, result.Processed)
	require.EqualValues(t, 0, result.Failed)
}

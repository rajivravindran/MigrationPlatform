package workflows

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/migration-platform/orchestrator/internal/activities"
	"github.com/migration-platform/orchestrator/internal/connectors"
)

type watchStub struct {
	conn           activities.LoadConnectorOutput
	objects        []connectors.ObjectInfo
	jobs           []activities.StartWatchObjectJobInput
	batches        []activities.StartBatchInput
	cursor         map[string]string
	staged         []activities.StageSftpObjectInput
	stagingBucket  string
	stagingPrefix  string
}

func (s *watchStub) LoadConnector(_ context.Context, _ activities.LoadConnectorInput) (activities.LoadConnectorOutput, error) {
	return s.conn, nil
}
func (s *watchStub) ListPrefixObjects(_ context.Context, _ activities.ListPrefixObjectsInput) (activities.ListPrefixObjectsOutput, error) {
	return activities.ListPrefixObjectsOutput{Objects: s.objects}, nil
}
func (s *watchStub) ListSftpObjects(_ context.Context, _ activities.ListSftpObjectsInput) (activities.ListSftpObjectsOutput, error) {
	bucket := s.stagingBucket
	if bucket == "" {
		bucket = "migration"
	}
	prefix := s.stagingPrefix
	if prefix == "" {
		prefix = "sftp-landing/"
	}
	return activities.ListSftpObjectsOutput{
		Objects: s.objects, StagingBucket: bucket, StagingPrefix: prefix,
	}, nil
}
func (s *watchStub) StageSftpObject(_ context.Context, in activities.StageSftpObjectInput) (activities.StageSftpObjectOutput, error) {
	s.staged = append(s.staged, in)
	bucket := s.stagingBucket
	if bucket == "" {
		bucket = "migration"
	}
	key := connectors.StagingObjectKey(s.stagingPrefix, in.ConnectorID, in.RemoteKey)
	return activities.StageSftpObjectOutput{
		Bucket: bucket, Key: key, ETag: in.ETag, Size: in.Size,
	}, nil
}
func (s *watchStub) StartWatchObjectJob(_ context.Context, in activities.StartWatchObjectJobInput) (activities.StartWatchObjectJobOutput, error) {
	s.jobs = append(s.jobs, in)
	wid := activities.WatchWorkflowID(in.OrgID, in.Bucket, in.Key, in.ETag)
	return activities.StartWatchObjectJobOutput{JobID: int64(len(s.jobs)), WorkflowID: wid, Created: true}, nil
}
func (s *watchStub) StartBatch(_ context.Context, in activities.StartBatchInput) (activities.StartBatchOutput, error) {
	s.batches = append(s.batches, in)
	wid := activities.BatchWorkflowID(in.OrgID, in.Bucket, in.Key, in.ETag)
	return activities.StartBatchOutput{BatchID: int64(len(s.batches)), WorkflowID: wid, Created: true}, nil
}
func (s *watchStub) MarkJobRunning(_ context.Context, _ activities.MarkJobRunningInput) error { return nil }
func (s *watchStub) MarkBatchRunning(_ context.Context, _ activities.MarkBatchRunningInput) error {
	return nil
}
func (s *watchStub) AdvanceConnectorCursor(_ context.Context, in activities.AdvanceConnectorCursorInput) error {
	s.cursor = in.Seen
	return nil
}
func (s *watchStub) LoadTemplate(_ context.Context, _ activities.LoadTemplateInput) (activities.LoadTemplateOutput, error) {
	return activities.LoadTemplateOutput{}, nil
}
func (s *watchStub) Ingest(_ context.Context, _ activities.IngestInput) (activities.IngestOutput, error) {
	return activities.IngestOutput{Done: true}, nil
}
func (s *watchStub) CallEndpoint(_ context.Context, _ activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
	return activities.CallEndpointOutput{Status: 200, Succeeded: true}, nil
}
func (s *watchStub) PersistOutcome(_ context.Context, _ activities.PersistOutcomeInput) error {
	return nil
}
func (s *watchStub) PersistStepOutcome(_ context.Context, _ activities.PersistStepOutcomeInput) error {
	return nil
}
func (s *watchStub) PublishProgress(_ context.Context, _ activities.PublishProgressInput) error {
	return nil
}
func (s *watchStub) FinalizeJob(_ context.Context, _ activities.FinalizeJobInput) error { return nil }
func (s *watchStub) LoadRowState(_ context.Context, _ activities.LoadRowStateInput) (activities.LoadRowStateOutput, error) {
	return activities.LoadRowStateOutput{}, nil
}

// Minimal BatchWorkflow activity stubs so child BatchWorkflow can quarantine immediately
// when Unpack fails — in this test we register a BatchWorkflow that succeeds via stubs.
func (s *watchStub) UnpackAndStageArchive(_ context.Context, _ activities.UnpackAndStageArchiveInput) (activities.UnpackAndStageArchiveOutput, error) {
	return activities.UnpackAndStageArchiveOutput{
		OnStageFailure: "stop",
		Stages: []activities.StagedFile{{
			StageKey: "a", File: "a.csv", TemplateKey: "t", OnFailure: "stop",
			Bucket: "migration", Key: "batches/1/stages/a/a.csv", Filename: "a.csv", Size: 1,
		}},
	}, nil
}
func (s *watchStub) MaterializeBatchStages(_ context.Context, in activities.MaterializeBatchStagesInput) (activities.MaterializeBatchStagesOutput, error) {
	out := activities.MaterializeBatchStagesOutput{}
	for i, st := range in.Stages {
		out.Stages = append(out.Stages, activities.ResolvedStage{
			StageRowID: int64(i + 1), StageIndex: i, StageKey: st.StageKey,
			TemplateKey: st.TemplateKey, RuleTemplateID: 7, RuleTemplateVersion: 1,
			OnFailure: st.OnFailure, Bucket: st.Bucket, Key: st.Key, Filename: st.Filename, Size: st.Size,
		})
	}
	return out, nil
}
func (s *watchStub) StartBatchStageJob(_ context.Context, in activities.StartBatchStageJobInput) (activities.StartBatchStageJobOutput, error) {
	return activities.StartBatchStageJobOutput{
		JobID: 100 + in.StageRowID, WorkflowID: activities.StageWorkflowID(in.BatchID, in.StageKey), Created: true,
	}, nil
}
func (s *watchStub) FinalizeBatchStage(_ context.Context, _ activities.FinalizeBatchStageInput) error {
	return nil
}
func (s *watchStub) FinalizeBatch(_ context.Context, _ activities.FinalizeBatchInput) error { return nil }
func (s *watchStub) QuarantineArchive(_ context.Context, _ activities.QuarantineArchiveInput) (activities.QuarantineArchiveOutput, error) {
	return activities.QuarantineArchiveOutput{}, nil
}

func TestWatchPrefixWorkflowStartsNewObjects(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(30 * time.Second)

	stub := &watchStub{
		conn: activities.LoadConnectorOutput{
			ID: 9, Kind: "watched_prefix", Name: "incoming",
			Config: map[string]any{
				"bucket": "migration",
				"prefix": "incoming/",
				"glob":   "*",
				"sort":   "lexical",
				"cursor": map[string]any{
					"seen": map[string]any{"incoming/old.csv": "etag-old"},
				},
			},
		},
		objects: []connectors.ObjectInfo{
			{Key: "incoming/old.csv", ETag: "etag-old", Size: 10},
			{Key: "incoming/new.csv", ETag: "etag-new", Size: 20},
			{Key: "incoming/pack.tar.gz", ETag: "etag-tar", Size: 30},
		},
	}

	env.RegisterWorkflow(MigrationWorkflow)
	env.RegisterWorkflow(BatchWorkflow)
	registerWatchStub(env, stub)

	env.ExecuteWorkflow(WatchPrefixWorkflow, WatchPrefixWorkflowInput{
		OrgID: 1, ScheduleID: 42, RuleTemplateID: 7, RuleTemplateVersion: 1, ConnectorID: 9,
		ScheduledTime: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result WatchPrefixWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, 3, result.Listed)
	require.Equal(t, 2, result.Started, "new.csv + pack.tar.gz should start")
	require.Equal(t, 1, result.Skipped, "old.csv skipped")
	require.Len(t, stub.jobs, 1)
	require.Equal(t, "incoming/new.csv", stub.jobs[0].Key)
	require.Len(t, stub.batches, 1)
	require.Equal(t, "incoming/pack.tar.gz", stub.batches[0].Key)
	require.Equal(t, "etag-new", stub.cursor["incoming/new.csv"])
	require.Equal(t, "etag-tar", stub.cursor["incoming/pack.tar.gz"])
}

func TestWatchPrefixWorkflowSftpStagesAndStarts(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(30 * time.Second)

	stub := &watchStub{
		conn: activities.LoadConnectorOutput{
			ID: 11, Kind: "watched_sftp", Name: "vendor-sftp",
			Config: map[string]any{
				"host": "sftp.example.com", "username": "drop",
				"path": "/incoming", "glob": "*",
				"cursor": map[string]any{
					"seen": map[string]any{"/incoming/old.csv": "1:10"},
				},
			},
		},
		objects: []connectors.ObjectInfo{
			{Key: "/incoming/old.csv", ETag: "1:10", Size: 10},
			{Key: "/incoming/new.csv", ETag: "2:20", Size: 20},
			{Key: "/incoming/pack.tar.gz", ETag: "3:30", Size: 30},
		},
		stagingBucket: "migration",
		stagingPrefix: "sftp-landing/",
	}

	env.RegisterWorkflow(MigrationWorkflow)
	env.RegisterWorkflow(BatchWorkflow)
	registerWatchStub(env, stub)

	env.ExecuteWorkflow(WatchPrefixWorkflow, WatchPrefixWorkflowInput{
		OrgID: 1, ScheduleID: 42, RuleTemplateID: 7, RuleTemplateVersion: 1, ConnectorID: 11,
		ScheduledTime: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result WatchPrefixWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, 3, result.Listed)
	require.Equal(t, 2, result.Started)
	require.Equal(t, 1, result.Skipped)
	require.Len(t, stub.staged, 2, "only new objects are staged")
	require.Len(t, stub.jobs, 1)
	require.Equal(t, "sftp-landing/11/incoming/new.csv", stub.jobs[0].Key)
	require.Equal(t, "migration", stub.jobs[0].Bucket)
	require.Len(t, stub.batches, 1)
	require.Equal(t, "sftp-landing/11/incoming/pack.tar.gz", stub.batches[0].Key)
	require.Equal(t, "2:20", stub.cursor["/incoming/new.csv"])
	require.Equal(t, "3:30", stub.cursor["/incoming/pack.tar.gz"])
}

func registerWatchStub(env *testsuite.TestWorkflowEnvironment, stub *watchStub) {
	env.RegisterActivityWithOptions(stub.LoadConnector, activities.RegisterOptions("LoadConnector"))
	env.RegisterActivityWithOptions(stub.ListPrefixObjects, activities.RegisterOptions("ListPrefixObjects"))
	env.RegisterActivityWithOptions(stub.ListSftpObjects, activities.RegisterOptions("ListSftpObjects"))
	env.RegisterActivityWithOptions(stub.StageSftpObject, activities.RegisterOptions("StageSftpObject"))
	env.RegisterActivityWithOptions(stub.StartWatchObjectJob, activities.RegisterOptions("StartWatchObjectJob"))
	env.RegisterActivityWithOptions(stub.StartBatch, activities.RegisterOptions("StartBatch"))
	env.RegisterActivityWithOptions(stub.MarkJobRunning, activities.RegisterOptions("MarkJobRunning"))
	env.RegisterActivityWithOptions(stub.MarkBatchRunning, activities.RegisterOptions("MarkBatchRunning"))
	env.RegisterActivityWithOptions(stub.AdvanceConnectorCursor, activities.RegisterOptions("AdvanceConnectorCursor"))
	env.RegisterActivityWithOptions(stub.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	env.RegisterActivityWithOptions(stub.Ingest, activities.RegisterOptions("Ingest"))
	env.RegisterActivityWithOptions(stub.CallEndpoint, activities.RegisterOptions("CallEndpoint"))
	env.RegisterActivityWithOptions(stub.PersistOutcome, activities.RegisterOptions("PersistOutcome"))
	env.RegisterActivityWithOptions(stub.PersistStepOutcome, activities.RegisterOptions("PersistStepOutcome"))
	env.RegisterActivityWithOptions(stub.PublishProgress, activities.RegisterOptions("PublishProgress"))
	env.RegisterActivityWithOptions(stub.FinalizeJob, activities.RegisterOptions("FinalizeJob"))
	env.RegisterActivityWithOptions(stub.LoadRowState, activities.RegisterOptions("LoadRowState"))
	env.RegisterActivityWithOptions(stub.UnpackAndStageArchive, activities.RegisterOptions("UnpackAndStageArchive"))
	env.RegisterActivityWithOptions(stub.MaterializeBatchStages, activities.RegisterOptions("MaterializeBatchStages"))
	env.RegisterActivityWithOptions(stub.StartBatchStageJob, activities.RegisterOptions("StartBatchStageJob"))
	env.RegisterActivityWithOptions(stub.FinalizeBatchStage, activities.RegisterOptions("FinalizeBatchStage"))
	env.RegisterActivityWithOptions(stub.FinalizeBatch, activities.RegisterOptions("FinalizeBatch"))
	env.RegisterActivityWithOptions(stub.QuarantineArchive, activities.RegisterOptions("QuarantineArchive"))
}

func TestWatchWorkflowIDStable(t *testing.T) {
	a := activities.WatchWorkflowID(1, "b", "k", "e")
	b := activities.WatchWorkflowID(1, "b", "k", "e")
	c := activities.WatchWorkflowID(1, "b", "k", "other")
	require.Equal(t, a, b)
	require.NotEqual(t, a, c)
	require.Contains(t, a, "watch-1-")
}

func TestBatchWorkflowOrderedStagesStopOnFailure(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(30 * time.Second)

	stub := &batchStub{
		stages: []activities.StagedFile{
			{StageKey: "one", File: "one.csv", TemplateKey: "t1", OnFailure: "stop", Bucket: "b", Key: "k1", Filename: "one.csv", Size: 1},
			{StageKey: "two", File: "two.csv", TemplateKey: "t2", OnFailure: "stop", Bucket: "b", Key: "k2", Filename: "two.csv", Size: 1},
		},
		failStage: "one",
	}

	env.RegisterWorkflow(MigrationWorkflow)
	env.RegisterWorkflow(BatchWorkflow)
	registerBatchStub(env, stub)

	env.ExecuteWorkflow(BatchWorkflow, BatchWorkflowInput{
		OrgID: 1, BatchID: 9, Bucket: "migration", Key: "incoming/pack.tar.gz",
	})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result BatchWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "failed", result.Status)
	require.Equal(t, 2, result.StagesTotal)
	require.Equal(t, 0, result.StagesOK)
	require.GreaterOrEqual(t, result.StagesFailed, 1)
	require.Equal(t, 0, stub.startedJobs, "no stage job should start when first StartBatchStageJob fails under stop")
}

func TestBatchWorkflowDAGParallelRootsThenDependent(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(30 * time.Second)

	// Ingest returns Done with no rows → child MigrationWorkflow succeeds.
	stub := &batchStub{
		stages: []activities.StagedFile{
			{StageKey: "a", File: "a.csv", TemplateKey: "t", OnFailure: "stop", Bucket: "b", Key: "ka", Filename: "a.csv", Size: 1},
			{StageKey: "b", File: "b.csv", TemplateKey: "t", OnFailure: "stop", Bucket: "b", Key: "kb", Filename: "b.csv", Size: 1},
			{StageKey: "c", File: "c.csv", TemplateKey: "t", OnFailure: "stop", DependsOn: []string{"a", "b"},
				Bucket: "b", Key: "kc", Filename: "c.csv", Size: 1},
		},
	}

	env.RegisterWorkflow(MigrationWorkflow)
	env.RegisterWorkflow(BatchWorkflow)
	registerBatchStub(env, stub)

	env.ExecuteWorkflow(BatchWorkflow, BatchWorkflowInput{
		OrgID: 1, BatchID: 11, Bucket: "migration", Key: "incoming/dag.tar.gz",
	})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result BatchWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "succeeded", result.Status)
	require.Equal(t, 3, result.StagesTotal)
	require.Equal(t, 3, result.StagesOK)
	require.Equal(t, 0, result.StagesFailed)
	require.Equal(t, 3, stub.startedJobs)
}

func TestBatchWorkflowDAGStopSkipsDependents(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(30 * time.Second)

	stub := &batchStub{
		stages: []activities.StagedFile{
			{StageKey: "a", File: "a.csv", TemplateKey: "t", OnFailure: "stop", Bucket: "b", Key: "ka", Filename: "a.csv", Size: 1},
			{StageKey: "b", File: "b.csv", TemplateKey: "t", OnFailure: "continue", Bucket: "b", Key: "kb", Filename: "b.csv", Size: 1},
			{StageKey: "c", File: "c.csv", TemplateKey: "t", OnFailure: "stop", DependsOn: []string{"a"},
				Bucket: "b", Key: "kc", Filename: "c.csv", Size: 1},
		},
		failStage: "a",
	}

	env.RegisterWorkflow(MigrationWorkflow)
	env.RegisterWorkflow(BatchWorkflow)
	registerBatchStub(env, stub)

	env.ExecuteWorkflow(BatchWorkflow, BatchWorkflowInput{
		OrgID: 1, BatchID: 12, Bucket: "migration", Key: "incoming/dag.tar.gz",
	})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result BatchWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "failed", result.Status)
	require.Equal(t, 3, result.StagesTotal)
	require.Equal(t, 0, result.StagesOK)
	require.Equal(t, 3, result.StagesFailed)
	// a fails at Start; stop skips b and c without starting jobs
	require.Equal(t, 0, stub.startedJobs)
}

func TestBatchWorkflowDAGContinueAllowsSibling(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(30 * time.Second)

	stub := &batchStub{
		stages: []activities.StagedFile{
			{StageKey: "a", File: "a.csv", TemplateKey: "t", OnFailure: "continue", Bucket: "b", Key: "ka", Filename: "a.csv", Size: 1},
			{StageKey: "b", File: "b.csv", TemplateKey: "t", OnFailure: "continue", Bucket: "b", Key: "kb", Filename: "b.csv", Size: 1},
			{StageKey: "c", File: "c.csv", TemplateKey: "t", OnFailure: "continue", DependsOn: []string{"a"},
				Bucket: "b", Key: "kc", Filename: "c.csv", Size: 1},
		},
		failStage: "a",
	}

	env.RegisterWorkflow(MigrationWorkflow)
	env.RegisterWorkflow(BatchWorkflow)
	registerBatchStub(env, stub)

	env.ExecuteWorkflow(BatchWorkflow, BatchWorkflowInput{
		OrgID: 1, BatchID: 13, Bucket: "migration", Key: "incoming/dag.tar.gz",
	})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result BatchWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "partial", result.Status)
	require.Equal(t, 1, result.StagesOK)       // b succeeds
	require.Equal(t, 2, result.StagesFailed)   // a failed + c skipped
	require.Equal(t, 1, stub.startedJobs)      // only b
}

type batchStub struct {
	stages      []activities.StagedFile
	failStage   string
	startedJobs int
	finalStatus string
}

func (s *batchStub) MarkBatchRunning(_ context.Context, _ activities.MarkBatchRunningInput) error {
	return nil
}
func (s *batchStub) UnpackAndStageArchive(_ context.Context, _ activities.UnpackAndStageArchiveInput) (activities.UnpackAndStageArchiveOutput, error) {
	return activities.UnpackAndStageArchiveOutput{OnStageFailure: "stop", Stages: s.stages}, nil
}
func (s *batchStub) MaterializeBatchStages(_ context.Context, in activities.MaterializeBatchStagesInput) (activities.MaterializeBatchStagesOutput, error) {
	out := activities.MaterializeBatchStagesOutput{}
	for i, st := range in.Stages {
		out.Stages = append(out.Stages, activities.ResolvedStage{
			StageRowID: int64(i + 1), StageIndex: i, StageKey: st.StageKey,
			TemplateKey: st.TemplateKey, RuleTemplateID: 1, RuleTemplateVersion: 1,
			OnFailure: st.OnFailure, DependsOn: append([]string(nil), st.DependsOn...),
			Bucket: st.Bucket, Key: st.Key, Filename: st.Filename, Size: st.Size,
		})
	}
	return out, nil
}
func (s *batchStub) StartBatchStageJob(_ context.Context, in activities.StartBatchStageJobInput) (activities.StartBatchStageJobOutput, error) {
	if s.failStage != "" && in.StageKey == s.failStage {
		return activities.StartBatchStageJobOutput{}, fmt.Errorf("forced stage failure")
	}
	s.startedJobs++
	return activities.StartBatchStageJobOutput{
		JobID: int64(s.startedJobs), WorkflowID: activities.StageWorkflowID(in.BatchID, in.StageKey), Created: true,
	}, nil
}
func (s *batchStub) MarkJobRunning(_ context.Context, _ activities.MarkJobRunningInput) error { return nil }
func (s *batchStub) FinalizeBatchStage(_ context.Context, _ activities.FinalizeBatchStageInput) error {
	return nil
}
func (s *batchStub) FinalizeBatch(_ context.Context, in activities.FinalizeBatchInput) error {
	s.finalStatus = in.Status
	return nil
}
func (s *batchStub) QuarantineArchive(_ context.Context, _ activities.QuarantineArchiveInput) (activities.QuarantineArchiveOutput, error) {
	return activities.QuarantineArchiveOutput{}, nil
}
func (s *batchStub) LoadTemplate(_ context.Context, _ activities.LoadTemplateInput) (activities.LoadTemplateOutput, error) {
	return activities.LoadTemplateOutput{}, nil
}
func (s *batchStub) Ingest(_ context.Context, _ activities.IngestInput) (activities.IngestOutput, error) {
	return activities.IngestOutput{Done: true}, nil
}
func (s *batchStub) CallEndpoint(_ context.Context, _ activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
	return activities.CallEndpointOutput{Status: 500, Succeeded: false, Error: "boom"}, nil
}
func (s *batchStub) PersistOutcome(_ context.Context, _ activities.PersistOutcomeInput) error {
	return nil
}
func (s *batchStub) PersistStepOutcome(_ context.Context, _ activities.PersistStepOutcomeInput) error {
	return nil
}
func (s *batchStub) PublishProgress(_ context.Context, _ activities.PublishProgressInput) error {
	return nil
}
func (s *batchStub) FinalizeJob(_ context.Context, _ activities.FinalizeJobInput) error { return nil }
func (s *batchStub) LoadRowState(_ context.Context, _ activities.LoadRowStateInput) (activities.LoadRowStateOutput, error) {
	return activities.LoadRowStateOutput{}, nil
}

func registerBatchStub(env *testsuite.TestWorkflowEnvironment, stub *batchStub) {
	env.RegisterActivityWithOptions(stub.MarkBatchRunning, activities.RegisterOptions("MarkBatchRunning"))
	env.RegisterActivityWithOptions(stub.UnpackAndStageArchive, activities.RegisterOptions("UnpackAndStageArchive"))
	env.RegisterActivityWithOptions(stub.MaterializeBatchStages, activities.RegisterOptions("MaterializeBatchStages"))
	env.RegisterActivityWithOptions(stub.StartBatchStageJob, activities.RegisterOptions("StartBatchStageJob"))
	env.RegisterActivityWithOptions(stub.MarkJobRunning, activities.RegisterOptions("MarkJobRunning"))
	env.RegisterActivityWithOptions(stub.FinalizeBatchStage, activities.RegisterOptions("FinalizeBatchStage"))
	env.RegisterActivityWithOptions(stub.FinalizeBatch, activities.RegisterOptions("FinalizeBatch"))
	env.RegisterActivityWithOptions(stub.QuarantineArchive, activities.RegisterOptions("QuarantineArchive"))
	env.RegisterActivityWithOptions(stub.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	env.RegisterActivityWithOptions(stub.Ingest, activities.RegisterOptions("Ingest"))
	env.RegisterActivityWithOptions(stub.CallEndpoint, activities.RegisterOptions("CallEndpoint"))
	env.RegisterActivityWithOptions(stub.PersistOutcome, activities.RegisterOptions("PersistOutcome"))
	env.RegisterActivityWithOptions(stub.PersistStepOutcome, activities.RegisterOptions("PersistStepOutcome"))
	env.RegisterActivityWithOptions(stub.PublishProgress, activities.RegisterOptions("PublishProgress"))
	env.RegisterActivityWithOptions(stub.FinalizeJob, activities.RegisterOptions("FinalizeJob"))
	env.RegisterActivityWithOptions(stub.LoadRowState, activities.RegisterOptions("LoadRowState"))
}

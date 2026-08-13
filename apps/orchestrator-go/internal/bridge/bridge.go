// Package bridge exposes a minimal internal HTTP API over the Temporal Go
// client so services without a mature Temporal SDK (the Rust API) can start,
// signal and cancel workflows and manage schedules.
//
// This listener must never be exposed publicly: it is bound on the compose /
// cluster network only and protected by a shared bearer token.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

type Server struct {
	temporal client.Client
	token    string
	logger   *slog.Logger
	testSFTP func(context.Context, int64, int64) error
}

func NewServer(c client.Client, token string, logger *slog.Logger, testSFTP func(context.Context, int64, int64) error) *Server {
	return &Server{temporal: c, token: token, logger: logger, testSFTP: testSFTP}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/workflows/start", s.auth(s.startWorkflow))
	mux.HandleFunc("POST /v1/workflows/signal", s.auth(s.signalWorkflow))
	mux.HandleFunc("POST /v1/workflows/cancel", s.auth(s.cancelWorkflow))
	mux.HandleFunc("POST /v1/schedules/create", s.auth(s.createSchedule))
	mux.HandleFunc("POST /v1/schedules/update", s.auth(s.updateSchedule))
	mux.HandleFunc("POST /v1/schedules/pause", s.auth(s.pauseSchedule))
	mux.HandleFunc("POST /v1/schedules/unpause", s.auth(s.unpauseSchedule))
	mux.HandleFunc("POST /v1/schedules/delete", s.auth(s.deleteSchedule))
	mux.HandleFunc("POST /v1/schedules/trigger", s.auth(s.triggerSchedule))
	mux.HandleFunc("POST /v1/connectors/sftp/test", s.auth(s.testSFTPConnector))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	return mux
}

type connectorTestRequest struct {
	OrgID       int64 `json:"orgId"`
	ConnectorID int64 `json:"connectorId"`
}

func (s *Server) testSFTPConnector(w http.ResponseWriter, r *http.Request) {
	var req connectorTestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	if req.OrgID <= 0 || req.ConnectorID <= 0 || s.testSFTP == nil {
		badRequest(w, fmt.Errorf("valid orgId and connectorId are required"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.testSFTP(ctx, req.OrgID, req.ConnectorID); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.token == "" || got != s.token {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

type startRequest struct {
	WorkflowID   string         `json:"workflowId"`
	WorkflowType string         `json:"workflowType"`
	TaskQueue    string         `json:"taskQueue"`
	Input        map[string]any `json:"input"`
}

func (s *Server) startWorkflow(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	if req.WorkflowType == "" {
		req.WorkflowType = "MigrationWorkflow"
	}
	if req.TaskQueue == "" {
		req.TaskQueue = "migration"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	run, err := s.temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                    req.WorkflowID,
		TaskQueue:             req.TaskQueue,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, req.WorkflowType, req.Input)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]string{"workflowId": run.GetID(), "runId": run.GetRunID()})
}

type signalRequest struct {
	WorkflowID string         `json:"workflowId"`
	Signal     string         `json:"signal"`
	Payload    map[string]any `json:"payload"`
}

func (s *Server) signalWorkflow(w http.ResponseWriter, r *http.Request) {
	var req signalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.temporal.SignalWorkflow(ctx, req.WorkflowID, "", req.Signal, req.Payload); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type cancelRequest struct {
	WorkflowID string `json:"workflowId"`
}

func (s *Server) cancelWorkflow(w http.ResponseWriter, r *http.Request) {
	var req cancelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.temporal.CancelWorkflow(ctx, req.WorkflowID, ""); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type scheduleRequest struct {
	ScheduleID           string         `json:"scheduleId"`
	Cron                 string         `json:"cron,omitempty"`
	Every                string         `json:"every,omitempty"`
	Timezone             string         `json:"timezone,omitempty"`
	OverlapPolicy        string         `json:"overlapPolicy,omitempty"`
	CatchupWindowSeconds int            `json:"catchupWindowSeconds,omitempty"`
	Paused               bool           `json:"paused,omitempty"`
	WorkflowType         string         `json:"workflowType,omitempty"`
	TaskQueue            string         `json:"taskQueue,omitempty"`
	Input                map[string]any `json:"input,omitempty"`
}

func overlapPolicy(s string) enums.ScheduleOverlapPolicy {
	switch s {
	case "buffer_one":
		return enums.SCHEDULE_OVERLAP_POLICY_BUFFER_ONE
	case "buffer_all":
		return enums.SCHEDULE_OVERLAP_POLICY_BUFFER_ALL
	case "cancel_other":
		return enums.SCHEDULE_OVERLAP_POLICY_CANCEL_OTHER
	case "allow_all":
		return enums.SCHEDULE_OVERLAP_POLICY_ALLOW_ALL
	default:
		return enums.SCHEDULE_OVERLAP_POLICY_SKIP
	}
}

func (s *Server) scheduleSpec(req scheduleRequest) (client.ScheduleSpec, error) {
	spec := client.ScheduleSpec{TimeZoneName: req.Timezone}
	if req.Cron != "" {
		spec.CronExpressions = []string{req.Cron}
		return spec, nil
	}
	if req.Every != "" {
		d, err := time.ParseDuration(req.Every)
		if err != nil {
			return spec, fmt.Errorf("invalid interval %q: %w", req.Every, err)
		}
		spec.Intervals = []client.ScheduleIntervalSpec{{Every: d}}
		return spec, nil
	}
	return spec, fmt.Errorf("schedule needs cron or every")
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	var req scheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	spec, err := s.scheduleSpec(req)
	if err != nil {
		badRequest(w, err)
		return
	}
	if req.WorkflowType == "" {
		req.WorkflowType = "WatchPrefixWorkflow"
	}
	if req.TaskQueue == "" {
		req.TaskQueue = "migration"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	_, err = s.temporal.ScheduleClient().Create(ctx, client.ScheduleOptions{
		ID:            req.ScheduleID,
		Spec:          spec,
		Overlap:       overlapPolicy(req.OverlapPolicy),
		CatchupWindow: time.Duration(req.CatchupWindowSeconds) * time.Second,
		Paused:        req.Paused,
		Action: &client.ScheduleWorkflowAction{
			// Empty ID → Temporal generates a unique workflow id per fire.
			Workflow:  req.WorkflowType,
			TaskQueue: req.TaskQueue,
			Args:      []any{req.Input},
		},
	})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request) {
	var req scheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	spec, err := s.scheduleSpec(req)
	if err != nil {
		badRequest(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	handle := s.temporal.ScheduleClient().GetHandle(ctx, req.ScheduleID)
	err = handle.Update(ctx, client.ScheduleUpdateOptions{
		DoUpdate: func(in client.ScheduleUpdateInput) (*client.ScheduleUpdate, error) {
			in.Description.Schedule.Spec = &spec
			return &client.ScheduleUpdate{Schedule: &in.Description.Schedule}, nil
		},
	})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) scheduleOp(w http.ResponseWriter, r *http.Request, op func(context.Context, client.ScheduleHandle) error) {
	var req scheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	handle := s.temporal.ScheduleClient().GetHandle(ctx, req.ScheduleID)
	if err := op(ctx, handle); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) pauseSchedule(w http.ResponseWriter, r *http.Request) {
	s.scheduleOp(w, r, func(ctx context.Context, h client.ScheduleHandle) error {
		return h.Pause(ctx, client.SchedulePauseOptions{})
	})
}

func (s *Server) unpauseSchedule(w http.ResponseWriter, r *http.Request) {
	s.scheduleOp(w, r, func(ctx context.Context, h client.ScheduleHandle) error {
		return h.Unpause(ctx, client.ScheduleUnpauseOptions{})
	})
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	s.scheduleOp(w, r, func(ctx context.Context, h client.ScheduleHandle) error {
		return h.Delete(ctx)
	})
}

func (s *Server) triggerSchedule(w http.ResponseWriter, r *http.Request) {
	s.scheduleOp(w, r, func(ctx context.Context, h client.ScheduleHandle) error {
		return h.Trigger(ctx, client.ScheduleTriggerOptions{})
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func badRequest(w http.ResponseWriter, err error) {
	http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
}

func serverError(w http.ResponseWriter, err error) {
	http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadGateway)
}

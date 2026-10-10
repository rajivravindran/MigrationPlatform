package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Config holds the switchable destination behaviours.
type Config struct {
	// Idempotency makes the destination honour IdempotencyHeader: a repeated
	// key for the same tenant returns the original receipt instead of writing.
	Idempotency bool `json:"idempotency"`
	// UniqueBusinessKey rejects a second write of the same tenant/entity/
	// external_id with 409 and the existing id (a "lookup-able" destination).
	UniqueBusinessKey bool   `json:"unique_business_key"`
	RequiredToken     string `json:"required_token,omitempty"`
	IdempotencyHeader string `json:"idempotency_header"`
}

// Receipt is one durable write accepted by the destination.
type Receipt struct {
	Seq            int64           `json:"seq"`
	ID             string          `json:"id"`
	Tenant         string          `json:"tenant"`
	Entity         string          `json:"entity"`
	ExternalID     string          `json:"external_id"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	PayloadSHA256  string          `json:"payload_sha256"`
	Payload        json.RawMessage `json:"payload"`
	CreatedAt      time.Time       `json:"created_at"`
}

// Attempt records every request the destination saw and what it did.
type Attempt struct {
	Seq            int64     `json:"seq"`
	At             time.Time `json:"at"`
	Method         string    `json:"method"`
	Path           string    `json:"path"`
	Tenant         string    `json:"tenant"`
	Entity         string    `json:"entity"`
	ExternalID     string    `json:"external_id,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	Outcome        string    `json:"outcome"`
	Status         int       `json:"status"`
	ReceiptID      string    `json:"receipt_id,omitempty"`
	RuleID         string    `json:"rule_id,omitempty"`
}

// Fault actions.
const (
	ActionReject               = "reject"
	ActionThrottle             = "throttle"
	ActionServerError          = "server_error"
	ActionTimeoutBeforeWrite   = "timeout_before_write"
	ActionCommitThenDisconnect = "commit_then_disconnect"
)

// FaultRule fires its action for matching create requests. Empty match fields
// match anything. Times == 0 means unlimited.
type FaultRule struct {
	ID         string `json:"id"`
	Tenant     string `json:"tenant,omitempty"`
	Entity     string `json:"entity,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	Action     string `json:"action"`
	Status     int    `json:"status,omitempty"`
	Times      int    `json:"times,omitempty"`
	DelayMs    int    `json:"delay_ms,omitempty"`
	Fired      int    `json:"fired"`
}

func (r *FaultRule) validate() error {
	switch r.Action {
	case ActionReject, ActionThrottle, ActionServerError, ActionTimeoutBeforeWrite, ActionCommitThenDisconnect:
	default:
		return fmt.Errorf("unknown action %q", r.Action)
	}
	if r.ID == "" {
		return errors.New("rule id is required")
	}
	if r.Times < 0 {
		return errors.New("times must be >= 0")
	}
	return nil
}

func (r *FaultRule) matches(tenant, entity, externalID string) bool {
	if r.Times > 0 && r.Fired >= r.Times {
		return false
	}
	return (r.Tenant == "" || r.Tenant == tenant) &&
		(r.Entity == "" || r.Entity == entity) &&
		(r.ExternalID == "" || r.ExternalID == externalID)
}

// Server is the fixture state. All mutations hold mu.
type Server struct {
	mu         sync.Mutex
	cfg        Config
	ledgerPath string
	ledger     *os.File
	receipts   []Receipt
	attempts   []Attempt
	faults     []*FaultRule
	byIdem     map[string]int // tenant|key -> receipts index
	byBizKey   map[string]int // tenant|entity|external_id -> first receipts index
	attemptSeq int64
}

func NewServer(ledgerPath string, cfg Config) (*Server, error) {
	if cfg.IdempotencyHeader == "" {
		cfg.IdempotencyHeader = "Idempotency-Key"
	}
	s := &Server{cfg: cfg, ledgerPath: ledgerPath}
	if err := s.openLedger(false); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) openLedger(truncate bool) error {
	if s.ledger != nil {
		_ = s.ledger.Close()
	}
	if err := os.MkdirAll(filepath.Dir(s.ledgerPath), 0o755); err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_RDWR | os.O_APPEND
	if truncate {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(s.ledgerPath, flags, 0o644)
	if err != nil {
		return err
	}
	s.ledger = f
	s.receipts = nil
	s.byIdem = map[string]int{}
	s.byBizKey = map[string]int{}
	if truncate {
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var r Receipt
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return fmt.Errorf("corrupt ledger line %d: %w", len(s.receipts)+1, err)
		}
		s.index(r)
	}
	return sc.Err()
}

func (s *Server) index(r Receipt) {
	s.receipts = append(s.receipts, r)
	i := len(s.receipts) - 1
	if r.IdempotencyKey != "" {
		s.byIdem[r.Tenant+"|"+r.IdempotencyKey] = i
	}
	bk := r.Tenant + "|" + r.Entity + "|" + r.ExternalID
	if _, ok := s.byBizKey[bk]; !ok {
		s.byBizKey[bk] = i
	}
}

// commit durably appends a receipt. Caller holds mu.
func (s *Server) commit(tenant, entity, externalID, idemKey string, payload []byte) (Receipt, error) {
	sum := sha256.Sum256(payload)
	seq := int64(len(s.receipts) + 1)
	r := Receipt{
		Seq:            seq,
		ID:             fmt.Sprintf("%s_%06d", entityPrefix(entity), seq),
		Tenant:         tenant,
		Entity:         entity,
		ExternalID:     externalID,
		IdempotencyKey: idemKey,
		PayloadSHA256:  hex.EncodeToString(sum[:]),
		Payload:        json.RawMessage(payload),
		CreatedAt:      time.Now().UTC(),
	}
	line, err := json.Marshal(r)
	if err != nil {
		return Receipt{}, err
	}
	if _, err := s.ledger.Write(append(line, '\n')); err != nil {
		return Receipt{}, err
	}
	if err := s.ledger.Sync(); err != nil {
		return Receipt{}, err
	}
	s.index(r)
	return r, nil
}

func entityPrefix(entity string) string {
	e := strings.TrimSuffix(entity, "s")
	if len(e) > 4 {
		e = e[:4]
	}
	return e
}

func (s *Server) record(a Attempt) {
	s.attemptSeq++
	a.Seq = s.attemptSeq
	a.At = time.Now().UTC()
	s.attempts = append(s.attempts, a)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/{entity}", s.handleCreate)
	mux.HandleFunc("GET /v1/{entity}", s.handleLookup)
	mux.HandleFunc("GET /v1/{entity}/{id}", s.handleGet)

	mux.HandleFunc("GET /_admin/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /_admin/receipts", s.handleReceipts)
	mux.HandleFunc("GET /_admin/attempts", s.handleAttempts)
	mux.HandleFunc("GET /_admin/faults", s.handleGetFaults)
	mux.HandleFunc("PUT /_admin/faults", s.handlePutFaults)
	mux.HandleFunc("DELETE /_admin/faults", s.handleClearFaults)
	mux.HandleFunc("GET /_admin/config", s.handleGetConfig)
	mux.HandleFunc("PUT /_admin/config", s.handlePutConfig)
	mux.HandleFunc("POST /_admin/reset", s.handleReset)
	return mux
}

func tenantOf(r *http.Request) string {
	if t := r.Header.Get("X-Tenant"); t != "" {
		return t
	}
	return "default"
}

func (s *Server) authorized(r *http.Request) bool {
	s.mu.Lock()
	tok := s.cfg.RequiredToken
	s.mu.Unlock()
	return tok == "" || r.Header.Get("Authorization") == "Bearer "+tok
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	entity := r.PathValue("entity")
	tenant := tenantOf(r)
	att := Attempt{Method: r.Method, Path: r.URL.Path, Tenant: tenant, Entity: entity}

	if !s.authorized(r) {
		s.mu.Lock()
		att.Outcome, att.Status = "unauthorized", http.StatusUnauthorized
		s.record(att)
		s.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var doc map[string]any
	if err == nil {
		err = json.Unmarshal(body, &doc)
	}
	externalID, _ := doc["external_id"].(string)
	if err != nil || externalID == "" {
		s.mu.Lock()
		att.Outcome, att.Status = "bad_request", http.StatusUnprocessableEntity
		s.record(att)
		s.mu.Unlock()
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "body must be a JSON object with a string external_id"})
		return
	}
	att.ExternalID = externalID

	s.mu.Lock()
	idemKey := ""
	if s.cfg.Idempotency {
		idemKey = r.Header.Get(s.cfg.IdempotencyHeader)
	}
	att.IdempotencyKey = r.Header.Get(s.cfg.IdempotencyHeader)

	// Honoured idempotency replays never consume fault budget: the original
	// write already happened.
	if idemKey != "" {
		if i, ok := s.byIdem[tenant+"|"+idemKey]; ok {
			prev := s.receipts[i]
			sum := sha256.Sum256(body)
			if prev.PayloadSHA256 != hex.EncodeToString(sum[:]) {
				att.Outcome, att.Status = "idempotency_conflict", http.StatusUnprocessableEntity
				s.record(att)
				s.mu.Unlock()
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "idempotency key reused with a different payload"})
				return
			}
			att.Outcome, att.Status, att.ReceiptID = "replayed", http.StatusOK, prev.ID
			s.record(att)
			s.mu.Unlock()
			w.Header().Set("Idempotent-Replayed", "true")
			writeJSON(w, http.StatusOK, receiptBody(prev))
			return
		}
	}

	var rule *FaultRule
	for _, f := range s.faults {
		if f.matches(tenant, entity, externalID) {
			f.Fired++
			rule = f
			break
		}
	}
	if rule != nil {
		att.RuleID = rule.ID
		switch rule.Action {
		case ActionReject:
			status := rule.Status
			if status == 0 {
				status = http.StatusUnprocessableEntity
			}
			att.Outcome, att.Status = "rejected", status
			s.record(att)
			s.mu.Unlock()
			writeJSON(w, status, map[string]any{"error": "rejected by fault rule " + rule.ID})
			return
		case ActionThrottle:
			att.Outcome, att.Status = "throttled", http.StatusTooManyRequests
			s.record(att)
			s.mu.Unlock()
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "throttled by fault rule " + rule.ID})
			return
		case ActionServerError:
			status := rule.Status
			if status == 0 {
				status = http.StatusInternalServerError
			}
			att.Outcome, att.Status = "server_error", status
			s.record(att)
			s.mu.Unlock()
			writeJSON(w, status, map[string]any{"error": "server error by fault rule " + rule.ID})
			return
		case ActionTimeoutBeforeWrite:
			att.Outcome, att.Status = "timeout_before_write", http.StatusGatewayTimeout
			s.record(att)
			delay := time.Duration(rule.DelayMs) * time.Millisecond
			if delay <= 0 {
				delay = 30 * time.Second
			}
			s.mu.Unlock()
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": "timed out before write"})
			return
		case ActionCommitThenDisconnect:
			rec, err := s.commit(tenant, entity, externalID, idemKey, body)
			if err != nil {
				att.Outcome, att.Status = "ledger_error", http.StatusInternalServerError
				s.record(att)
				s.mu.Unlock()
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			att.Outcome, att.Status, att.ReceiptID = "committed_then_disconnected", 0, rec.ID
			s.record(att)
			s.mu.Unlock()
			disconnect(w)
			return
		}
	}

	if s.cfg.UniqueBusinessKey {
		if i, ok := s.byBizKey[tenant+"|"+entity+"|"+externalID]; ok {
			prev := s.receipts[i]
			att.Outcome, att.Status, att.ReceiptID = "conflict", http.StatusConflict, prev.ID
			s.record(att)
			s.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{"error": "external_id already exists", "id": prev.ID})
			return
		}
	}

	rec, err := s.commit(tenant, entity, externalID, idemKey, body)
	if err != nil {
		att.Outcome, att.Status = "ledger_error", http.StatusInternalServerError
		s.record(att)
		s.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	att.Outcome, att.Status, att.ReceiptID = "created", http.StatusCreated, rec.ID
	s.record(att)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, receiptBody(rec))
}

// disconnect closes the client connection without writing a response, which
// is what a destination crash or network cut after commit looks like.
func disconnect(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic(http.ErrAbortHandler)
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = conn.Close()
}

func receiptBody(r Receipt) map[string]any {
	return map[string]any{"id": r.ID, "entity": r.Entity, "external_id": r.ExternalID, "receipt_seq": r.Seq}
}

func (s *Server) handleLookup(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	entity, tenant := r.PathValue("entity"), tenantOf(r)
	ext := r.URL.Query().Get("external_id")
	s.mu.Lock()
	out := []map[string]any{}
	for _, rec := range s.receipts {
		if rec.Tenant == tenant && rec.Entity == entity && (ext == "" || rec.ExternalID == ext) {
			out = append(out, receiptBody(rec))
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	entity, tenant, id := r.PathValue("entity"), tenantOf(r), r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.receipts {
		if rec.Tenant == tenant && rec.Entity == entity && rec.ID == id {
			body := receiptBody(rec)
			body["payload"] = rec.Payload
			writeJSON(w, http.StatusOK, body)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
}

func (s *Server) handleReceipts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	out := []Receipt{}
	for _, rec := range s.receipts {
		if (q.Get("tenant") == "" || q.Get("tenant") == rec.Tenant) &&
			(q.Get("entity") == "" || q.Get("entity") == rec.Entity) &&
			(q.Get("external_id") == "" || q.Get("external_id") == rec.ExternalID) {
			out = append(out, rec)
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAttempts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	out := []Attempt{}
	for _, a := range s.attempts {
		if (q.Get("tenant") == "" || q.Get("tenant") == a.Tenant) &&
			(q.Get("entity") == "" || q.Get("entity") == a.Entity) {
			out = append(out, a)
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetFaults(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.faults)
}

func (s *Server) handlePutFaults(w http.ResponseWriter, r *http.Request) {
	var rules []*FaultRule
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rules); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	seen := map[string]bool{}
	for _, f := range rules {
		if err := f.validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if seen[f.ID] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "duplicate rule id " + f.ID})
			return
		}
		seen[f.ID] = true
		f.Fired = 0
	}
	s.mu.Lock()
	s.faults = rules
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, rules)
}

func (s *Server) handleClearFaults(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.faults = nil
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, []any{})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg
	if cfg.RequiredToken != "" {
		cfg.RequiredToken = "set"
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var cfg Config
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if cfg.IdempotencyHeader == "" {
		cfg.IdempotencyHeader = "Idempotency-Key"
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReset truncates the ledger and clears attempts and faults. Config is
// kept unless the caller also PUTs a new one.
func (s *Server) handleReset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openLedger(true); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.attempts = nil
	s.attemptSeq = 0
	s.faults = nil
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

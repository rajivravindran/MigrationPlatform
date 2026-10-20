package activities

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/migration-platform/orchestrator/internal/template"
)

// Error types CallEndpoint returns (ADR 0003).
const (
	// ErrTypeOutcomeUnknown: the request may have been applied; the next
	// attempt resolves it under the delivery policy. Retryable.
	ErrTypeOutcomeUnknown = "OutcomeUnknown"
	// ErrTypeOperationBusy: another attempt holds the operation's lease.
	ErrTypeOperationBusy = "OperationBusy"
	// ErrTypeSecretUnavailable: auth.secretRef is missing, revoked or not
	// the job's organization's. Non-retryable; nothing was sent.
	ErrTypeSecretUnavailable = "DestinationSecretUnavailable"
)

// Operation states.
const (
	opPending    = "pending"
	opSending    = "sending"
	opSucceeded  = "succeeded"
	opFailed     = "failed"
	opUnknown    = "unknown"
	opUnresolved = "unresolved"
)

// operationLease outlives the CallEndpoint start-to-close timeout, after which
// Temporal has cancelled the holder's context.
const operationLease = 2*time.Minute + 15*time.Second

type operationRef struct {
	JobID     int64
	RowIndex  int64
	StepIndex int
	StepName  string
}

// opSnapshot is the ledger row as seen when an attempt starts.
type opSnapshot struct {
	Exists        bool
	State         string
	PayloadSHA256 string
	Attempts      int
	LeaseAttempt  string
	LeaseUntil    time.Time
	FirstSentAt   time.Time
	ReceiptStatus int
	Receipt       json.RawMessage
	Now           time.Time
	// DecisionGeneration is the logical operator decision (a from-start
	// retry or a confirm-not-applied) that last authorized a send on this
	// operation. Empty for operations never authorized that way, and for
	// rows written before generations existed.
	DecisionGeneration string
}

type attemptIntent struct {
	PayloadSHA256  string
	Mode           string
	RetentionHours int
	// Resend re-applies an operation that already succeeded (an explicit
	// from-start retry). ConfirmNotApplied lets an unresolved one be sent.
	Resend            bool
	ConfirmNotApplied bool
	// Generation identifies the operator decision that carries Resend or
	// ConfirmNotApplied: one retry request is one generation, however many
	// times Temporal redelivers its activities. The permission is consumed
	// the first time a generation sends; a redelivery of the same generation
	// replays the receipt (or stays unresolved) instead of sending again
	// (principal review F1). Empty means an unscoped legacy decision.
	Generation string
}

// decisionConsumed reports whether the operator decision in the intent was
// already spent on this operation by the same generation.
func decisionConsumed(s opSnapshot, in attemptIntent) bool {
	return in.Generation != "" && s.DecisionGeneration == in.Generation
}

type decisionKind int

const (
	decideSend decisionKind = iota
	decideLookupThenSend
	decideReplay
	decideBusy
	decideUnresolved
)

type attemptDecision struct {
	Kind   decisionKind
	Reason string
}

// decideAttempt is the operation state machine for one attempt (ADR 0003 §1).
func decideAttempt(s opSnapshot, in attemptIntent) attemptDecision {
	if !s.Exists {
		return attemptDecision{Kind: decideSend}
	}
	samePayload := s.PayloadSHA256 == in.PayloadSHA256
	state := s.State
	if state == opSending && !s.Now.Before(s.LeaseUntil) {
		state = opUnknown
	}
	switch state {
	case opPending, opFailed:
		return attemptDecision{Kind: decideSend}
	case opSending:
		return attemptDecision{Kind: decideBusy, Reason: "another attempt is sending this operation"}
	case opSucceeded:
		if in.Resend && !decisionConsumed(s, in) {
			return attemptDecision{Kind: decideSend}
		}
		if samePayload {
			// A redelivered from-start retry lands here once its generation
			// has sent: the effect exists, replay its receipt.
			return attemptDecision{Kind: decideReplay}
		}
		return attemptDecision{Kind: decideUnresolved, Reason: "payload changed after the operation succeeded; the new payload is a new intent"}
	case opUnresolved:
		if in.ConfirmNotApplied {
			if decisionConsumed(s, in) {
				return attemptDecision{Kind: decideUnresolved, Reason: "the confirm-not-applied decision was already used by this retry and the send it authorized has an unknown outcome; check the destination again"}
			}
			return attemptDecision{Kind: decideSend}
		}
		return attemptDecision{Kind: decideUnresolved, Reason: "operation is unresolved; retry with confirm_not_applied once the destination shows no effect"}
	case opUnknown:
		if in.ConfirmNotApplied {
			if decisionConsumed(s, in) {
				return attemptDecision{Kind: decideUnresolved, Reason: "the confirm-not-applied decision was already used by this retry and the send it authorized has an unknown outcome; check the destination again"}
			}
			return attemptDecision{Kind: decideSend}
		}
		if !samePayload {
			return attemptDecision{Kind: decideUnresolved, Reason: "outcome of an earlier attempt is unknown and the payload has changed"}
		}
		switch in.Mode {
		case template.DeliveryIdempotent:
			retention := time.Duration(in.RetentionHours) * time.Hour
			if !s.FirstSentAt.IsZero() && s.Now.Sub(s.FirstSentAt) > retention {
				return attemptDecision{Kind: decideUnresolved, Reason: fmt.Sprintf(
					"outcome unknown and the first attempt is older than the %dh idempotency retention", in.RetentionHours)}
			}
			return attemptDecision{Kind: decideSend}
		case template.DeliveryLookup:
			return attemptDecision{Kind: decideLookupThenSend}
		default:
			return attemptDecision{Kind: decideUnresolved, Reason: "outcome of an earlier attempt is unknown and the destination has no deduplication or lookup policy"}
		}
	}
	return attemptDecision{Kind: decideUnresolved, Reason: fmt.Sprintf("unrecognised operation state %q", s.State)}
}

type beginInput struct {
	Op           operationRef
	AttemptID    string
	OperationKey string
	Intent       attemptIntent
}

type beginResult struct {
	Decision attemptDecision
	Snapshot opSnapshot
}

// Attempt outcomes recorded on finish.
const (
	outcomeResponded     = "responded"
	outcomeNotSent       = "not_sent"
	outcomeUnknown       = "unknown"
	outcomeFoundByLookup = "found_by_lookup"
)

type finishInput struct {
	Op         operationRef
	AttemptID  string
	Outcome    string
	State      string
	HTTPStatus int
	Receipt    json.RawMessage
	Error      string
}

// operationLedger persists operations and attempts.
type operationLedger interface {
	begin(ctx context.Context, in beginInput) (beginResult, error)
	finish(ctx context.Context, in finishInput) error
}

type pgOperationLedger struct{ db *pgxpool.Pool }

func (l pgOperationLedger) begin(ctx context.Context, in beginInput) (beginResult, error) {
	var res beginResult
	err := pgx.BeginFunc(ctx, l.db, func(tx pgx.Tx) error {
		op := in.Op
		if _, err := tx.Exec(ctx, `
            INSERT INTO operations (job_id, row_index, step_index, step_name, operation_key, payload_sha256, state)
            VALUES ($1, $2, $3, $4, $5, $6, 'pending')
            ON CONFLICT (job_id, row_index, step_index) DO NOTHING`,
			op.JobID, op.RowIndex, op.StepIndex, op.StepName, in.OperationKey, in.Intent.PayloadSHA256); err != nil {
			return err
		}
		var (
			s                      opSnapshot
			leaseAttempt, decision *string
			leaseUntil, firstSent  *time.Time
			receiptStatus          *int
		)
		if err := tx.QueryRow(ctx, `
            SELECT state, payload_sha256, attempts, lease_attempt, lease_until, first_sent_at,
                   receipt_status, receipt_json, now(), attempts > 0 OR state <> 'pending', decision_generation
            FROM operations WHERE job_id = $1 AND row_index = $2 AND step_index = $3
            FOR UPDATE`, op.JobID, op.RowIndex, op.StepIndex).Scan(
			&s.State, &s.PayloadSHA256, &s.Attempts, &leaseAttempt, &leaseUntil, &firstSent,
			&receiptStatus, &s.Receipt, &s.Now, &s.Exists, &decision); err != nil {
			return err
		}
		if leaseAttempt != nil {
			s.LeaseAttempt = *leaseAttempt
		}
		if decision != nil {
			s.DecisionGeneration = *decision
		}
		if leaseUntil != nil {
			s.LeaseUntil = *leaseUntil
		}
		if firstSent != nil {
			s.FirstSentAt = *firstSent
		}
		if receiptStatus != nil {
			s.ReceiptStatus = *receiptStatus
		}
		res = beginResult{Decision: decideAttempt(s, in.Intent), Snapshot: s}

		switch res.Decision.Kind {
		case decideBusy:
			return nil
		case decideReplay:
			_, err := tx.Exec(ctx, `
                INSERT INTO operation_attempts (job_id, attempt_id, row_index, step_index, attempt_no, finished_at, outcome, http_status)
                VALUES ($1, $2, $3, $4, $5, now(), 'replayed', $6)
                ON CONFLICT (job_id, attempt_id) DO NOTHING`,
				op.JobID, in.AttemptID, op.RowIndex, op.StepIndex, s.Attempts, zeroToNil(s.ReceiptStatus))
			return err
		case decideUnresolved:
			if _, err := tx.Exec(ctx, `
                UPDATE operations SET state = 'unresolved', lease_attempt = NULL, lease_until = NULL,
                       last_error = $4, updated_at = now()
                WHERE job_id = $1 AND row_index = $2 AND step_index = $3`,
				op.JobID, op.RowIndex, op.StepIndex, res.Decision.Reason); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
                INSERT INTO operation_attempts (job_id, attempt_id, row_index, step_index, attempt_no, finished_at, outcome, error)
                VALUES ($1, $2, $3, $4, $5, now(), 'refused', $6)
                ON CONFLICT (job_id, attempt_id) DO NOTHING`,
				op.JobID, in.AttemptID, op.RowIndex, op.StepIndex, s.Attempts, res.Decision.Reason)
			return err
		}
		// A send authorized by an operator decision records the generation
		// that spent it, so redelivery of that generation cannot spend it again.
		var spent *string
		if in.Intent.Generation != "" && (in.Intent.Resend || in.Intent.ConfirmNotApplied) {
			g := in.Intent.Generation
			spent = &g
		}
		if _, err := tx.Exec(ctx, `
            UPDATE operations SET state = 'sending', attempts = attempts + 1,
                   operation_key = $4, payload_sha256 = $5,
                   lease_attempt = $6, lease_until = now() + $7::interval,
                   first_sent_at = COALESCE(first_sent_at, now()), updated_at = now(),
                   decision_generation = COALESCE($8, decision_generation)
            WHERE job_id = $1 AND row_index = $2 AND step_index = $3`,
			op.JobID, op.RowIndex, op.StepIndex, in.OperationKey, in.Intent.PayloadSHA256,
			in.AttemptID, fmt.Sprintf("%d seconds", int(operationLease.Seconds())), spent); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
            INSERT INTO operation_attempts (job_id, attempt_id, row_index, step_index, attempt_no)
            VALUES ($1, $2, $3, $4, $5)
            ON CONFLICT (job_id, attempt_id) DO NOTHING`,
			op.JobID, in.AttemptID, op.RowIndex, op.StepIndex, s.Attempts+1)
		return err
	})
	return res, err
}

// errLeaseLost means another attempt took the operation over; this
// attempt's result was not recorded.
var errLeaseLost = errors.New("operation lease lost before the attempt finished")

func (l pgOperationLedger) finish(ctx context.Context, in finishInput) error {
	return pgx.BeginFunc(ctx, l.db, func(tx pgx.Tx) error {
		op := in.Op
		tag, err := tx.Exec(ctx, `
            UPDATE operations SET state = $5, lease_attempt = NULL, lease_until = NULL,
                   receipt_status = COALESCE($6, receipt_status),
                   receipt_json = CASE WHEN $5 = 'succeeded' THEN $7 ELSE receipt_json END,
                   last_error = $8, updated_at = now()
            WHERE job_id = $1 AND row_index = $2 AND step_index = $3 AND lease_attempt = $4`,
			op.JobID, op.RowIndex, op.StepIndex, in.AttemptID, in.State,
			zeroToNil(in.HTTPStatus), nullIfJSON(in.Receipt), nullIf(in.Error))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errLeaseLost
		}
		_, err = tx.Exec(ctx, `
            UPDATE operation_attempts SET finished_at = now(), outcome = $3, http_status = $4, error = $5
            WHERE job_id = $1 AND attempt_id = $2`,
			op.JobID, in.AttemptID, in.Outcome, zeroToNil(in.HTTPStatus), nullIf(in.Error))
		return err
	})
}

func payloadDigest(payload map[string]any, hasBody bool) string {
	raw := []byte("null")
	if hasBody || len(payload) > 0 {
		raw, _ = json.Marshal(payload)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

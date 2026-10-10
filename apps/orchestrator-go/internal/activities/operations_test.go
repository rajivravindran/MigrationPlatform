package activities

import (
	"testing"
	"time"

	"github.com/migration-platform/orchestrator/internal/template"
)

func TestDecideAttempt(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	op := func(state string) opSnapshot {
		return opSnapshot{Exists: true, State: state, PayloadSHA256: "p1", Now: now,
			FirstSentAt: now.Add(-time.Hour)}
	}
	intent := func(mode string) attemptIntent {
		return attemptIntent{PayloadSHA256: "p1", Mode: mode, RetentionHours: 24}
	}
	with := func(s opSnapshot, f func(*opSnapshot)) opSnapshot { f(&s); return s }
	withIntent := func(i attemptIntent, f func(*attemptIntent)) attemptIntent { f(&i); return i }

	cases := []struct {
		name string
		snap opSnapshot
		in   attemptIntent
		want decisionKind
	}{
		{"first attempt sends", opSnapshot{}, intent(template.DeliveryManual), decideSend},
		{"not sent before", op(opPending), intent(template.DeliveryManual), decideSend},
		{"refused before", op(opFailed), intent(template.DeliveryManual), decideSend},
		{"succeeded replays", op(opSucceeded), intent(template.DeliveryManual), decideReplay},
		{"succeeded with explicit resend sends", op(opSucceeded),
			withIntent(intent(template.DeliveryManual), func(i *attemptIntent) { i.Resend = true }), decideSend},
		{"succeeded with a changed payload is unresolved", op(opSucceeded),
			withIntent(intent(template.DeliveryIdempotent), func(i *attemptIntent) { i.PayloadSHA256 = "p2" }), decideUnresolved},
		{"live lease is busy", with(op(opSending), func(s *opSnapshot) { s.LeaseUntil = now.Add(time.Minute) }),
			intent(template.DeliveryIdempotent), decideBusy},
		{"live lease is busy even when confirmed", with(op(opSending), func(s *opSnapshot) { s.LeaseUntil = now.Add(time.Minute) }),
			withIntent(intent(template.DeliveryManual), func(i *attemptIntent) { i.ConfirmNotApplied = true }), decideBusy},
		{"expired lease, manual is unresolved", with(op(opSending), func(s *opSnapshot) { s.LeaseUntil = now.Add(-time.Second) }),
			intent(template.DeliveryManual), decideUnresolved},
		{"expired lease, idempotent re-sends", with(op(opSending), func(s *opSnapshot) { s.LeaseUntil = now }),
			intent(template.DeliveryIdempotent), decideSend},
		{"unknown, idempotent within retention re-sends", op(opUnknown), intent(template.DeliveryIdempotent), decideSend},
		{"unknown, idempotent past retention is unresolved",
			with(op(opUnknown), func(s *opSnapshot) { s.FirstSentAt = now.Add(-25 * time.Hour) }),
			intent(template.DeliveryIdempotent), decideUnresolved},
		{"unknown, lookup looks first", op(opUnknown), intent(template.DeliveryLookup), decideLookupThenSend},
		{"unknown, manual is unresolved", op(opUnknown), intent(template.DeliveryManual), decideUnresolved},
		{"unknown, changed payload is unresolved", op(opUnknown),
			withIntent(intent(template.DeliveryIdempotent), func(i *attemptIntent) { i.PayloadSHA256 = "p2" }), decideUnresolved},
		{"unknown, confirmed not applied sends", op(opUnknown),
			withIntent(intent(template.DeliveryManual), func(i *attemptIntent) { i.ConfirmNotApplied = true }), decideSend},
		{"unresolved stays unresolved", op(opUnresolved), intent(template.DeliveryIdempotent), decideUnresolved},
		{"unresolved, confirmed not applied sends", op(opUnresolved),
			withIntent(intent(template.DeliveryManual), func(i *attemptIntent) { i.ConfirmNotApplied = true }), decideSend},
		{"unrecognised state is unresolved", op("weird"), intent(template.DeliveryIdempotent), decideUnresolved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideAttempt(tc.snap, tc.in)
			if got.Kind != tc.want {
				t.Fatalf("decideAttempt = %v (%q), want %v", got.Kind, got.Reason, tc.want)
			}
			if got.Kind == decideUnresolved && got.Reason == "" {
				t.Fatal("an unresolved decision must say why")
			}
		})
	}
}

func TestDefaultDeliveryModeRefusesToGuessForCreates(t *testing.T) {
	for method, want := range map[string]string{
		"GET": template.DeliveryIdempotent, "PUT": template.DeliveryIdempotent, "DELETE": template.DeliveryIdempotent,
		"POST": template.DeliveryManual, "PATCH": template.DeliveryManual,
	} {
		if got := (template.Destination{Method: method}).EffectiveDeliveryMode(); got != want {
			t.Errorf("%s default mode = %s, want %s", method, got, want)
		}
	}
	declared := template.Destination{Method: "POST", Delivery: &template.Delivery{Mode: template.DeliveryIdempotent}}
	if got := declared.EffectiveDeliveryMode(); got != template.DeliveryIdempotent {
		t.Errorf("declared mode ignored: %s", got)
	}
}

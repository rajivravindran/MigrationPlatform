// Package template defines the Go representation of a rule template.
// The single source of truth is packages/rule-schema/schema/rule-template.schema.json;
// this file is kept in lockstep via the golden fixture round-trip test.
package template

import "encoding/json"

type SourceField struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type Source struct {
	Type        string                 `json:"type"`
	Schema      []SourceField          `json:"schema,omitempty"`
	Options     map[string]any         `json:"options,omitempty"`
	ConnectorID *int64                 `json:"connectorId,omitempty"`
}

type PreprocessStep struct {
	ID    string         `json:"id"`
	Field string         `json:"field"`
	Fn    string         `json:"fn"`
	Args  map[string]any `json:"args,omitempty"`
	Code  string         `json:"code,omitempty"`
}

type Auth struct {
	Type       string `json:"type"`
	SecretRef  string `json:"secretRef,omitempty"`
	HeaderName string `json:"headerName,omitempty"`
}

// Destination is the HTTP target for each row. URL/path/query/headers may be
// templated per row using the same `{"$from":"field"}` / `{"$literal":"..."}`
// sigils as the JSON body mapping.
type Destination struct {
	Type              string             `json:"type"`
	Method            string             `json:"method"`
	URL               string             `json:"url"`
	PathParams        map[string]any     `json:"pathParams,omitempty"`
	QueryParams       map[string]any     `json:"queryParams,omitempty"`
	Headers           map[string]any     `json:"headers,omitempty"`
	Auth              *Auth              `json:"auth,omitempty"`
	IdempotencyKey    json.RawMessage    `json:"idempotencyKey,omitempty"`
	IdempotencyHeader string             `json:"idempotencyHeader,omitempty"`
}

type Retry struct {
	MaxAttempts       int    `json:"maxAttempts,omitempty"`
	Backoff           string `json:"backoff,omitempty"`
	InitialIntervalMs int    `json:"initialIntervalMs,omitempty"`
}

// Step is one HTTP call in a multi-step chain. Later steps may reference
// earlier responses via {"$fromResponse":"stepName","path":"$.jsonpath"}.
type Step struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Mapping     map[string]any `json:"mapping,omitempty"`
	Destination Destination    `json:"destination"`
	// OnFailure controls chain behavior after this step definitively fails
	// (4xx or retries exhausted). "" or "stop" (default) ends the chain;
	// "continue" runs later steps. The row is still marked failed if any
	// step failed.
	OnFailure string `json:"onFailure,omitempty"`
}

// ContinuesOnFailure reports whether later steps should run after this step fails.
func (s *Step) ContinuesOnFailure() bool {
	return s.OnFailure == "continue"
}

// PayloadTemplate returns the step's payload mapping (may be nil for
// payload-less calls such as GET).
func (s *Step) PayloadTemplate() map[string]any {
	if s.Mapping == nil {
		return nil
	}
	if m, ok := s.Mapping["payload"].(map[string]any); ok {
		return m
	}
	return nil
}

type RuleTemplate struct {
	ID          string                 `json:"id"`
	Version     int                    `json:"version"`
	Name        string                 `json:"name"`
	Source      Source                 `json:"source"`
	Preprocess  []PreprocessStep       `json:"preprocess"`
	Mapping     map[string]any         `json:"mapping,omitempty"`
	Destination Destination            `json:"destination,omitempty"`
	Steps       []Step                 `json:"steps,omitempty"`
	Retry       *Retry                 `json:"retry,omitempty"`
	Concurrency int                    `json:"concurrency,omitempty"`
}

func (rt *RuleTemplate) PayloadTemplate() map[string]any {
	if m, ok := rt.Mapping["payload"].(map[string]any); ok {
		return m
	}
	return nil
}

// ExecutionSteps normalizes either template shape into an ordered step list.
// Single-destination templates become a one-step chain named "main".
func (rt *RuleTemplate) ExecutionSteps() []Step {
	if len(rt.Steps) > 0 {
		return rt.Steps
	}
	return []Step{{Name: "main", Mapping: rt.Mapping, Destination: rt.Destination}}
}

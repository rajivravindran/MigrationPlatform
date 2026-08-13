// Package batch implements archive manifest parsing and safe unpack for P2
// batch packages (tar.gz / zip with root-level manifest.json).
package batch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var safeStageID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Manifest is the root document required inside every batch archive.
type Manifest struct {
	Version        int             `json:"version"`
	BatchID        string          `json:"batchId,omitempty"`
	OnStageFailure string          `json:"onStageFailure,omitempty"` // stop | continue
	Stages         []ManifestStage `json:"stages"`
}

// ManifestStage is one step in a batch.
// When no stage declares dependsOn, stages run in array order (sequential).
// When any stage declares dependsOn, BatchWorkflow schedules a DAG (P4a).
type ManifestStage struct {
	ID             string   `json:"id"`
	File           string   `json:"file"`
	TemplateKey    string   `json:"templateKey"`
	OnStageFailure string   `json:"onStageFailure,omitempty"` // optional override
	DependsOn      []string `json:"dependsOn,omitempty"`      // optional stage ids (P4a)
}

// ParseManifest unmarshals and validates a batch manifest.
func ParseManifest(raw []byte) (*Manifest, error) {
	var m Manifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest json: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("manifest json: trailing content")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks required fields, path safety, and optional dependsOn DAG.
func (m *Manifest) Validate() error {
	if m.Version != 1 {
		return fmt.Errorf("unsupported manifest version %d (want 1)", m.Version)
	}
	if len(m.Stages) == 0 {
		return fmt.Errorf("manifest stages must be non-empty")
	}
	onFail := strings.ToLower(strings.TrimSpace(m.OnStageFailure))
	if onFail == "" {
		onFail = "stop"
		m.OnStageFailure = "stop"
	}
	if onFail != "stop" && onFail != "continue" {
		return fmt.Errorf("onStageFailure must be stop|continue, got %q", m.OnStageFailure)
	}
	m.OnStageFailure = onFail

	seen := map[string]struct{}{}
	for i, s := range m.Stages {
		if strings.TrimSpace(s.ID) == "" {
			return fmt.Errorf("stages[%d].id is required", i)
		}
		if !safeStageID.MatchString(s.ID) {
			return fmt.Errorf("stages[%d].id %q is not a safe path component", i, s.ID)
		}
		if _, ok := seen[s.ID]; ok {
			return fmt.Errorf("duplicate stage id %q", s.ID)
		}
		seen[s.ID] = struct{}{}
		if strings.TrimSpace(s.File) == "" {
			return fmt.Errorf("stages[%d].file is required", i)
		}
		if err := ValidateRelPath(s.File); err != nil {
			return fmt.Errorf("stages[%d].file: %w", i, err)
		}
		if strings.TrimSpace(s.TemplateKey) == "" {
			return fmt.Errorf("stages[%d].templateKey is required", i)
		}
		ov := strings.ToLower(strings.TrimSpace(s.OnStageFailure))
		if ov != "" && ov != "stop" && ov != "continue" {
			return fmt.Errorf("stages[%d].onStageFailure must be stop|continue", i)
		}
		if ov != "" {
			m.Stages[i].OnStageFailure = ov
		}
		// Normalize dependsOn: trim, drop empties.
		if len(s.DependsOn) > 0 {
			norm := make([]string, 0, len(s.DependsOn))
			depSeen := map[string]struct{}{}
			for j, d := range s.DependsOn {
				d = strings.TrimSpace(d)
				if d == "" {
					return fmt.Errorf("stages[%d].dependsOn[%d] is empty", i, j)
				}
				if d == s.ID {
					return fmt.Errorf("stages[%d] %q depends on itself", i, s.ID)
				}
				if _, ok := depSeen[d]; ok {
					continue
				}
				depSeen[d] = struct{}{}
				norm = append(norm, d)
			}
			m.Stages[i].DependsOn = norm
		}
	}

	if m.UsesDependsOn() {
		if err := validateDependsOnDAG(m.Stages); err != nil {
			return err
		}
	}
	return nil
}

// UsesDependsOn reports whether any stage declares a non-empty dependsOn list.
// When false, BatchWorkflow keeps ordered sequential execution.
func (m *Manifest) UsesDependsOn() bool {
	for _, s := range m.Stages {
		if len(s.DependsOn) > 0 {
			return true
		}
	}
	return false
}

func validateDependsOnDAG(stages []ManifestStage) error {
	ids := map[string]struct{}{}
	for _, s := range stages {
		ids[s.ID] = struct{}{}
	}
	for _, s := range stages {
		for _, d := range s.DependsOn {
			if _, ok := ids[d]; !ok {
				return fmt.Errorf("stage %q dependsOn unknown stage %q", s.ID, d)
			}
		}
	}
	// Kahn topological sort — residual nodes imply a cycle.
	indeg := map[string]int{}
	children := map[string][]string{}
	for _, s := range stages {
		if _, ok := indeg[s.ID]; !ok {
			indeg[s.ID] = 0
		}
		for _, d := range s.DependsOn {
			indeg[s.ID]++
			children[d] = append(children[d], s.ID)
		}
	}
	var queue []string
	for _, s := range stages {
		if indeg[s.ID] == 0 {
			queue = append(queue, s.ID)
		}
	}
	seen := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		seen++
		for _, c := range children[n] {
			indeg[c]--
			if indeg[c] == 0 {
				queue = append(queue, c)
			}
		}
	}
	if seen != len(stages) {
		var cyclic []string
		for _, s := range stages {
			if indeg[s.ID] > 0 {
				cyclic = append(cyclic, s.ID)
			}
		}
		return fmt.Errorf("dependsOn cycle involving stages: %s", strings.Join(cyclic, ", "))
	}
	return nil
}

// StageNode is a minimal view for DAG scheduling helpers.
type StageNode struct {
	ID        string
	DependsOn []string
}

// UsesDependsOnNodes is the list-form of Manifest.UsesDependsOn.
func UsesDependsOnNodes(nodes []StageNode) bool {
	for _, n := range nodes {
		if len(n.DependsOn) > 0 {
			return true
		}
	}
	return false
}

// StageRunStatus is a terminal or pending scheduling state for a stage.
type StageRunStatus string

const (
	StagePending   StageRunStatus = "pending"
	StageSucceeded StageRunStatus = "succeeded"
	StageFailed    StageRunStatus = "failed"
	StageSkipped   StageRunStatus = "skipped"
)

// ReadyStageIDs returns pending stages whose dependencies have all succeeded.
func ReadyStageIDs(nodes []StageNode, status map[string]StageRunStatus) []string {
	var ready []string
	for _, n := range nodes {
		if status[n.ID] != StagePending {
			continue
		}
		ok := true
		for _, d := range n.DependsOn {
			if status[d] != StageSucceeded {
				ok = false
				break
			}
		}
		if ok {
			ready = append(ready, n.ID)
		}
	}
	return ready
}

// DepsBlocking reports whether a pending stage should be skipped because a
// dependency failed or was skipped. Returns false when deps are still pending.
func DepsBlocking(n StageNode, status map[string]StageRunStatus) bool {
	for _, d := range n.DependsOn {
		switch status[d] {
		case StageFailed, StageSkipped:
			return true
		}
	}
	return false
}

// EffectiveOnFailure returns the stage override or batch default.
func (m *Manifest) EffectiveOnFailure(stage ManifestStage) string {
	if stage.OnStageFailure != "" {
		return stage.OnStageFailure
	}
	if m.OnStageFailure != "" {
		return m.OnStageFailure
	}
	return "stop"
}

// ValidateRelPath rejects absolute paths, parent traversal, and empty segments.
func ValidateRelPath(p string) error {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimSpace(p)
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, ":") {
		return fmt.Errorf("absolute or drive path not allowed: %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("unsafe path segment in %q", p)
		}
	}
	return nil
}

// IsArchive reports whether key basename looks like a supported batch package.
func IsArchive(key string) bool {
	base := strings.ToLower(key)
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return strings.HasSuffix(base, ".tar.gz") || strings.HasSuffix(base, ".tgz") || strings.HasSuffix(base, ".zip")
}

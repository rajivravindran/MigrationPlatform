// Package control holds the contract for operator control commands
// (pause/resume) shared by the workflow that applies them and the reconciler
// that converges persisted job status with workflow state (architecture
// review A3). It has no Temporal dependency so both sides can import it.
package control

// Signal and query names on MigrationWorkflow.
const (
	SignalPause     = "pause"
	SignalResume    = "resume"
	QueryPauseState = "pauseState"
)

// Command is the payload of pause/resume signals. CommandID is the stable
// identity of one operator request; redelivering it is harmless. Signals
// sent before command identities existed decode with an empty id.
type Command struct {
	CommandID string `json:"commandId,omitempty"`
}

// PauseState answers QueryPauseState.
type PauseState struct {
	Paused bool `json:"paused"`
	// LastCommandID is the id of the last pause/resume applied.
	LastCommandID string `json:"lastCommandId,omitempty"`
	// Acknowledged is true once the pause is effective: no further row is
	// dispatched until a resume. It is set between rows and between batches.
	Acknowledged bool `json:"acknowledged"`
}

// Requested control states persisted on jobs.control_requested.
const (
	RequestPause  = "pause"
	RequestResume = "resume"
)

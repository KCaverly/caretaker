package agent

// EventKind identifies a provider lifecycle event consumed by caretaker.
type EventKind string

const (
	ThreadStarted       EventKind = "thread/started"
	ThreadStatusChanged EventKind = "thread/status/changed"
	TurnStarted         EventKind = "turn/started"
	TurnCompleted       EventKind = "turn/completed"
	Error               EventKind = "error"
	Disconnected        EventKind = "disconnect"
)

// Event is the provider-neutral lifecycle state caretaker needs for session
// persistence, status badges, and attention notifications.
type Event struct {
	Kind EventKind

	ThreadID string
	TurnID   string
	Status   string

	WaitingOnApproval  bool
	WaitingOnUserInput bool

	Message string
	Err     error
}

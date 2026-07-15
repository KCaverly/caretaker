// Package agent defines the agent providers caretaker can host.
package agent

// Provider identifies the CLI that owns an agent conversation.
type Provider string

const (
	Claude Provider = "claude"
	Codex  Provider = "codex"
)

// Valid reports whether p is a provider caretaker knows how to launch.
func (p Provider) Valid() bool {
	switch p {
	case Claude, Codex:
		return true
	default:
		return false
	}
}

// String returns the provider's configuration and display name.
func (p Provider) String() string { return string(p) }

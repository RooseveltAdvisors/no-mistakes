package agent

import (
	"context"
	"fmt"
)

// labeledCandidate wraps a concrete backend agent with an operator-facing
// subscription candidate label (for example "kimi" or "grok"). Name() returns
// the label so fallback logs and attempt telemetry stay inspectable, while
// session provider matching still accepts the underlying backend name so
// review/fixer session continuity is preserved for session-capable adapters.
type labeledCandidate struct {
	inner Agent
	label string
}

// WithCandidateLabel returns an Agent that reports label from Name() while
// delegating all work to inner. Empty labels return inner unchanged.
func WithCandidateLabel(inner Agent, label string) Agent {
	if inner == nil || label == "" {
		return inner
	}
	return &labeledCandidate{inner: inner, label: label}
}

func (a *labeledCandidate) Name() string { return a.label }

func (a *labeledCandidate) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	result, err := a.inner.Run(ctx, opts)
	if result != nil && result.Provider == "" {
		// Persist the backend identity (codex/pi/...) so session resume keys
		// remain adapter-native rather than the operator label.
		result.Provider = a.inner.Name()
	}
	return result, err
}

func (a *labeledCandidate) Close() error { return a.inner.Close() }

func (a *labeledCandidate) SupportsSessionResume() bool {
	return SupportsSessionResume(a.inner)
}

func (a *labeledCandidate) SupportsSessionProvider(provider string) bool {
	// Session identities are always backend-native (codex/claude/...). The
	// operator label is for logs only, even when it happens to equal the
	// backend name (candidate name "codex" wrapping agent codex).
	return SupportsSessionProvider(a.inner, provider)
}

func (a *labeledCandidate) ReportsAgentAttempts() bool {
	return ReportsAgentAttempts(a.inner)
}

func (a *labeledCandidate) NeutralizesGateInstructions() bool {
	return NeutralizesGateInstructions(a.inner)
}

// CandidateLabel returns the operator label when a is a labeled candidate.
// Steering wrappers are unwrapped so production WithSteering(WithCandidateLabel(...))
// agents still expose the subscription label.
func CandidateLabel(a Agent) string {
	switch v := a.(type) {
	case *labeledCandidate:
		return v.label
	case steeredAgent:
		return CandidateLabel(v.Agent)
	default:
		return ""
	}
}

// BackendName returns the underlying backend agent name for a possibly labeled
// or steered candidate, or a.Name() otherwise.
func BackendName(a Agent) string {
	switch v := a.(type) {
	case *labeledCandidate:
		if v.inner != nil {
			return BackendName(v.inner)
		}
		return ""
	case steeredAgent:
		return BackendName(v.Agent)
	default:
		if a == nil {
			return ""
		}
		return a.Name()
	}
}

// DescribeAgent returns an inspectable name for logs.
func DescribeAgent(a Agent) string {
	if a == nil {
		return ""
	}
	if label := CandidateLabel(a); label != "" {
		return fmt.Sprintf("%s(%s)", label, BackendName(a))
	}
	return a.Name()
}

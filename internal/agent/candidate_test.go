package agent

import (
	"context"
	"errors"
	"testing"
)

func TestWithCandidateLabel_PreservesBackendSessionProvider(t *testing.T) {
	inner := &fallbackTestAgent{name: "codex", resumable: true, run: func() (*Result, error) {
		return &Result{Text: "ok", SessionID: "s1"}, nil
	}}
	ag := WithCandidateLabel(inner, "codex-sub")
	if ag.Name() != "codex-sub" {
		t.Fatalf("Name() = %q", ag.Name())
	}
	if !SupportsSessionResume(ag) {
		t.Fatal("expected session resume support")
	}
	if !SupportsSessionProvider(ag, "codex") {
		t.Fatal("expected backend provider codex to match")
	}
	if !SupportsSessionProvider(ag, "codex-sub") {
		t.Fatal("candidate label must identify its session")
	}
	same := WithCandidateLabel(inner, "codex")
	if !SupportsSessionProvider(same, "codex") {
		t.Fatal("label equal to backend name must still match the backend session provider")
	}
	res, err := ag.Run(context.Background(), RunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != "codex-sub" {
		t.Fatalf("Provider = %q, want codex-sub", res.Provider)
	}
	if got := DescribeAgent(ag); got != "codex-sub(codex)" {
		t.Fatalf("DescribeAgent = %q", got)
	}
	if BackendName(ag) != "codex" {
		t.Fatalf("BackendName = %q", BackendName(ag))
	}
}

func TestFallbackAgent_ResumesExactLabeledCandidate(t *testing.T) {
	first := &fallbackTestAgent{
		name:      "pi",
		resumable: true,
		run:       func() (*Result, error) { return nil, errors.New("agent start: unavailable") },
	}
	second := &fallbackTestAgent{
		name:      "pi",
		resumable: true,
		run:       func() (*Result, error) { return &Result{Text: "ok", SessionID: "grok-session"}, nil },
	}
	ag := NewFallback([]Agent{
		WithCandidateLabel(first, "kimi"),
		WithCandidateLabel(second, "grok"),
	})

	result, err := ag.Run(context.Background(), RunOpts{})
	if err != nil {
		t.Fatalf("initial fallback: %v", err)
	}
	if result.Provider != "grok" {
		t.Fatalf("initial provider = %q, want grok", result.Provider)
	}

	if _, err := ag.Run(context.Background(), RunOpts{Session: &SessionRef{ID: result.SessionID, Agent: result.Provider}}); err != nil {
		t.Fatalf("resume fallback: %v", err)
	}
	if first.calls != 1 {
		t.Fatalf("first candidate calls = %d, want 1", first.calls)
	}
	if second.calls != 2 {
		t.Fatalf("second candidate calls = %d, want 2", second.calls)
	}
}

func TestFallbackAgent_RejectsAmbiguousLegacyProvider(t *testing.T) {
	first := &fallbackTestAgent{name: "pi", resumable: true, run: func() (*Result, error) {
		return &Result{Text: "first"}, nil
	}}
	second := &fallbackTestAgent{name: "pi", resumable: true, run: func() (*Result, error) {
		return &Result{Text: "second"}, nil
	}}
	ag := NewFallback([]Agent{
		WithCandidateLabel(first, "kimi"),
		WithCandidateLabel(second, "grok"),
	})
	if SupportsSessionProvider(ag, "pi") {
		t.Fatal("ambiguous backend identity must not be resumable")
	}
	_, err := ag.Run(context.Background(), RunOpts{Session: &SessionRef{ID: "legacy", Agent: "pi"}})
	if err == nil || err.Error() != `session provider "pi" is ambiguous` {
		t.Fatalf("legacy resume error = %v", err)
	}
	if first.calls != 0 || second.calls != 0 {
		t.Fatalf("ambiguous resume invoked candidates: %d/%d", first.calls, second.calls)
	}
}

func TestFallbackAgent_RejectsLabelBackendCollision(t *testing.T) {
	label := &fallbackTestAgent{name: "codex", resumable: true, run: func() (*Result, error) {
		return &Result{Text: "label"}, nil
	}}
	backend := &fallbackTestAgent{name: "codex", resumable: true, run: func() (*Result, error) {
		return &Result{Text: "backend"}, nil
	}}
	ag := NewFallback([]Agent{
		WithCandidateLabel(label, "codex"),
		WithCandidateLabel(backend, "codex-fast"),
	})
	if SupportsSessionProvider(ag, "codex") {
		t.Fatal("label/backend collision must not be resumable")
	}
	_, err := ag.Run(context.Background(), RunOpts{Session: &SessionRef{ID: "legacy", Agent: "codex"}})
	if err == nil || err.Error() != `session provider "codex" is ambiguous` {
		t.Fatalf("collision resume error = %v", err)
	}
	if label.calls != 0 || backend.calls != 0 {
		t.Fatalf("collision resume invoked candidates: %d/%d", label.calls, backend.calls)
	}
}

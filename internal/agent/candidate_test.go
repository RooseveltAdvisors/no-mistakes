package agent

import (
	"context"
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
	// A label that is not the backend name is not a session provider unless the
	// backend itself would accept it.
	if SupportsSessionProvider(ag, "codex-sub") {
		t.Fatal("distinct label must not be treated as a session provider")
	}
	same := WithCandidateLabel(inner, "codex")
	if !SupportsSessionProvider(same, "codex") {
		t.Fatal("label equal to backend name must still match the backend session provider")
	}
	res, err := ag.Run(context.Background(), RunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", res.Provider)
	}
	if got := DescribeAgent(ag); got != "codex-sub(codex)" {
		t.Fatalf("DescribeAgent = %q", got)
	}
	if BackendName(ag) != "codex" {
		t.Fatalf("BackendName = %q", BackendName(ag))
	}
}

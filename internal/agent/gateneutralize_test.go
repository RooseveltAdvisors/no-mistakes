package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// optOutAgent builds an adapter with the trusted opt-out ON, mirroring how the
// daemon constructs gate agents when disable_project_settings=true.
func optOutAgent(t *testing.T, name types.AgentName, extraArgs []string) Agent {
	t.Helper()
	a, err := NewWithOptions(name, string(name), extraArgs, Options{DisableProjectSettings: true})
	if err != nil {
		t.Fatalf("NewWithOptions(%s): %v", name, err)
	}
	return a
}

// TestNeutralizesGateInstructions_OnlyVerifiedHarnessesUnderOptOut is the core
// fail-closed contract: under the opt-out, only codex, claude, and pi (whose
// suppression knobs are empirically verified) neutralize the target repo's
// project agent settings/instructions; every other harness reports false and is
// refused rather than launched with project instructions loaded.
func TestNeutralizesGateInstructions_OnlyVerifiedHarnessesUnderOptOut(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentCodex, types.AgentClaude, types.AgentPi} {
		if !NeutralizesGateInstructions(optOutAgent(t, name, nil)) {
			t.Errorf("%s must neutralize under the opt-out with its default knob", name)
		}
	}
	unverified := []types.AgentName{types.AgentOpenCode, types.AgentCopilot, types.AgentRovoDev}
	for _, name := range unverified {
		if NeutralizesGateInstructions(optOutAgent(t, name, nil)) {
			t.Errorf("%s has no verified knob; must NOT report neutralized", name)
		}
	}
	acp, err := NewWithOptions(types.AgentName("acp:some-target"), "acpx", nil, Options{DisableProjectSettings: true})
	if err != nil {
		t.Fatalf("acp NewWithOptions: %v", err)
	}
	if NeutralizesGateInstructions(acp) {
		t.Error("acp adapter must NOT report neutralized")
	}
	if NeutralizesGateInstructions(NewNoop()) {
		t.Error("noop agent must NOT report neutralized")
	}
	if NeutralizesGateInstructions(nil) {
		t.Error("nil agent must NOT report neutralized")
	}
}

// TestNeutralizesGateInstructions_FalseWithoutOptOut proves codex, claude, and pi
// do NOT claim neutralization when the repo did not opt out - the gate only consults
// this under the opt-out, but the value must be honest.
func TestNeutralizesGateInstructions_FalseWithoutOptOut(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentCodex, types.AgentClaude, types.AgentPi} {
		a, err := NewWithOptions(name, string(name), nil, Options{}) // no opt-out
		if err != nil {
			t.Fatalf("NewWithOptions(%s): %v", name, err)
		}
		if NeutralizesGateInstructions(a) {
			t.Errorf("%s must not report neutralized when the repo did not opt out", name)
		}
	}
}

// TestEnsureGateNeutralized_RefusesUnsupportedUnderOptOut proves the gate fails
// closed for an unsupported harness and admits codex, claude, and pi.
func TestEnsureGateNeutralized_RefusesUnsupportedUnderOptOut(t *testing.T) {
	if err := EnsureGateNeutralized(optOutAgent(t, types.AgentCodex, nil)); err != nil {
		t.Errorf("codex must pass the gate under opt-out: %v", err)
	}
	if err := EnsureGateNeutralized(optOutAgent(t, types.AgentClaude, nil)); err != nil {
		t.Errorf("claude must pass the gate under opt-out: %v", err)
	}
	if err := EnsureGateNeutralized(optOutAgent(t, types.AgentPi, nil)); err != nil {
		t.Errorf("pi must pass the gate under opt-out: %v", err)
	}
	err := EnsureGateNeutralized(optOutAgent(t, types.AgentOpenCode, nil))
	if err == nil {
		t.Fatal("opencode must be refused by the gate under opt-out")
	}
	if !strings.Contains(err.Error(), "does not neutralize") || !strings.Contains(err.Error(), "opencode") {
		t.Errorf("refusal error should name the harness and reason, got: %v", err)
	}
	if err := EnsureGateNeutralized(nil); err == nil {
		t.Error("a nil agent must be refused")
	}
}

// TestNeutralizesGateInstructions_ThroughProductionWrapping mirrors how the
// daemon builds the run agent (WithSteering per adapter, then NewFallback) and
// proves the capability propagates through both wrappers and fails closed if ANY
// fallback member is unverified.
func TestNeutralizesGateInstructions_ThroughProductionWrapping(t *testing.T) {
	if !NeutralizesGateInstructions(WithSteering(optOutAgent(t, types.AgentCodex, nil), "/evidence")) {
		t.Error("WithSteering(codex) must remain neutralized under opt-out")
	}
	if NeutralizesGateInstructions(WithSteering(optOutAgent(t, types.AgentOpenCode, nil), "/evidence")) {
		t.Error("WithSteering(opencode) must remain non-neutralized")
	}
	allVerified := NewFallback([]Agent{
		WithSteering(optOutAgent(t, types.AgentCodex, nil), "/evidence"),
		WithSteering(optOutAgent(t, types.AgentClaude, nil), "/evidence"),
	})
	if err := EnsureGateNeutralized(allVerified); err != nil {
		t.Errorf("fallback [codex, claude] must pass under opt-out: %v", err)
	}
	oneUnverified := NewFallback([]Agent{
		WithSteering(optOutAgent(t, types.AgentCodex, nil), "/evidence"),
		WithSteering(optOutAgent(t, types.AgentOpenCode, nil), "/evidence"),
	})
	if err := EnsureGateNeutralized(oneUnverified); err == nil {
		t.Error("fallback [codex, opencode] must be refused under opt-out")
	}
}

// TestNeutralizesGateInstructions_HonestOnEffectiveOverride proves the capability
// is honest about the EFFECTIVE knob value: preserving operator overrides are
// admitted, while available defeating overrides fail closed.
func TestNeutralizesGateInstructions_HonestOnEffectiveOverride(t *testing.T) {
	// codex: project_doc_max_bytes=0 preserves suppression -> admitted.
	if !NeutralizesGateInstructions(optOutAgent(t, types.AgentCodex, []string{"-c", "project_doc_max_bytes=0"})) {
		t.Error("codex with an explicit project_doc_max_bytes=0 must stay neutralized")
	}
	// codex: project_doc_max_bytes>0 re-enables the doc -> fails closed.
	if NeutralizesGateInstructions(optOutAgent(t, types.AgentCodex, []string{"-c", "project_doc_max_bytes=4096"})) {
		t.Error("codex with project_doc_max_bytes=4096 must fail closed")
	}
	if err := EnsureGateNeutralized(optOutAgent(t, types.AgentCodex, []string{"-c", "project_doc_max_bytes=4096"})); err == nil {
		t.Error("codex with the knob defeated must be refused by the gate")
	}
	// claude: --setting-sources user preserves suppression -> admitted.
	if !NeutralizesGateInstructions(optOutAgent(t, types.AgentClaude, []string{"--setting-sources", "user"})) {
		t.Error("claude with an explicit --setting-sources user must stay neutralized")
	}
	// claude: --setting-sources re-adding project -> fails closed.
	if NeutralizesGateInstructions(optOutAgent(t, types.AgentClaude, []string{"--setting-sources", "user,project"})) {
		t.Error("claude with --setting-sources user,project must fail closed")
	}
	if err := EnsureGateNeutralized(optOutAgent(t, types.AgentClaude, []string{"--setting-sources", "user,local"})); err == nil {
		t.Error("claude re-adding local must be refused by the gate")
	}
	// pi: explicit -nc/--no-context-files preserves suppression -> admitted.
	if !NeutralizesGateInstructions(optOutAgent(t, types.AgentPi, []string{"--no-context-files"})) {
		t.Error("pi with an explicit --no-context-files must stay neutralized")
	}
	if !NeutralizesGateInstructions(optOutAgent(t, types.AgentPi, []string{"-nc"})) {
		t.Error("pi with an explicit -nc must stay neutralized")
	}
}

// neutralizeStubAgent is a minimal Agent whose name and gate-neutralization
// capability are set directly, so FilterGateNeutralizing and
// ErrGateNeutralizationRefused can be tested without depending on any real
// adapter's knob.
type neutralizeStubAgent struct {
	name        string
	neutralizes bool
}

func (a *neutralizeStubAgent) Name() string { return a.name }

func (a *neutralizeStubAgent) Run(context.Context, RunOpts) (*Result, error) {
	return nil, errors.New("neutralizeStubAgent.Run not implemented")
}

func (a *neutralizeStubAgent) Close() error { return nil }

func (a *neutralizeStubAgent) NeutralizesGateInstructions() bool { return a.neutralizes }

// TestFilterGateNeutralizing_MixedListKeepsOnlyNeutralizingMembers proves a
// mixed fallback candidate list is split per member: neutralizing members
// survive into the fallback, the rest are refused individually rather than
// voiding the whole list. This is the fix for the defect where a fallback
// wrapper's NeutralizesGateInstructions failed closed over ALL members.
func TestFilterGateNeutralizing_MixedListKeepsOnlyNeutralizingMembers(t *testing.T) {
	pi := &neutralizeStubAgent{name: "pi", neutralizes: true}
	claude := &neutralizeStubAgent{name: "claude", neutralizes: true}
	codex := &neutralizeStubAgent{name: "codex", neutralizes: true}
	antigravity := &neutralizeStubAgent{name: "antigravity", neutralizes: false}

	neutralized, refused := FilterGateNeutralizing([]Agent{pi, claude, codex, antigravity})
	if len(neutralized) != 3 || len(refused) != 1 {
		t.Fatalf("got %d neutralized, %d refused; want 3 neutralized, 1 refused", len(neutralized), len(refused))
	}
	if refused[0].Name() != "antigravity" {
		t.Errorf("refused member = %q, want antigravity", refused[0].Name())
	}
	if fb := NewFallback(neutralized); !NeutralizesGateInstructions(fb) {
		t.Error("a fallback built from the filtered (all-neutralizing) slice must report neutralized")
	}
}

// TestFilterGateNeutralizing_AllUnverifiedRefusesNone proves an
// all-unverified candidate list yields zero neutralizing members - the
// signal callers use to still refuse the run when nothing survives the
// filter, preserving the fail-closed guarantee.
func TestFilterGateNeutralizing_AllUnverifiedRefusesNone(t *testing.T) {
	opencode := &neutralizeStubAgent{name: "opencode", neutralizes: false}
	copilot := &neutralizeStubAgent{name: "copilot", neutralizes: false}

	neutralized, refused := FilterGateNeutralizing([]Agent{opencode, copilot})
	if len(neutralized) != 0 || len(refused) != 2 {
		t.Fatalf("got %d neutralized, %d refused; want 0 neutralized, 2 refused", len(neutralized), len(refused))
	}
}

// TestErrGateNeutralizationRefused_NamesActualMembersNotFallbackFirst proves
// the refusal error names every non-neutralizing member directly from the
// refused slice, never a fallback wrapper's Name() (which forwards to
// members[0] regardless of which member actually lacks the knob). This is
// the exact misdirection the fix removes: a perfectly valid agent like "pi"
// could be named as the refusal reason while the real culprit was a
// different, later member.
func TestErrGateNeutralizationRefused_NamesActualMembersNotFallbackFirst(t *testing.T) {
	pi := &neutralizeStubAgent{name: "pi", neutralizes: true}
	antigravity := &neutralizeStubAgent{name: "antigravity", neutralizes: false}

	// Mirror the reported defect's ordering: pi is members[0], so the old
	// buggy path (EnsureGateNeutralized called on the unfiltered fallback)
	// would have named "pi" via fallbackAgent.Name(). Confirm that identity
	// first so the assertions below are meaningful.
	buggyFallback := NewFallback([]Agent{pi, antigravity})
	if buggyFallback.Name() != "pi" {
		t.Fatalf("setup invariant broken: fallback.Name() = %q, want pi", buggyFallback.Name())
	}

	_, refused := FilterGateNeutralizing([]Agent{pi, antigravity})
	err := ErrGateNeutralizationRefused(refused)
	if err == nil {
		t.Fatal("expected a refusal error")
	}
	if !strings.HasPrefix(err.Error(), "antigravity ") {
		t.Errorf("refusal error must lead with the actual non-neutralizing member antigravity, never the fallback's first member, got: %v", err)
	}
	if !strings.Contains(err.Error(), "antigravity") {
		t.Errorf("refusal error must name the actual non-neutralizing member antigravity, got: %v", err)
	}
}

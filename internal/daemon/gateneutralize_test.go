package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fakeLookPath makes every probed agent binary resolve, so agent resolution is
// deterministic and independent of what is installed on the test host.
func fakeLookPath(bin string) (string, error) { return "/fake/bin/" + bin, nil }

// TestNewPipelineAgent_OptOut_AdmitsVerifiedHarness proves that under the trusted
// opt-out (disable_project_settings=true), a verified harness passes the gate and
// its pipeline agent reports neutralized.
func TestNewPipelineAgent_OptOut_AdmitsVerifiedHarness(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentCodex, types.AgentClaude, types.AgentPi} {
		cfg := &config.Config{Agent: name, DisableProjectSettings: true}
		ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath)
		if err != nil {
			t.Fatalf("%s must pass under opt-out, got: %v", name, err)
		}
		if !agent.NeutralizesGateInstructions(ag) {
			t.Errorf("%s pipeline agent must report neutralized under opt-out", name)
		}
		_ = ag.Close()
	}
}

// TestNewPipelineAgent_OptOut_RefusesUnverifiedHarness is the captain-mandated
// fail-closed contract at the daemon wiring: under the opt-out, a harness with no
// verified neutralization knob is refused rather than launched with project
// instructions loaded.
func TestNewPipelineAgent_OptOut_RefusesUnverifiedHarness(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentOpenCode, types.AgentCopilot} {
		cfg := &config.Config{Agent: name, DisableProjectSettings: true}
		if _, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath); err == nil {
			t.Fatalf("%s must be refused under opt-out", name)
		} else if !strings.Contains(err.Error(), "does not neutralize") || !strings.Contains(err.Error(), string(name)) {
			t.Errorf("%s refusal should name the harness and reason, got: %v", name, err)
		}
	}
}

// TestNewPipelineAgent_NoOptOut_AdmitsEveryHarness is the backward-compat
// guarantee: when the repo did NOT opt out, every harness - including ones with
// no suppression knob - is admitted and runs exactly as before.
func TestNewPipelineAgent_NoOptOut_AdmitsEveryHarness(t *testing.T) {
	// rovodev is omitted: its resolution runs a real version probe that a fake
	// binary path cannot satisfy. opencode/pi/copilot already prove that an
	// unverified adapter is admitted when the repo did not opt out.
	for _, name := range []types.AgentName{types.AgentCodex, types.AgentClaude, types.AgentOpenCode, types.AgentPi, types.AgentCopilot} {
		cfg := &config.Config{Agent: name} // DisableProjectSettings defaults false
		ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath)
		if err != nil {
			t.Fatalf("%s must be admitted when the repo did not opt out, got: %v", name, err)
		}
		_ = ag.Close()
	}
}

// TestNewPipelineAgent_OptOut_RefusesDefeatedKnob proves the gate fails closed
// even for a verified harness when an operator override defeats its knob.
func TestNewPipelineAgent_OptOut_RefusesDefeatedKnob(t *testing.T) {
	cfg := &config.Config{
		Agent:                  types.AgentCodex,
		DisableProjectSettings: true,
		AgentArgsOverride:      map[string][]string{"codex": {"-c", "project_doc_max_bytes=8192"}},
	}
	if _, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath); err == nil {
		t.Fatal("codex with its knob overridden must be refused under opt-out")
	} else if !strings.Contains(err.Error(), "does not neutralize") {
		t.Errorf("refusal should explain the reason, got: %v", err)
	}
}

// TestNewPipelineAgent_OptOut_FallbackOfOnlyVerifiedMembersRuns proves an
// ordered fallback list of exclusively verified harnesses passes under
// opt-out.
func TestNewPipelineAgent_OptOut_FallbackOfOnlyVerifiedMembersRuns(t *testing.T) {
	cfg := &config.Config{Agents: []types.AgentName{types.AgentCodex, types.AgentClaude}, DisableProjectSettings: true}
	if ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath); err != nil {
		t.Fatalf("a fallback list of only verified harnesses must pass under opt-out, got: %v", err)
	} else {
		_ = ag.Close()
	}
}

// TestNewPipelineAgent_OptOut_MixedFallbackRunsFilteringUnverifiedMembers is
// the regression test for the gate-fallback filter defect: a fallback list
// containing a non-neutralizing member (opencode, alongside pi/claude/codex)
// must still run under opt-out, with the unverified member filtered out
// rather than voiding the whole run. Before the fix, EnsureGateNeutralized
// was called on the already-built fallback wrapper, whose
// NeutralizesGateInstructions fails closed over the WHOLE member set - a
// single unverified member refused every member, including the verified
// ones.
func TestNewPipelineAgent_OptOut_MixedFallbackRunsFilteringUnverifiedMembers(t *testing.T) {
	cfg := &config.Config{
		Agents:                 []types.AgentName{types.AgentPi, types.AgentClaude, types.AgentCodex, types.AgentOpenCode},
		DisableProjectSettings: true,
	}
	ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath)
	if err != nil {
		t.Fatalf("a mixed fallback list must still run under opt-out by filtering the unverified member, got: %v", err)
	}
	defer func() { _ = ag.Close() }()
	if !agent.NeutralizesGateInstructions(ag) {
		t.Error("the filtered fallback must report neutralized")
	}
}

// TestNewPipelineAgent_OptOut_AllUnverifiedFallbackRefuses proves the
// fail-closed property holds absolutely: when NO member of the fallback list
// has a verified knob, the run is still refused rather than launched with any
// of them.
func TestNewPipelineAgent_OptOut_AllUnverifiedFallbackRefuses(t *testing.T) {
	cfg := &config.Config{
		Agents:                 []types.AgentName{types.AgentOpenCode, types.AgentCopilot},
		DisableProjectSettings: true,
	}
	if _, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath); err == nil {
		t.Fatal("an all-unverified fallback list must be refused under opt-out")
	}
}

// TestNewPipelineAgent_OptOut_RefusalNamesActualUnverifiedMembers proves the
// refusal error names the non-neutralizing members themselves, not a fallback
// wrapper's first member. fallbackAgent.Name() forwards to members[0]
// regardless of which member actually lacks the knob, which is exactly the
// misdirection reported against the defect (the error named "pi", a
// perfectly valid agent, while the actual non-neutralizing member was later
// in the list).
func TestNewPipelineAgent_OptOut_RefusalNamesActualUnverifiedMembers(t *testing.T) {
	cfg := &config.Config{
		Agents:                 []types.AgentName{types.AgentOpenCode, types.AgentCopilot},
		DisableProjectSettings: true,
	}
	_, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath)
	if err == nil {
		t.Fatal("expected a refusal error")
	}
	if !strings.Contains(err.Error(), string(types.AgentOpenCode)) || !strings.Contains(err.Error(), string(types.AgentCopilot)) {
		t.Errorf("refusal error should name every non-neutralizing member, got: %v", err)
	}
}

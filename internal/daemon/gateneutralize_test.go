package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/quota"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fakeLookPath makes every probed agent binary resolve, so agent resolution is
// deterministic and independent of what is installed on the test host.
func fakeLookPath(bin string) (string, error) { return "/fake/bin/" + bin, nil }

// TestNewPipelineAgent_OptOut_AdmitsVerifiedHarness proves that under the trusted
// opt-out (disable_project_settings=true), a verified harness passes the gate and
// its pipeline agent reports neutralized.
func TestNewPipelineAgent_OptOut_AdmitsVerifiedHarness(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentCodex, types.AgentClaude} {
		cfg := &config.Config{Agent: name, DisableProjectSettings: true}
		ag, err := newPipelineAgent(context.Background(), cfg, fakeLookPath)
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
	for _, name := range []types.AgentName{types.AgentOpenCode, types.AgentPi, types.AgentCopilot} {
		cfg := &config.Config{Agent: name, DisableProjectSettings: true}
		if _, err := newPipelineAgent(context.Background(), cfg, fakeLookPath); err == nil {
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
		ag, err := newPipelineAgent(context.Background(), cfg, fakeLookPath)
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
	if _, err := newPipelineAgent(context.Background(), cfg, fakeLookPath); err == nil {
		t.Fatal("codex with its knob overridden must be refused under opt-out")
	} else if !strings.Contains(err.Error(), "does not neutralize") {
		t.Errorf("refusal should explain the reason, got: %v", err)
	}
}

// TestNewPipelineAgent_OptOut_FallbackRefusesAnyUnverifiedMember proves an
// ordered fallback list fails closed under opt-out if any member is unverified.
func TestNewPipelineAgent_OptOut_FallbackRefusesAnyUnverifiedMember(t *testing.T) {
	cfg := &config.Config{Agents: []types.AgentName{types.AgentCodex, types.AgentOpenCode}, DisableProjectSettings: true}
	if _, err := newPipelineAgent(context.Background(), cfg, fakeLookPath); err == nil {
		t.Fatal("a fallback list containing an unverified harness must be refused under opt-out")
	}
	cfg = &config.Config{Agents: []types.AgentName{types.AgentCodex, types.AgentClaude}, DisableProjectSettings: true}
	if ag, err := newPipelineAgent(context.Background(), cfg, fakeLookPath); err != nil {
		t.Fatalf("a fallback list of only verified harnesses must pass under opt-out, got: %v", err)
	} else {
		_ = ag.Close()
	}
}

// TestNewPipelineAgent_SubscriptionRouteBuildsLabeledCandidates proves run-start
// subscription routing constructs ordered labeled backends with per-candidate args
// and never requires Claude when it is absent from the route.
func TestNewPipelineAgent_SubscriptionRouteBuildsLabeledCandidates(t *testing.T) {
	rem := 90.0
	cfg := &config.Config{
		Agent: types.AgentSubscription,
		SubscriptionAgents: config.SubscriptionAgentsConfig{
			Candidates: []config.SubscriptionCandidate{
				{Name: "kimi", Agent: types.AgentPi, QuotaProvider: "kimi", Provider: "kimi-coding", Model: "k3"},
				{Name: "codex", Agent: types.AgentCodex, QuotaProvider: "codex"},
			},
		},
		QuotaFetch: func(context.Context, string, []string) (*quota.Snapshot, error) {
			return &quota.Snapshot{
				SchemaVersion: 3,
				Providers: []quota.ProviderSnapshot{
					{
						Provider: "claude",
						State:    quota.ProviderState{Status: "fresh"},
						QuotaSemantics: quota.QuotaSemantics{
							Status: "known",
							EffectiveAvailability: []quota.EffectiveAvailability{{
								Scope: "all_models", Status: "known", EffectivePercentRemaining: float64Ptr(99), Pace: &quota.Pace{Status: "behind"},
							}},
						},
					},
					{
						Provider: "kimi",
						State:    quota.ProviderState{Status: "fresh"},
						QuotaSemantics: quota.QuotaSemantics{
							Status: "known",
							EffectiveAvailability: []quota.EffectiveAvailability{{
								Scope: "all_models", Status: "known", EffectivePercentRemaining: float64Ptr(40), Pace: &quota.Pace{Status: "on_pace"},
							}},
						},
					},
					{
						Provider: "codex",
						State:    quota.ProviderState{Status: "fresh"},
						QuotaSemantics: quota.QuotaSemantics{
							Status: "known",
							EffectiveAvailability: []quota.EffectiveAvailability{{
								Scope: "all_models", Status: "known", EffectivePercentRemaining: &rem, Pace: &quota.Pace{Status: "behind"},
							}},
						},
					},
				},
			}, nil
		},
	}
	ag, err := newPipelineAgent(context.Background(), cfg, fakeLookPath)
	if err != nil {
		t.Fatalf("newPipelineAgent: %v", err)
	}
	defer ag.Close()
	if ag.Name() != "codex" {
		t.Fatalf("fallback Name() = %q, want codex label", ag.Name())
	}
	if cfg.SubscriptionRoute == nil || len(cfg.SubscriptionRoute.Ordered) != 2 {
		t.Fatalf("route = %+v", cfg.SubscriptionRoute)
	}
	if cfg.SubscriptionRoute.Ordered[0].Name != "codex" || cfg.SubscriptionRoute.Ordered[1].Name != "kimi" {
		t.Fatalf("ordered = %+v", cfg.SubscriptionRoute.Ordered)
	}
	for _, d := range cfg.SubscriptionRoute.Decisions {
		if d.Name == "claude" {
			t.Fatalf("claude entered route decisions: %+v", d)
		}
	}
	if !agent.SupportsSessionProvider(ag, "codex") {
		t.Fatal("subscription codex candidate must still expose backend session provider")
	}
	if agent.SupportsSessionProvider(ag, "claude") {
		t.Fatal("claude must not appear as a session provider when absent from the route")
	}
}

func float64Ptr(v float64) *float64 { return &v }

// subscriptionCfgWithCodexPrimary builds the reported production shape: agent
// subscription ranks native codex ahead of Pi-backed Kimi/Grok. Quota is forced
// so codex wins while the unverified Pi candidates remain on the ordered route.
func subscriptionCfgWithCodexPrimary(optOut bool) *config.Config {
	codexRem := 95.0
	kimiRem := 40.0
	grokRem := 30.0
	return &config.Config{
		Agent:                  types.AgentSubscription,
		DisableProjectSettings: optOut,
		SubscriptionAgents: config.SubscriptionAgentsConfig{
			Candidates: []config.SubscriptionCandidate{
				{Name: "kimi", Agent: types.AgentPi, QuotaProvider: "kimi", Provider: "kimi-coding", Model: "k3"},
				{Name: "codex", Agent: types.AgentCodex, QuotaProvider: "codex"},
				{Name: "grok", Agent: types.AgentPi, QuotaProvider: "grok", Provider: "xai", Model: "grok-4.5"},
			},
		},
		QuotaFetch: func(context.Context, string, []string) (*quota.Snapshot, error) {
			return &quota.Snapshot{
				SchemaVersion: 3,
				Providers: []quota.ProviderSnapshot{
					{
						Provider: "kimi",
						State:    quota.ProviderState{Status: "fresh"},
						QuotaSemantics: quota.QuotaSemantics{
							Status: "known",
							EffectiveAvailability: []quota.EffectiveAvailability{{
								Scope: "all_models", Status: "known", EffectivePercentRemaining: &kimiRem, Pace: &quota.Pace{Status: "on_pace"},
							}},
						},
					},
					{
						Provider: "codex",
						State:    quota.ProviderState{Status: "fresh"},
						QuotaSemantics: quota.QuotaSemantics{
							Status: "known",
							EffectiveAvailability: []quota.EffectiveAvailability{{
								Scope: "all_models", Status: "known", EffectivePercentRemaining: &codexRem, Pace: &quota.Pace{Status: "behind"},
							}},
						},
					},
					{
						Provider: "grok",
						State:    quota.ProviderState{Status: "fresh"},
						QuotaSemantics: quota.QuotaSemantics{
							Status: "known",
							EffectiveAvailability: []quota.EffectiveAvailability{{
								Scope: "all_models", Status: "known", EffectivePercentRemaining: &grokRem, Pace: &quota.Pace{Status: "on_pace"},
							}},
						},
					},
				},
			}, nil
		},
	}
}

// TestNewPipelineAgent_SubscriptionOptOut_KeepsNativeCodexAndDropsUnverifiedPi is
// the reported firstmate regression: disable_project_settings + AGENTS.md must
// not block a subscription route whose selected concrete backend is native
// codex. Unverified Pi-backed Kimi/Grok candidates are dropped from the launch
// set rather than poisoning the whole route or pretending they neutralize.
func TestNewPipelineAgent_SubscriptionOptOut_KeepsNativeCodexAndDropsUnverifiedPi(t *testing.T) {
	cfg := subscriptionCfgWithCodexPrimary(true)
	ag, err := newPipelineAgent(context.Background(), cfg, fakeLookPath)
	if err != nil {
		t.Fatalf("subscription route with native codex under opt-out must pass, got: %v", err)
	}
	defer ag.Close()
	if !agent.NeutralizesGateInstructions(ag) {
		t.Fatal("subscription-selected native codex must report neutralized under opt-out")
	}
	if ag.Name() != "codex" {
		t.Fatalf("Name() = %q, want codex label", ag.Name())
	}
	if cfg.SubscriptionRoute == nil || len(cfg.SubscriptionRoute.Ordered) != 1 {
		t.Fatalf("opt-out must keep only neutralizing candidates on the launch route, got %+v", cfg.SubscriptionRoute)
	}
	if cfg.SubscriptionRoute.Ordered[0].Name != "codex" || cfg.SubscriptionRoute.Ordered[0].Agent != types.AgentCodex {
		t.Fatalf("remaining route entry = %+v, want native codex", cfg.SubscriptionRoute.Ordered[0])
	}
	if agent.SupportsSessionProvider(ag, "pi") {
		t.Fatal("dropped Pi candidates must not remain as session providers under opt-out")
	}
	if !agent.SupportsSessionProvider(ag, "codex") {
		t.Fatal("native codex session provider must survive subscription labeling")
	}
}

// TestNewPipelineAgent_SubscriptionOptOut_RefusesWhenOnlyUnverifiedCandidatesRemain
// proves Pi-only subscription routes fail closed under the opt-out with an
// accurate refusal (no contradictory "set agent: codex" while already on a route
// that cannot neutralize).
func TestNewPipelineAgent_SubscriptionOptOut_RefusesWhenOnlyUnverifiedCandidatesRemain(t *testing.T) {
	kimiRem := 80.0
	grokRem := 70.0
	cfg := &config.Config{
		Agent:                  types.AgentSubscription,
		DisableProjectSettings: true,
		SubscriptionAgents: config.SubscriptionAgentsConfig{
			Candidates: []config.SubscriptionCandidate{
				{Name: "kimi", Agent: types.AgentPi, QuotaProvider: "kimi", Provider: "kimi-coding", Model: "k3"},
				{Name: "grok", Agent: types.AgentPi, QuotaProvider: "grok", Provider: "xai", Model: "grok-4.5"},
			},
		},
		QuotaFetch: func(context.Context, string, []string) (*quota.Snapshot, error) {
			return &quota.Snapshot{
				SchemaVersion: 3,
				Providers: []quota.ProviderSnapshot{
					{
						Provider: "kimi",
						State:    quota.ProviderState{Status: "fresh"},
						QuotaSemantics: quota.QuotaSemantics{
							Status: "known",
							EffectiveAvailability: []quota.EffectiveAvailability{{
								Scope: "all_models", Status: "known", EffectivePercentRemaining: &kimiRem, Pace: &quota.Pace{Status: "behind"},
							}},
						},
					},
					{
						Provider: "grok",
						State:    quota.ProviderState{Status: "fresh"},
						QuotaSemantics: quota.QuotaSemantics{
							Status: "known",
							EffectiveAvailability: []quota.EffectiveAvailability{{
								Scope: "all_models", Status: "known", EffectivePercentRemaining: &grokRem, Pace: &quota.Pace{Status: "on_pace"},
							}},
						},
					},
				},
			}, nil
		},
	}
	_, err := newPipelineAgent(context.Background(), cfg, fakeLookPath)
	if err == nil {
		t.Fatal("Pi-only subscription route must be refused under disable_project_settings")
	}
	msg := err.Error()
	for _, want := range []string{"does not neutralize", "kimi", "grok", "pi"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must mention %q, got: %v", want, err)
		}
	}
	// Must not claim the selected agent is already a verified harness while
	// simultaneously telling the operator to set agent: codex/claude as if the
	// concrete backend were already codex.
	if strings.Contains(msg, `gate agent "kimi" does not neutralize`) && strings.Contains(msg, "set 'agent' to codex or claude") {
		t.Errorf("refusal must not contradict itself about the selected agent, got: %v", err)
	}
}

// TestNewPipelineAgent_SubscriptionNoOptOut_KeepsUnverifiedFallbacks is the
// backward-compat guarantee: without disable_project_settings, Pi candidates
// remain on the subscription fallback route exactly as before.
func TestNewPipelineAgent_SubscriptionNoOptOut_KeepsUnverifiedFallbacks(t *testing.T) {
	cfg := subscriptionCfgWithCodexPrimary(false)
	ag, err := newPipelineAgent(context.Background(), cfg, fakeLookPath)
	if err != nil {
		t.Fatalf("subscription route without opt-out must pass, got: %v", err)
	}
	defer ag.Close()
	if cfg.SubscriptionRoute == nil || len(cfg.SubscriptionRoute.Ordered) != 3 {
		t.Fatalf("without opt-out the full ranked route must remain, got %+v", cfg.SubscriptionRoute)
	}
	want := []string{"codex", "kimi", "grok"}
	for i, name := range want {
		if cfg.SubscriptionRoute.Ordered[i].Name != name {
			t.Fatalf("ordered[%d] = %q, want %q (route=%+v)", i, cfg.SubscriptionRoute.Ordered[i].Name, name, cfg.SubscriptionRoute.Ordered)
		}
	}
}

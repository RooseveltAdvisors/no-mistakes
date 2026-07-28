package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/quota"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadGlobal_SubscriptionAgentsCandidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `
agent: subscription
subscription_agents:
  quota_axi_path: /usr/bin/quota-axi
  candidates:
    - name: kimi
      agent: pi
      quota_provider: kimi
      provider: kimi-coding
      model: k3
    - name: codex
      agent: codex
      quota_provider: codex
    - name: grok
      agent: pi
      quota_provider: grok
      args:
        - --provider
        - xai
        - --model
        - grok-4.5
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.Agent != types.AgentSubscription {
		t.Fatalf("Agent = %q", cfg.Agent)
	}
	if cfg.SubscriptionAgents.QuotaAXIPath != "/usr/bin/quota-axi" {
		t.Fatalf("QuotaAXIPath = %q", cfg.SubscriptionAgents.QuotaAXIPath)
	}
	if len(cfg.SubscriptionAgents.Candidates) != 3 {
		t.Fatalf("candidates = %d", len(cfg.SubscriptionAgents.Candidates))
	}
	kimi := cfg.SubscriptionAgents.Candidates[0]
	if kimi.Name != "kimi" || kimi.Agent != types.AgentPi || kimi.QuotaProvider != "kimi" {
		t.Fatalf("kimi = %+v", kimi)
	}
	args := kimi.EffectiveArgs(nil)
	if strings.Join(args, " ") != "--provider kimi-coding --model k3" {
		t.Fatalf("kimi args = %v", args)
	}
	grok := cfg.SubscriptionAgents.Candidates[2]
	if strings.Join(grok.EffectiveArgs(nil), " ") != "--provider xai --model grok-4.5" {
		t.Fatalf("grok args = %v", grok.EffectiveArgs(nil))
	}
}

func TestLoadGlobal_SubscriptionAgentsRejectsDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `
agent: subscription
subscription_agents:
  candidates:
    - name: kimi
      agent: pi
      quota_provider: kimi
    - name: kimi
      agent: codex
      quota_provider: codex
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(path); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("error = %v, want duplicated", err)
	}
}

func TestLoadGlobal_LegacyAgentUnchangedWithoutSubscriptionBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agent: [codex, claude]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Fatalf("Agent = %q", cfg.Agent)
	}
	if len(cfg.Agents) != 2 || cfg.Agents[1] != types.AgentClaude {
		t.Fatalf("Agents = %v", cfg.Agents)
	}
	if len(cfg.SubscriptionAgents.Candidates) != 0 {
		t.Fatalf("unexpected candidates: %+v", cfg.SubscriptionAgents.Candidates)
	}
}

func TestResolveAgent_SubscriptionRanksByQuotaAndExcludesClaude(t *testing.T) {
	cfg := &Config{
		Agent: types.AgentSubscription,
		SubscriptionAgents: SubscriptionAgentsConfig{
			Candidates: []SubscriptionCandidate{
				{Name: "kimi", Agent: types.AgentPi, QuotaProvider: "kimi", Provider: "kimi-coding", Model: "k3"},
				{Name: "codex", Agent: types.AgentCodex, QuotaProvider: "codex"},
				{Name: "grok", Agent: types.AgentPi, QuotaProvider: "grok", Provider: "xai", Model: "grok-4.5"},
			},
		},
		QuotaFetch: func(context.Context, string, []string) (*quota.Snapshot, error) {
			return &quota.Snapshot{
				SchemaVersion: 3,
				Providers: []quota.ProviderSnapshot{
					knownSnap("claude", 99, "behind"),
					knownSnap("kimi", 40, "on_pace"),
					knownSnap("codex", 85, "behind"),
					knownSnap("grok", 70, "behind"),
				},
			}, nil
		},
	}
	lookPath := func(bin string) (string, error) {
		switch bin {
		case "pi", "codex", "claude", "quota-axi":
			return "/bin/" + bin, nil
		default:
			return "", os.ErrNotExist
		}
	}
	if err := cfg.ResolveAgent(context.Background(), lookPath); err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
	if cfg.SubscriptionRoute == nil {
		t.Fatal("expected SubscriptionRoute")
	}
	got := make([]string, 0, len(cfg.SubscriptionRoute.Ordered))
	for _, rc := range cfg.SubscriptionRoute.Ordered {
		got = append(got, rc.Name)
	}
	want := []string{"codex", "grok", "kimi"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("route = %v, want %v (%s)", got, want, cfg.SubscriptionRoute.Summary)
	}
	for _, d := range cfg.SubscriptionRoute.Decisions {
		if d.Name == "claude" || d.QuotaProvider == "claude" {
			t.Fatalf("claude entered decisions: %+v", d)
		}
	}
	if cfg.Agent != types.AgentCodex {
		t.Fatalf("primary backend Agent = %q, want codex", cfg.Agent)
	}
	// kimi/grok keep distinct pi args
	var kimiArgs, grokArgs []string
	for _, rc := range cfg.SubscriptionRoute.Ordered {
		switch rc.Name {
		case "kimi":
			kimiArgs = rc.Args
		case "grok":
			grokArgs = rc.Args
		}
	}
	if strings.Join(kimiArgs, " ") != "--provider kimi-coding --model k3" {
		t.Fatalf("kimi args = %v", kimiArgs)
	}
	if strings.Join(grokArgs, " ") != "--provider xai --model grok-4.5" {
		t.Fatalf("grok args = %v", grokArgs)
	}
}

func TestResolveAgent_SubscriptionUnknownQuotaFallsBackToConfigOrder(t *testing.T) {
	cfg := &Config{
		Agent: types.AgentSubscription,
		SubscriptionAgents: SubscriptionAgentsConfig{
			Candidates: []SubscriptionCandidate{
				{Name: "kimi", Agent: types.AgentPi, QuotaProvider: "kimi"},
				{Name: "codex", Agent: types.AgentCodex, QuotaProvider: "codex"},
			},
		},
		QuotaFetch: func(context.Context, string, []string) (*quota.Snapshot, error) {
			return nil, errors.New("quota-axi exploded")
		},
	}
	lookPath := func(bin string) (string, error) {
		return "/bin/" + bin, nil
	}
	if err := cfg.ResolveAgent(context.Background(), lookPath); err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
	if cfg.SubscriptionRoute.Ordered[0].Name != "kimi" {
		t.Fatalf("primary = %s", cfg.SubscriptionRoute.Ordered[0].Name)
	}
	if !strings.Contains(cfg.SubscriptionRoute.Summary, "snapshot unavailable") {
		t.Fatalf("summary = %s", cfg.SubscriptionRoute.Summary)
	}
}

func TestResolveAgent_SubscriptionRefusesWhenNoCandidatesRunnable(t *testing.T) {
	cfg := &Config{
		Agent: types.AgentSubscription,
		SubscriptionAgents: SubscriptionAgentsConfig{
			Candidates: []SubscriptionCandidate{
				{Name: "kimi", Agent: types.AgentPi, QuotaProvider: "kimi"},
			},
		},
		QuotaFetch: func(context.Context, string, []string) (*quota.Snapshot, error) {
			return &quota.Snapshot{SchemaVersion: 3}, nil
		},
	}
	lookPath := func(string) (string, error) { return "", os.ErrNotExist }
	err := cfg.ResolveAgent(context.Background(), lookPath)
	if err == nil || !strings.Contains(err.Error(), "no runnable subscription_agents") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveAgent_SubscriptionMissingConfig(t *testing.T) {
	cfg := &Config{Agent: types.AgentSubscription}
	err := cfg.ResolveAgent(context.Background(), func(string) (string, error) { return "/x", nil })
	if err == nil || !strings.Contains(err.Error(), "subscription_agents.candidates") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveAgent_SubscriptionNotMixedIntoFallbackList(t *testing.T) {
	cfg := &Config{Agents: []types.AgentName{types.AgentSubscription, types.AgentCodex}}
	err := cfg.ResolveAgent(context.Background(), func(string) (string, error) { return "/x", nil })
	if err == nil || !strings.Contains(err.Error(), "cannot appear in an ordered fallback list") {
		t.Fatalf("error = %v", err)
	}
}

func TestMerge_RepoAgentOverridesSubscriptionMode(t *testing.T) {
	global := &GlobalConfig{
		Agent: types.AgentSubscription,
		SubscriptionAgents: SubscriptionAgentsConfig{
			Candidates: []SubscriptionCandidate{
				{Name: "kimi", Agent: types.AgentPi, QuotaProvider: "kimi"},
			},
		},
	}
	repo := &RepoConfig{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex}}
	cfg := Merge(global, repo)
	if cfg.Agent != types.AgentCodex {
		t.Fatalf("Agent = %q", cfg.Agent)
	}
	// Global candidates remain available if a later resolve uses subscription,
	// but the effective agent is the repo override.
	if len(cfg.SubscriptionAgents.Candidates) != 1 {
		t.Fatalf("candidates not copied: %+v", cfg.SubscriptionAgents)
	}
}

func knownSnap(name string, remaining float64, pace string) quota.ProviderSnapshot {
	r := remaining
	return quota.ProviderSnapshot{
		Provider: name,
		State:    quota.ProviderState{Status: "fresh"},
		QuotaSemantics: quota.QuotaSemantics{
			Status: "known",
			EffectiveAvailability: []quota.EffectiveAvailability{{
				Scope:                     "all_models",
				Status:                    "known",
				EffectivePercentRemaining: &r,
				Pace:                      &quota.Pace{Status: pace},
			}},
		},
	}
}

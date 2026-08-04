package quota

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func floatPtr(v float64) *float64 { return &v }

func TestParseSnapshot_SchemaVersion(t *testing.T) {
	snap, err := ParseSnapshot([]byte(`{"schemaVersion":3,"providers":[]}`))
	if err != nil {
		t.Fatalf("ParseSnapshot() error = %v", err)
	}
	if snap.SchemaVersion != 3 {
		t.Fatalf("SchemaVersion = %d", snap.SchemaVersion)
	}
	if _, err := ParseSnapshot([]byte(`{"schemaVersion":4,"providers":[]}`)); err == nil {
		t.Fatal("expected schemaVersion 4 to fail")
	}
}

func TestParseSnapshot_SchemaVersion2NormalizesBindingHeadroom(t *testing.T) {
	snap, err := ParseSnapshot([]byte(`{"schemaVersion":2,"providers":[{
		"provider":"codex",
		"windows":[
			{"id":"five_hour","percentRemaining":82},
			{"id":"weekly","percentRemaining":47},
			{"id":"model:gpt-special:5h","percentRemaining":12}
		],
		"state":{"status":"fresh","stale":false}
	}]}`))
	if err != nil {
		t.Fatalf("ParseSnapshot() error = %v", err)
	}
	sel := Select([]Candidate{{
		Name: "codex", Agent: types.AgentCodex, QuotaProvider: "codex", Runnable: true,
	}}, snap, nil)
	if got := sel.Ordered[0].Remaining; got == nil || *got != 12 {
		t.Fatalf("binding remaining = %v, want 12", got)
	}
	if sel.Ordered[0].Pace != "unknown" {
		t.Fatalf("pace = %q, want unknown", sel.Ordered[0].Pace)
	}
}

func TestParseSnapshot_SchemaVersion2PreservesStaleAndUnknown(t *testing.T) {
	snap, err := ParseSnapshot([]byte(`{"schemaVersion":2,"providers":[
		{"provider":"kimi","windows":[{"id":"weekly","percentRemaining":99}],"state":{"status":"stale","stale":true}},
		{"provider":"grok","windows":[],"state":{"status":"fresh","stale":false}}
	]}`))
	if err != nil {
		t.Fatalf("ParseSnapshot() error = %v", err)
	}
	sel := Select([]Candidate{
		{Name: "kimi", Agent: types.AgentPi, QuotaProvider: "kimi", Runnable: true},
		{Name: "grok", Agent: types.AgentPi, QuotaProvider: "grok", Runnable: true},
	}, snap, nil)
	for _, decision := range sel.Decisions {
		if decision.RankGroup != 2 || decision.Remaining != nil {
			t.Fatalf("%s was treated as healthy: %+v", decision.Name, decision)
		}
	}
}

func TestSelect_PrefersHigherKnownHeadroomAndPace(t *testing.T) {
	snap := &Snapshot{
		SchemaVersion: 3,
		Providers: []ProviderSnapshot{
			knownProvider("kimi", 40, "ahead"),
			knownProvider("codex", 80, "behind"),
			knownProvider("grok", 80, "on_pace"),
		},
	}
	candidates := []Candidate{
		{Name: "kimi", Agent: types.AgentPi, QuotaProvider: "kimi", Runnable: true},
		{Name: "codex", Agent: types.AgentCodex, QuotaProvider: "codex", Runnable: true},
		{Name: "grok", Agent: types.AgentPi, QuotaProvider: "grok", Runnable: true},
	}
	sel := Select(candidates, snap, nil)
	if len(sel.Ordered) != 3 {
		t.Fatalf("Ordered len = %d, want 3: %+v", len(sel.Ordered), sel.Ordered)
	}
	// codex 80% behind beats grok 80% on_pace; kimi 40% last.
	want := []string{"codex", "grok", "kimi"}
	for i, name := range want {
		if sel.Ordered[i].Name != name {
			t.Fatalf("Ordered[%d] = %q, want %q (summary %s)", i, sel.Ordered[i].Name, name, sel.Summary)
		}
	}
	if !strings.Contains(sel.Summary, "codex") || !strings.Contains(sel.Summary, "->") {
		t.Fatalf("Summary = %q", sel.Summary)
	}
}

func TestSelect_SustainableTieBreaksByConfigOrder(t *testing.T) {
	snap := &Snapshot{
		SchemaVersion: 3,
		Providers: []ProviderSnapshot{
			knownProvider("kimi", 90, "behind"),
			knownProvider("codex", 90, "behind"),
			knownProvider("grok", 90, "behind"),
		},
	}
	candidates := []Candidate{
		{Name: "kimi", Agent: types.AgentPi, QuotaProvider: "kimi", Runnable: true},
		{Name: "codex", Agent: types.AgentCodex, QuotaProvider: "codex", Runnable: true},
		{Name: "grok", Agent: types.AgentPi, QuotaProvider: "grok", Runnable: true},
	}
	sel := Select(candidates, snap, nil)
	want := []string{"kimi", "codex", "grok"}
	for i, name := range want {
		if sel.Ordered[i].Name != name {
			t.Fatalf("Ordered[%d] = %q, want %q", i, sel.Ordered[i].Name, name)
		}
	}
}

func TestSelect_UnknownAndStaleNeverTreatedAsHealthy(t *testing.T) {
	snap := &Snapshot{
		SchemaVersion: 3,
		Providers: []ProviderSnapshot{
			{
				Provider: "kimi",
				State:    ProviderState{Status: "stale", Stale: true},
				QuotaSemantics: QuotaSemantics{
					Status: "known",
					EffectiveAvailability: []EffectiveAvailability{{
						Scope:                     "all_models",
						Status:                    "known",
						EffectivePercentRemaining: floatPtr(99),
						Pace:                      &Pace{Status: "behind"},
					}},
				},
			},
			{
				Provider: "codex",
				State:    ProviderState{Status: "fresh"},
				QuotaSemantics: QuotaSemantics{
					Status: "unknown",
				},
			},
			knownProvider("grok", 10, "on_pace"),
		},
	}
	candidates := []Candidate{
		{Name: "kimi", Agent: types.AgentPi, QuotaProvider: "kimi", Runnable: true},
		{Name: "codex", Agent: types.AgentCodex, QuotaProvider: "codex", Runnable: true},
		{Name: "grok", Agent: types.AgentPi, QuotaProvider: "grok", Runnable: true},
	}
	sel := Select(candidates, snap, nil)
	if sel.Ordered[0].Name != "grok" {
		t.Fatalf("primary = %q, want grok; decisions=%v summary=%s", sel.Ordered[0].Name, reasons(sel), sel.Summary)
	}
	for _, d := range sel.Decisions {
		switch d.Name {
		case "kimi", "codex":
			if d.RankGroup != 2 {
				t.Fatalf("%s RankGroup = %d, want 2 (unknown/stale): %s", d.Name, d.RankGroup, d.Reason)
			}
			if strings.Contains(strings.ToLower(d.Reason), "healthy") == false && !strings.Contains(d.Reason, "not treated as healthy") {
				// both branches mention not treated as healthy
				if !strings.Contains(d.Reason, "not treated as healthy") {
					t.Fatalf("%s reason should refuse healthy treatment: %s", d.Name, d.Reason)
				}
			}
		}
	}
}

func TestSelect_MissingStateStatusIsUnknown(t *testing.T) {
	snap := &Snapshot{
		SchemaVersion: 3,
		Providers: []ProviderSnapshot{{
			Provider: "codex",
			QuotaSemantics: QuotaSemantics{
				Status: "known",
				EffectiveAvailability: []EffectiveAvailability{{
					Scope:                     "all_models",
					Status:                    "known",
					EffectivePercentRemaining: floatPtr(99),
				}},
			},
		}},
	}
	sel := Select([]Candidate{{
		Name: "codex", Agent: types.AgentCodex, QuotaProvider: "codex", Runnable: true,
	}}, snap, nil)
	decision := sel.Decisions[0]
	if decision.RankGroup != 2 || decision.Remaining != nil {
		t.Fatalf("missing state.status treated as healthy: %+v", decision)
	}
	if !strings.Contains(decision.Reason, `quota state "unknown"`) {
		t.Fatalf("missing state.status reason = %q, want unknown", decision.Reason)
	}
}

func TestSelect_PressuredExhaustedRanksBelowHeadroom(t *testing.T) {
	snap := &Snapshot{
		SchemaVersion: 3,
		Providers: []ProviderSnapshot{
			knownProvider("kimi", 0, "ahead"),
			knownProvider("codex", 55, "on_pace"),
		},
	}
	candidates := []Candidate{
		{Name: "kimi", Agent: types.AgentPi, QuotaProvider: "kimi", Runnable: true},
		{Name: "codex", Agent: types.AgentCodex, QuotaProvider: "codex", Runnable: true},
	}
	sel := Select(candidates, snap, nil)
	if sel.Ordered[0].Name != "codex" || sel.Ordered[1].Name != "kimi" {
		t.Fatalf("order = %v, want codex then kimi", names(sel.Ordered))
	}
	if sel.Ordered[1].RankGroup != 1 {
		t.Fatalf("exhausted RankGroup = %d, want 1", sel.Ordered[1].RankGroup)
	}
}

func TestSelect_SnapshotErrorUsesConfigOrderWithoutInventingHealth(t *testing.T) {
	candidates := []Candidate{
		{Name: "kimi", Agent: types.AgentPi, QuotaProvider: "kimi", Runnable: true},
		{Name: "codex", Agent: types.AgentCodex, QuotaProvider: "codex", Runnable: true},
	}
	sel := Select(candidates, nil, errors.New("quota-axi missing"))
	if names(sel.Ordered)[0] != "kimi" || names(sel.Ordered)[1] != "codex" {
		t.Fatalf("order = %v", names(sel.Ordered))
	}
	for _, d := range sel.Ordered {
		if d.Remaining != nil {
			t.Fatalf("invented remaining for %s: %v", d.Name, *d.Remaining)
		}
		if d.RankGroup != 2 {
			t.Fatalf("%s RankGroup = %d, want 2", d.Name, d.RankGroup)
		}
	}
	if !strings.Contains(sel.Summary, "snapshot unavailable") {
		t.Fatalf("Summary = %q", sel.Summary)
	}
}

func TestSelect_SkipsUnrunnableAndNeverAddsUnconfiguredProvider(t *testing.T) {
	snap := &Snapshot{
		SchemaVersion: 3,
		Providers: []ProviderSnapshot{
			knownProvider("claude", 99, "behind"),
			knownProvider("codex", 10, "behind"),
			knownProvider("kimi", 50, "behind"),
		},
	}
	candidates := []Candidate{
		{Name: "kimi", Agent: types.AgentPi, QuotaProvider: "kimi", Runnable: false, Probe: "pi"},
		{Name: "codex", Agent: types.AgentCodex, QuotaProvider: "codex", Runnable: true},
	}
	sel := Select(candidates, snap, nil)
	if len(sel.Ordered) != 1 || sel.Ordered[0].Name != "codex" {
		t.Fatalf("Ordered = %v, want only codex", names(sel.Ordered))
	}
	for _, d := range sel.Decisions {
		if d.Name == "claude" || d.QuotaProvider == "claude" {
			t.Fatalf("claude leaked into decisions: %+v", d)
		}
	}
	// Claude is in the snapshot with great headroom but was never a candidate.
	raw, _ := json.Marshal(sel)
	if strings.Contains(string(raw), "claude") {
		t.Fatalf("claude appeared in selection payload: %s", raw)
	}
}

func TestSelect_ModelScopePreferredWhenHinted(t *testing.T) {
	snap := &Snapshot{
		SchemaVersion: 3,
		Providers: []ProviderSnapshot{{
			Provider: "codex",
			State:    ProviderState{Status: "fresh"},
			QuotaSemantics: QuotaSemantics{
				Status: "known",
				EffectiveAvailability: []EffectiveAvailability{
					{
						Scope:                     "all_models",
						Status:                    "known",
						EffectivePercentRemaining: floatPtr(90),
						Pace:                      &Pace{Status: "behind"},
					},
					{
						Scope:                     "model:gpt-special",
						Status:                    "known",
						EffectivePercentRemaining: floatPtr(12),
						Pace:                      &Pace{Status: "ahead"},
					},
				},
			},
		}},
	}
	candidates := []Candidate{{
		Name:          "codex",
		Agent:         types.AgentCodex,
		QuotaProvider: "codex",
		Model:         "gpt-special",
		Runnable:      true,
	}}
	sel := Select(candidates, snap, nil)
	if sel.Ordered[0].Remaining == nil || *sel.Ordered[0].Remaining != 12 {
		t.Fatalf("remaining = %v, want 12 from model scope", sel.Ordered[0].Remaining)
	}
}

func knownProvider(name string, remaining float64, pace string) ProviderSnapshot {
	return ProviderSnapshot{
		Provider: name,
		State:    ProviderState{Status: "fresh", Stale: false},
		QuotaSemantics: QuotaSemantics{
			Status: "known",
			EffectiveAvailability: []EffectiveAvailability{{
				Scope:                     "all_models",
				Status:                    "known",
				EffectivePercentRemaining: floatPtr(remaining),
				Pace:                      &Pace{Status: pace},
			}},
		},
	}
}

func names(ds []Decision) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Name
	}
	return out
}

func reasons(sel Selection) []string {
	out := make([]string, 0, len(sel.Decisions))
	for _, d := range sel.Decisions {
		out = append(out, d.Name+": "+d.Reason)
	}
	return out
}

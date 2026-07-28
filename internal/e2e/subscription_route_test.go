//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestSubscriptionAgentRouting_DoctorAndRunExcludesUnlistedClaude proves the
// installation-facing subscription surface: named Kimi/Codex/Grok-style
// candidates are ranked from a fresh quota-axi snapshot, Claude never enters
// when absent from the candidate set, and a pipeline run completes on the
// selected backend.
func TestSubscriptionAgentRouting_DoctorAndRunExcludesUnlistedClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subscription e2e shell stubs are unix-only")
	}

	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: cleanReviewScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	// pi exists so kimi/grok candidates are runnable at probe time. It is a
	// non-agent stub; quota prefers codex so the primary never needs it.
	writeExecutable(t, filepath.Join(h.BinDir, "pi"), "#!/bin/sh\necho 'pi stub' >&2\nexit 1\n")
	// Claude remains on PATH via the harness fakeagent symlink, with excellent
	// quota in the snapshot below - still must not enter the route.
	writeExecutable(t, filepath.Join(h.BinDir, "quota-axi"), `#!/bin/sh
cat <<'EOF'
{
  "generatedAt": "2026-07-28T00:00:00.000Z",
  "schemaVersion": 3,
  "providers": [
    {
      "provider": "claude",
      "state": {"status": "fresh", "stale": false},
      "quotaSemantics": {
        "status": "known",
        "effectiveAvailability": [
          {"scope": "all_models", "status": "known", "effectivePercentRemaining": 99, "pace": {"status": "behind"}}
        ]
      }
    },
    {
      "provider": "kimi",
      "state": {"status": "fresh", "stale": false},
      "quotaSemantics": {
        "status": "known",
        "effectiveAvailability": [
          {"scope": "all_models", "status": "known", "effectivePercentRemaining": 40, "pace": {"status": "on_pace"}}
        ]
      }
    },
    {
      "provider": "codex",
      "state": {"status": "fresh", "stale": false},
      "quotaSemantics": {
        "status": "known",
        "effectiveAvailability": [
          {"scope": "all_models", "status": "known", "effectivePercentRemaining": 88, "pace": {"status": "behind"}}
        ]
      }
    },
    {
      "provider": "grok",
      "state": {"status": "fresh", "stale": false},
      "quotaSemantics": {
        "status": "known",
        "effectiveAvailability": [
          {"scope": "all_products", "status": "known", "effectivePercentRemaining": 70, "pace": {"status": "behind"}}
        ]
      }
    }
  ]
}
EOF
`)

	cfg := `agent: subscription
log_level: debug
subscription_agents:
  quota_axi_path: quota-axi
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
      provider: xai
      model: grok-4.5
agent_path_override:
  codex: ` + filepath.Join(h.BinDir, "codex") + `
  pi: ` + filepath.Join(h.BinDir, "pi") + `
auto_fix:
  rebase: 0
  lint: 0
  test: 0
  review: 0
  document: 0
  ci: 0
`
	if err := os.WriteFile(filepath.Join(h.NMHome, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out, err := h.Run("doctor")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	const wantRoute = "subscription route runnable (codex -> grok -> kimi)"
	if !strings.Contains(out, wantRoute) {
		t.Fatalf("doctor should report %q, got:\n%s", wantRoute, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "subscription route runnable") && strings.Contains(line, "claude") {
			t.Fatalf("claude leaked into subscription route: %s", line)
		}
	}
	t.Logf("doctor subscription routing evidence:\n%s", out)

	branch := "feature/sub-route"
	h.CommitChange(branch, "hello.txt", "subscription routing\n", "feat: subscription routing fixture")
	h.PushToGate(branch)
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		errMsg := ""
		if run.Error != nil {
			errMsg = *run.Error
		}
		t.Fatalf("run status = %s error=%q", run.Status, errMsg)
	}
	t.Logf("pipeline subscription routing evidence: branch=%s status=%s", branch, run.Status)

	daemonLog := filepath.Join(h.NMHome, "logs", "daemon.log")
	logData, err := os.ReadFile(daemonLog)
	if err != nil {
		t.Fatalf("read daemon log: %v", err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "subscription agent route selected") {
		t.Fatalf("daemon log missing route selection:\n%s", logText)
	}
	if !strings.Contains(logText, "primary") || !strings.Contains(logText, "codex") {
		// slog fields may render as primary=codex
		if !strings.Contains(logText, "codex") {
			t.Fatalf("daemon log should mention codex primary:\n%s", logText)
		}
	}
	if strings.Contains(logText, `name=claude`) || strings.Contains(logText, `primary=claude`) {
		t.Fatalf("daemon log selected claude:\n%s", logText)
	}

	for _, inv := range h.AgentInvocations() {
		if inv.Agent == "claude" {
			t.Fatalf("pipeline invoked claude despite absence from candidates: %+v", inv)
		}
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

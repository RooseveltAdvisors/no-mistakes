//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
	_ "modernc.org/sqlite"
)

// These journeys drive the gate-agent neutralization filter the way an end
// user experiences it: a real no-mistakes binary, a real daemon, a real
// push-triggered pipeline, and fakeagent processes on PATH. They pin the
// contract of the filter change:
//
//   1. A mixed fallback list (an unverified member next to verified ones)
//      under the trusted disable_project_settings opt-out RUNS, with the
//      unverified member filtered out before any launch decision, a bounded
//      warning naming it, and telemetry labelling the run with the agent
//      that actually runs.
//   2. An all-unverified list under the same opt-out still REFUSES, and the
//      refusal names every real culprit, not the fallback wrapper's first
//      member.
//   3. Without the opt-out the fallback list behaves exactly as before:
//      the unverified primary launches.

// evidenceDir returns the reviewer-evidence directory for this run (set by the
// test step) or "" when evidence capture was not requested.
func evidenceDir() string {
	return strings.TrimSpace(os.Getenv("NM_EVIDENCE_DIR"))
}

// writeEvidence stores a reviewer-visible artifact for a scenario.
func writeEvidence(t *testing.T, name, content string) string {
	t.Helper()
	dir := evidenceDir()
	if dir == "" {
		return ""
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("evidence dir %s: %v", dir, err)
		return ""
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Logf("write evidence %s: %v", path, err)
		return ""
	}
	return path
}

// writeFallbackAgentConfig rewrites the global config with an ordered
// fallback agent list, pinning every member's binary to the harness fake.
func writeFallbackAgentConfig(t *testing.T, h *Harness, agents []string) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("agent: [" + strings.Join(agents, ", ") + "]\n")
	sb.WriteString("log_level: debug\n")
	sb.WriteString("agent_path_override:\n")
	for _, name := range agents {
		fmt.Fprintf(&sb, "  %s: '%s'\n", name, filepath.Join(h.BinDir, name))
	}
	sb.WriteString("auto_fix:\n  rebase: 0\n  lint: 0\n  test: 0\n  review: 0\n  document: 0\n  ci: 0\n")
	path := filepath.Join(h.NMHome, "config.yaml")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write fallback agent config: %v", err)
	}
}

// commitTrustedRepoConfig commits .no-mistakes.yaml onto the default branch
// (the trusted source the daemon reads disable_project_settings from) and
// pushes it to origin before init, so the gate clone carries it.
func commitTrustedRepoConfig(t *testing.T, h *Harness, extra string) {
	t.Helper()
	content := "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\nallow_repo_commands: true\n" + extra
	h.CommitChange("main", ".no-mistakes.yaml", content, "set trusted repo config")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push trusted config to origin main: %v\n%s", err, out)
	}
}

// daemonLog returns the daemon's bounded lifecycle log, which carries the
// slog output of the run manager (including the filter's warning).
func daemonLog(t *testing.T, h *Harness) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.NMHome, "logs", "daemon.log"))
	if err != nil {
		return ""
	}
	return string(data)
}

// invocationAgents returns the agent name of every fakeagent launch in order.
func invocationAgents(h *Harness) []string {
	var names []string
	for _, inv := range h.AgentInvocations() {
		names = append(names, inv.Agent)
	}
	return names
}

// telemetryRecorder is a local umami stand-in: the daemon and CLI POST their
// events to it instead of the real telemetry host, so the test can assert the
// labels end users' telemetry would carry.
type telemetryRecorder struct {
	srv    *httptest.Server
	mu     sync.Mutex
	events []map[string]any // each event's "payload" object
}

func startTelemetryRecorder(t *testing.T) *telemetryRecorder {
	t.Helper()
	rec := &telemetryRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Payload map[string]any `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.Payload != nil {
			rec.mu.Lock()
			rec.events = append(rec.events, body.Payload)
			rec.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(rec.srv.Close)
	t.Setenv("NO_MISTAKES_UMAMI_HOST", rec.srv.URL)
	t.Setenv("NO_MISTAKES_UMAMI_WEBSITE_ID", "e2e-gate-neutralize")
	t.Setenv("NO_MISTAKES_TELEMETRY", "on") // harness defaults to off
	return rec
}

// find returns the data maps of every event with the given name.
func (r *telemetryRecorder) find(name string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, ev := range r.events {
		if ev["name"] == name {
			if data, ok := ev["data"].(map[string]any); ok {
				out = append(out, data)
			}
		}
	}
	return out
}

// waitForData polls until an event whose data satisfies pred arrives.
func (r *telemetryRecorder) waitForData(t *testing.T, name string, pred func(map[string]any) bool, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, data := range r.find(name) {
			if pred(data) {
				return data
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

// TestGateNeutralizeOptOutJourney drives the three observable behaviors of the
// gate-fallback filter through the real push -> pipeline -> completion path.
func TestGateNeutralizeOptOutJourney(t *testing.T) {
	t.Run("mixed_fallback_runs_filtered_member_never_launches", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: cleanReviewScenario(t)})
		writeFallbackAgentConfig(t, h, []string{"opencode", "codex"})
		commitTrustedRepoConfig(t, h, "disable_project_settings: true\n")
		telemetry := startTelemetryRecorder(t)

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}
		h.CommitChange("gate-mixed", "hello.txt", "hello world\n", "add hello.txt")
		h.PushToGate("gate-mixed")
		run := h.WaitForRun("gate-mixed", 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("mixed fallback under opt-out did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}

		// The surviving member carries every step; the filtered member must
		// never have been launched (fail-closed: no unneutralized process).
		agents := invocationAgents(h)
		if len(agents) == 0 {
			t.Fatal("expected agent invocations for a completed run")
		}
		for _, name := range agents {
			if name != "codex" {
				t.Errorf("unverified member %q launched under the opt-out; invocations: %v", name, agents)
			}
		}
		writeEvidence(t, "gate-optout-1-mixed-agent-invocations.jsonl", string(mustReadFile(t, h.AgentLog)))

		// The drop is observable in the daemon log, naming the refused member.
		logText := daemonLog(t, h)
		if !strings.Contains(logText, "gate agent candidate(s) do not neutralize project agent-instruction files under disable_project_settings") ||
			!strings.Contains(logText, "opencode") {
			t.Errorf("daemon log must warn and name the dropped member")
		}
		if idx := strings.Index(logText, "gate agent candidate(s) do not neutralize"); idx >= 0 {
			end := idx + 600
			if end > len(logText) {
				end = len(logText)
			}
			writeEvidence(t, "gate-optout-1-daemon-warn.log", logText[idx:end])
		}

		// Telemetry for the run must label the agent that actually runs,
		// not the configured-but-filtered resolved[0].
		started := telemetry.waitForData(t, "run", func(data map[string]any) bool {
			return data["action"] == "started"
		}, 10*time.Second)
		if started == nil {
			t.Fatal("no run started telemetry event reached the collector")
		}
		if got := started["agent"]; got != "codex" {
			t.Errorf("run started telemetry agent = %v, want codex (the surviving primary)", got)
		}
		for _, data := range telemetry.find("run") {
			if data["agent"] == "opencode" {
				t.Errorf("telemetry labelled the run with filtered-out agent opencode: %v", data)
			}
		}
		if raw, err := json.MarshalIndent(telemetry.find("run"), "", "  "); err == nil {
			writeEvidence(t, "gate-optout-1-run-telemetry-events.json", string(raw)+"\n")
		}
	})

	t.Run("all_unverified_fallback_refuses_naming_actual_members", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: cleanReviewScenario(t)})
		// copilot is not one of the harness's default fakes; it only needs to
		// be resolvable so the configured list survives agent resolution.
		if err := os.Symlink(h.FakeAgent, filepath.Join(h.BinDir, "copilot")); err != nil {
			t.Fatalf("symlink copilot: %v", err)
		}
		writeFallbackAgentConfig(t, h, []string{"opencode", "copilot"})
		commitTrustedRepoConfig(t, h, "disable_project_settings: true\n")

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}
		h.CommitChange("gate-all-unverified", "hello.txt", "hello world\n", "add hello.txt")
		h.PushToGate("gate-all-unverified")
		run := h.WaitForRun("gate-all-unverified", 90*time.Second)
		if run.Status != types.RunFailed {
			t.Fatalf("all-unverified fallback under opt-out must refuse: status=%s", run.Status)
		}
		if run.Error == nil {
			t.Fatal("refusal must surface a run error")
		}
		for _, want := range []string{"opencode", "copilot", "does not neutralize"} {
			if !strings.Contains(*run.Error, want) {
				t.Errorf("refusal error must name %q, got: %s", want, *run.Error)
			}
		}
		if len(run.Steps) != 0 {
			t.Errorf("refusal must fail before any pipeline step starts, got %d steps", len(run.Steps))
		}
		if invs := h.AgentInvocations(); len(invs) != 0 {
			t.Errorf("no agent may be launched when every candidate is refused, got %v", invocationAgents(h))
		}
		writeEvidence(t, "gate-optout-2-refusal-run-error.txt", fmt.Sprintf("run error:\n%s\n\nrefused members: opencode, copilot\n", *run.Error))

		logText := daemonLog(t, h)
		if !strings.Contains(logText, "gate agent candidate(s) do not neutralize project agent-instruction files under disable_project_settings") {
			t.Error("daemon log must warn when candidates are dropped")
		}
		if idx := strings.Index(logText, "gate agent candidate(s) do not neutralize"); idx >= 0 {
			end := idx + 700
			if end > len(logText) {
				end = len(logText)
			}
			writeEvidence(t, "gate-optout-2-daemon-warn.log", logText[idx:end])
		}
	})

	t.Run("eval_replay_refuses_unneutralized_candidate_under_opt_out", func(t *testing.T) {
		scenario := filepath.Join(t.TempDir(), "eval-scenario.yaml")
		catchAll := `actions:
  - structured:
      findings: []
      summary: "clean"
      tested: ["fakeagent"]
      testing_summary: "simulated"
      artifacts: []
      risk_level: low
      risk_rationale: "clean"
      risk_scope: source-or-external
      title: "fake: change"
      body: "fake body"
`
		if err := os.WriteFile(scenario, []byte(catchAll), 0o644); err != nil {
			t.Fatal(err)
		}
		h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario})
		writeFallbackAgentConfig(t, h, []string{"opencode", "codex"})
		commitTrustedRepoConfig(t, h, "disable_project_settings: true\n")

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}
		h.CommitChange("gate-eval-optout", "hello.txt", "hello world\n", "add hello.txt")
		h.PushToGate("gate-eval-optout")
		run := h.WaitForRun("gate-eval-optout", 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("opt-out run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}
		if out, err := h.Run("eval", "capture", run.ID); err != nil {
			t.Fatalf("eval capture: %v\n%s", err, out)
		}

		// Adversarial: replaying with a candidate that has no verified knob must
		// fail closed under the same opt-out, naming the candidate, and the
		// candidate process must never launch.
		pipelineInvocations := len(h.AgentInvocations())
		out, err := h.Run("eval", "run", "--cases", "all", "--candidate", "opencode+test", "--repeats", "1")
		if err == nil {
			t.Fatalf("eval replay of an unneutralized candidate must fail closed, got success:\n%s", out)
		}
		if !strings.Contains(out, "replay invocation(s) failed") {
			t.Errorf("eval run must report the failed replay, got:\n%s", out)
		}
		if len(h.AgentInvocations()) != pipelineInvocations {
			t.Errorf("unneutralized eval candidate was launched: %v", invocationAgents(h))
		}
		refusalRows := evalEvaluationRows(t, h)
		writeEvidence(t, "gate-optout-4-eval-refusal.txt", "eval run --candidate opencode+test output:\n"+out+"\npersisted evaluations (candidate | status | error):\n"+strings.Join(refusalRows, "\n")+"\n")
		opencodeRow := findEvalRow(t, refusalRows, "opencode+test")
		if opencodeRow == "" {
			t.Fatalf("no persisted evaluation for opencode+test, rows: %v", refusalRows)
		}
		if strings.Contains(opencodeRow, "\tcompleted\t") {
			t.Errorf("unneutralized candidate evaluation must not be completed: %s", opencodeRow)
		}
		if !strings.Contains(opencodeRow, "does not neutralize") || !strings.Contains(opencodeRow, "opencode") {
			t.Errorf("persisted refusal must name the unneutralized candidate, got: %s", opencodeRow)
		}

		// The same opt-out still lets a verified candidate replay to completion.
		out, err = h.Run("eval", "run", "--cases", "all", "--candidate", "codex+test", "--repeats", "1")
		if err != nil {
			t.Fatalf("eval replay of a verified candidate under the opt-out: %v\n%s", err, out)
		}
		if !strings.Contains(out, "local eval session") {
			t.Fatalf("expected a completed eval session, got:\n%s", out)
		}
		reportOut, _ := h.Run("eval", "report")
		codexRow := findEvalRow(t, evalEvaluationRows(t, h), "codex+test")
		if codexRow == "" || !strings.Contains(codexRow, "\tcompleted\t") {
			t.Errorf("verified candidate must replay to completion under the same opt-out, rows: %v", evalEvaluationRows(t, h))
		}
		writeEvidence(t, "gate-optout-4-eval-verified-candidate.txt", "eval run --candidate codex+test output:\n"+out+"\neval report:\n"+reportOut)
	})

	t.Run("without_opt_out_fallback_list_runs_unchanged", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: cleanReviewScenario(t)})
		writeFallbackAgentConfig(t, h, []string{"opencode", "codex"})
		// No disable_project_settings: the trusted config stays harness-default.

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}
		h.CommitChange("gate-no-optout", "hello.txt", "hello world\n", "add hello.txt")
		h.PushToGate("gate-no-optout")
		run := h.WaitForRun("gate-no-optout", 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("fallback list outside the opt-out must run as before: status=%s error=%v", run.Status, deref(run.Error))
		}
		agents := invocationAgents(h)
		if len(agents) == 0 {
			t.Fatal("expected agent invocations for a completed run")
		}
		if agents[0] != "opencode" {
			t.Errorf("configured primary must run outside the opt-out, first invocation = %v", agents)
		}
		for _, name := range agents {
			if name == "codex" {
				t.Errorf("fallback member must not be needed when the primary works: %v", agents)
			}
		}
		writeEvidence(t, "gate-no-optout-3-agent-invocations.jsonl", string(mustReadFile(t, h.AgentLog)))
	})
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// evalEvaluationRows reads the persisted evaluation records from the local eval
// registry (the same sqlite store `eval report` renders) as
// "candidate<TAB>status<TAB>error" lines, resolving each row's stored JSON
// result file so a scenario can assert the recorded refusal reason rather than
// only the CLI's exit code.
func evalEvaluationRows(t *testing.T, h *Harness) []string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(h.NMHome, "eval", "registry.sqlite")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open eval registry: %v", err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT candidate, status, path FROM evaluations ORDER BY completed_at, candidate")
	if err != nil {
		t.Fatalf("query evaluations: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var candidate, status, path string
		if err := rows.Scan(&candidate, &status, &path); err != nil {
			t.Fatalf("scan evaluation: %v", err)
		}
		evalErr := ""
		if data, readErr := os.ReadFile(path); readErr == nil {
			var payload struct {
				Error string `json:"error"`
			}
			if jsonErr := json.Unmarshal(data, &payload); jsonErr == nil {
				evalErr = payload.Error
			}
		}
		out = append(out, strings.Join([]string{candidate, status, evalErr}, "\t"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate evaluations: %v", err)
	}
	return out
}

// findEvalRow returns the persisted evaluation row for a candidate spelling
// "agent+model", or "" when none exists.
func findEvalRow(t *testing.T, rows []string, candidate string) string {
	t.Helper()
	for _, row := range rows {
		if strings.HasPrefix(row, candidate+"\t") {
			return row
		}
	}
	return ""
}

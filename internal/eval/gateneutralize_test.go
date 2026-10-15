package eval

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const optOutFindings = `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`

// setupCapturedRunWithOptOut captures a fixture run whose trusted repository
// config disables project settings, the condition under which a candidate must
// have a verified gate-neutralization knob before launch.
func setupCapturedRunWithOptOut(t *testing.T, ctx context.Context) (*paths.Paths, *db.DB, *db.Run, *db.Repo, *db.StepRound) {
	t.Helper()
	return setupCapturedRunWithRepoConfig(t, ctx, 0, optOutFindings, fixtureRepoConfigYAML+"disable_project_settings: true\n")
}

// TestReplayRefusesUnneutralizedCandidateUnderOptOut proves the eval replay
// launch path fails closed the same way the daemon does: with the captured repo
// config disabling project settings, a candidate without a verified
// gate-neutralization knob is refused before the review step runs it, while a
// verified candidate still runs under the same flag.
func TestReplayRefusesUnneutralizedCandidateUnderOptOut(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRunWithOptOut(t, ctx)
	defer sourceDB.Close()

	fakeDir := t.TempDir()
	marker := filepath.Join(fakeDir, "opencode-launched")
	opencode := filepath.Join(fakeDir, "opencode")
	var script string
	if runtime.GOOS == "windows" {
		opencode += ".cmd"
		script = "@echo off\r\recho. >\"" + marker + "\"\r\nmore >nul\r\n"
	} else {
		script = "#!/bin/sh\ntouch \"" + marker + "\"\ncat >/dev/null\n"
	}
	if err := os.WriteFile(opencode, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	installFakeReviewAgent(t, p, `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}

	if _, evaluations, err := Replay(ctx, store, ReplayOptions{Set: "all", Candidate: Candidate{Agent: types.AgentOpenCode, Model: "test"}, Repeats: 1}); err == nil {
		t.Fatal("replay of an unverified candidate under the opt-out must fail closed")
	} else if len(evaluations) != 1 {
		t.Fatalf("evaluations = %#v, want 1", evaluations)
	} else {
		got := evaluations[0]
		if got.Status == "completed" {
			t.Fatalf("unverified candidate outcome = %#v, want refused before the review step", got)
		}
		if !strings.Contains(got.Error, "does not neutralize") || !strings.Contains(got.Error, "opencode") {
			t.Fatalf("refusal error = %q, want the unneutralized candidate named", got.Error)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("review step launched the unneutralized candidate: %v", err)
	}

	if _, evaluations, err := Replay(ctx, store, ReplayOptions{Set: "all", Candidate: Candidate{Agent: types.AgentClaude, Model: "test"}, Repeats: 1}); err != nil {
		t.Fatalf("replay of a verified candidate under the opt-out: %v", err)
	} else if len(evaluations) != 1 || evaluations[0].Status != "completed" {
		t.Fatalf("verified candidate evaluations = %#v, want one completed run under the same flag", evaluations)
	}
}

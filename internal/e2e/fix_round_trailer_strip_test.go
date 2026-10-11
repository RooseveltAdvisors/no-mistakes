//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const aiTrailers = "Co-authored-by: firstmate-worker <worker@example.invalid>\nCo-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>\n"

// startFixRoundRun pushes a branch whose review round auto-fixes one finding,
// with the daemon's trailer strip pointed at strip.
func startFixRoundRun(t *testing.T, strip, branch string) *Harness {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: configurableFixCommitScenario(t)})
	t.Setenv(git.TrailerStripEnvVar, strip)

	globalConfig := filepath.Join(h.NMHome, "config.yaml")
	globalData, err := os.ReadFile(globalConfig)
	if err != nil {
		t.Fatalf("read global config: %v", err)
	}
	globalSource := strings.Replace(string(globalData), "  review: 0\n", "  review: 1\n", 1)
	if err := os.WriteFile(globalConfig, []byte(globalSource), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	h.PushToGate(branch)
	return h
}

// agentGit runs git in the run worktree the way a fix agent would, with env
// overrides applied on top of the test process environment.
func agentGit(t *testing.T, dir string, env map[string]string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = mergedEnv(os.Environ(), env)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestFixRoundCommitTrailerStripJourney drives a real pipeline run with a
// review fix round. It proves the pipeline's fix-round commit is born through
// the fleet strip, that commits and amends the fix agent makes itself inside
// the daemon's run worktree go through the gate's commit-msg hook (stripped
// when the strip resolves, refused when it does not), and that the branch
// reaching upstream - the squash body GitHub assembles - carries no trailer.
func TestFixRoundCommitTrailerStripJourney(t *testing.T) {
	stripDir := t.TempDir()
	strip := filepath.Join(stripDir, "fm-git-strip-ai-trailers.sh")
	invocations := filepath.Join(stripDir, "invocations.log")
	// Stand-in for the firstmate fleet asset in commit-msg hook mode: records
	// each invocation and rewrites the message file without co-author lines.
	script := "#!/bin/sh\nset -e\nprintf '%s\\n' \"$(head -n1 \"$1\")\" >> '" + invocations + "'\n" +
		"grep -iv '^co-authored-by:' \"$1\" > \"$1.tmp\" || true\nmv \"$1.tmp\" \"$1\"\n"
	if err := os.WriteFile(strip, []byte(script), 0o755); err != nil {
		t.Fatalf("write strip: %v", err)
	}
	const branch = "feature/fix-round-trailer-strip"
	h := startFixRoundRun(t, strip, branch)

	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 60*time.Second)

	log, _ := os.ReadFile(invocations)
	t.Logf("strip invocations after the pipeline fix-round commit:\n%s", log)
	if !strings.Contains(string(log), "guard unsafe value") {
		t.Fatalf("fleet strip never saw the pipeline's fix-round commit message; invocations:\n%s", log)
	}

	wt := paths.WithRoot(h.NMHome).WorktreeDir(h.repoID(), gated.ID)
	fixHead, err := agentGit(t, wt, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("read run worktree HEAD in %s: %v\n%s", wt, err, fixHead)
	}
	fixHead = strings.TrimSpace(fixHead)

	// The fix agent commits on its own, with trailers.
	if out, err := agentGit(t, wt, nil, "commit", "--allow-empty", "-m", "agent: own commit\n\n"+aiTrailers); err != nil {
		t.Fatalf("agent commit with strip: %v\n%s", err, out)
	}
	created, _ := agentGit(t, wt, nil, "log", "-1", "--format=%B")
	t.Logf("agent-created commit message in run worktree:\n%s", created)
	if strings.Contains(strings.ToLower(created), "co-authored-by") || !strings.Contains(created, "agent: own commit") {
		t.Fatalf("agent-created commit not stripped:\n%s", created)
	}
	// ...and amends it, re-adding trailers.
	if out, err := agentGit(t, wt, nil, "commit", "--amend", "--allow-empty", "-m", "agent: amended commit\n\n"+aiTrailers); err != nil {
		t.Fatalf("agent amend with strip: %v\n%s", err, out)
	}
	amended, _ := agentGit(t, wt, nil, "log", "-1", "--format=%B")
	t.Logf("agent-amended commit message in run worktree:\n%s", amended)
	if strings.Contains(strings.ToLower(amended), "co-authored-by") || !strings.Contains(amended, "agent: amended commit") {
		t.Fatalf("agent-amended commit not stripped:\n%s", amended)
	}

	// Without a resolvable strip, the gate hook refuses both create and amend.
	noStrip := map[string]string{git.TrailerStripEnvVar: ""}
	before, _ := agentGit(t, wt, nil, "rev-parse", "HEAD")
	out, err := agentGit(t, wt, noStrip, "commit", "--allow-empty", "-m", "agent: unstripped\n\n"+aiTrailers)
	t.Logf("agent commit without strip: err=%v\n%s", err, out)
	if err == nil || !strings.Contains(out, "trailer stripper") {
		t.Fatalf("agent commit without strip was not refused: err=%v\n%s", err, out)
	}
	out, err = agentGit(t, wt, noStrip, "commit", "--amend", "--allow-empty", "-m", "agent: unstripped amend\n\n"+aiTrailers)
	t.Logf("agent amend without strip: err=%v\n%s", err, out)
	if err == nil || !strings.Contains(out, "trailer stripper") {
		t.Fatalf("agent amend without strip was not refused: err=%v\n%s", err, out)
	}
	after, _ := agentGit(t, wt, nil, "rev-parse", "HEAD")
	if before != after {
		t.Fatalf("HEAD moved from %s to %s despite refusal", before, after)
	}

	// Firstmate-shaped run: no configured path, the strip lives in the run
	// worktree's bin/.
	binStrip := filepath.Join(wt, "bin", "fm-git-strip-ai-trailers.sh")
	if err := os.MkdirAll(filepath.Dir(binStrip), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binStrip, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := agentGit(t, wt, noStrip, "commit", "--allow-empty", "-m", "agent: firstmate-shaped\n\n"+aiTrailers); err != nil {
		t.Fatalf("agent commit with worktree bin/ strip: %v\n%s", err, out)
	}
	fmShaped, _ := agentGit(t, wt, nil, "log", "-1", "--format=%B")
	t.Logf("firstmate-shaped agent commit message (strip from worktree bin/):\n%s", fmShaped)
	if strings.Contains(strings.ToLower(fmShaped), "co-authored-by") || !strings.Contains(fmShaped, "agent: firstmate-shaped") {
		t.Fatalf("firstmate-shaped agent commit not stripped:\n%s", fmShaped)
	}
	if err := os.RemoveAll(filepath.Join(wt, "bin")); err != nil {
		t.Fatal(err)
	}

	// Restore the run to the pipeline's own fix head and let it deliver.
	if out, err := agentGit(t, wt, nil, "reset", "--hard", fixHead); err != nil {
		t.Fatalf("restore fix head: %v\n%s", err, out)
	}
	h.Respond(gated.ID, types.StepReview, types.ActionApprove)
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
	}

	body, err := h.runGit(context.Background(), h.UpstreamDir, "log", "--format=%B", "main..refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read upstream commit messages: %v\n%s", err, body)
	}
	t.Logf("upstream branch commit messages (squash body source):\n%s", body)
	if !strings.Contains(string(body), "no-mistakes(review): guard unsafe value") {
		t.Fatalf("fix-round commit missing upstream:\n%s", body)
	}
	if strings.Contains(strings.ToLower(string(body)), "co-authored-by") {
		t.Fatalf("upstream branch carries an AI co-author trailer:\n%s", body)
	}
}

// TestFixRoundCommitRefusedWithoutStripJourney is the fail-closed half: with
// no fleet strip resolvable the pipeline's fix-round commit is refused, the
// run fails naming the missing stripper, and nothing reaches upstream.
func TestFixRoundCommitRefusedWithoutStripJourney(t *testing.T) {
	const branch = "feature/fix-round-no-strip"
	h := startFixRoundRun(t, filepath.Join(t.TempDir(), "missing-strip.sh"), branch)

	run := h.WaitForRun(branch, 60*time.Second)
	errText := ""
	if run.Error != nil {
		errText = *run.Error
	}
	t.Logf("run status: %s, error: %s", run.Status, errText)
	if run.Status == types.RunCompleted {
		t.Fatalf("run completed without a trailer strip; want refusal")
	}
	if !strings.Contains(errText, "trailer stripper") {
		t.Fatalf("run error %q does not name the missing stripper", errText)
	}
	if sha, err := h.runGit(context.Background(), h.UpstreamDir, "rev-parse", "--verify", "-q", "refs/heads/"+branch); err == nil {
		t.Fatalf("branch reached upstream at %s despite refusal", strings.TrimSpace(string(sha)))
	}
}

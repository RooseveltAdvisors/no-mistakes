//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRebaseKeepsDivergedRemoteLineageJourney drives the rebase step's
// diverged-lineage target end to end. The remote branch gained a commit that
// reverts the branch's own first commit (a reviewer's or bot's change), while
// the operator rebased onto a moved main locally and added more work. The
// pipeline must publish a fast-forward of the remote branch that keeps the
// revert and replays only the operator's new commit: neither the shared commit
// (which would silently undo the revert) nor main's commit (a duplicate of the
// base branch) may be replayed.
func TestRebaseKeepsDivergedRemoteLineageJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	initFromOwnWorktree(t, h, "init-lineage")

	const branch = "feature/lineage"
	h.CommitChange(branch, "shared.txt", "A\n", "shared commit A")
	gitIn(t, h, h.WorkDir, "push", "origin", branch)
	gitIn(t, h, h.WorkDir, "revert", "--no-edit", "HEAD")
	revert := gitIn(t, h, h.WorkDir, "rev-parse", "HEAD")
	gitIn(t, h, h.WorkDir, "push", "origin", branch)
	gitIn(t, h, h.WorkDir, "reset", "--hard", "HEAD~1")
	gitIn(t, h, h.UpstreamDir, "config", "receive.denyNonFastForwards", "true")

	mainAdvance := advanceMain(t, h, "main-advance.txt", "main advanced\n", "advance main under the branch")
	h.Checkout(branch)
	gitIn(t, h, h.WorkDir, "rebase", "main")
	submitted := h.CommitChange(branch, "local.txt", "B\n", "local commit B")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("lineage run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	published := h.UpstreamBranchSHA(branch)
	t.Logf("submitted=%s revert=%s published=%s", submitted, revert, published)
	history := gitIn(t, h, h.UpstreamDir, "log", "--oneline", "--graph", "--stat", published)
	t.Logf("published history:\n%s", history)
	writeEvidence(t, "rebase-diverged-lineage-published-history.txt", "remote revert: "+revert+"\nsubmitted: "+submitted+"\npublished: "+published+"\n\n"+history+"\n")

	if !isAncestorIn(t, h, h.UpstreamDir, revert, published) {
		t.Fatalf("published head %s does not keep the remote revert %s", published, revert)
	}
	replayed := strings.Fields(gitIn(t, h, h.UpstreamDir, "rev-list", revert+".."+published))
	if len(replayed) != 1 {
		t.Fatalf("replayed %d commits onto the remote lineage, want only the local commit B:\n%s",
			len(replayed), gitIn(t, h, h.UpstreamDir, "log", "--oneline", revert+".."+published))
	}
	if subject := gitIn(t, h, h.UpstreamDir, "log", "-1", "--format=%s", replayed[0]); subject != "local commit B" {
		t.Fatalf("replayed commit = %q, want local commit B", subject)
	}
	if isAncestorIn(t, h, h.UpstreamDir, mainAdvance, published) {
		t.Fatalf("published head unexpectedly carries main's commit %s", mainAdvance)
	}
	if _, err := h.runGit(context.Background(), h.UpstreamDir, "cat-file", "-e", published+":shared.txt"); err == nil {
		t.Fatalf("published head re-introduced shared.txt, undoing the remote revert")
	}
}

// TestAxiRunReportsNonFastForwardJourney drives `axi run` against a gate
// branch that carries a commit the local head does not contain. The CLI must
// report a non-fast-forward with the remote head and a new-branch recovery
// route rather than a bare `signal: broken pipe`, and must not move the gate.
func TestAxiRunReportsNonFastForwardJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	initFromOwnWorktree(t, h, "init-nonff")

	const branch = "feature/nonff"
	h.CommitChange(branch, "feature.txt", "feature\n", "feature work")
	h.PushToGate(branch)
	run := h.WaitForRun(branch, 120*time.Second)
	t.Logf("first run status=%s", run.Status)

	gateDir := h.gateDir(t)
	gateHead := gitIn(t, h, gateDir, "rev-parse", "refs/heads/"+branch)
	tree := gitIn(t, h, gateDir, "rev-parse", gateHead+"^{tree}")
	gateOnly := gitIn(t, h, gateDir, "-c", "user.name=Bot", "-c", "user.email=bot@example.com",
		"commit-tree", tree, "-p", gateHead, "-m", "gate-only commit")
	gitIn(t, h, gateDir, "update-ref", "refs/heads/"+branch, gateOnly)

	gitIn(t, h, h.WorkDir, "reset", "--hard", "main")
	h.CommitChange(branch, "other.txt", "other\n", "unrelated local work")

	out, err := h.Run("axi", "run", "--intent", "non-ff probe", "--skip", "ci",
		"--launch-nonce", "nonff-nonce", "--validation-generation", "gen-1")
	t.Logf("axi run output:\n%s", out)
	writeEvidence(t, "axi-run-non-fast-forward.txt", out)
	if err == nil && !strings.Contains(out, "non-fast-forward") {
		t.Fatalf("axi run succeeded without reporting the non-fast-forward:\n%s", out)
	}
	if strings.Contains(out, "broken pipe") {
		t.Fatalf("axi run collapsed the refusal into a broken pipe:\n%s", out)
	}
	for _, want := range []string{"non-fast-forward", gateOnly, "new branch name"} {
		if !strings.Contains(out, want) {
			t.Errorf("axi run output missing %q", want)
		}
	}
	if got := gitIn(t, h, gateDir, "rev-parse", "refs/heads/"+branch); got != gateOnly {
		t.Fatalf("gate head moved to %s, want unchanged %s", got, gateOnly)
	}
}

func (h *Harness) gateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("gate dir %s: %v", dir, err)
	}
	return dir
}

func writeEvidence(t *testing.T, name, content string) {
	t.Helper()
	dir := os.Getenv("NM_EVIDENCE_OUT")
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Logf("write evidence %s: %v", name, err)
	}
}

// TestRecoverRefusesToDiscardUnincorporatedGateLineageJourney drives
// `axi status` and `axi sync --recover --keep-local` through the real binary
// against a stranded branch whose gate head carries a commit that neither the
// recorded pipeline head nor the local head incorporates. Status must name
// that lineage and the new-branch route, and keep-local must refuse without
// moving the gate ref, the local head, or custody.
func TestRecoverRefusesToDiscardUnincorporatedGateLineageJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	initFromOwnWorktree(t, h, "init-recover")

	const branch = "feature/recover"
	submitted := h.CommitChange(branch, "feature.txt", "feature\n", "feature work")
	h.PushToGate(branch)
	first := h.WaitForRun(branch, 120*time.Second)
	if out, err := h.Run("daemon", "stop"); err != nil {
		t.Logf("daemon stop: %v\n%s", err, out)
	}

	gateDir := h.gateDir(t)
	ident := []string{"-c", "user.name=Bot", "-c", "user.email=bot@example.com"}
	tree := gitIn(t, h, gateDir, "rev-parse", submitted+"^{tree}")
	preserved := gitIn(t, h, gateDir, append(ident, "commit-tree", tree, "-p", submitted, "-m", "no-mistakes(review): fix")...)
	gitIn(t, h, gateDir, "update-ref", "refs/heads/"+branch, preserved)
	gitIn(t, h, h.WorkDir, "fetch", "--no-tags", gateDir, "refs/heads/"+branch+":refs/no-mistakes/recovery-test")
	gateTip := gitIn(t, h, gateDir, append(ident, "commit-tree", tree, "-p", submitted, "-m", "unincorporated remote lineage")...)
	gitIn(t, h, gateDir, "update-ref", "refs/heads/"+branch, gateTip)

	h.Checkout(branch)
	gitIn(t, h, h.WorkDir, "reset", "--hard", submitted)
	localHead := h.CommitChange(branch, "local-lineage.txt", "local\n", "local lineage")

	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	base := gitIn(t, h, h.WorkDir, "rev-parse", "main")
	if err := database.UpdateRunStatusWithVerifiedHead(first.ID, types.RunCancelled, strings.Repeat("f", 40)); err != nil {
		t.Fatal(err)
	}
	newer, err := database.InsertRun(h.repoID(), branch, submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(newer.ID, types.RunFailed, preserved); err != nil {
		t.Fatal(err)
	}
	database.Close()

	status, _ := h.Run("axi", "status")
	t.Logf("axi status:\n%s", status)
	writeEvidence(t, "axi-status-unincorporated-gate-head.txt", status)
	for _, want := range []string{"blocked_recover_unincorporated_remote_head", gateTip, "unincorporated remote lineage", "new branch name"} {
		if !strings.Contains(status, want) {
			t.Errorf("axi status missing %q", want)
		}
	}

	recovered, recErr := h.Run("axi", "sync", "--recover", "--keep-local")
	t.Logf("axi sync --recover --keep-local (err=%v):\n%s", recErr, recovered)
	writeEvidence(t, "axi-sync-recover-keep-local-refused.txt", recovered)
	if !strings.Contains(recovered, "blocked_recover_unincorporated_remote_head") {
		t.Errorf("keep-local recovery did not refuse with the unincorporated-lineage safety")
	}
	if got := gitIn(t, h, gateDir, "rev-parse", "refs/heads/"+branch); got != gateTip {
		t.Fatalf("gate head moved to %s, want unchanged %s", got, gateTip)
	}
	if got := gitIn(t, h, h.WorkDir, "rev-parse", "HEAD"); got != localHead {
		t.Fatalf("local head moved to %s, want unchanged %s", got, localHead)
	}
	database, err = db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if run, err := database.GetRun(newer.ID); err != nil || run.CustodyReturnedAt != nil {
		t.Fatalf("custody returned after refused keep-local: run=%#v err=%v", run, err)
	}
}

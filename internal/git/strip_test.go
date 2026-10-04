package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testStripScript stands in for firstmate's bin/fm-git-strip-ai-trailers.sh in
// hook mode: it rewrites the message file in place, dropping the AI co-author
// trailer lines a fix round can emit (fleet identity and model names) and
// keeping everything else.
const testStripScript = `#!/bin/sh
# fm-git-strip-ai-trailers.sh test stand-in (commit-msg hook mode).
file=$1
out="$file.out"
: >"$out"
while IFS= read -r line || [ -n "$line" ]; do
	lowered=$(printf '%s' "$line" | tr '[:upper:]' '[:lower:]')
	case "$lowered" in
	co-authored-by:*firstmate-worker*|co-authored-by:*worker@firstmate.local*|co-authored-by:*claude*|co-authored-by:*cursor*|co-authored-by:*codex*|co-authored-by:*noreply@anthropic.com*)
		continue ;;
	esac
	printf '%s\n' "$line"
done <"$file" >"$out"
mv "$out" "$file"
`

const dirtyFixRoundMessage = `fix(payments): claim the collection slot

- a body line kept intact.

Co-authored-by: firstmate-worker <worker@firstmate.local>
Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Co-authored-by: cursor <cursor@cursor.example>`

func requirePosixExec(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fleet trailer strip is a POSIX script; fleet launches do not install it on Windows")
	}
}

func writeExecutable(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// configureFleetStrip points TrailerStripEnvVar at a strip kept outside every
// repository, the way a Portal-shaped install configures the fleet asset.
func configureFleetStrip(t *testing.T) {
	t.Helper()
	t.Setenv(TrailerStripEnvVar, writeExecutable(t, filepath.Join(t.TempDir(), "fleet", "bin", trailerStripScript), testStripScript))
}

// newGateRunWorktree reproduces a real run: the source repository is cloned
// into a bare gate, the gate's hooks come only from RefreshManagedGateHooks,
// hook isolation is applied as gate init applies it, and the run worktree is a
// linked worktree created by WorktreeAdd. setup runs in the source repository
// before the gate is created, so committed content (a firstmate bin/) arrives
// through the checkout like any other file.
func newGateRunWorktree(t *testing.T, setup func(src string)) (src, gate, wt string) {
	t.Helper()
	ctx := context.Background()
	src = initTestRepo(t)
	if setup != nil {
		setup(src)
	}
	gate = filepath.Join(t.TempDir(), "gate.git")
	run(t, src, "git", "clone", "--bare", src, gate)
	if err := RefreshManagedGateHooks(gate); err != nil {
		t.Fatal(err)
	}
	isolated, err := EnsureHooksPathIsolation(ctx, gate)
	if err != nil || !isolated {
		t.Fatalf("isolate gate hooks path: isolated=%v err=%v", isolated, err)
	}
	wt = filepath.Join(t.TempDir(), "run")
	if err := WorktreeAdd(ctx, gate, wt, run(t, src, "git", "rev-parse", "HEAD")); err != nil {
		t.Fatal(err)
	}
	if err := CopyLocalUserIdentity(ctx, src, wt); err != nil {
		t.Fatal(err)
	}
	return src, gate, wt
}

func gitInDir(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "core.editor=true"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func assertCleanOfAIcoAuthors(t *testing.T, where, message string) {
	t.Helper()
	if strings.Contains(strings.ToLower(message), "co-authored-by:") {
		t.Fatalf("%s still carries an AI co-author trailer:\n%s", where, message)
	}
}

func TestGateRun_PortalShaped_StripsPipelineAndAgentCommits(t *testing.T) {
	requirePosixExec(t)
	configureFleetStrip(t)
	_, _, wt := newGateRunWorktree(t, nil)

	stripped, err := StripCommitMessage(context.Background(), wt, dirtyFixRoundMessage)
	if err != nil {
		t.Fatal(err)
	}
	assertCleanOfAIcoAuthors(t, "stripped message", stripped)
	if !strings.HasPrefix(stripped, "fix(payments): claim the collection slot\n\n- a body line kept intact.") {
		t.Fatalf("subject/body not preserved, got:\n%s", stripped)
	}
	run(t, wt, "git", "commit", "--allow-empty", "-m", stripped)
	assertCleanOfAIcoAuthors(t, "pipeline fix-round commit", run(t, wt, "git", "log", "-1", "--format=%B"))

	// The fix agent's own commit and amend go through the gate's commit-msg hook.
	run(t, wt, "git", "commit", "--allow-empty", "-m", dirtyFixRoundMessage)
	assertCleanOfAIcoAuthors(t, "agent commit", run(t, wt, "git", "log", "-1", "--format=%B"))
	run(t, wt, "git", "commit", "--amend", "--allow-empty", "-m", dirtyFixRoundMessage)
	committed := run(t, wt, "git", "log", "-1", "--format=%B")
	assertCleanOfAIcoAuthors(t, "agent amend", committed)
	if !strings.Contains(committed, "fix(payments): claim the collection slot") {
		t.Fatalf("committed message lost its subject:\n%s", committed)
	}
}

func TestGateRun_FirstmateShaped_ResolvesWorktreeBin(t *testing.T) {
	requirePosixExec(t)
	t.Setenv(TrailerStripEnvVar, "")
	_, _, wt := newGateRunWorktree(t, func(src string) {
		writeExecutable(t, filepath.Join(src, "bin", trailerStripScript), testStripScript)
		run(t, src, "git", "add", ".")
		run(t, src, "git", "commit", "-m", "add fleet strip")
	})

	stripped, err := StripCommitMessage(context.Background(), wt, dirtyFixRoundMessage)
	if err != nil {
		t.Fatal(err)
	}
	assertCleanOfAIcoAuthors(t, "stripped message", stripped)
	run(t, wt, "git", "commit", "--allow-empty", "-m", dirtyFixRoundMessage)
	assertCleanOfAIcoAuthors(t, "agent commit", run(t, wt, "git", "log", "-1", "--format=%B"))
}

func TestGateRun_NoStrip_RefusesEveryCommit(t *testing.T) {
	requirePosixExec(t)
	t.Setenv(TrailerStripEnvVar, "")
	_, _, wt := newGateRunWorktree(t, nil)
	head := run(t, wt, "git", "rev-parse", "HEAD")

	if _, err := StripCommitMessage(context.Background(), wt, dirtyFixRoundMessage); !errors.Is(err, ErrNoTrailerStrip) {
		t.Fatalf("StripCommitMessage without a strip: got %v, want ErrNoTrailerStrip", err)
	}
	out, err := gitInDir(wt, "commit", "--allow-empty", "-m", "no-mistakes: apply agent fixes")
	if err == nil {
		t.Fatal("agent commit succeeded without a fleet strip")
	}
	if !strings.Contains(out, trailerStripScript) || !strings.Contains(out, TrailerStripEnvVar) {
		t.Fatalf("refusal does not name the missing stripper:\n%s", out)
	}
	if got := run(t, wt, "git", "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved from %s to %s despite the refusal", head, got)
	}
}

func TestStripCommitMessage_CleanMessagePassesThroughByteIdentical(t *testing.T) {
	requirePosixExec(t)
	configureFleetStrip(t)
	_, _, wt := newGateRunWorktree(t, nil)

	clean := "no-mistakes(review): tighten the gate label check"
	stripped, err := StripCommitMessage(context.Background(), wt, clean)
	if err != nil {
		t.Fatal(err)
	}
	if stripped != clean {
		t.Fatalf("clean message not byte-identical: got %q want %q", stripped, clean)
	}
}

// conflictingRebase builds a run worktree whose detached head carries two
// commits with AI trailers, each conflicting with the upstream branch, so
// `git rebase upstream` stops twice.
func conflictingRebase(t *testing.T) string {
	t.Helper()
	_, _, wt := newGateRunWorktree(t, func(src string) {
		base := run(t, src, "git", "rev-parse", "HEAD")
		run(t, src, "git", "checkout", "-b", "upstream")
		writeFile(t, filepath.Join(src, "a.txt"), "upstream a\n")
		writeFile(t, filepath.Join(src, "b.txt"), "upstream b\n")
		run(t, src, "git", "add", ".")
		run(t, src, "git", "commit", "-m", "upstream side")
		run(t, src, "git", "checkout", "-b", "feature", base)
		writeFile(t, filepath.Join(src, "a.txt"), "feature a\n")
		run(t, src, "git", "add", ".")
		run(t, src, "git", "commit", "-m", dirtyFixRoundMessage)
		writeFile(t, filepath.Join(src, "b.txt"), "feature b\n")
		run(t, src, "git", "add", ".")
		run(t, src, "git", "commit", "-m", "fix(payments): keep the decline reason\n\nCo-authored-by: firstmate-worker <worker@firstmate.local>")
	})
	if _, err := gitInDir(wt, "rebase", "--exec", RestripRebaseExec, "upstream"); err == nil {
		t.Fatal("expected the rebase to stop on a conflict")
	}
	return wt
}

func TestGateRun_RebaseAmend_StripsEveryStop(t *testing.T) {
	requirePosixExec(t)
	configureFleetStrip(t)
	wt := conflictingRebase(t)

	if err := StripRebaseAmendMessage(context.Background(), wt); err != nil {
		t.Fatal(err)
	}
	messageFile := run(t, wt, "git", "rev-parse", "--path-format=absolute", "--git-path", "rebase-merge/message")
	inProgress, err := os.ReadFile(messageFile)
	if err != nil {
		t.Fatal(err)
	}
	assertCleanOfAIcoAuthors(t, "in-progress rebase message", string(inProgress))

	// The fix agent resolves each stop and continues; RestripRebaseExec
	// re-commits each pick, including the second stop, through the gate's
	// commit-msg hook.
	writeFile(t, filepath.Join(wt, "a.txt"), "resolved a\n")
	run(t, wt, "git", "add", "a.txt")
	if _, err := gitInDir(wt, "rebase", "--continue"); err == nil {
		t.Fatal("expected the rebase to stop on the second conflict")
	}
	writeFile(t, filepath.Join(wt, "b.txt"), "resolved b\n")
	run(t, wt, "git", "add", "b.txt")
	if out, err := gitInDir(wt, "rebase", "--continue"); err != nil {
		t.Fatalf("rebase --continue: %v\n%s", err, out)
	}

	body := run(t, wt, "git", "log", "--format=%B", "upstream..HEAD")
	assertCleanOfAIcoAuthors(t, "rebased commits", body)
	for _, want := range []string{"fix(payments): claim the collection slot", "fix(payments): keep the decline reason"} {
		if !strings.Contains(body, want) {
			t.Fatalf("rebased commits lost %q:\n%s", want, body)
		}
	}
}

func TestGateRun_RebaseAmend_RefusedWithoutStrip(t *testing.T) {
	requirePosixExec(t)
	t.Setenv(TrailerStripEnvVar, "")
	wt := conflictingRebase(t)

	if err := StripRebaseAmendMessage(context.Background(), wt); !errors.Is(err, ErrNoTrailerStrip) {
		t.Fatalf("StripRebaseAmendMessage without a strip: got %v, want ErrNoTrailerStrip", err)
	}
	writeFile(t, filepath.Join(wt, "a.txt"), "resolved a\n")
	run(t, wt, "git", "add", "a.txt")
	out, err := gitInDir(wt, "rebase", "--continue")
	if err == nil {
		t.Fatal("rebase --continue completed without a fleet strip")
	}
	if !strings.Contains(out, trailerStripScript) {
		t.Fatalf("refusal does not name the missing stripper:\n%s", out)
	}
	if _, err := os.Stat(run(t, wt, "git", "rev-parse", "--path-format=absolute", "--git-path", "rebase-merge")); err != nil {
		t.Fatalf("rebase no longer in progress after the refusal: %v", err)
	}
}

func TestSquashBodyAssembledFromStrippedFixRoundCommitsIsClean(t *testing.T) {
	requirePosixExec(t)
	configureFleetStrip(t)
	_, _, wt := newGateRunWorktree(t, nil)
	base := run(t, wt, "git", "rev-parse", "HEAD")

	// The shape GitHub assembles at squash-merge time: every commit message of
	// the PR concatenated. Each message is born through the strip first.
	for _, message := range []string{
		dirtyFixRoundMessage,
		"no-mistakes(review): tighten the gate label check",
		"fix(payments): keep the decline reason on rollback\n\nCo-authored-by: firstmate-worker <worker@firstmate.local>",
	} {
		stripped, err := StripCommitMessage(context.Background(), wt, message)
		if err != nil {
			t.Fatal(err)
		}
		run(t, wt, "git", "commit", "--allow-empty", "-m", stripped)
	}

	body := run(t, wt, "git", "log", "--format=%B", base+"..HEAD")
	assertCleanOfAIcoAuthors(t, "squash body", body)
	for _, want := range []string{"fix(payments): claim the collection slot", "no-mistakes(review): tighten the gate label check"} {
		if !strings.Contains(body, want) {
			t.Fatalf("squash body lost %q:\n%s", want, body)
		}
	}
}

package git

import (
	"context"
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
// keeping everything else, including human trailers. Its comment names the
// real script so resolveCommitMsgStripper's fleet-hook check matches it the
// way it matches the installed wrapper.
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

// installStrip writes content as the commit-msg hook of hooksDir, executable
// the way fm-spawn installs the fleet hook.
func installStrip(t *testing.T, hooksDir, content string) string {
	t.Helper()
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(hooksDir, "commit-msg")
	if err := os.WriteFile(hook, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hook, 0o500); err != nil {
		t.Fatal(err)
	}
	return hook
}

func assertCleanOfAIcoAuthors(t *testing.T, where, message string) {
	t.Helper()
	if strings.Contains(strings.ToLower(message), "co-authored-by:") {
		t.Fatalf("%s still carries an AI co-author trailer:\n%s", where, message)
	}
}

func TestStripCommitMessage_StripsFleetAndModelTrailersBeforeCreate(t *testing.T) {
	t.Parallel()
	requirePosixExec(t)
	dir := initTestRepo(t)
	installStrip(t, filepath.Join(dir, ".git", "hooks"), testStripScript)

	stripped, err := StripCommitMessage(context.Background(), dir, dirtyFixRoundMessage)
	if err != nil {
		t.Fatal(err)
	}
	assertCleanOfAIcoAuthors(t, "stripped message", stripped)
	if !strings.HasPrefix(stripped, "fix(payments): claim the collection slot\n\n- a body line kept intact.") {
		t.Fatalf("subject/body not preserved, got:\n%s", stripped)
	}

	run(t, dir, "git", "commit", "--allow-empty", "-m", stripped)
	committed := run(t, dir, "git", "log", "-1", "--format=%B")
	assertCleanOfAIcoAuthors(t, "create-branch commit", committed)
	if !strings.Contains(committed, "fix(payments): claim the collection slot") {
		t.Fatalf("committed message lost its subject:\n%s", committed)
	}
}

func TestStripCommitMessage_CleanMessagePassesThroughByteIdentical(t *testing.T) {
	t.Parallel()
	requirePosixExec(t)
	dir := initTestRepo(t)
	installStrip(t, filepath.Join(dir, ".git", "hooks"), testStripScript)

	clean := "no-mistakes(review): tighten the gate label check"
	stripped, err := StripCommitMessage(context.Background(), dir, clean)
	if err != nil {
		t.Fatal(err)
	}
	if stripped != clean {
		t.Fatalf("clean message not byte-identical: got %q want %q", stripped, clean)
	}
}

func TestStripCommitMessage_NoFleetStripLeavesMessageUntouched(t *testing.T) {
	t.Parallel()
	requirePosixExec(t)
	dir := initTestRepo(t) // no commit-msg hook, no in-worktree strip script

	stripped, err := StripCommitMessage(context.Background(), dir, dirtyFixRoundMessage)
	if err != nil {
		t.Fatal(err)
	}
	if stripped != dirtyFixRoundMessage {
		t.Fatalf("message changed without a fleet strip:\n%s", stripped)
	}
}

func TestStripCommitMessage_ResolvesStripFromWorktreeBin(t *testing.T) {
	t.Parallel()
	requirePosixExec(t)
	dir := initTestRepo(t)
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(binDir, "fm-git-strip-ai-trailers.sh")
	if err := os.WriteFile(script, []byte(testStripScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o500); err != nil {
		t.Fatal(err)
	}

	stripped, err := StripCommitMessage(context.Background(), dir, dirtyFixRoundMessage)
	if err != nil {
		t.Fatal(err)
	}
	assertCleanOfAIcoAuthors(t, "stripped message", stripped)
}

func TestStripCommitMessage_DoesNotDriveAProjectsOwnCommitMsgHook(t *testing.T) {
	t.Parallel()
	requirePosixExec(t)
	dir := initTestRepo(t)
	sentinel := filepath.Join(t.TempDir(), "project-hook-ran")
	hook := installStrip(t, filepath.Join(dir, ".git", "hooks"),
		"#!/bin/sh\nprintf '%s\\n' ran >> '"+sentinel+"'\n")

	stripped, err := StripCommitMessage(context.Background(), dir, dirtyFixRoundMessage)
	if err != nil {
		t.Fatal(err)
	}
	if stripped != dirtyFixRoundMessage {
		t.Fatalf("message changed by a non-fleet hook:\n%s", stripped)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("helper drove the project's own commit-msg hook %s; only the fleet strip may run here", hook)
	}
}

func TestStripRebaseAmendMessage_StripsBeforeRebaseContinue(t *testing.T) {
	t.Parallel()
	requirePosixExec(t)
	dir := initTestRepo(t)
	// Keep git's own completion of the amended pick non-interactive, the way
	// the pipeline's NonInteractiveEnv does for every git call it runs.
	run(t, dir, "git", "config", "core.editor", "true")
	base := run(t, dir, "git", "rev-parse", "HEAD")

	// topic side of the conflict
	run(t, dir, "git", "checkout", "-b", "topic")
	writeFile(t, filepath.Join(dir, "conflict.txt"), "topic side\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "topic side")

	// mainline side: an amend-destined commit that carries AI trailers, made
	// BEFORE the strip hook exists so it enters the rebase dirty.
	run(t, dir, "git", "checkout", "-b", "mainline", base)
	writeFile(t, filepath.Join(dir, "conflict.txt"), "mainline side\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", dirtyFixRoundMessage)

	// Stop the rebase on the conflict, then install the fleet strip, matching
	// a fix round that runs with the strip available.
	cmd := exec.Command("git", "rebase", "topic")
	cmd.Dir = dir
	if err := cmd.Run(); err == nil {
		t.Fatal("expected the rebase to stop on a conflict")
	}
	messageFile := filepath.Join(dir, ".git", "rebase-merge", "message")
	if _, err := os.Stat(messageFile); err != nil {
		t.Fatalf("expected a stopped rebase message file: %v", err)
	}
	installStrip(t, filepath.Join(dir, ".git", "hooks"), testStripScript)

	if err := StripRebaseAmendMessage(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	inProgress, err := os.ReadFile(messageFile)
	if err != nil {
		t.Fatal(err)
	}
	assertCleanOfAIcoAuthors(t, "in-progress rebase message", string(inProgress))
	if !strings.Contains(string(inProgress), "fix(payments): claim the collection slot") {
		t.Fatalf("rebase message lost its subject:\n%s", inProgress)
	}

	run(t, dir, "git", "add", "conflict.txt")
	run(t, dir, "git", "rebase", "--continue")
	amended := run(t, dir, "git", "log", "-1", "--format=%B")
	assertCleanOfAIcoAuthors(t, "amend-branch commit", amended)
}

func TestSquashBodyAssembledFromStrippedFixRoundCommitsIsClean(t *testing.T) {
	t.Parallel()
	requirePosixExec(t)
	dir := initTestRepo(t)
	installStrip(t, filepath.Join(dir, ".git", "hooks"), testStripScript)
	base := run(t, dir, "git", "rev-parse", "HEAD")

	// The shape GitHub assembles at squash-merge time: every commit message of
	// the PR concatenated. Each message is born through the strip first.
	for _, message := range []string{
		dirtyFixRoundMessage,
		"no-mistakes(review): tighten the gate label check",
		"fix(payments): keep the decline reason on rollback\n\nCo-authored-by: firstmate-worker <worker@firstmate.local>",
	} {
		stripped, err := StripCommitMessage(context.Background(), dir, message)
		if err != nil {
			t.Fatal(err)
		}
		run(t, dir, "git", "commit", "--allow-empty", "-m", stripped)
	}

	body := run(t, dir, "git", "log", "--format=%B", base+"..HEAD")
	if strings.Contains(strings.ToLower(body), "co-authored-by:") {
		t.Fatalf("squash body assembled from stripped commits carries a trailer:\n%s", body)
	}
	for _, want := range []string{"fix(payments): claim the collection slot", "no-mistakes(review): tighten the gate label check"} {
		if !strings.Contains(body, want) {
			t.Fatalf("squash body lost %q:\n%s", want, body)
		}
	}
}

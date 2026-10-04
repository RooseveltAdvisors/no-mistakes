package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// The fleet's AI-co-author trailer strip (firstmate's
// bin/fm-git-strip-ai-trailers.sh) is a firstmate fleet asset, not part of
// no-mistakes. An ordinary worker pane installs it as its commit-msg hook, but a
// no-mistakes run commits in a linked worktree of the gate, whose hooks resolve
// to the gate's hooks dir - so a fix-round commit (or the message of a
// fix-round rebase that is mid-conflict) would be born without the strip ever
// seeing it, the invocation hole that lets Co-authored-by trailers reach main
// through a GitHub squash body (bead fm-z0i73). The pipeline therefore runs the
// strip on every message it commits, the gate's managed commit-msg hook runs it
// on every commit the fix agent makes itself, and both refuse to commit when no
// strip resolves.

// TrailerStripEnvVar names the absolute path where this install keeps the
// firstmate fleet asset fm-git-strip-ai-trailers.sh. Runs of the firstmate
// repository itself fall back to the copy in the worktree's bin/.
const TrailerStripEnvVar = "NO_MISTAKES_AI_TRAILER_STRIP"

const trailerStripScript = "fm-git-strip-ai-trailers.sh"

// ErrNoTrailerStrip is returned when neither the configured path nor the
// worktree's bin/ holds an executable fleet strip; the commit is refused.
var ErrNoTrailerStrip = fmt.Errorf("commit refused: AI co-author trailer stripper %s not found; set %s to its path", trailerStripScript, TrailerStripEnvVar)

// resolveCommitMsgStripper returns the fleet strip for the worktree dir: the
// configured TrailerStripEnvVar path first, then dir/bin/fm-git-strip-ai-trailers.sh.
// The gate's managed commit-msg hook applies the same order.
func resolveCommitMsgStripper(dir string) (string, error) {
	if configured := os.Getenv(TrailerStripEnvVar); configured != "" && isExecutableFile(configured) {
		return configured, nil
	}
	if script := filepath.Join(dir, "bin", trailerStripScript); isExecutableFile(script) {
		return script, nil
	}
	return "", ErrNoTrailerStrip
}

// isExecutableFile reports whether path is an executable regular file.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

// runStripper invokes the fleet strip in its commit-msg hook mode on
// messageFile: the script rewrites that file in place. A failing strip fails the
// caller: a message that could not be proven trailer-free is never committed.
func runStripper(ctx context.Context, dir, stripper, messageFile string) error {
	cmd := exec.CommandContext(ctx, stripper, messageFile)
	cmd.Dir = dir
	shellenv.ConfigureShellCommand(cmd)
	if out, err := shellenv.CombinedOutputShellCommand(cmd); err != nil {
		return fmt.Errorf("run AI co-author trailer strip %s on %s: %w: %s",
			stripper, messageFile, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// StripCommitMessage rewrites message through the fleet AI-co-author trailer
// strip before a pipeline-authored fix-round commit is born, and returns the
// message to commit. It returns ErrNoTrailerStrip when no strip resolves for
// dir. A message the strip does not change is returned byte-identical.
func StripCommitMessage(ctx context.Context, dir, message string) (string, error) {
	if runtime.GOOS == "windows" {
		// The strip is a POSIX shell script and the fleet does not run on
		// Windows; the managed commit-msg hook is not installed there either.
		return message, nil
	}
	stripper, err := resolveCommitMsgStripper(dir)
	if err != nil {
		return "", err
	}
	staged, err := os.CreateTemp("", "no-mistakes-commit-msg-*")
	if err != nil {
		return "", fmt.Errorf("stage commit message for AI trailer strip: %w", err)
	}
	messageFile := staged.Name()
	defer func() { _ = os.Remove(messageFile) }()
	if _, err := staged.WriteString(message); err != nil {
		_ = staged.Close()
		return "", fmt.Errorf("stage commit message for AI trailer strip: %w", err)
	}
	if err := staged.Close(); err != nil {
		return "", fmt.Errorf("stage commit message for AI trailer strip: %w", err)
	}
	if err := runStripper(ctx, dir, stripper, messageFile); err != nil {
		return "", err
	}
	stripped, err := os.ReadFile(messageFile)
	if err != nil {
		return "", fmt.Errorf("read stripped commit message: %w", err)
	}
	return unchangedWhenClean(message, string(stripped)), nil
}

// RestripRebaseExec is the `git rebase --exec` command a fix-round rebase runs
// after every replayed pick. `git rebase --continue` births a stopped pick
// without running commit-msg hooks, so the amend re-commits each pick through
// the gate's managed commit-msg hook, including every stop the fix agent
// continues past.
const RestripRebaseExec = "git commit --amend --no-edit --allow-empty"

// StripRebaseAmendMessage rewrites the in-progress message of a rebase that
// stopped on conflicts - the amend side of a fix round - and returns
// ErrNoTrailerStrip when no strip resolves, so the rebase is refused before the
// fix agent is asked to continue it. Later stops of the same rebase are
// stripped by RestripRebaseExec. It is a no-op when no rebase is in progress.
func StripRebaseAmendMessage(ctx context.Context, dir string) error {
	messageFile, err := Run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", "rebase-merge/message")
	if err != nil {
		return fmt.Errorf("resolve rebase message file: %w", err)
	}
	messageFile = strings.TrimSpace(messageFile)
	if _, err := os.Stat(messageFile); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat rebase message file: %w", err)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	stripper, err := resolveCommitMsgStripper(dir)
	if err != nil {
		return err
	}
	return runStripper(ctx, dir, stripper, messageFile)
}

// unchangedWhenClean returns original untouched when the strip's rewrite only
// differs by trailing newlines (the script re-emits every line with a trailing
// newline), so a clean message passes through byte-identical; otherwise it
// returns the stripped text.
func unchangedWhenClean(original, stripped string) string {
	if strings.TrimRight(stripped, "\n") == strings.TrimRight(original, "\n") {
		return original
	}
	return strings.TrimRight(stripped, "\n")
}

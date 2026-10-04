package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// The fleet's AI-co-author trailer strip (firstmate's
// bin/fm-git-strip-ai-trailers.sh) is installed per task as the commit-msg
// hook, so an ordinary worker `git commit` or `git commit --amend` rewrites the
// message file in place before the commit object exists. A no-mistakes run
// executes outside that pane: the daemon's git carries no core.hooksPath
// override, so a fix-round commit (or the message of a fix-round rebase that is
// mid-conflict) can be born without the strip ever seeing it - the invocation
// hole that lets Co-authored-by trailers reach main through a GitHub squash body
// (bead fm-z0i73). These helpers run the same strip on the message file first,
// so a fix-round commit is never born with an AI co-author trailer, and a
// repository without the fleet strip commits exactly as it does today.

// commitMsgStripMarker is the name that identifies the fleet strip among every
// other commit-msg hook a repository could configure. Only a hook whose file
// content names fm-git-strip-ai-trailers is driven from here: a project's own
// commit-msg hook keeps running exactly once - at git's own commit time - rather
// than being re-driven from this helper on a temp file it may not expect.
const commitMsgStripMarker = "fm-git-strip-ai-trailers"

// resolveCommitMsgStripper locates the fleet strip the way an ordinary commit
// does: git's own hooks resolution first (core.hooksPath from config or
// GIT_CONFIG env, else the repository hooks directory), which is where
// fm-spawn installs the per-task commit-msg hook, then the in-worktree
// bin/fm-git-strip-ai-trailers.sh that runs of the firstmate repository itself
// carry. Returns "" when no fleet strip resolves - the caller then leaves the
// message untouched, matching a machine with no fleet installed.
func resolveCommitMsgStripper(ctx context.Context, dir string) string {
	if runtime.GOOS == "windows" {
		// The strip is a POSIX shell script; fleet launches do not install it
		// on Windows, so nothing resolves and commits behave as before.
		return ""
	}
	if hooksDir, err := Run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", "hooks"); err == nil {
		if hook := filepath.Join(strings.TrimSpace(hooksDir), "commit-msg"); isExecutableFile(hook) &&
			hookNamesFleetStrip(hook) {
			return hook
		}
	}
	if script := filepath.Join(dir, "bin", "fm-git-strip-ai-trailers.sh"); isExecutableFile(script) {
		return script
	}
	return ""
}

// isExecutableFile reports whether path is an executable regular file.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

// hookNamesFleetStrip reports whether a commit-msg hook file is (or wraps) the
// fleet strip, by reading a bounded prefix of its content. The installed hook
// embeds the absolute fm-git-strip-ai-trailers.sh path, and the script itself
// names itself in its usage text, so both forms match; a project's own hook
// (husky, commitizen, ...) does not and is left for git to run itself.
func hookNamesFleetStrip(hook string) bool {
	prefix := make([]byte, 4096)
	f, err := os.Open(hook)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	n, _ := f.Read(prefix)
	return strings.Contains(string(prefix[:n]), commitMsgStripMarker)
}

// runStripper invokes the fleet strip in its commit-msg hook mode on
// messageFile: the script rewrites that file in place. dir becomes the hook's
// working directory so the installed wrapper's own hook chaining resolves the
// hooks of the repository the message belongs to rather than of whatever
// directory the pipeline process happens to live in. A failing strip fails the
// caller: a message that could not be proven trailer-free is never committed.
func runStripper(ctx context.Context, dir, stripper, messageFile string) error {
	cmd := exec.CommandContext(ctx, stripper, messageFile)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("run AI co-author trailer strip %s on %s: %w: %s",
			stripper, messageFile, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// StripCommitMessage rewrites message through the fleet AI-co-author trailer
// strip before a pipeline-authored fix-round commit is born, and returns the
// message to commit. When no fleet strip resolves in dir (a repository that was
// never launched through the fleet), message is returned unchanged. When the
// strip runs, a message it does not change is returned byte-identical to the
// input, so clean fix-round messages keep their exact bytes.
func StripCommitMessage(ctx context.Context, dir, message string) (string, error) {
	stripper := resolveCommitMsgStripper(ctx, dir)
	if stripper == "" {
		return message, nil
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

// StripRebaseAmendMessage rewrites the in-progress message of a rebase that
// stopped on conflicts - the amend side of a fix round: when the fix agent
// resolves the conflicts and runs `git rebase --continue`, git births that
// message as the new commit object, so the strip has to run on the file first.
// It is a no-op when no rebase is in progress or when no fleet strip resolves
// in dir.
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
	stripper := resolveCommitMsgStripper(ctx, dir)
	if stripper == "" {
		return nil
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

//go:build unix

package quota

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDefaultFetchPreservesStderr(t *testing.T) {
	helper := quotaFetchHelper(t)
	t.Setenv("NM_QUOTA_FETCH_HELPER", "error")
	_, err := DefaultFetch(context.Background(), helper, nil)
	if err == nil || !strings.Contains(err.Error(), "quota helper stderr") {
		t.Fatalf("DefaultFetch() error = %v, want captured stderr", err)
	}
}

func TestDefaultFetchBoundsAndDrainsStderr(t *testing.T) {
	helper := quotaFetchHelper(t)
	t.Setenv("NM_QUOTA_FETCH_HELPER", "large-error")
	_, err := DefaultFetch(context.Background(), helper, nil)
	if err == nil {
		t.Fatal("DefaultFetch() error = nil, want helper failure")
	}
	message := err.Error()
	if !strings.Contains(message, "quota helper stderr start") {
		t.Fatalf("DefaultFetch() lost useful stderr prefix: %q", message)
	}
	if !strings.Contains(message, "stderr bytes truncated") {
		t.Fatalf("DefaultFetch() did not disclose truncation: %q", message)
	}
	if strings.Contains(message, "quota helper stderr end") {
		t.Fatalf("DefaultFetch() retained stderr beyond the bound: %d bytes", len(message))
	}
	if len(message) > quotaStderrLimit+512 {
		t.Fatalf("DefaultFetch() error grew past stderr bound: %d bytes", len(message))
	}
}

func TestDefaultFetchReapsGrandchildOnCleanExit(t *testing.T) {
	helper := quotaFetchHelper(t)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	t.Setenv("NM_QUOTA_FETCH_HELPER", "reap")
	t.Setenv("NM_QUOTA_FETCH_PID_FILE", pidFile)

	if _, err := DefaultFetch(context.Background(), helper, nil); err != nil {
		t.Fatalf("DefaultFetch(): %v", err)
	}
	payload, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(payload)))
	if err != nil || pid <= 0 {
		t.Fatalf("grandchild pid = %q: %v", payload, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("quota helper grandchild %d survived clean wrapper exit", pid)
}

func quotaFetchHelper(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quota-fetch-helper")
	script := `#!/bin/sh
case "$NM_QUOTA_FETCH_HELPER" in
  error)
    echo "quota helper stderr" >&2
    exit 23
    ;;
  large-error)
    echo "quota helper stderr start" >&2
    i=0
    while [ "$i" -lt 4096 ]; do
      printf '0123456789abcdef' >&2
      i=$((i + 1))
    done
    echo "quota helper stderr end" >&2
    exit 23
    ;;
  reap)
    sleep 120 &
    child=$!
    echo "$child" > "$NM_QUOTA_FETCH_PID_FILE"
    printf '%s\n' '{"schemaVersion":3,"providers":[]}'
    exit 0
    ;;
esac
exit 24
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write quota helper: %v", err)
	}
	return path
}

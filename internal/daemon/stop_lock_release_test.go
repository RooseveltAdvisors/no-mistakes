package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestStopDetachedDaemonReleasesShutdownConnectionBeforeWaiting pins the other
// half of the shutdown contract: the daemon's accept loop drains in-flight
// connections before its process can exit, so the stopping CLI must close the
// connection that carried the shutdown request before it waits. Holding it open
// deadlocks the wait against the exit it is waiting for, which leaves the old
// daemon alive (and holding the singleton lock) for the whole stop timeout.
func TestStopDetachedDaemonReleasesShutdownConnectionBeforeWaiting(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_TEST_DAEMON_STOP_TIMEOUT", "2s")

	srv := ipc.NewServer()
	srv.Handle(ipc.MethodShutdown, func(context.Context, json.RawMessage) (interface{}, error) {
		go srv.Close()
		return &ipc.ShutdownResult{OK: true}, nil
	})
	if err := srv.Listen(p.Socket()); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.ServeReady() }()
	t.Cleanup(func() {
		srv.Close()
		<-serveDone
	})

	// The daemon is "alive" until its accept loop has drained every connection,
	// which is exactly when the real process reaches its exit path.
	var drained atomic.Bool
	go func() {
		<-serveDone
		drained.Store(true)
		serveDone <- nil
	}()
	oldHealth := daemonHealthCheck
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return !drained.Load(), nil }
	t.Cleanup(func() { daemonHealthCheck = oldHealth })

	if err := stopDetachedDaemon(p, daemonPIDFile{}); err != nil {
		t.Fatalf("stopDetachedDaemon: %v", err)
	}
	if !drained.Load() {
		t.Fatal("stopDetachedDaemon returned before the daemon could finish shutting down")
	}
}

// TestWaitForDaemonStopWaitsForSingletonLockRelease pins the restart contract:
// the daemon's IPC socket dies at the start of shutdown, but its singleton lock
// (lock.go) is released only when the process actually exits. Reporting
// "stopped" at socket death lets `daemon restart` launch the next daemon into a
// lock the old one still holds, and that child dies immediately with
// "daemon child N exited before readiness: exit status 1".
func TestWaitForDaemonStopWaitsForSingletonLockRelease(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_TEST_DAEMON_STOP_TIMEOUT", "10s")

	// A stand-in for a daemon whose socket is already gone while the process
	// is still winding down (run drain, telemetry flush, log close).
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "NM_DAEMON_HELPER_PROCESS=block")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-reaped:
		case <-time.After(2 * time.Second):
		}
	})

	startedAt, err := daemonProcessStartTime(pid)
	if err != nil {
		t.Fatal(err)
	}
	stopping := daemonPIDFile{PID: pid, StartedAt: startedAt.UTC()}
	if err := writeDaemonPIDFile(p.PIDFile(), stopping); err != nil {
		t.Fatal(err)
	}

	oldHealth := daemonHealthCheck
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return false, nil }
	t.Cleanup(func() { daemonHealthCheck = oldHealth })

	oldKill := daemonKillPID
	killed := false
	daemonKillPID = func(target int) error {
		killed = true
		return oldKill(target)
	}
	t.Cleanup(func() { daemonKillPID = oldKill })

	const exitDelay = 300 * time.Millisecond
	go func() {
		time.Sleep(exitDelay)
		_ = cmd.Process.Kill()
	}()

	started := time.Now()
	if err := waitForDaemonStop(p, stopping); err != nil {
		t.Fatalf("waitForDaemonStop: %v", err)
	}
	elapsed := time.Since(started)

	if elapsed < exitDelay-50*time.Millisecond {
		t.Fatalf("waitForDaemonStop returned after %v, before the daemon process exited; the singleton lock is still held at that point", elapsed)
	}
	running, err := daemonProcessRunning(pid)
	if err == nil && running {
		t.Fatal("waitForDaemonStop reported the daemon stopped while its process was still alive")
	}
	if killed {
		t.Fatal("waitForDaemonStop killed a daemon that was already exiting on its own")
	}
	if _, err := os.Stat(p.PIDFile()); !os.IsNotExist(err) {
		t.Fatalf("pid file should be cleaned up after a graceful stop, stat err = %v", err)
	}
}

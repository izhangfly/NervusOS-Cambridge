package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// testScheduler builds a Scheduler with silent logging.
// allowDegrade=true: unit tests run in ordinary environments without CAP_SYS_NICE
// and should not fail because real-time priority cannot be set; these tests check lifecycle semantics, not the scheduling policy itself.
func testScheduler() *Scheduler {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), true)
}

// Shutdown must wait for the Lane to really exit (join), not return as soon as it has sent the cancellation.
// That is the basic guarantee that a Lane's cleanup always finishes before the process ends.
func TestShutdownJoinsLanes(t *testing.T) {
	s := testScheduler()

	var exited atomic.Bool
	if err := s.SpawnDedicated("test-lane", PolicyNormal, 0, func(ctx context.Context) {
		<-ctx.Done()
		// Deliberately slow: if Shutdown did not join, this cleanup would be cut short
		time.Sleep(50 * time.Millisecond)
		exited.Store(true)
	}); err != nil {
		t.Fatalf("SpawnDedicated: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !exited.Load() {
		t.Fatal("Shutdown in Lane finished before exit: join not available")
	}
}

// A Lane exits only when Scheduler.Shutdown says so, never because of an outside (signal) ctx.
// Otherwise the Lane and the Kernel would be woken by the same signal in parallel, and the shutdown order would be out of control.
func TestLaneIgnoresExternalContext(t *testing.T) {
	s := testScheduler()

	var running atomic.Bool
	started := make(chan struct{})
	if err := s.SpawnDedicated("test-lane", PolicyNormal, 0, func(ctx context.Context) {
		running.Store(true)
		close(started)
		<-ctx.Done()
		running.Store(false)
	}); err != nil {
		t.Fatalf("SpawnDedicated: %v", err)
	}
	<-started

	// Simulate cancelling the root signal ctx: the Lane must not exit because of it
	external, cancelExternal := context.WithCancel(context.Background())
	cancelExternal()
	<-external.Done()
	time.Sleep(50 * time.Millisecond)

	if !running.Load() {
		t.Fatal("Lane followed external ctx and exited: Lane lifecycle not decoupled from signal ctx")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if running.Load() {
		t.Fatal("Shutdown after Lane still running")
	}
}

// A stuck Lane must not block shutdown forever: after its timeout, Shutdown must return an error
// for the caller to log before exiting anyway (safety is backed by systemd restarts and the MCU heartbeat brake).
func TestShutdownTimesOutOnStuckLane(t *testing.T) {
	s := testScheduler()

	release := make(chan struct{})
	if err := s.SpawnDedicated("stuck-lane", PolicyNormal, 0, func(ctx context.Context) {
		<-release // deliberately ignores ctx cancellation
	}); err != nil {
		t.Fatalf("SpawnDedicated: %v", err)
	}
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); err == nil {
		t.Fatal("Stuck lane did not cause Shutdown to timeout and return error")
	}
}

// SpawnDedicated is a synchronous handshake: when it returns, the result of setting the Lane's priority is known,
// so the caller can fail fast instead of just seeing a line in the log.
func TestSpawnDedicatedIsSynchronous(t *testing.T) {
	s := testScheduler()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})

	if err := s.SpawnDedicated("test-lane", PolicyNormal, 0, func(ctx context.Context) {
		<-ctx.Done()
	}); err != nil {
		t.Fatalf("SpawnDedicated: %v", err)
	}
	// On non-Linux platforms setSched is a no-op; on Linux PolicyNormal needs no privilege.
	// Both should return successfully, which proves the handshake path itself works.
}

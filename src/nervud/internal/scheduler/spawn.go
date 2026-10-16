package scheduler

import (
	"context"
	"fmt"
	"runtime"
	"sync"

	"log/slog"
)

// New creates a scheduler.
//
// allowDegrade decides whether, when setting the real-time priority fails, to fall back to normal priority and keep running with only a warning.
func New(log *slog.Logger, allowDegrade bool) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		log:          log,
		allowDegrade: allowDegrade,
		ctx:          ctx,
		cancel:       cancel,
	}
}

// Scheduler creates execution Lanes bound to OS threads with a real-time priority,
// and owns the lifecycle of every Lane (cancel + wait for exit).
type Scheduler struct {
	log          *slog.Logger
	allowDegrade bool

	ctx    context.Context    // parent ctx of every lane
	cancel context.CancelFunc // fired by Shutdown
	wg     sync.WaitGroup     // waits for every lane to really exit (join)
}

// SpawnDedicated starts a long-running goroutine on a dedicated thread with a real-time priority.
//
// How it works:
//
//	go func { // 1) start a new goroutine
//	 runtime.LockOSThread // 2) pin it to a dedicated OS thread:
//	 //  from then on the Go scheduler won't schedule other goroutines onto
//	 //  this thread, nor move the current goroutine to another thread.
//	 //  That's the precondition for the priority to take effect reliably.
//	 //  Note: there's deliberately no defer UnlockOSThread, because
//	 //  this thread has a real-time priority and must not go back to the Go runtime
//	 //  for reuse (an ordinary goroutine would inherit the high priority by accident).
//	 //  Letting the thread end with the goroutine after fn returns is cleanest.
//	 lane.applySelf // 3) set SCHED_FIFO/RR + priority on the current (locked) thread
//	 ready <- err // 4) report the result back to the caller synchronously
//	 fn(ctx) // 5) run the actual loop until Shutdown cancels ctx
//	}(
//
// The function is a synchronous handshake: it blocks until step 4), so when it returns the priority is known to be set
//
// (or known to be impossible to set). Only that way can the caller react to "the Safety Lane has no real-time priority"
//
// instead of just seeing an error line in the log.
//
// Panic policy: panics inside a lane aren't recovered. nervud as a whole crashes and exits, with systemd restart +
// the MCU heartbeat brake as the backstop. Recovering and continuing is forbidden: a Safety path in an unknown state is more dangerous than a dead one.
//
// Parameters:
//   - name: the Lane name, for logs and diagnostics
//   - policy: the scheduling policy (FIFO/RR/NORMAL)
//   - priority: the real-time priority (1..99; ignored and set to 0 for NORMAL)
//   - fn: the long-running loop, which must return promptly on ctx.Done
//
// Note the ctx fn receives is the Scheduler's own ctx, not the root signal ctx.
func (s *Scheduler) SpawnDedicated(name string, policy Policy, priority int, fn func(context.Context)) error {
	lane := &Lane{name: name, policy: policy, priority: priority}

	// Capacity 1: even if the caller stops receiving for some reason, the goroutine won't get stuck here and leak
	ready := make(chan error, 1)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		// 2) Bind to a dedicated OS thread. No UnlockOSThread on purpose: see the long note above
		runtime.LockOSThread()

		// 3) Apply the real-time scheduling policy. A real syscall on Linux; a no-op on non-Linux
		err := lane.applySelf()
		ready <- err // 4) report back synchronously; the caller decides between fail-fast and degrading

		if err != nil && !s.allowDegrade {
			// Production semantics: if the real-time priority can't be set, don't run this lane, and let the kernel exit as a whole
			return
		}
		if err != nil {
			s.log.Warn("scheduler: lane degraded to normal priority (dev mode)",
				"lane", name, "policy", policy.String(), "priority", priority, "err", err)
		} else {
			s.log.Info("scheduler: lane ready",
				"lane", name, "policy", policy.String(), "priority", priority)
		}

		// 5) Run the actual loop
		fn(s.ctx)
		s.log.Info("scheduler: lane exited", "lane", name)
	}()

	if err := <-ready; err != nil && !s.allowDegrade {
		return fmt.Errorf("lane %s: Can not set real-time scheduling policy %s/%d (production environment does not allow downgrade;"+
			"check CAP_SYS_NICE, LimitRTPRIO and kernel CONFIG_RT_GROUP_SCHED): %w",
			name, policy.String(), priority, err)
	}
	return nil
}

// Shutdown cancels every lane and waits for them to really exit (join).
//
// Why the join must be explicit: lanes run on dedicated OS threads, and if main just returns, the process ends at once
// without any guarantee that a lane's cleanup (revoking, brake confirmation, flushing the audit) finishes. Go waits for no goroutine.
//
// It's always called after Kernel.Run returns, giving a fixed shutdown order:
//
//	SIGTERM -> Kernel.stopAll (stop modules in reverse) -> Kernel.Run returns -> Shutdown (reclaim lanes) -> main returns
//
// Lanes are the lowest layer of infrastructure, so they're reclaimed last; ctx caps this step, and on timeout it gives up waiting and returns
// an error. The process still exits; safety is backed by systemd restart and the MCU heartbeat brake.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.cancel()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("scheduler: lane not exited within timeout: %w", ctx.Err())
	}
}

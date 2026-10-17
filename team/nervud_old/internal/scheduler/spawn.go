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
// allowDegrade decides whether, when setting real-time priority fails, to fall back to normal priority and carry on with only a warning
func New(log *slog.Logger, allowDegrade bool) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		log:          log,
		allowDegrade: allowDegrade,
		ctx:          ctx,
		cancel:       cancel,
	}
}

// Scheduler creates execution Lanes bound to OS threads with real-time priority,
// and owns the lifecycle of every Lane (cancellation + waiting for exit).
type Scheduler struct {
	log          *slog.Logger
	allowDegrade bool

	ctx    context.Context    // parent ctx of every lane
	cancel context.CancelFunc // fired by Shutdown
	wg     sync.WaitGroup     // waits for every lane to really exit (join)
}

// SpawnDedicated starts a long-running goroutine on a dedicated thread with real-time priority.
//
// How it goes:
//
//	go func() {                     // 1) start a new goroutine
//	    runtime.LockOSThread()      // 2) pin it to a dedicated OS thread:
//	                                //    from now on the Go scheduler puts no other goroutine on
//	                                //    this thread and never moves this goroutine to another one.
//	                                //    That is what makes the scheduling priority stick.
//	                                //    Note: there is deliberately NO defer UnlockOSThread, because
//	                                //    this thread has real-time priority and must not go back to the Go runtime
//	                                //    for reuse (an ordinary goroutine would inherit the high priority by accident).
//	                                //    Letting the thread end with the goroutine after fn returns is cleanest.
//	    lane.applySelf()            // 3) set SCHED_FIFO/RR + priority on the current (locked) thread
//	    ready <- err                // 4) report the result back to the caller synchronously
//	    fn(ctx)                     // 5) run the actual loop until Shutdown cancels ctx
//	}(
//
// This function is a SYNCHRONOUS handshake: it blocks until step 4), so when it returns the priority is known to be set
// (or known to have failed). Only then can the caller react to "the Safety Lane has no real-time priority"
// instead of just seeing a line in the log.
//
// Panic policy: a panic inside a lane is NOT recovered. nervud crashes as a whole and exits; systemd restarts it and
// the MCU heartbeat brake is the backstop. Recovering and carrying on is forbidden: a Safety path in an unknown state is more dangerous than a dead one.
//
// Parameters:
//   - name     the Lane's name, for logs and diagnosis
//   - policy   scheduling policy (FIFO/RR/NORMAL)
//   - priority real-time priority (1..99; ignored and set to 0 for NORMAL)
//   - fn       the long-running loop; it must return promptly on ctx.Done()
//
// Note that the ctx fn receives is the Scheduler's own ctx, not the root signal ctx.
func (s *Scheduler) SpawnDedicated(name string, policy Policy, priority int, fn func(context.Context)) error {
	lane := &Lane{name: name, policy: policy, priority: priority}

	// Capacity 1: even if the caller stops receiving for some reason, the goroutine never blocks here and leaks
	ready := make(chan error, 1)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		// 2) Bind a dedicated OS thread. Deliberately no UnlockOSThread: see the long note above
		runtime.LockOSThread()

		// 3) Apply the real-time scheduling policy. A real syscall on Linux; a no-op elsewhere
		err := lane.applySelf()
		ready <- err // 4) report back synchronously; the caller decides between failing fast and degrading

		if err != nil && !s.allowDegrade {
			// Production semantics: if real-time priority cannot be set, do not run this lane; let the whole kernel exit
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

// Shutdown cancels every lane and waits for each to really exit (join).
//
// Why an explicit join is needed: lanes run on dedicated OS threads, and if main simply returns the process ends at once;
// the lanes' cleanup (revoking authority, confirming the brake, flushing the audit) is not guaranteed to finish. Go does not wait for any goroutine.
//
// It is always called after Kernel.Run returns, which gives a fixed shutdown order:
//
//	SIGTERM -> Kernel.stopAll (stop modules in reverse) -> Kernel.Run returns -> Shutdown (reclaim lanes) -> main returns
//
// Lanes are the lowest layer of infrastructure, so they are reclaimed last; ctx bounds this step, and on timeout it gives up waiting and returns
// an error. The process still exits, with systemd restarts and the MCU heartbeat brake as the safety backstop.
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

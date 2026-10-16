// Package logging provides a non-blocking, bounded log writer that drops when full
//
// # Why the kernel needs it
//
// The default slog handler writes to stderr synchronously. As soon as stderr / journald slows down or stalls
//
//	(pipe not being read, disk full, journald restarting), any goroutine writing a log line blocks on
//
// that write. For an ordinary service that's just latency; for nervud it's a safety problem:
//
//   - a real-time Lane writing one lifecycle log line on an RT-priority thread can be held up,
//     when it should give up the CPU as soon as possible;
//   - worse is the shutdown path: if some goroutine is stuck in a synchronous write holding the handler lock,
//     main / kernel calling the same logger during shutdown get stuck on that lock too,
//     and exit limits such as laneStopTimeout / moduleStopTimeout all stop working.
//
// AsyncWriter takes writes off the caller's thread: the caller only pushes bytes into a bounded queue
//
//	(non-blocking), and a separate goroutine does the real write. When the queue is full it drops and counts,
//
// rather than blocking: for the TCB, losing a few log lines is far better than stalling a safety path
package logging

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// defaultFlushTimeout caps how long Close waits for the queue to drain
//
// Close happens before the process exits. Even if the underlying writer is completely stuck, Close must return within
// this limit, or exiting itself gets stuck on logging, which is exactly the failure this package removes
const defaultFlushTimeout = 500 * time.Millisecond

// AsyncWriter is a non-blocking io.Writer wrapper
//
// Concurrency-safe: Write may be called by any number of goroutines at once (slog handlers can be called from
// several module goroutines anyway)
type AsyncWriter struct {
	dst io.Writer

	queue chan []byte
	stop  chan struct{}
	done  chan struct{}

	stopOnce     sync.Once
	dropped      atomic.Uint64
	flushTimeout time.Duration
}

// NewAsyncWriter starts a background goroutine that writes the bytes in queue to dst
//
// queueDepth is how many messages the queue can hold. It isn't a byte budget: log lines have a bounded length,
// so a count is enough and easier to reason about. The deeper it is, the less likely logs are dropped, but the more memory it uses
// and the longer the flush at shutdown
func NewAsyncWriter(dst io.Writer, queueDepth int) *AsyncWriter {
	if queueDepth < 1 {
		queueDepth = 1
	}
	w := &AsyncWriter{
		dst:          dst,
		queue:        make(chan []byte, queueDepth),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
		flushTimeout: defaultFlushTimeout,
	}
	go w.run()
	return w
}

// Write copies p into the queue and never blocks
//
// The copy is required: slog reuses the buffer it passes in, and p's contents may be overwritten as soon as Write returns,
// while the real write happens on another goroutine, after this return
//
// The return value is always (len(p), nil): even if this line is dropped, slog isn't told about an error. A failed log write
// shouldn't hand business logic an error to deal with; that would only spread a logging problem into a business problem
func (w *AsyncWriter) Write(p []byte) (int, error) {
	b := make([]byte, len(p))
	copy(b, p)

	select {
	case w.queue <- b:
	case <-w.stop:
		// Already closing: bypass the queue and write synchronously, doing our best to make the last lines during exit visible.
		// The process is exiting by now, a brief synchronous block is acceptable, and Close has flushTimeout as a backstop
		_, _ = w.dst.Write(p)
	default:
		// Queue full: drop and count. Never block the caller; that's the whole reason this package exists
		w.dropped.Add(1)
	}
	return len(p), nil
}

func (w *AsyncWriter) run() {
	defer close(w.done)
	for {
		select {
		case b := <-w.queue:
			_, _ = w.dst.Write(b) // logging is best effort; a failed write has nowhere to report and shouldn't panic
		case <-w.stop:
			// Drain what's left in the queue, then exit
			for {
				select {
				case b := <-w.queue:
					_, _ = w.dst.Write(b)
				default:
					return
				}
			}
		}
	}
}

// Dropped returns the total number of dropped log lines, to be recorded once at shutdown
func (w *AsyncWriter) Dropped() uint64 { return w.dropped.Load() }

// Close stops the background goroutine and does its best to drain the queue within flushTimeout
//
// Idempotent. Returns on timeout and never waits forever: the exit path itself must not get stuck on logging
func (w *AsyncWriter) Close() error {
	w.stopOnce.Do(func() { close(w.stop) })
	select {
	case <-w.done:
	case <-time.After(w.flushTimeout):
	}
	return nil
}

// This file implements each connection's own bounded outbound queue.
//
// Dispatch forwarding requires "while connection A handles a Request, write a Dispatch to connection B's socket".
// That's a cross-connection write that only B's own writer goroutine can perform safely (frame.go's
// WriteFrame requires a single writer per connection). This file turns that into a general mechanism: any
// goroutine can safely push an Envelope into a connection's outbox, while the actual socket
// write always happens only in that connection's own writer goroutine (see runWriter in conn.go)
package ipc

import (
	"sync"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"google.golang.org/protobuf/proto"
)

// outboundQueue is a single connection's bounded outbound queue, bounded by bytes rather than message count
// (unbounded queues counted only by message count are explicitly forbidden). push never blocks the caller: the caller may
// be another connection's goroutine (when forwarding a Dispatch), and blocking it would let one slow consumer hold up
// unrelated connections
type outboundQueue struct {
	mu       sync.Mutex
	items    []*ipcv1.Envelope
	bytes    int
	maxBytes int
	closed   bool

	// wake is a capacity-1 doorbell: push/close use a non-blocking send to tell the writer "something new / time to
	// exit". On waking, the writer always goes back under the lock to recheck the queue state, so extra or early
	// wake-ups only cause one harmless empty spin, and needn't correspond exactly to each push
	wake chan struct{}
}

func newOutboundQueue(maxBytes int) *outboundQueue {
	return &outboundQueue{maxBytes: maxBytes, wake: make(chan struct{}, 1)}
}

// push tries to add env to the queue. If the queue is closed, or adding it would exceed maxBytes, it always returns
// false, without any attempt to "kick out old messages to make room": the architecture requires that RPC Responses are never silently
// dropped. Full is full, and the caller decides what to do (usually closing this connection as a slow consumer)
func (q *outboundQueue) push(env *ipcv1.Envelope) bool {
	size := proto.Size(env)

	q.mu.Lock()
	if q.closed || q.bytes+size > q.maxBytes {
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, env)
	q.bytes += size
	q.mu.Unlock()

	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// pop blocks until the queue is non-empty, or closed and drained. In the latter case it returns (nil, false).
//
// Closing doesn't truncate entries already queued: close only blocks new pushes, and what's already queued is still popped
// and written one by one. That way a connection's normal wrap-up (send the last Response, then close) and an error wrap-up
// (the socket can't be written any more) are both covered by the same logic, without treating them differently
func (q *outboundQueue) pop() (*ipcv1.Envelope, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			env := q.items[0]
			q.items[0] = nil // don't keep a reference to the dequeued envelope; helps the GC
			q.items = q.items[1:]
			q.bytes -= proto.Size(env)
			q.mu.Unlock()
			return env, true
		}
		if q.closed {
			q.mu.Unlock()
			return nil, false
		}
		q.mu.Unlock()
		<-q.wake
	}
}

// close marks the queue as no longer accepting pushes and wakes a writer that may be blocked in pop.
// Idempotent: calling it repeatedly is safe (both serve's cleanup path and the writer's own error path may
// trigger it)
func (q *outboundQueue) close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	q.mu.Unlock()

	select {
	case q.wake <- struct{}{}:
	default:
	}
}

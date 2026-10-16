// This file is request ID allocation and in-flight request registration (the pending map).
//
// In the protocol the matching key is (connection, request_id), so both structures in this file **belong to one connection**,
// not to the whole process. The same number on different connections never clashes, even when two processes belong to the same Package and
// share a UID.
package sdk

import (
	"sync"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// requestIDGen is the per-connection atomically increasing request_id allocator.
//
// The protocol says (envelope.proto Request.request_id):
//   - **0 is permanently reserved**; the first request is 1
//   - **never wrap around**: on reaching the limit, close and rebuild the connection
//
// "Never wrap" isn't fussiness. Wrapping would let a late response match a new request: both would have the same
// request_id on the same connection, and the receiver would have no way to tell them apart. Better to rebuild the connection.
type requestIDGen struct {
	mu   sync.Mutex
	next uint64
}

func newRequestIDGen() *requestIDGen { return &requestIDGen{next: 1} }

// nextID returns the next request_id. When exhausted it returns ErrRequestIDExhausted.
//
// A mutex rather than atomic.AddUint64: overflow detection needs "read, check, write" to happen atomically,
// while atomic.Add silently wraps to 0 on overflow, which is exactly the two things the protocol forbids (wrapping + using 0).
func (g *requestIDGen) nextID() (uint64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.next == 0 {
		// The last allocation pushed the counter to its end (see the wrap-around guard below), so refuse permanently from now on
		return 0, ErrRequestIDExhausted
	}
	id := g.next
	if id == ^uint64(0) {
		g.next = 0 // mark exhausted; the next call fails directly
	} else {
		g.next = id + 1
	}
	return id, nil
}

// pendingMap registers in-flight requests: request_id → the channel waiting for the response.
//
// A buffered channel of capacity 1: the reader goroutine must **never block** delivering a response. With an unbuffered
// channel, a caller that already timed out and left would leave the reader stuck on the send forever, starving every other
// request on the connection. Buffer 1 + single delivery guarantees the reader never blocks.
type pendingMap struct {
	mu sync.Mutex
	m  map[uint64]chan *ipcv1.Response
}

func newPendingMap() *pendingMap {
	return &pendingMap{m: make(map[uint64]chan *ipcv1.Response)}
}

// add registers an in-flight request and returns the channel that receives its response.
func (p *pendingMap) add(id uint64) chan *ipcv1.Response {
	ch := make(chan *ipcv1.Response, 1)
	p.mu.Lock()
	p.m[id] = ch
	p.mu.Unlock()
	return ch
}

// remove unregisters an in-flight request. Callers must call it on timeout/cancellation, or the map leaks.
func (p *pendingMap) remove(id uint64) {
	p.mu.Lock()
	delete(p.m, id)
	p.mu.Unlock()
}

// deliver hands the response to its waiter and returns whether it matched.
//
// No match (a late, duplicate or never-sent request_id) returns false, and the caller
// logs it and **drops it**; it must never close the connection because of it: a timed-out request getting a late response is completely
// normal timing, and treating it as a protocol violation would make ordinary slow responses kick connections off often.
//
// On a match it's deleted from the map at once: the protocol guarantees at most one final Response per Request,
// and deleting lets a second response with the same ID fall naturally into "no match → drop".
func (p *pendingMap) deliver(id uint64, resp *ipcv1.Response) bool {
	p.mu.Lock()
	ch, ok := p.m[id]
	if ok {
		delete(p.m, id)
	}
	p.mu.Unlock()
	if !ok {
		return false
	}
	ch <- resp // capacity 1 and each id is delivered once, so it never blocks
	return true
}

// failAll fails every in-flight request with the same reason, for when the connection drops.
//
// Closing the channel rather than delivering an error value: waiters use "received nil" to tell "the connection is gone" from
// "received a real response", cleaner than defining yet another sentinel response.
func (p *pendingMap) failAll() {
	p.mu.Lock()
	m := p.m
	p.m = make(map[uint64]chan *ipcv1.Response)
	p.mu.Unlock()
	for _, ch := range m {
		close(ch)
	}
}

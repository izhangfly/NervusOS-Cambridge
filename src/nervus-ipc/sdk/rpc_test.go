package sdk

import (
	"errors"
	"sync"
	"testing"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

func TestRequestIDGen_StartsAtOneAndNeverUsesZero(t *testing.T) {
	// The protocol says 0 is permanently reserved and the first request is 1. Using 0 would leave the receiver unable to tell
	// "this field wasn't set" from "this is request number 0", since proto3 doesn't send zero values.
	g := newRequestIDGen()
	for i := uint64(1); i <= 100; i++ {
		id, err := g.nextID()
		if err != nil {
			t.Fatalf("nextID: %v", err)
		}
		if id != i {
			t.Fatalf("id = %d, want %d", id, i)
		}
	}
}

func TestRequestIDGen_ExhaustsInsteadOfWrapping(t *testing.T) {
	// Wrapping would let a late response match a new request: both have the same request_id and connection,
	// and the receiver has no way to tell them apart. So the protocol requires rebuilding the connection on exhaustion.
	g := newRequestIDGen()
	g.next = ^uint64(0) // jump straight to the last usable value

	last, err := g.nextID()
	if err != nil {
		t.Fatalf("last id should still be issued: %v", err)
	}
	if last != ^uint64(0) {
		t.Fatalf("last = %d, want max uint64", last)
	}

	// From then on it fails **permanently**, and never wraps to 0 or 1
	for i := 0; i < 3; i++ {
		id, err := g.nextID()
		if !errors.Is(err, ErrRequestIDExhausted) {
			t.Fatalf("call %d: err = %v, want ErrRequestIDExhausted", i, err)
		}
		if id != 0 {
			t.Fatalf("exhausted generator must not issue an id, got %d", id)
		}
	}
}

func TestRequestIDGen_ConcurrentUniqueness(t *testing.T) {
	// Concurrent allocation must not repeat a number: a repeat means two in-flight requests share one pending slot,
	// the later one overwrites the earlier one, and the earlier one never gets its response.
	g := newRequestIDGen()
	const n = 500

	var wg sync.WaitGroup
	ids := make([]uint64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := g.nextID()
			if err != nil {
				t.Errorf("nextID: %v", err)
				return
			}
			ids[i] = id
		}(i)
	}
	wg.Wait()

	seen := make(map[uint64]bool, n)
	for _, id := range ids {
		if id == 0 {
			t.Fatal("id 0 issued")
		}
		if seen[id] {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = true
	}
}

func TestPendingMap_DeliverAndRemove(t *testing.T) {
	p := newPendingMap()
	ch := p.add(7)

	if !p.deliver(7, &ipcv1.Response{RequestId: 7}) {
		t.Fatal("deliver should hit")
	}
	if got := <-ch; got.GetRequestId() != 7 {
		t.Fatalf("delivered id = %d, want 7", got.GetRequestId())
	}

	// The protocol guarantees at most one final Response per Request. A second response with the same ID must fall into
	// "no match → drop", and must never be delivered again (that would give the caller two results).
	if p.deliver(7, &ipcv1.Response{RequestId: 7}) {
		t.Fatal("second deliver for same id must miss")
	}
}

func TestPendingMap_LateResponseIsDroppedNotFatal(t *testing.T) {
	// A caller that timed out and left: a late response arrives after remove. deliver must return false
	// so the caller drops it, rather than blocking on the send or closing the connection as a protocol violation.
	// A timed-out request getting a late response is completely normal timing.
	p := newPendingMap()
	p.add(9)
	p.remove(9)

	done := make(chan bool, 1)
	go func() { done <- p.deliver(9, &ipcv1.Response{RequestId: 9}) }()

	select {
	case hit := <-done:
		if hit {
			t.Fatal("deliver to removed id should miss")
		}
	default:
		// Getting here means deliver hasn't returned yet; the receive below will catch it
	}
	if hit := <-done; hit {
		t.Fatal("deliver to removed id should miss")
	}
}

func TestPendingMap_OutOfOrderDelivery(t *testing.T) {
	// Out-of-order responses: nervud doesn't guarantee replies in request order. Three in-flight requests come back in the order 3,1,2,
	// and each must land on its own channel.
	p := newPendingMap()
	chs := map[uint64]chan *ipcv1.Response{
		1: p.add(1), 2: p.add(2), 3: p.add(3),
	}
	for _, id := range []uint64{3, 1, 2} {
		if !p.deliver(id, &ipcv1.Response{RequestId: id}) {
			t.Fatalf("deliver %d missed", id)
		}
	}
	for id, ch := range chs {
		got := <-ch
		if got.GetRequestId() != id {
			t.Errorf("channel %d got response for %d", id, got.GetRequestId())
		}
	}
}

func TestPendingMap_FailAllClosesChannels(t *testing.T) {
	// Connection dropped: every in-flight request must be woken. Waiters use "received nil / channel closed" to
	// tell "the connection is gone" from "got a real response". Not waking them leaks a goroutine forever.
	p := newPendingMap()
	a, b := p.add(1), p.add(2)
	p.failAll()

	for i, ch := range []chan *ipcv1.Response{a, b} {
		select {
		case resp, ok := <-ch:
			if ok {
				t.Errorf("channel %d: got %v, want closed", i, resp)
			}
		default:
			t.Errorf("channel %d not closed by failAll", i)
		}
	}
}

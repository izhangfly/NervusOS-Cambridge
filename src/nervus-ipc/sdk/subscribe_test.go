package sdk

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// ---- Backpressure policy (unit tests of Subscription.deliver) ------------------------
//
// These use the local method rather than the wire: the three delivery_class values differ only "at the moment the buffer fills",
// and that moment can't be reproduced deterministically on a real connection. The policy needs deterministic coverage; the wire has its own e2e.

func newTestSub(class ipcv1.DeliveryClass, buffer int) *Subscription {
	return &Subscription{id: 1, endpointID: 7, eventID: 3, class: class, ch: make(chan Event, buffer)}
}

func deliverN(t *testing.T, s *Subscription, from, to uint64) error {
	t.Helper()
	for seq := from; seq <= to; seq++ {
		if err := s.deliver(Event{SubscriptionID: s.id, Sequence: seq}); err != nil {
			return err
		}
	}
	return nil
}

// When the buffer fills, RELIABLE **terminates the subscription** instead of dropping one.
//
// Dropping would let the caller think it got the complete sequence, which is exactly what RELIABLE promises can't happen.
func TestSubscription_ReliableOverflowTerminates(t *testing.T) {
	s := newTestSub(ipcv1.DeliveryClass_DELIVERY_CLASS_RELIABLE, 2)

	if err := deliverN(t, s, 1, 2); err != nil {
		t.Fatalf("the first two should fit: %v", err)
	}
	err := s.deliver(Event{Sequence: 3})
	if !errors.Is(err, ErrSubscriptionOverflow) {
		t.Fatalf("third err = %v, want ErrSubscriptionOverflow", err)
	}
}

// LOSSY drops the new ones, and attaches the drop count to **the next delivered** event.
func TestSubscription_LossyFoldsDropsIntoNextDelivered(t *testing.T) {
	s := newTestSub(ipcv1.DeliveryClass_DELIVERY_CLASS_LOSSY, 1)

	// 1 goes into the buffer; 2 and 3 are dropped
	if err := deliverN(t, s, 1, 3); err != nil {
		t.Fatalf("LOSSY should not terminate the subscription: %v", err)
	}
	first := <-s.C()
	if first.Sequence != 1 {
		t.Fatalf("first.Sequence = %d, want 1 (the new ones should be dropped, not the head)", first.Sequence)
	}
	if first.Dropped != 0 {
		t.Errorf("first.Dropped = %d, want 0: the drops happened after it", first.Dropped)
	}

	// Once the buffer empties, the two backlogged drops must ride on the next one
	if err := s.deliver(Event{Sequence: 4}); err != nil {
		t.Fatalf("deliver 4: %v", err)
	}
	next := <-s.C()
	if next.Sequence != 4 {
		t.Fatalf("next.Sequence = %d, want 4", next.Sequence)
	}
	if next.Dropped != 2 {
		t.Errorf("next.Dropped = %d, want 2 (seq 2 and 3)", next.Dropped)
	}
	if got := s.LocalDropped(); got != 2 {
		t.Errorf("LocalDropped = %d, want 2", got)
	}
}

// STATE evicts **the oldest** one, keeps the latest value, and counts the evicted one in this one's Dropped.
//
// Dropping new and keeping old would invert STATE's semantics entirely: the caller would get an ever staler value.
func TestSubscription_StateEvictsOldestAndCounts(t *testing.T) {
	s := newTestSub(ipcv1.DeliveryClass_DELIVERY_CLASS_STATE, 2)

	if err := deliverN(t, s, 1, 4); err != nil {
		t.Fatalf("STATE should not terminate the subscription: %v", err)
	}

	// Buffer of 2, 4 delivered: what's left must be the latest two
	got := []Event{<-s.C(), <-s.C()}
	if got[0].Sequence != 3 || got[1].Sequence != 4 {
		t.Fatalf("kept seq %d,%d, want 3,4 (the latest values)", got[0].Sequence, got[1].Sequence)
	}
	// seq 1 and 2 were each evicted once, each counted in the one that pushed it out
	if got[0].Dropped != 1 || got[1].Dropped != 1 {
		t.Errorf("Dropped = %d,%d, want 1,1", got[0].Dropped, got[1].Dropped)
	}
	if n := s.LocalDropped(); n != 2 {
		t.Errorf("LocalDropped = %d, want 2", n)
	}
}

// **A delivery_class this build doesn't recognise is treated as RELIABLE**.
//
// When the protocol adds a class in future, treating an unknown value as "fine to drop" is the most dangerous default.
func TestSubscription_UnknownClassFailsClosedToReliable(t *testing.T) {
	s := newTestSub(ipcv1.DeliveryClass(99), 1)
	if err := s.deliver(Event{Sequence: 1}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.deliver(Event{Sequence: 2}); !errors.Is(err, ErrSubscriptionOverflow) {
		t.Fatalf("err = %v, want ErrSubscriptionOverflow", err)
	}
}

// ---- Wire round trips -------------------------------------------------------------

func subscribeOK(subID uint64, class ipcv1.DeliveryClass) func(net.Conn, *ipcv1.Envelope) []*ipcv1.Envelope {
	return autoHandshake(func(_ net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		s := env.GetSubscribe()
		if s == nil {
			return nil
		}
		return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_SubscribeResult{
			SubscribeResult: &ipcv1.SubscribeResult{
				RequestId: s.GetRequestId(),
				Outcome: &ipcv1.SubscribeResult_Success{Success: &ipcv1.SubscribeSuccess{
					SubscriptionId: subID, DeliveryClass: class,
				}},
			},
		}}}
	})
}

func eventEnv(subID, seq uint64, payload []byte) *ipcv1.Envelope {
	return &ipcv1.Envelope{Body: &ipcv1.Envelope_Event{Event: &ipcv1.Event{
		SubscriptionId: subID, Sequence: seq, EndpointId: 7, EventId: 3, Payload: payload,
	}}}
}

func mustSubscribe(t *testing.T, c *Client, buffer int) *Subscription {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sub, err := c.Subscribe(ctx, SubscribeRequest{EndpointID: 7, EventID: 3, Buffer: buffer})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return sub
}

// A successful subscription must give the caller the delivery_class.
//
// Without it, a caller seeing a sequence gap can't tell whether to "do nothing" or
// treat it as "data permanently lost": the same symptom, two opposite responses.
func TestSubscribe_ExposesDeliveryClass(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(subscribeOK(11, ipcv1.DeliveryClass_DELIVERY_CLASS_STATE))

	c, err := Connect(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	sub := mustSubscribe(t, c, 0)
	if sub.ID() != 11 {
		t.Errorf("ID = %d, want 11", sub.ID())
	}
	if sub.DeliveryClass() != ipcv1.DeliveryClass_DELIVERY_CLASS_STATE {
		t.Errorf("DeliveryClass = %v, want STATE", sub.DeliveryClass())
	}
}

// subscription_id 0 is reserved; accepting it would make every later Event match no subscription.
func TestSubscribe_ZeroSubscriptionIDIsProtocolViolation(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(subscribeOK(0, ipcv1.DeliveryClass_DELIVERY_CLASS_RELIABLE))

	c, err := Connect(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.Subscribe(ctx, SubscribeRequest{EndpointID: 7, EventID: 3}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}

func TestSubscribe_FailureBecomesStatusError(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(func(_ net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		s := env.GetSubscribe()
		if s == nil {
			return nil
		}
		return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_SubscribeResult{
			SubscribeResult: &ipcv1.SubscribeResult{
				RequestId: s.GetRequestId(),
				Outcome: &ipcv1.SubscribeResult_Failure{Failure: &ipcv1.Failure{
					Code: ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED,
				}},
			},
		}}}
	}))

	c, err := Connect(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = c.Subscribe(ctx, SubscribeRequest{EndpointID: 7, EventID: 3})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED {
		t.Fatalf("err = %v, want PERMISSION_DENIED StatusError", err)
	}
}

// Event(43) must be routed to its subscription by subscription_id, keeping sequence and payload intact.
func TestSubscribe_EventsRouteToSubscription(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(func(_ net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		s := env.GetSubscribe()
		if s == nil {
			return nil
		}
		// The subscription result and the events after it are sent back in the same batch: bytes on the connection are ordered,
		// so the client always registers the subscription before the events arrive.
		return []*ipcv1.Envelope{
			{Body: &ipcv1.Envelope_SubscribeResult{SubscribeResult: &ipcv1.SubscribeResult{
				RequestId: s.GetRequestId(),
				Outcome: &ipcv1.SubscribeResult_Success{Success: &ipcv1.SubscribeSuccess{
					SubscriptionId: 11,
					DeliveryClass:  ipcv1.DeliveryClass_DELIVERY_CLASS_RELIABLE,
				}},
			}}},
			eventEnv(11, 1, []byte("a")),
			eventEnv(11, 2, []byte("b")),
		}
	}))

	c, err := Connect(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	sub := mustSubscribe(t, c, 0)
	for i, want := range []string{"a", "b"} {
		select {
		case ev := <-sub.C():
			if string(ev.Payload) != want {
				t.Fatalf("event %d payload = %q, want %q", i+1, ev.Payload, want)
			}
			if ev.Sequence != uint64(i+1) {
				t.Fatalf("event %d sequence = %d", i+1, ev.Sequence)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("event %d timed out", i+1)
		}
	}
}

// An Event with an unknown subscription_id is only dropped; **the connection stays open**.
//
// There's always a window between unsubscribing and in-flight events; treating it as a protocol violation would kick connections off in normal timing.
func TestSubscribe_UnknownSubscriptionEventDoesNotKillConn(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(func(_ net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if env.GetResolveEndpoint() == nil {
			return nil
		}
		return []*ipcv1.Envelope{
			eventEnv(999, 1, nil),
			{Body: &ipcv1.Envelope_ResolveEndpointResult{
				ResolveEndpointResult: &ipcv1.ResolveEndpointResult{
					RequestId: env.GetResolveEndpoint().GetRequestId(),
					Outcome: &ipcv1.ResolveEndpointResult_Success{
						Success: &ipcv1.ResolveEndpointSuccess{EndpointId: 7, InterfaceMajor: 1},
					},
				},
			}},
		}
	}))

	c, err := Connect(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// A Resolve right after the ghost event must still get a result: the connection wasn't torn down.
	if _, err := c.ResolveEndpoint(ctx, ResolveRequest{
		InterfaceID: "x", MinMajor: 1, MaxMajor: 1,
	}); err != nil {
		t.Fatalf("connection unusable after the ghost event: %v", err)
	}
}

// SubscriptionClosed(45) closes the channel and carries the reason out.
func TestSubscription_ServerCloseCarriesReason(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(func(_ net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		s := env.GetSubscribe()
		if s == nil {
			return nil
		}
		return []*ipcv1.Envelope{
			{Body: &ipcv1.Envelope_SubscribeResult{SubscribeResult: &ipcv1.SubscribeResult{
				RequestId: s.GetRequestId(),
				Outcome: &ipcv1.SubscribeResult_Success{Success: &ipcv1.SubscribeSuccess{
					SubscriptionId: 11,
					DeliveryClass:  ipcv1.DeliveryClass_DELIVERY_CLASS_RELIABLE,
				}},
			}}},
			{Body: &ipcv1.Envelope_SubscriptionClosed{
				SubscriptionClosed: &ipcv1.SubscriptionClosed{
					SubscriptionId: 11,
					Reason:         ipcv1.SubscriptionClosedReason_SUBSCRIPTION_CLOSED_REASON_ENDPOINT_DIED,
				},
			}},
		}
	}))

	c, err := Connect(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	sub := mustSubscribe(t, c, 0)
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("want closed channel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel was not closed")
	}

	var closed *SubscriptionClosedError
	if !errors.As(sub.Err(), &closed) {
		t.Fatalf("Err = %v, want *SubscriptionClosedError", sub.Err())
	}
	if closed.Reason != ipcv1.SubscriptionClosedReason_SUBSCRIPTION_CLOSED_REASON_ENDPOINT_DIED {
		t.Errorf("Reason = %v, want ENDPOINT_DIED", closed.Reason)
	}
	if !errors.Is(sub.Err(), ErrSubscriptionClosed) {
		t.Error("*SubscriptionClosedError must Unwrap to ErrSubscriptionClosed")
	}
}

// When the connection drops, every subscription channel is closed, and Err points at ErrClosed.
//
// Otherwise the caller's range would hang forever, with no other way to notice the connection is gone.
func TestSubscription_ConnectionCloseClosesAll(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(subscribeOK(11, ipcv1.DeliveryClass_DELIVERY_CLASS_RELIABLE))

	c, err := Connect(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	sub := mustSubscribe(t, c, 0)
	_ = c.Close()

	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("want closed channel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscription channel still not closed after the connection closed")
	}
	if !errors.Is(sub.Err(), ErrClosed) {
		t.Fatalf("Err = %v, want ErrClosed", sub.Err())
	}
}

// After a successful unsubscribe the channel closes with Err nil: normal completion and abnormal termination must be distinguishable.
func TestUnsubscribe_ClosesChannelWithoutError(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(func(_ net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if s := env.GetSubscribe(); s != nil {
			return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_SubscribeResult{
				SubscribeResult: &ipcv1.SubscribeResult{
					RequestId: s.GetRequestId(),
					Outcome: &ipcv1.SubscribeResult_Success{Success: &ipcv1.SubscribeSuccess{
						SubscriptionId: 11,
						DeliveryClass:  ipcv1.DeliveryClass_DELIVERY_CLASS_RELIABLE,
					}},
				},
			}}}
		}
		if u := env.GetUnsubscribe(); u != nil {
			return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_UnsubscribeResult{
				UnsubscribeResult: &ipcv1.UnsubscribeResult{
					RequestId: u.GetRequestId(),
					Outcome: &ipcv1.UnsubscribeResult_Success{
						Success: &ipcv1.UnsubscribeSuccess{},
					},
				},
			}}}
		}
		return nil
	}))

	c, err := Connect(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	sub := mustSubscribe(t, c, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Unsubscribe(ctx, sub); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if _, ok := <-sub.C(); ok {
		t.Fatal("want closed channel")
	}
	if sub.Err() != nil {
		t.Errorf("Err = %v, want nil (unsubscribing yourself is normal completion)", sub.Err())
	}
}

// ---- Provider side ---------------------------------------------------------------

// An endpoint that hasn't registered can't publish events: endpoint_id is still 0, and nervud can't check ownership.
func TestPublishEvent_RequiresRegisteredEndpoint(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(nil))

	h, err := NewServiceHost(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("NewServiceHost: %v", err)
	}
	defer func() { _ = h.Close() }()

	e := h.NewEndpoint(RegisterRequest{InterfaceID: "x", Major: 1})
	if err := e.PublishEvent(1, nil); err == nil {
		t.Fatal("an unregistered endpoint published an event")
	}
}

// PublishEvent's wire shape: uses **the Provider's own** endpoint handle, and passes the timestamp through unchanged.
func TestPublishEvent_WireShape(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(registerThen(nil))

	h, err := NewServiceHost(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("NewServiceHost: %v", err)
	}
	defer func() { _ = h.Close() }()

	e := h.NewEndpoint(RegisterRequest{
		InterfaceID: "nervus.interface.motion.base", Major: 1, ResourceHandle: "base.main",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	epID, err := e.Register(ctx)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := e.PublishEventAt(5, []byte("frame"), 12345); err != nil {
		t.Fatalf("PublishEventAt: %v", err)
	}

	waitFor(t, func() bool { return f.recvCount() >= 3 })
	f.mu.Lock()
	last := f.received[len(f.received)-1]
	f.mu.Unlock()

	p := last.GetPublishEvent()
	if p == nil {
		t.Fatalf("last frame is %T, want PublishEvent", last.GetBody())
	}
	if p.GetEndpointId() != epID {
		t.Errorf("endpoint_id = %d, want %d (must be the Provider's own handle)", p.GetEndpointId(), epID)
	}
	if p.GetEventId() != 5 || string(p.GetPayload()) != "frame" {
		t.Errorf("event_id = %d, payload = %q", p.GetEventId(), p.GetPayload())
	}
	if p.GetMonotonicTimestampNanos() != 12345 {
		t.Errorf("timestamp = %d, want 12345 (passed through unchanged)", p.GetMonotonicTimestampNanos())
	}
}

// **Without a timestamp it must send 0, and not invent one for the caller**.
//
// The meaningful moment is when the data was produced; passing off the publish time as that would produce a plausible-looking
// number with queueing delay mixed in, which consumers can't tell apart.
func TestPublishEvent_OmittedTimestampStaysZero(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(registerThen(nil))

	h, err := NewServiceHost(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("NewServiceHost: %v", err)
	}
	defer func() { _ = h.Close() }()

	e := h.NewEndpoint(RegisterRequest{
		InterfaceID: "nervus.interface.motion.base", Major: 1, ResourceHandle: "base.main",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := e.PublishEvent(5, nil); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}

	waitFor(t, func() bool { return f.recvCount() >= 3 })
	f.mu.Lock()
	last := f.received[len(f.received)-1]
	f.mu.Unlock()

	if ts := last.GetPublishEvent().GetMonotonicTimestampNanos(); ts != 0 {
		t.Errorf("timestamp = %d, want 0", ts)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the condition")
}

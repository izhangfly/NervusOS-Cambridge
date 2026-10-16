// This file is the subscription side: the consumer's Subscribe/Unsubscribe/Event, and the provider's PublishEvent.
//
// # How the three connections relate
//
//	subscriber --Subscribe(40)--> nervud        sets up an (endpoint, event_id) subscription
//	Provider --PublishEvent(53)--> nervud       reports one event
//	nervud --Event(43)--> each subscriber       fans out, with each one's own subscription_id and sequence
//
// The Provider **can't see** subscribers, and shouldn't: a subscriber's permission may be revoked after it subscribes,
// and the Provider would have no way of knowing. It only says "this event happened on this endpoint", and delivery decisions stay in
// nervud. That's why the provider side only has PublishEvent and no "send to so-and-so" API.
//
// # Backpressure appears twice in this file
//
// The first time between nervud and this process (the kernel merges/drops/drops subscriptions by delivery_class),
// the second between this file's read loop and the caller's range. The rules in both places are **deliberately the same**:
// the read loop must never block stuffing events into a Subscription's channel, or one slow consumer
// would starve every request response and Ping on this connection with it.
package sdk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// DefaultSubscriptionBuffer is the default local buffer depth of one subscription.
//
// 64 rather than 1: the read loop is shared by the whole connection, and a buffer that's too shallow would turn an ordinary GC pause into
// visible drops. Not larger either: the buffer is there to absorb jitter, not to hoard data for the caller;
// callers that really need to hoard should write to disk or forward to a bigger queue in their range.
const DefaultSubscriptionBuffer = 64

var (
	// ErrSubscriptionOverflow: the local buffer is full and the subscription is RELIABLE.
	//
	// RELIABLE doesn't allow silent drops, so here the **subscription is terminated** rather than one event dropped, the same rule as nervud
	// disconnecting a slow subscriber in the same situation, just one hop later.
	ErrSubscriptionOverflow = errors.New("sdk: reliable subscription overflowed local buffer")

	// ErrSubscriptionClosed: the subscription was terminated by the server; see *SubscriptionClosedError for why.
	ErrSubscriptionClosed = errors.New("sdk: subscription closed by server")
)

// SubscriptionClosedError carries the termination reason the server gave.
type SubscriptionClosedError struct {
	Reason ipcv1.SubscriptionClosedReason
}

func (e *SubscriptionClosedError) Error() string {
	return fmt.Sprintf("%s: %s", ErrSubscriptionClosed.Error(), e.Reason)
}

func (e *SubscriptionClosedError) Unwrap() error { return ErrSubscriptionClosed }

// Event is one received push event.
type Event struct {
	SubscriptionID uint64
	// Sequence increases monotonically within this subscription, starting at 1. A gap = some event wasn't delivered.
	Sequence uint64
	// EndpointID is the endpoint the event came from (the handle from the subscriber's point of view).
	EndpointID uint64
	EventID    uint32
	// Payload is event-specific Protobuf bytes, typed by (endpoint, event_id).
	Payload []byte

	// Dropped is the number lost since the last event **you actually received**.
	//
	// It **counts together** drops on the nervud side and drops in this SDK's buffer. Merging is deliberate:
	// for the caller both are the same thing, "this many events you didn't see", and what to do about it
	// is decided entirely by DeliveryClass, not by which hop dropped them.
	//
	// To tell which hop dropped them (for troubleshooting), look at Subscription.LocalDropped().
	Dropped uint64

	// MonotonicTimestampNanos is the production time the Provider gave, passed through unchanged. 0 means not provided.
	// **It isn't the time received**: events go through nervud's fan-out and two queue levels, and the two may differ a lot.
	MonotonicTimestampNanos uint64
}

// Subscription is an established subscription.
//
// Usage:
//
//	sub, err := c.Subscribe(ctx, sdk.SubscribeRequest{EndpointID: ep.EndpointID, EventID: 2})
//	defer c.Unsubscribe(context.Background(), sub)
//	for ev := range sub.C() {
//	    ...
//	}
//	if err := sub.Err(); err != nil { /* ended abnormally */ }
type Subscription struct {
	id         uint64
	endpointID uint64
	eventID    uint32
	class      ipcv1.DeliveryClass

	ch chan Event

	mu sync.Mutex
	// pendingDropped is the local drop count that hasn't been attached to any delivered event yet.
	// It's cleared on successful delivery and counted into that event's Dropped.
	pendingDropped uint64
	localDropped   uint64
	closed         bool
	err            error
}

// ID returns the subscription handle scoped to this connection.
func (s *Subscription) ID() uint64 { return s.id }

// EndpointID returns the endpoint the events come from.
func (s *Subscription) EndpointID() uint64 { return s.endpointID }

// EventID returns the subscribed event ID.
func (s *Subscription) EventID() uint32 { return s.eventID }

// DeliveryClass returns the delivery class nervud declared.
//
// **Dropped must be interpreted by it**: under STATE, Dropped>0 only means intermediate states were merged and this one
// is the latest value, so nothing needs fetching; under LOSSY it means permanent loss. The same number, two opposite treatments.
func (s *Subscription) DeliveryClass() ipcv1.DeliveryClass { return s.class }

// C is the event channel. It's closed when the subscription ends, so it can be ranged over directly.
func (s *Subscription) C() <-chan Event { return s.ch }

// Err gives why the subscription ended. A normal unsubscribe returns nil.
//
// Only meaningful after C() is closed; always nil before that.
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// LocalDropped is the running count of events dropped by this SDK's buffer (excluding drops on the nervud side).
//
// It's separate from Event.Dropped only for troubleshooting: a non-zero value means **this process consumes too slowly**, and you should increase
// SubscribeRequest.Buffer or make the handler faster, rather than investigating the kernel.
func (s *Subscription) LocalDropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.localDropped
}

// deliver is called by the read loop; a non-nil return means the subscription must end.
//
// **It never blocks**: the read loop is shared by the whole connection. Blocking here would starve the request responses,
// Pings and even other subscriptions on the same connection; one slow consumer bringing down the whole connection is exactly what the delivery_class
// mechanism exists to prevent.
func (s *Subscription) deliver(ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}

	// carried is the local part of "how many were lost since you last actually received one". It only counts as reported once it
	// rides on an event that really gets delivered, so it's only cleared after trySend succeeds.
	carried := s.pendingDropped
	if s.trySend(ev, carried) {
		s.pendingDropped = 0
		return nil
	}

	switch s.class {
	case ipcv1.DeliveryClass_DELIVERY_CLASS_STATE:
		// Merge: drop the **oldest** one in the queue and squeeze the newest in. STATE means
		// "only the latest value matters", and dropping the new while keeping the old would turn the meaning upside down.
		//
		// The one squeezed out must count towards this one's Dropped, otherwise merging would become a silent drop;
		// the caller would see a sequence gap without the count, which is exactly why Dropped exists.
		if s.evictOldest() {
			carried++
			if s.trySend(ev, carried) {
				s.pendingDropped = 0
				return nil
			}
		}
		s.localDropped++
		s.pendingDropped = carried + 1
		return nil

	case ipcv1.DeliveryClass_DELIVERY_CLASS_LOSSY:
		s.localDropped++
		s.pendingDropped = carried + 1
		return nil

	default:
		// RELIABLE, and any class this build doesn't know.
		//
		// **Unknown classes are treated as RELIABLE**: when the protocol adds a value, treating it as "can be dropped freely"
		// is the most dangerous default, because the caller would think it has the complete sequence. Better to end the subscription.
		return ErrSubscriptionOverflow
	}
}

// trySend delivers without blocking; dropped is the local drop count to attach to this one.
func (s *Subscription) trySend(ev Event, dropped uint64) bool {
	ev.Dropped += dropped
	select {
	case s.ch <- ev:
		return true
	default:
		return false
	}
}

// evictOldest squeezes out the oldest item at the head of the queue and returns whether one really was removed.
func (s *Subscription) evictOldest() bool {
	select {
	case <-s.ch:
		s.localDropped++
		return true
	default:
		return false
	}
}

// closeWith closes the subscription and records the reason. Idempotent.
func (s *Subscription) closeWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.err = err
	close(s.ch)
}

// SubscribeRequest is one subscription request.
type SubscribeRequest struct {
	// EndpointID must be an endpoint handle already Resolved on this connection.
	EndpointID uint64
	// EventID is the stable numeric event ID in the interface schema.
	EventID uint32
	// Payload is the serialised bytes of subscription parameters (filters, sample rate and so on); may be empty.
	// Typed by (endpoint, event_id).
	Payload []byte

	// Scope is the instance scope: when an endpoint has several independently observable instances,
	// it says which one to watch (a stream_id, an operation_id and the like).
	//
	// When the event declares EventMeta.scoped it's **required and non-zero**, and nervud checks the instance
	// is yours; when not declared it must be left 0, and filling it in is refused.
	//
	// **0 doesn't mean "all"**: that's the broadcast this mechanism exists to eliminate; not separating instances would mean you
	// receive other people's progress, detailed failure causes and resource handles.
	Scope uint64
	// Buffer is the local event buffer depth. <=0 means DefaultSubscriptionBuffer.
	Buffer int
}

// eventTransport is the subscription implementation's narrow dependency on its host connection. Both Client and ServiceHost satisfy it.
//
// It's defined on the consumer side (this file) rather than having the two hosts each write their own subscription logic: the subscription state machine
// has nothing to do with the two roles, and duplicate implementations would only slowly grow apart.
type eventTransport interface {
	writeEnvelope(*ipcv1.Envelope) error
	nextRequestID() (uint64, error)
	closedErr() error
}

// pendingSubscribe is an in-flight Subscribe: the subscription object is already built, just waiting for nervud to give
// the subscription_id and delivery_class.
//
// **The subscription must be registered in the read loop, not when the caller's goroutine wakes up.** nervud may well
// push the first event right after sending SubscribeResult; both are on the same byte stream with no gap
// in between. If registration happened after the caller got the result, those events would fall into "unknown
// subscription_id" and be dropped without a trace: the caller would only see the sequence not starting at 1
// and think it subscribed late.
type pendingSubscribe struct {
	sub *Subscription
	ch  chan *ipcv1.Envelope
}

// subscriber is the subscription state machine; Client and ServiceHost each hold one.
type subscriber struct {
	tr  eventTransport
	log *slog.Logger

	mu sync.Mutex
	// subs is indexed by subscription_id. Keys are allocated by nervud and **never reused within a connection**,
	// so no extra generation field is needed to tell old and new subscriptions apart.
	subs map[uint64]*Subscription
	// pendingSubs registers in-flight Subscribes, indexed by request_id.
	pendingSubs map[uint64]*pendingSubscribe
	// ctl registers in-flight Unsubscribes, indexed by request_id.
	ctl    map[uint64]chan *ipcv1.Envelope
	closed bool
}

func newSubscriber(tr eventTransport, log *slog.Logger) *subscriber {
	return &subscriber{
		tr:          tr,
		log:         log,
		subs:        make(map[uint64]*Subscription),
		pendingSubs: make(map[uint64]*pendingSubscribe),
		ctl:         make(map[uint64]chan *ipcv1.Envelope),
	}
}

// subscribe starts one subscription and waits for the result.
func (b *subscriber) subscribe(ctx context.Context, req SubscribeRequest) (*Subscription, error) {
	if req.EndpointID == 0 {
		return nil, errors.New("sdk: SubscribeRequest.EndpointID is required")
	}
	buffer := req.Buffer
	if buffer <= 0 {
		buffer = DefaultSubscriptionBuffer
	}

	id, err := b.tr.nextRequestID()
	if err != nil {
		return nil, err
	}
	env := &ipcv1.Envelope{Body: &ipcv1.Envelope_Subscribe{Subscribe: &ipcv1.Subscribe{
		RequestId:  id,
		EndpointId: req.EndpointID,
		EventId:    req.EventID,
		Payload:    req.Payload,
		Scope:      req.Scope,
	}}}

	// The subscription object is built **before sending the request**, so when the read loop gets the SubscribeResult
	// it can register it on the spot and the events right after aren't missed. See the notes on pendingSubscribe.
	pending := &pendingSubscribe{
		sub: &Subscription{
			endpointID: req.EndpointID,
			eventID:    req.EventID,
			ch:         make(chan Event, buffer),
		},
		ch: make(chan *ipcv1.Envelope, 1),
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, b.tr.closedErr()
	}
	b.pendingSubs[id] = pending
	b.mu.Unlock()

	if err := b.tr.writeEnvelope(env); err != nil {
		b.mu.Lock()
		delete(b.pendingSubs, id)
		b.mu.Unlock()
		return nil, err
	}

	var res *ipcv1.Envelope
	select {
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pendingSubs, id)
		b.mu.Unlock()
		return nil, ctx.Err()
	case env, ok := <-pending.ch:
		if !ok {
			return nil, b.tr.closedErr()
		}
		res = env
	}

	result := res.GetSubscribeResult()
	if f := result.GetFailure(); f != nil {
		return nil, statusErrorFrom(f)
	}
	success := result.GetSuccess()
	if success == nil {
		return nil, fmt.Errorf("%w: SubscribeResult has neither outcome", ErrProtocol)
	}
	if success.GetSubscriptionId() == 0 {
		// 0 is reserved. Accepting it would make every later Event match no subscription.
		return nil, fmt.Errorf("%w: SubscribeSuccess carries subscription_id 0", ErrProtocol)
	}
	return pending.sub, nil
}

// finishSubscribe completes a subscription's registration **in the read loop**, then hands the result to the waiter.
//
// Registration and delivery happen under the same lock, so when the read loop handles the next frame (which may be the first event),
// the subscription is certainly already in the table.
func (b *subscriber) finishSubscribe(requestID uint64, env *ipcv1.Envelope) bool {
	result := env.GetSubscribeResult()

	b.mu.Lock()
	pending, ok := b.pendingSubs[requestID]
	if !ok {
		b.mu.Unlock()
		return false
	}
	delete(b.pendingSubs, requestID)

	if success := result.GetSuccess(); success != nil && success.GetSubscriptionId() != 0 {
		pending.sub.id = success.GetSubscriptionId()
		pending.sub.class = success.GetDeliveryClass()
		b.subs[pending.sub.id] = pending.sub
	}
	b.mu.Unlock()

	pending.ch <- env
	return true
}

// unsubscribe takes down a subscription.
//
// **It's removed from the table only after UnsubscribeResult arrives**: bytes on the connection are ordered, so Events arriving before the result
// still belong to this subscription, and none come after it. Removing it early would turn those in-flight events
// into "unknown subscription_id", dropped with a noisy log line.
func (b *subscriber) unsubscribe(ctx context.Context, sub *Subscription) error {
	if sub == nil {
		return errors.New("sdk: nil subscription")
	}
	id, err := b.tr.nextRequestID()
	if err != nil {
		return err
	}
	env := &ipcv1.Envelope{Body: &ipcv1.Envelope_Unsubscribe{Unsubscribe: &ipcv1.Unsubscribe{
		RequestId:      id,
		SubscriptionId: sub.id,
	}}}

	res, err := b.roundTrip(ctx, id, env)
	if err != nil {
		return err
	}
	result := res.GetUnsubscribeResult()
	if result == nil {
		return fmt.Errorf("%w: expected UnsubscribeResult, got %T", ErrProtocol, res.GetBody())
	}

	b.mu.Lock()
	delete(b.subs, sub.id)
	b.mu.Unlock()
	// A normal unsubscribe: err stays nil, and the caller's range ends naturally.
	sub.closeWith(nil)

	if f := result.GetFailure(); f != nil {
		return statusErrorFrom(f)
	}
	return nil
}

// roundTrip sends a control request and waits for the result with the same request_id.
func (b *subscriber) roundTrip(
	ctx context.Context, id uint64, env *ipcv1.Envelope,
) (*ipcv1.Envelope, error) {
	ch := make(chan *ipcv1.Envelope, 1)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, b.tr.closedErr()
	}
	b.ctl[id] = ch
	b.mu.Unlock()

	if err := b.tr.writeEnvelope(env); err != nil {
		b.mu.Lock()
		delete(b.ctl, id)
		b.mu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.ctl, id)
		b.mu.Unlock()
		return nil, ctx.Err()
	case res, ok := <-ch:
		if !ok {
			return nil, b.tr.closedErr()
		}
		return res, nil
	}
}

// deliverControl hands Subscribe/UnsubscribeResults to their waiters. Returns whether it matched.
//
// No match **doesn't close the connection**: a subscribe request that already timed out and left getting a late result is normal timing,
// the same rule as for Responses.
func (b *subscriber) deliverControl(id uint64, env *ipcv1.Envelope) bool {
	b.mu.Lock()
	ch, ok := b.ctl[id]
	if ok {
		delete(b.ctl, id)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	ch <- env
	return true
}

// deliverEvent delivers one Event to its subscription.
//
// An unknown subscription_id is dropped and logged without closing the connection: there's always a window between unsubscribing and in-flight events.
func (b *subscriber) deliverEvent(e *ipcv1.Event) {
	b.mu.Lock()
	sub, ok := b.subs[e.GetSubscriptionId()]
	b.mu.Unlock()
	if !ok {
		b.log.Debug("sdk: dropping event for unknown subscription",
			"subscription_id", e.GetSubscriptionId(), "event_id", e.GetEventId())
		return
	}

	err := sub.deliver(Event{
		SubscriptionID:          e.GetSubscriptionId(),
		Sequence:                e.GetSequence(),
		EndpointID:              e.GetEndpointId(),
		EventID:                 e.GetEventId(),
		Payload:                 e.GetPayload(),
		Dropped:                 e.GetDropped(),
		MonotonicTimestampNanos: e.GetMonotonicTimestampNanos(),
	})
	if err == nil {
		return
	}

	// RELIABLE overflow. Only this subscription ends, **without closing the connection**: the same connection may have
	// other subscriptions that keep up, and tearing everything down for one slow consumer would be an overreaction.
	b.mu.Lock()
	delete(b.subs, sub.id)
	b.mu.Unlock()
	sub.closeWith(err)
	b.log.Warn("sdk: reliable subscription terminated by local backpressure",
		"subscription_id", sub.id, "event_id", sub.eventID)
}

// deliverClosed handles the server's SubscriptionClosed(45).
func (b *subscriber) deliverClosed(c *ipcv1.SubscriptionClosed) {
	b.mu.Lock()
	sub, ok := b.subs[c.GetSubscriptionId()]
	if ok {
		delete(b.subs, c.GetSubscriptionId())
	}
	b.mu.Unlock()
	if !ok {
		b.log.Debug("sdk: SubscriptionClosed for unknown subscription",
			"subscription_id", c.GetSubscriptionId())
		return
	}
	sub.closeWith(&SubscriptionClosedError{Reason: c.GetReason()})
}

// closeAll ends every subscription and in-flight control request when the connection drops.
func (b *subscriber) closeAll(err error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	subs := b.subs
	ctl := b.ctl
	pending := b.pendingSubs
	b.subs = make(map[uint64]*Subscription)
	b.ctl = make(map[uint64]chan *ipcv1.Envelope)
	b.pendingSubs = make(map[uint64]*pendingSubscribe)
	b.mu.Unlock()

	for _, sub := range subs {
		sub.closeWith(err)
	}
	// Close rather than deliver an error value: waiters use "received nil" to tell "the connection is gone" from
	// "received a real result", the same rule as pendingMap.failAll.
	for _, ch := range ctl {
		close(ch)
	}
	for _, p := range pending {
		close(p.ch)
	}
}

// handleSubscriptionBody dispatches the four subscription-related bodies and returns whether it handled one.
//
// Client's and ServiceHost's read loops share it: subscriptions mean exactly the same in both roles,
// and writing one each would only let one side slowly miss a branch.
func (b *subscriber) handleSubscriptionBody(env *ipcv1.Envelope) bool {
	switch body := env.GetBody().(type) {
	case *ipcv1.Envelope_SubscribeResult:
		if !b.finishSubscribe(body.SubscribeResult.GetRequestId(), env) {
			b.log.Debug("sdk: dropping unmatched SubscribeResult",
				"request_id", body.SubscribeResult.GetRequestId())
		}
		return true

	case *ipcv1.Envelope_UnsubscribeResult:
		if !b.deliverControl(body.UnsubscribeResult.GetRequestId(), env) {
			b.log.Debug("sdk: dropping unmatched UnsubscribeResult",
				"request_id", body.UnsubscribeResult.GetRequestId())
		}
		return true

	case *ipcv1.Envelope_Event:
		b.deliverEvent(body.Event)
		return true

	case *ipcv1.Envelope_SubscriptionClosed:
		b.deliverClosed(body.SubscriptionClosed)
		return true

	default:
		return false
	}
}

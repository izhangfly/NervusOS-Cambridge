// Package subscription implements event subscription and fan-out.
//
// # How the three connections relate
//
//	subscriber -> nervud: Subscribe(40) sets up an (endpoint, event_id) subscription
//	Provider -> nervud: PublishEvent(53) reports an event
//	nervud -> subscriber: Event(43) fans out, each carrying its subscription_id and sequence
//
// Delivery decisions stay in nervud, not the Provider. The Provider doesn't know who the subscribers are and shouldn't:
// a subscriber's permission may be revoked after it subscribed, and the Provider would never know. It only says "this event
// happened on this endpoint", and who receives it is the kernel's business.
//
// # Backpressure sits between nervud and the subscriber
//
// PublishEvent is one-way and has no result. Giving it a result would turn the Provider's event loop into
// request-response, and one slow subscriber could hold up the whole Provider. The right place is each subscriber's own
// outbound queue: the slow one gets coalesced, dropped or disconnected according to its delivery_class, without affecting the others.
package subscription

import (
	"sync"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// Sink is a subscriber connection's outbound port. ipc.conn satisfies it implicitly.
//
// Deliver returning false means the queue is full (slow consumer). The Registry then decides by delivery_class
// whether to coalesce, drop or close the subscription.
type Sink interface {
	Deliver(env *ipcv1.Envelope) bool
}

// Key uniquely identifies an event source: one event of one endpoint on one Provider connection,
// plus an optional instance scope.
//
// It uses the Provider-side (connection, endpoint_id) rather than the interface name: one interface can have several
// Providers, and a subscriber subscribes to the one it resolved, not "everyone implementing this interface".
type Key struct {
	ProviderConn any
	EndpointID   uint64
	EventID      uint32

	// Scope separates several independently observable instances on one endpoint. 0 = no separation.
	//
	// # The problem it solves
	//
	// One built-in endpoint runs every operation on the machine, and one camera has several streams open.
	// Without Scope a subscriber would get every instance's events, which isn't just wasted bandwidth,
	// it's an information leak: other people's progress, failure details and resource handles would all be delivered.
	//
	// # Why uint64 rather than any
	//
	// Real instance identifiers are all numeric handles (operation_id, stream_id), and any as a
	// map key would panic at run time on an incomparable dynamic type; that panic would happen on the fan-out
	// hot path and take all of nervud with it.
	//
	// # Exact match, no wildcards
	//
	// A different Scope means you don't receive it. "Subscribe to everything" would mean going back to broadcast, which is exactly what this field
	// exists to fix. Ops tools that need the whole-machine view use the admin channel, not the App control plane.
	Scope uint64
}

// subscription is one established subscription.
type subscription struct {
	id       uint64
	sink     Sink
	conn     any // the subscriber's connection, for bulk clean-up per connection
	key      Key
	meta     *ipcv1.EventMeta
	sequence uint64
	// dropped is the number coalesced or dropped since the last delivered event.
	// Reset after a successful delivery: it describes "what happened between this one and the last".
	dropped uint64
	// callerEndpointID is the endpoint handle the subscriber itself sees. Event.endpoint_id uses it,
	// not the Provider-side one; the two are numbers in different namespaces, and mixing them would route the subscriber
	// to an endpoint it has never seen.
	callerEndpointID uint64
}

// Registry holds every subscription.
//
// Two indexes point at the same subscriptions: byKey for fan-out (the hot path), byConn for bulk clean-up when a connection
// drops. Every add and remove must change both, or you get ghost subscriptions that never receive events,
// or deliveries to closed connections.
type Registry struct {
	mu     sync.RWMutex
	byKey  map[Key][]*subscription
	byConn map[any]map[uint64]*subscription
	// nextID is allocated per subscriber connection and never reused (see the comment on Subscribe).
	nextID map[any]uint64
}

func New() *Registry {
	return &Registry{
		byKey:  make(map[Key][]*subscription),
		byConn: make(map[any]map[uint64]*subscription),
		nextID: make(map[any]uint64),
	}
}

// Subscribe registers a subscription and returns a connection-scoped subscription_id.
//
// Handles are never reused: reuse would let in-flight Events arriving after an unsubscribe be mistaken for the new subscription's data,
// and in telemetry/state streams that mistake is almost impossible to notice: the numbers match, the connection matches,
// and the receiver has no way to tell. Same reasoning as endpoint_ids and lease handles.
func (r *Registry) Subscribe(
	callerConn any, sink Sink, callerEndpointID uint64, key Key, meta *ipcv1.EventMeta,
) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextID[callerConn]++
	id := r.nextID[callerConn]

	s := &subscription{
		id: id, sink: sink, conn: callerConn, key: key, meta: meta,
		callerEndpointID: callerEndpointID,
	}
	r.byKey[key] = append(r.byKey[key], s)
	if r.byConn[callerConn] == nil {
		r.byConn[callerConn] = make(map[uint64]*subscription)
	}
	r.byConn[callerConn][id] = s
	return id
}

// Unsubscribe removes a subscription. Returns whether it really existed.
func (r *Registry) Unsubscribe(callerConn any, id uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	subs := r.byConn[callerConn]
	s, ok := subs[id]
	if !ok {
		return false
	}
	delete(subs, id)
	if len(subs) == 0 {
		delete(r.byConn, callerConn)
	}
	r.removeFromKeyLocked(s)
	return true
}

func (r *Registry) removeFromKeyLocked(s *subscription) {
	list := r.byKey[s.key]
	for i, candidate := range list {
		if candidate != s {
			continue
		}
		list = append(list[:i], list[i+1:]...)
		break
	}
	if len(list) == 0 {
		delete(r.byKey, s.key)
		return
	}
	r.byKey[s.key] = list
}

// DeliveryClassOf gives an event's effective delivery class.
//
// Unspecified fails closed to RELIABLE, the strictest tier (no drops allowed).
// Treating an omission as "free to drop" would be the dangerous default: the client would think it had the full sequence.
func DeliveryClassOf(meta *ipcv1.EventMeta) ipcv1.DeliveryClass {
	switch c := meta.GetDeliveryClass(); c {
	case ipcv1.DeliveryClass_DELIVERY_CLASS_RELIABLE,
		ipcv1.DeliveryClass_DELIVERY_CLASS_STATE,
		ipcv1.DeliveryClass_DELIVERY_CLASS_LOSSY:
		return c
	default:
		return ipcv1.DeliveryClass_DELIVERY_CLASS_RELIABLE
	}
}

// Closed is a subscription ended by backpressure; the caller sends SubscriptionClosed and cleans up accordingly.
type Closed struct {
	Conn           any
	SubscriptionID uint64
	Reason         ipcv1.SubscriptionClosedReason
}

// Publish fans one event out to every subscriber on key.
//
// Returns the subscriptions that must be closed (RELIABLE ones that couldn't be delivered). The caller sends
// SubscriptionClosed and calls Unsubscribe; this function never touches the subscriber connection's lifecycle directly.
//
// Each subscriber counts its own sequence: sequence is "the Nth in this subscription", not a global number.
// With a global number, a subscriber joining midway would see a sequence starting at some huge number,
// with no way to tell whether the earlier ones were "not subscribed yet" or "lost".
func (r *Registry) Publish(key Key, payload []byte, timestampNanos uint64) []Closed {
	r.mu.Lock()
	defer r.mu.Unlock()

	var closed []Closed
	for _, s := range r.byKey[key] {
		class := DeliveryClassOf(s.meta)
		s.sequence++
		env := &ipcv1.Envelope{Body: &ipcv1.Envelope_Event{Event: &ipcv1.Event{
			SubscriptionId:          s.id,
			Sequence:                s.sequence,
			EndpointId:              s.callerEndpointID,
			EventId:                 key.EventID,
			Payload:                 payload,
			Dropped:                 s.dropped,
			MonotonicTimestampNanos: timestampNanos,
		}}}

		if s.sink.Deliver(env) {
			s.dropped = 0
			continue
		}

		// Couldn't deliver. The right reaction differs completely between the three classes
		switch class {
		case ipcv1.DeliveryClass_DELIVERY_CLASS_RELIABLE:
			// Must not drop silently. Only this one subscription may be ended, not the whole connection:
			// the same subscriber may have other subscriptions it can keep up with
			closed = append(closed, Closed{
				Conn: s.conn, SubscriptionID: s.id,
				Reason: ipcv1.SubscriptionClosedReason_SUBSCRIPTION_CLOSED_REASON_BACKPRESSURE,
			})
		case ipcv1.DeliveryClass_DELIVERY_CLASS_STATE,
			ipcv1.DeliveryClass_DELIVERY_CLASS_LOSSY:
			// This one didn't go out; record it in dropped so the next one carries it.
			// sequence was already incremented, so the gap the subscriber sees matches dropped
			s.dropped++
		}
	}
	return closed
}

// CloseConn clears every subscription under a connection and returns the cleared ids.
//
// Called when a subscriber disconnects. No SubscriptionClosed needed: the other end is gone.
func (r *Registry) CloseConn(callerConn any) []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	subs := r.byConn[callerConn]
	if len(subs) == 0 {
		delete(r.nextID, callerConn)
		return nil
	}
	ids := make([]uint64, 0, len(subs))
	for id, s := range subs {
		ids = append(ids, id)
		r.removeFromKeyLocked(s)
	}
	delete(r.byConn, callerConn)
	// nextID is deleted too: with the connection gone, its handle space is gone. Keeping it would only waste memory,
	// and the next connection is a new map key starting from 1 again; the same number on different connections was never
	// the same thing (the lookup key is (connection, handle)).
	delete(r.nextID, callerConn)
	return ids
}

// CloseScope ends every subscription pointing at one instance.
//
// Called when an instance disappears: once an operation reaches its final state and is reclaimed, no more events will come from it.
// Other instances on the same endpoint aren't closed: they're still alive.
//
// Relationship to CloseProviderEndpoint: containment. That one closes a whole endpoint (the endpoint itself
// became invalid), this one only one instance. Using the wrong one goes wrong in opposite directions: the former would kill others' subscriptions,
// the latter would miss ones that should close.
func (r *Registry) CloseScope(
	providerConn any, endpointID, scope uint64, reason ipcv1.SubscriptionClosedReason,
) []Closed {
	r.mu.Lock()
	defer r.mu.Unlock()

	var closed []Closed
	for key, list := range r.byKey {
		if key.ProviderConn != providerConn || key.EndpointID != endpointID ||
			key.Scope != scope {
			continue
		}
		for _, s := range list {
			closed = append(closed, Closed{
				Conn: s.conn, SubscriptionID: s.id, Reason: reason,
			})
			if subs := r.byConn[s.conn]; subs != nil {
				delete(subs, s.id)
				if len(subs) == 0 {
					delete(r.byConn, s.conn)
				}
			}
		}
		delete(r.byKey, key)
	}
	return closed
}

// CloseProviderEndpoint ends every subscription pointing at one Provider endpoint.
//
// Called when an endpoint becomes invalid or loses authority: no events will come after that, and leaving subscribers waiting is worse than
// telling them clearly. It spans every instance scope: with the endpoint gone, the instances on it
// are gone too.
func (r *Registry) CloseProviderEndpoint(
	providerConn any, endpointID uint64, reason ipcv1.SubscriptionClosedReason,
) []Closed {
	r.mu.Lock()
	defer r.mu.Unlock()

	var closed []Closed
	for key, list := range r.byKey {
		if key.ProviderConn != providerConn || key.EndpointID != endpointID {
			continue
		}
		for _, s := range list {
			closed = append(closed, Closed{
				Conn: s.conn, SubscriptionID: s.id, Reason: reason,
			})
			if subs := r.byConn[s.conn]; subs != nil {
				delete(subs, s.id)
				if len(subs) == 0 {
					delete(r.byConn, s.conn)
				}
			}
		}
		delete(r.byKey, key)
	}
	return closed
}

// Len returns the total number of subscriptions, for diagnostics and tests.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, subs := range r.byConn {
		n += len(subs)
	}
	return n
}

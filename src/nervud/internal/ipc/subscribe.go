// This file connects the Envelope's three subscription bodies to internal/subscription:
//
//	Subscribe(40)       subscriber -> nervud    sets up an (endpoint, event_id) subscription
//	Unsubscribe(42)     subscriber -> nervud    takes it down
//	PublishEvent(53)    Provider -> nervud      reports one event, which nervud fans out
//
// Admission goes entirely through the endpoint module, the same source as method calls: the binding is still alive, the generation hasn't drifted,
// and interface-level and event-level permissions both pass. This file makes no security decision itself.
package ipc

import (
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/endpoint"
	"github.com/nervus-os/nervud/internal/subscription"
)

// defaultEventPayloadBytes is the conservative limit when EventMeta.max_payload_bytes isn't specified.
//
// 0 means use the default, not unlimited. Events are pushed, and subscribers have no way to refuse them other than backpressure;
// allowing unlimited size would let one Provider blow up every subscriber's outbound queue.
const defaultEventPayloadBytes = 64 << 10

func subscribeFailure(reqID uint64, code ipcv1.StatusCode) *ipcv1.Envelope {
	return &ipcv1.Envelope{Body: &ipcv1.Envelope_SubscribeResult{
		SubscribeResult: &ipcv1.SubscribeResult{
			RequestId: reqID,
			Outcome: &ipcv1.SubscribeResult_Failure{
				Failure: &ipcv1.Failure{Code: code},
			},
		},
	}}
}

func unsubscribeFailure(reqID uint64, code ipcv1.StatusCode) *ipcv1.Envelope {
	return &ipcv1.Envelope{Body: &ipcv1.Envelope_UnsubscribeResult{
		UnsubscribeResult: &ipcv1.UnsubscribeResult{
			RequestId: reqID,
			Outcome: &ipcv1.UnsubscribeResult_Failure{
				Failure: &ipcv1.Failure{Code: code},
			},
		},
	}}
}

func (co *conn) handleSubscribe(req *ipcv1.Subscribe) bool {
	reqID := req.GetRequestId()
	if reqID == 0 {
		// request_id 0 is reserved, the same rule as Request/AcquireControl
		co.s.auditViolation(co.caller, errZeroRequestID)
		return false
	}
	if co.s.endpoints == nil || co.s.subscriptions == nil {
		return co.enqueue(subscribeFailure(reqID, ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE))
	}

	route, routeErr := co.s.endpoints.RouteEvent(co, req.GetEndpointId(), req.GetEventId())
	if routeErr.Code != ipcv1.StatusCode_STATUS_CODE_UNSPECIFIED {
		return co.enqueue(subscribeFailure(reqID, routeErr.Code))
	}

	scope, code := co.admitSubscription(route, req)
	if code != ipcv1.StatusCode_STATUS_CODE_OK {
		return co.enqueue(subscribeFailure(reqID, code))
	}

	id := co.s.subscriptions.Subscribe(
		co, co, req.GetEndpointId(),
		subscription.Key{
			ProviderConn: route.ProviderConn,
			EndpointID:   route.ProviderEndpointID,
			EventID:      req.GetEventId(),
			Scope:        scope,
		},
		route.Event.Meta,
	)

	// delivery_class goes back to the subscriber with SubscribeSuccess: it decides whether a client seeing a sequence
	// gap should "do nothing" or treat it as "data permanently lost". Without being told, the client can't judge
	return co.enqueue(&ipcv1.Envelope{Body: &ipcv1.Envelope_SubscribeResult{
		SubscribeResult: &ipcv1.SubscribeResult{
			RequestId: reqID,
			Outcome: &ipcv1.SubscribeResult_Success{
				Success: &ipcv1.SubscribeSuccess{
					SubscriptionId: id,
					DeliveryClass:  subscription.DeliveryClassOf(route.Event.Meta),
				},
			},
		},
	}})
}

// admitSubscription decides one subscription's instance scope.
//
// The contract decides: EventMeta.scoped = this event is per instance, and Subscribe.scope is required and
// must belong to this caller; undeclared = endpoint scope, so whoever can Resolve it can subscribe.
//
// When the two sides disagree everything fails closed; a guess in either direction has real consequences: guessing "per instance"
// would stop legitimate subscriptions getting events, guessing "per endpoint" would send out other people's events.
func (co *conn) admitSubscription(
	route endpoint.EventRoute, req *ipcv1.Subscribe,
) (uint64, ipcv1.StatusCode) {
	scope := req.GetScope()

	if !route.Event.Meta.GetScoped() {
		if scope != 0 {
			// The contract says this event isn't per instance, but the caller named one. Silently ignoring it would make it think
			// it's observing one instance while actually receiving everything.
			return 0, ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT
		}
		return 0, ipcv1.StatusCode_STATUS_CODE_OK
	}

	if scope == 0 {
		// Per-instance events must say which one to watch. 0 doesn't mean "all":
		// that's exactly the broadcast this mechanism is meant to eliminate.
		return 0, ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT
	}

	// A built-in endpoint: ownership belongs to the kernel itself, so ask it directly.
	if route.Admit != nil {
		result := route.Admit(endpoint.BuiltinSubscribeCall{
			Caller:  co.caller,
			EventID: req.GetEventId(),
			Scope:   scope,
		})
		if result.Code != ipcv1.StatusCode_STATUS_CODE_OK {
			return 0, result.Code
		}
		return scope, ipcv1.StatusCode_STATUS_CODE_OK
	}

	// An external Provider: check the ownership table. The Provider registers it in advance through BindEventScope,
	// and anything not registered is refused: letting through an unregistered scope would mean going back to broadcast.
	provider, ok := route.ProviderConn.(*conn)
	if !ok || provider == nil {
		return 0, ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE
	}
	if !co.s.eventScopes.allows(provider, route.ProviderEndpointID, scope, co) {
		// NOT_FOUND rather than PERMISSION_DENIED: the latter would tell the caller "this instance
		// exists, it's just not yours", which is information in itself. Consistent with operation's indistinguishable projection.
		return 0, ipcv1.StatusCode_STATUS_CODE_NOT_FOUND
	}
	return scope, ipcv1.StatusCode_STATUS_CODE_OK
}

// handleBindEventScope handles one instance ownership registration from a Provider.
//
// One-way, no result. For the same reason as PublishEvent: a result would add a round trip to the Provider's handling
// flow, which it doesn't need. This body and the DispatchResult that follows travel on the same ordered
// connection, so the registration has certainly taken effect by the time nervud builds the caller's Response.
//
// So failures here are only audited and dropped, with no reply and no connection close.
func (co *conn) handleBindEventScope(req *ipcv1.BindEventScope) bool {
	if co.s.endpoints == nil || co.s.eventScopes == nil {
		return true
	}
	// Only components that export interfaces may register: a pure consumer sending this body is going the wrong direction.
	if !co.componentType.CanProvideInterfaces() {
		co.log.Warn("ipc: component type may not send BindEventScope, closing")
		co.s.auditViolation(co.caller, errUnexpectedBody)
		return false
	}
	if req.GetScope() == 0 {
		co.s.auditScopeRejected(co.caller, req.GetEndpointId(), req.GetScope(),
			ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
		return true
	}

	// The endpoint must really belong to this connection. Without this check, any system service could register ownership for
	// another endpoint, and so insert itself into someone else's event stream.
	if !co.s.endpoints.OwnsEndpoint(co, req.GetEndpointId()) {
		co.s.auditScopeRejected(co.caller, req.GetEndpointId(), req.GetScope(),
			ipcv1.StatusCode_STATUS_CODE_NOT_FOUND)
		return true
	}

	if req.GetReleased() {
		co.s.eventScopes.release(co, req.GetEndpointId(), req.GetScope())
		return true
	}

	// Ownership is proved by the route, not self-reported: what the Provider says is "it belongs to the caller of the call I'm
	// handling right now", and nervud itself knows who made that call.
	entry, ok := co.s.dispatch.origin(req.GetOriginRouteId(), co)
	if !ok || entry.source == nil {
		co.s.auditScopeRejected(co.caller, req.GetEndpointId(), req.GetScope(),
			ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)
		return true
	}
	co.s.eventScopes.bind(co, req.GetEndpointId(), req.GetScope(), entry.source)
	return true
}

func (co *conn) handleUnsubscribe(req *ipcv1.Unsubscribe) bool {
	reqID := req.GetRequestId()
	if reqID == 0 {
		co.s.auditViolation(co.caller, errZeroRequestID)
		return false
	}
	if co.s.subscriptions == nil {
		return co.enqueue(unsubscribeFailure(reqID, ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE))
	}
	if !co.s.subscriptions.Unsubscribe(co, req.GetSubscriptionId()) {
		return co.enqueue(unsubscribeFailure(reqID, ipcv1.StatusCode_STATUS_CODE_NOT_FOUND))
	}
	// After a success result, no more Events will arrive on that subscription_id
	return co.enqueue(&ipcv1.Envelope{Body: &ipcv1.Envelope_UnsubscribeResult{
		UnsubscribeResult: &ipcv1.UnsubscribeResult{
			RequestId: reqID,
			Outcome: &ipcv1.UnsubscribeResult_Success{
				Success: &ipcv1.UnsubscribeSuccess{},
			},
		},
	}})
}

// handlePublishEvent handles one event report from a Provider.
//
// One-way, no result. A result would turn the Provider's event loop into request-response,
// and one slow subscriber could hold up the whole Provider; the right place for backpressure is between nervud and the subscriber.
//
// So failures here (the endpoint doesn't belong to this connection, the event_id isn't in the contract, the payload is over the limit)
// are only audited and this one is dropped, with no reply and no connection close.
func (co *conn) handlePublishEvent(req *ipcv1.PublishEvent) bool {
	if co.s.endpoints == nil || co.s.subscriptions == nil {
		return true
	}
	// Only components that export interfaces may report events: a pure consumer sending this body is going the wrong direction
	if !co.componentType.CanProvideInterfaces() {
		co.log.Warn("ipc: component type may not send PublishEvent, closing")
		co.s.auditViolation(co.caller, errUnexpectedBody)
		return false
	}

	event, routeErr := co.s.endpoints.LookupProviderEvent(
		co, req.GetEndpointId(), req.GetEventId())
	if routeErr.Code != ipcv1.StatusCode_STATUS_CODE_UNSPECIFIED {
		// The Provider pushed an endpoint it doesn't own, or an event not declared in the contract.
		// Drop and audit: this isn't a capability gap but a Provider-side bug or an attempt to overstep
		co.s.auditPublishRejected(co.caller, req.GetEndpointId(), req.GetEventId(), routeErr.Code)
		return true
	}

	// Payload limit: 0 means use the conservative default, not unlimited
	maxPayload := int(event.Meta.GetMaxPayloadBytes())
	if maxPayload == 0 {
		maxPayload = defaultEventPayloadBytes
	}
	if len(req.GetPayload()) > maxPayload {
		co.s.auditPublishRejected(co.caller, req.GetEndpointId(), req.GetEventId(),
			ipcv1.StatusCode_STATUS_CODE_RESOURCE_EXHAUSTED)
		return true
	}

	closed := co.s.subscriptions.Publish(
		subscription.Key{
			ProviderConn: endpoint.ConnHandle(co),
			EndpointID:   req.GetEndpointId(),
			EventID:      req.GetEventId(),
		},
		req.GetPayload(),
		req.GetMonotonicTimestampNanos(),
	)
	co.s.closeSubscriptions(closed)
	return true
}

// closeSubscriptions sends SubscriptionClosed to the terminated subscribers and clears their registrations.
//
// Notify first, then clear: the other way round, a notification arriving after clearing would be taken as
// "a close message for a subscription that doesn't exist", and the subscriber couldn't tell whether it forgot to tear one down.
func (s *Server) closeSubscriptions(closed []subscription.Closed) {
	for _, c := range closed {
		target, ok := c.Conn.(*conn)
		if !ok || target == nil {
			continue
		}
		target.Deliver(&ipcv1.Envelope{Body: &ipcv1.Envelope_SubscriptionClosed{
			SubscriptionClosed: &ipcv1.SubscriptionClosed{
				SubscriptionId: c.SubscriptionID,
				Reason:         c.Reason,
			},
		}})
		s.subscriptions.Unsubscribe(c.Conn, c.SubscriptionID)
	}
}

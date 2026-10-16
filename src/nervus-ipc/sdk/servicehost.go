// This file is the provider-side ServiceHost: it reports in to nervud (RegisterEndpoint), then loops receiving
// the Dispatches nervud forwards, executing them and replying with DispatchResults.
//
// # It isn't a server
//
// A Service never holds the client socket and has no right to decide who the result is written to. It only knows the
// route_id nervud gives, a number that nervud maps internally to (source connection, source request_id, deadline,
// binding generation, method schema); **none of that goes on the wire**, so a Service can't forge
// what it doesn't know. That's exactly why the client's request_id can't be reused directly for global routing.
//
// Typical use:
//
//	h, err := sdk.NewServiceHost(sdk.Config{ComponentID: "main"})
//	h.Handle(methodSetVelocity, func(ctx sdk.CallContext, payload []byte) ([]byte, error) { ... })
//	ep, err := h.RegisterEndpoint(ctx, sdk.RegisterRequest{...})
//	h.Serve()   // blocks
package sdk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// CallContext is the context of one Dispatch, handed to the handler.
type CallContext struct {
	// Ctx already has its deadline set from Dispatch.remaining_ms. Handlers must respect it:
	// remaining_ms is this call's **remaining** budget (queuing and forwarding already used some of it),
	// not the original timeout. If you go over, nervud has long since answered DEADLINE_EXCEEDED, and your result
	// is discarded as a late result.
	Ctx context.Context

	// MethodID is the method being called.
	MethodID uint32
	// RouteID is the connection-scoped route number nervud assigned to this Dispatch. Business code must not
	// treat it as an identity or permission credential; it's only for filling in origin_route_id when calling the generic
	// Transfer Control.
	RouteID uint64
	// EndpointID is this Service-side endpoint handle (the one RegisterEndpoint returned).
	EndpointID uint64

	// Caller is the **trusted** caller context nervud attaches, from the kernel's SO_PEERCRED,
	// not filled in by the caller.
	//
	// You can read it, but **can't** use it to bypass the nervud Policy already in effect, still less invent identities
	// or permission decisions. GrantedPermissions is a read-only projection of nervud's decision, for your second
	// fail-closed check ("my method needs permission X, X isn't in the projection, refuse"), not for you
	// to decide again.
	Caller *ipcv1.CallerContext

	// Execution is the trusted execution snapshot nervud froze after the method gate. Methods needing a control lease
	// must use its resource generation, motion epoch, command
	// sequence and absolute deadline for a last staleness check right before touching the device. LeaseId is only for correlation and can't be used as a
	// transferable bearer capability.
	Execution *ipcv1.ExecutionContext
}

// Handler handles one method call.
//
// Returning (payload, nil) means success; returning an error means failure:
//   - a *StatusError maps directly to the code and error_detail on the wire
//   - any other error is normalised to INTERNAL, with the original text only in the local log
//
// The latter is deliberate: putting an arbitrary Go error's string into the caller's hands would leak internal implementation details
// (paths, SQL, stack information) across the process boundary. To give callers distinguishable reasons, build a
// *StatusError explicitly with the interface's own typed error_detail.
type Handler func(cc CallContext, payload []byte) ([]byte, error)

type handlerKey struct {
	endpointID uint64
	methodID   uint32
}

type controlWaiter struct {
	ch            chan *ipcv1.Envelope
	beforeDeliver func(*ipcv1.Envelope)
}

type inflightCall struct {
	endpointID uint64
	cancel     context.CancelFunc
}

// ServiceHost is the provider side of a control-plane connection.
type ServiceHost struct {
	co  *conn
	ids *requestIDGen
	log *slog.Logger

	mu sync.RWMutex
	// handlers must be addressed by endpoint_id + method_id together. Every interface's method_ids
	// start at 1, and a table keyed only by method_id would let a later-registered interface overwrite an earlier one.
	handlers map[handlerKey]Handler
	// legacyHandlers is the template for the Handle + RegisterEndpoint compatibility API. On each report-in
	// it's snapshotted and installed under the endpoint_id returned that time.
	legacyHandlers map[uint32]Handler

	// pending registers the control requests and ordinary Requests this Service connection sends itself.
	pendingMu sync.Mutex
	pending   map[uint64]*controlWaiter

	inflightMu sync.Mutex
	inflight   map[uint64]*inflightCall

	// subs is the subscription state machine; see subscribe.go. Services subscribe to other people's events too:
	// nervus.camerad subscribes to the vendor Provider's frame-ready events.
	subs *subscriber

	readErr  error
	readDone chan struct{}
}

// writeEnvelope / nextRequestID let ServiceHost satisfy eventTransport
// (closedErr already exists).
func (h *ServiceHost) writeEnvelope(env *ipcv1.Envelope) error { return h.co.writeEnvelope(env) }
func (h *ServiceHost) nextRequestID() (uint64, error)          { return h.ids.nextID() }

// Subscribe subscribes to events on an endpoint. Exactly the same semantics as Client.Subscribe.
func (h *ServiceHost) Subscribe(ctx context.Context, req SubscribeRequest) (*Subscription, error) {
	return h.subs.subscribe(ctx, req)
}

// Unsubscribe takes down a subscription and closes its event channel.
func (h *ServiceHost) Unsubscribe(ctx context.Context, sub *Subscription) error {
	return h.subs.unsubscribe(ctx, sub)
}

// NewServiceHost sets up the connection and completes the handshake.
func NewServiceHost(cfg Config) (*ServiceHost, error) {
	cfg.applyDefaults()
	if cfg.ComponentID == "" {
		return nil, errors.New("sdk: Config.ComponentID is required")
	}

	co, err := dial(cfg.SockPath, cfg.DialTimeout)
	if err != nil {
		return nil, err
	}
	if err := co.handshake(cfg.SDKName, cfg.SDKVersion, cfg.ComponentID, cfg.HandshakeTimeout); err != nil {
		co.close()
		return nil, err
	}
	// From v2 on, Dispatch carries an ExecutionContext unconditionally, and there's no minor gate any more. The reason that
	// check existed (1.0's Dispatch had no caller identity, so Providers couldn't authenticate) disappeared
	// with the major: being able to complete a handshake means the peer is v2.

	h := &ServiceHost{
		co:             co,
		ids:            newRequestIDGen(),
		log:            cfg.Log,
		handlers:       make(map[handlerKey]Handler),
		legacyHandlers: make(map[uint32]Handler),
		pending:        make(map[uint64]*controlWaiter),
		inflight:       make(map[uint64]*inflightCall),
		readDone:       make(chan struct{}),
	}
	h.subs = newSubscriber(h, cfg.Log)
	// The read loop starts at construction, not at Serve: RegisterEndpoint needs to read its own response,
	// and the call order is always "report in first, then Serve". If the read loop were in Serve, there'd be
	// no reader during report-in, and RegisterEndpoint would never get its result: a deadlock.
	go h.serve()
	return h, nil
}

// Identity returns the identity verified at the handshake.
func (h *ServiceHost) Identity() Identity { return h.co.Identity() }

// Limits returns the budgets in effect for this connection.
func (h *ServiceHost) Limits() *ipcv1.ConnectionLimits { return h.co.Limits() }

// Handle registers a method_id handler in compatibility mode.
//
// RegisterEndpoint snapshots these functions on each report-in and binds them to the new endpoint.
// When several interfaces have clashing method_ids, use NewEndpoint instead; unregistered methods are answered with NOT_FOUND
// (fail closed).
func (h *ServiceHost) Handle(methodID uint32, fn Handler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.legacyHandlers[methodID] = fn
}

// Close closes the connection. Idempotent.
func (h *ServiceHost) Close() error {
	h.co.close()
	<-h.readDone
	return nil
}

// RegisterRequest is one endpoint report-in.
type RegisterRequest struct {
	InterfaceID  string
	Major, Minor uint32
	// SchemaHash is the interface schema hash this Service implements. A mismatch with the value nervud registered is refused;
	// that check stops a Provider compiled against an old schema from quietly serving a new interface.
	SchemaHash []byte
	// ResourceHandle is the Resource this endpoint serves, such as "base.main".
	ResourceHandle string
}

// EndpointHost binds a set of handlers to one endpoint report-in.
//
// method_ids are only unique within one interface major, so when one ServiceHost carries several interfaces,
// it must use EndpointHost and can't put every handler into one global method_id table.
type EndpointHost struct {
	host *ServiceHost
	req  RegisterRequest

	mu         sync.Mutex
	handlers   map[uint32]Handler
	endpointID uint64
	registered bool
}

// NewEndpoint creates an endpoint that hasn't reported in yet. It doesn't open a new connection; every endpoint in
// the same process still shares the ServiceHost's one generic IPC connection.
func (h *ServiceHost) NewEndpoint(req RegisterRequest) *EndpointHost {
	req.SchemaHash = append([]byte(nil), req.SchemaHash...)
	return &EndpointHost{
		host:     h,
		req:      req,
		handlers: make(map[uint32]Handler),
	}
}

// Handle registers a method for this endpoint. It must be done before Register.
func (e *EndpointHost) Handle(methodID uint32, fn Handler) error {
	if methodID == 0 {
		return errors.New("sdk: method id 0 is reserved")
	}
	if fn == nil {
		return errors.New("sdk: nil handler")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.registered {
		return errors.New("sdk: cannot change handlers while endpoint is registered")
	}
	e.handlers[methodID] = fn
	return nil
}

// Register atomically reports the endpoint in and installs its handlers.
//
// A successful RegisterEndpointResult and the following Dispatch may arrive back to back; ServiceHost installs the handlers before
// the read loop delivers the success result, so there's no window of "reported in successfully but the first frame can't find the method".
func (e *EndpointHost) Register(ctx context.Context) (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.registered {
		return 0, errors.New("sdk: endpoint is already registered")
	}
	handlers := cloneHandlers(e.handlers)
	id, err := e.host.registerEndpoint(ctx, e.req, handlers)
	if err != nil {
		return 0, err
	}
	e.endpointID = id
	e.registered = true
	return id, nil
}

// EndpointID returns the handle currently reported in; ok=false when not reported in.
func (e *EndpointHost) EndpointID() (id uint64, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.endpointID, e.registered
}

// Unregister withdraws the endpoint. After success it can Register again, keeping its original handlers.
func (e *EndpointHost) Unregister(ctx context.Context, drain bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.registered {
		return errors.New("sdk: endpoint is not registered")
	}
	if err := e.host.UnregisterEndpoint(ctx, e.endpointID, drain); err != nil {
		return err
	}
	e.endpointID = 0
	e.registered = false
	return nil
}

// PublishEvent reports one event on this endpoint, without a production time.
//
// Suitable for pure signals and state changes ("the stream opened", "the device went away"): what matters about these events is that
// "it happened", and the order received is all the timing information there is.
//
// Events carrying sampled data should use PublishEventAt with the capture time.
func (e *EndpointHost) PublishEvent(eventID uint32, payload []byte) error {
	return e.PublishEventAt(eventID, payload, 0)
}

// PublishEventAt reports one event with its production time.
//
// # The timestamp must come from the caller; this SDK doesn't fill it in
//
// The meaningful moment is **when the data was produced**, not "when the Go code got round to publishing".
// For a camera frame the right value is the CLOCK_MONOTONIC timestamp the V4L2 buffer carries, for an IMU the moment of the sampling
// interrupt; those are at the driver layer, out of the SDK's reach. Having the SDK stamp a casual
// time.Now() would produce a number that looks reasonable but has queuing delay mixed in, and consumers
// couldn't tell whether it's the real capture time.
//
// If the real capture time isn't available, pass 0 (meaning not provided); don't pass off the publish time as it.
//
// # One-way, no result
//
// Reporting doesn't wait for a result: a result would turn the Provider's event loop into request-response,
// and one slow subscriber could hold up the whole Provider. The right place for backpressure is between nervud and the subscriber.
//
// The error returned only means **the local write failed** (the connection is down). nervud refusing the event
// (event_id not in the contract, over max_payload_bytes) gets no reply at all; it only leaves
// an ipc.PublishEventRejected entry in the kernel audit.
func (e *EndpointHost) PublishEventAt(eventID uint32, payload []byte, monotonicNanos uint64) error {
	e.mu.Lock()
	id, registered := e.endpointID, e.registered
	e.mu.Unlock()
	if !registered {
		return errors.New("sdk: endpoint is not registered")
	}
	return e.host.co.writeEnvelope(&ipcv1.Envelope{
		Body: &ipcv1.Envelope_PublishEvent{PublishEvent: &ipcv1.PublishEvent{
			EndpointId:              id,
			EventId:                 eventID,
			Payload:                 payload,
			MonotonicTimestampNanos: monotonicNanos,
		}},
	})
}

// BindEventScope registers ownership of an event instance: this scope belongs to the caller of this call.
//
// # When you need it
//
// Your endpoint has several independently observable instances (several streams on one camera),
// and the matching events declare EventMeta.scoped. Without registering, subscribers can never subscribe;
// nervud fails closed and refuses unregistered scopes outright.
//
// # Where to call it
//
// **In the handler that creates the instance, before returning the response.** originRouteID is
// CallContext.RouteID: it proves "this instance belongs to whoever is calling me right now", and nervud itself knows
// who that is; you don't need to (and can't) self-report the caller's identity.
//
//	func (s *svc) openStream(cc sdk.CallContext, payload []byte) ([]byte, error) {
//	    id := s.nextStreamID()
//	    if err := s.ep.BindEventScope(id, cc.RouteID); err != nil { ... }
//	    return proto.Marshal(&OpenStreamResponse{StreamId: id})
//	}
//
// No need to worry about ordering: this call and the response after it travel on the same ordered connection, so by the time the caller gets the stream_id
// the registration has certainly taken effect.
//
// # One-way, no result
//
// The error returned only means **the local write failed** (the connection is down). nervud refusing the registration
// (the endpoint isn't yours, the route doesn't exist) gets no reply; it only leaves
// an ipc.BindEventScopeRejected entry in the kernel audit.
func (e *EndpointHost) BindEventScope(scope, originRouteID uint64) error {
	return e.sendEventScope(scope, originRouteID, false)
}

// ReleaseEventScope revokes one registration: the instance has been destroyed.
//
// **It must be revoked explicitly.** Otherwise a service repeatedly opening and closing streams would make nervud's ownership table grow
// without bound. Disconnects and endpoint withdrawal clean up as well, but that covers crashes, not normal closes.
func (e *EndpointHost) ReleaseEventScope(scope uint64) error {
	return e.sendEventScope(scope, 0, true)
}

func (e *EndpointHost) sendEventScope(scope, originRouteID uint64, released bool) error {
	if scope == 0 {
		return errors.New("sdk: event scope 0 is reserved")
	}
	e.mu.Lock()
	id, registered := e.endpointID, e.registered
	e.mu.Unlock()
	if !registered {
		return errors.New("sdk: endpoint is not registered")
	}
	return e.host.co.writeEnvelope(&ipcv1.Envelope{
		Body: &ipcv1.Envelope_BindEventScope{BindEventScope: &ipcv1.BindEventScope{
			EndpointId:    id,
			Scope:         scope,
			OriginRouteId: originRouteID,
			Released:      released,
		}},
	})
}

// ResolveEndpoint resolves a consumer-side endpoint on the same Service control connection.
//
// When a Provider needs to call back into a system interface (especially Transfer Control) it must use this method rather than
// opening a separate Client connection: Dispatch.route_id only means something on the connection that received that Dispatch.
func (h *ServiceHost) ResolveEndpoint(ctx context.Context, req ResolveRequest) (Endpoint, error) {
	id, err := h.ids.nextID()
	if err != nil {
		return Endpoint{}, err
	}
	env := &ipcv1.Envelope{Body: &ipcv1.Envelope_ResolveEndpoint{
		ResolveEndpoint: &ipcv1.ResolveEndpoint{
			RequestId:         id,
			InterfaceId:       req.InterfaceID,
			MinInterfaceMajor: req.MinMajor,
			MaxInterfaceMajor: req.MaxMajor,
			Selector:          req.selector(),
		},
	}}
	res, err := h.roundTrip(ctx, id, env, nil)
	if err != nil {
		return Endpoint{}, err
	}
	r := res.GetResolveEndpointResult()
	if r == nil {
		return Endpoint{}, fmt.Errorf("%w: expected ResolveEndpointResult", ErrProtocol)
	}
	if failure := r.GetFailure(); failure != nil {
		return Endpoint{}, statusErrorFrom(failure)
	}
	success := r.GetSuccess()
	if success == nil || success.GetEndpointId() == 0 {
		return Endpoint{}, fmt.Errorf("%w: ResolveEndpoint success has invalid endpoint id", ErrProtocol)
	}
	return Endpoint{
		EndpointID:     success.GetEndpointId(),
		Major:          success.GetInterfaceMajor(),
		Minor:          success.GetInterfaceMinor(),
		SchemaHash:     append([]byte(nil), success.GetInterfaceSchemaHash()...),
		ResourceHandle: success.GetResourceHandle(),
	}, nil
}

// Call sends an ordinary Request from the same Service control connection.
//
// It uses the same wire as Client.Call; the only difference is that the reader is managed centrally by ServiceHost,
// so handlers can safely call back into kernel system interfaces while handling a Dispatch without fighting the service-side
// Dispatch reader for the same socket.
func (h *ServiceHost) Call(
	ctx context.Context,
	endpointID uint64,
	methodID uint32,
	payload []byte,
	timeout time.Duration,
) ([]byte, error) {
	id, err := h.ids.nextID()
	if err != nil {
		return nil, err
	}
	env := &ipcv1.Envelope{Body: &ipcv1.Envelope_Request{Request: &ipcv1.Request{
		RequestId:  id,
		EndpointId: endpointID,
		MethodId:   methodID,
		TimeoutMs:  uint32(timeout.Milliseconds()),
		Payload:    payload,
	}}}
	res, err := h.roundTrip(ctx, id, env, nil)
	if err != nil {
		return nil, err
	}
	response := res.GetResponse()
	if response == nil {
		return nil, fmt.Errorf("%w: expected Response", ErrProtocol)
	}
	if failure := response.GetFailure(); failure != nil {
		return nil, statusErrorFrom(failure)
	}
	success := response.GetSuccess()
	if success == nil {
		return nil, fmt.Errorf("%w: Response has neither outcome", ErrProtocol)
	}
	return append([]byte(nil), success.GetPayload()...), nil
}

// RegisterEndpoint reports in to nervud.
//
// This is **reporting in, not creating permissions by itself**: only endpoints the manifest declared, matching the signature/permission
// profile, can be registered. nervud decides independently; declaring something doesn't mean getting it.
//
// The endpointID returned is **a Service-side handle**, in a different namespace from callers' endpoint_ids:
// the same number on an App connection and this connection means nothing in common.
func (h *ServiceHost) RegisterEndpoint(ctx context.Context, req RegisterRequest) (uint64, error) {
	h.mu.RLock()
	handlers := cloneHandlers(h.legacyHandlers)
	h.mu.RUnlock()
	return h.registerEndpoint(ctx, req, handlers)
}

func (h *ServiceHost) registerEndpoint(
	ctx context.Context,
	req RegisterRequest,
	handlers map[uint32]Handler,
) (uint64, error) {
	id, err := h.ids.nextID()
	if err != nil {
		return 0, err
	}
	env := &ipcv1.Envelope{Body: &ipcv1.Envelope_RegisterEndpoint{
		RegisterEndpoint: &ipcv1.RegisterEndpoint{
			RequestId:           id,
			InterfaceId:         req.InterfaceID,
			InterfaceMajor:      req.Major,
			InterfaceMinor:      req.Minor,
			InterfaceSchemaHash: req.SchemaHash,
			ResourceHandle:      req.ResourceHandle,
		},
	}}

	res, err := h.roundTrip(ctx, id, env, func(res *ipcv1.Envelope) {
		r := res.GetRegisterEndpointResult()
		if r == nil {
			return
		}
		if success := r.GetSuccess(); success != nil && success.GetEndpointId() != 0 {
			h.installEndpointHandlers(success.GetEndpointId(), handlers)
		}
	})
	if err != nil {
		return 0, err
	}
	r := res.GetRegisterEndpointResult()
	if r == nil {
		return 0, fmt.Errorf("%w: expected RegisterEndpointResult", ErrProtocol)
	}
	if f := r.GetFailure(); f != nil {
		return 0, statusErrorFrom(f)
	}
	success := r.GetSuccess()
	if success == nil || success.GetEndpointId() == 0 {
		return 0, fmt.Errorf("%w: RegisterEndpoint success has invalid endpoint id", ErrProtocol)
	}
	return success.GetEndpointId(), nil
}

// UnregisterEndpoint gracefully withdraws an endpoint (SHUTTING_DOWN in NRCP §16.2).
//
// drain=true means wait for in-flight Dispatches to finish before withdrawing.
//
// **Note**: nervud doesn't wire drain yet (the execution layer has no in-flight Dispatch tracking), so drain=true
// degrades to withdrawing immediately, ending in-flight calls with UNAVAILABLE.
func (h *ServiceHost) UnregisterEndpoint(ctx context.Context, endpointID uint64, drain bool) error {
	id, err := h.ids.nextID()
	if err != nil {
		return err
	}
	env := &ipcv1.Envelope{Body: &ipcv1.Envelope_UnregisterEndpoint{
		UnregisterEndpoint: &ipcv1.UnregisterEndpoint{
			RequestId: id, EndpointId: endpointID, Drain: drain,
		},
	}}
	res, err := h.roundTrip(ctx, id, env, func(res *ipcv1.Envelope) {
		r := res.GetUnregisterEndpointResult()
		if r != nil && r.GetSuccess() != nil {
			h.invalidateEndpoint(endpointID)
		}
	})
	if err != nil {
		return err
	}
	r := res.GetUnregisterEndpointResult()
	if r == nil {
		return fmt.Errorf("%w: expected UnregisterEndpointResult", ErrProtocol)
	}
	if f := r.GetFailure(); f != nil {
		return statusErrorFrom(f)
	}
	return nil
}

// roundTrip sends a control request or ordinary Request carrying a request_id and waits for its result
// Envelope. Dispatches are still driven independently by the read loop.
func (h *ServiceHost) roundTrip(
	ctx context.Context,
	id uint64,
	env *ipcv1.Envelope,
	beforeDeliver func(*ipcv1.Envelope),
) (*ipcv1.Envelope, error) {
	ch := make(chan *ipcv1.Envelope, 1)
	h.pendingMu.Lock()
	h.pending[id] = &controlWaiter{ch: ch, beforeDeliver: beforeDeliver}
	h.pendingMu.Unlock()

	drop := func() bool {
		h.pendingMu.Lock()
		_, present := h.pending[id]
		if present {
			delete(h.pending, id)
		}
		h.pendingMu.Unlock()
		return present
	}

	if err := h.co.writeEnvelope(env); err != nil {
		drop()
		return nil, err
	}
	select {
	case <-ctx.Done():
		if drop() {
			return nil, ctx.Err()
		}
		// deliverControl has already taken the waiter out of pending, which means the authoritative result is being delivered.
		// Wait for it to be written into the capacity-1 channel, so we avoid the torn state where Register succeeded and installed the handlers but the caller
		// only saw context canceled because of a select race.
		res, ok := <-ch
		if !ok {
			return nil, h.closedErr()
		}
		return res, nil
	case res, ok := <-ch:
		if !ok {
			return nil, h.closedErr()
		}
		return res, nil
	}
}

func cloneHandlers(src map[uint32]Handler) map[uint32]Handler {
	out := make(map[uint32]Handler, len(src))
	for methodID, handler := range src {
		out[methodID] = handler
	}
	return out
}

func (h *ServiceHost) installEndpointHandlers(endpointID uint64, handlers map[uint32]Handler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for methodID, handler := range handlers {
		h.handlers[handlerKey{endpointID: endpointID, methodID: methodID}] = handler
	}
}

func (h *ServiceHost) invalidateEndpoint(endpointID uint64) {
	h.mu.Lock()
	for key := range h.handlers {
		if key.endpointID == endpointID {
			delete(h.handlers, key)
		}
	}
	h.mu.Unlock()

	h.inflightMu.Lock()
	var calls []*inflightCall
	for _, call := range h.inflight {
		if call.endpointID == endpointID {
			calls = append(calls, call)
		}
	}
	h.inflightMu.Unlock()
	for _, call := range calls {
		call.cancel()
	}
}

// Serve blocks until the connection drops and returns why (returning nil when this side's Close caused it).
//
// The read loop already started in NewServiceHost, and Serve only waits for it to end. So the recommended call order
// is: Handle to register every handler → RegisterEndpoint to report in → Serve to block.
//
// **Be sure to finish calling Handle before reporting in.** nervud may forward Dispatches the moment report-in succeeds,
// and methods not registered yet are answered with NOT_FOUND; the caller would think the method doesn't exist rather than
// "the service isn't ready yet", an error that's hard to trace back.
func (h *ServiceHost) Serve() error {
	<-h.readDone
	return h.readErr
}

func (h *ServiceHost) serve() {
	defer close(h.readDone)
	defer h.failAllPending()

	for {
		env, err := h.co.readEnvelope()
		if err != nil {
			select {
			case <-h.co.closed:
				// A read failure caused by this side's own Close isn't an anomaly
			default:
				h.readErr = err
			}
			h.co.close()
			return
		}
		if stop := h.handleEnvelope(env); stop {
			h.co.close()
			return
		}
	}
}

// handleEnvelope dispatches one received Envelope and returns whether the connection should end.
func (h *ServiceHost) handleEnvelope(env *ipcv1.Envelope) bool {
	// The four subscription-related bodies mean the same in both roles, so they go to the shared state machine.
	if h.subs.handleSubscriptionBody(env) {
		return false
	}

	switch body := env.GetBody().(type) {
	case *ipcv1.Envelope_Dispatch:
		// startDispatch registers the route in the read loop before starting the goroutine. That way a CancelDispatch arriving right
		// after the Dispatch can't be lost by getting ahead of the registration.
		if !h.startDispatch(body.Dispatch) {
			h.readErr = fmt.Errorf("%w: duplicate or invalid dispatch route", ErrProtocol)
			return true
		}
		return false

	case *ipcv1.Envelope_CancelDispatch:
		h.cancelDispatch(body.CancelDispatch.GetRouteId())
		return false

	case *ipcv1.Envelope_RegisterEndpointResult:
		h.deliverControl(body.RegisterEndpointResult.GetRequestId(), env)
		return false

	case *ipcv1.Envelope_UnregisterEndpointResult:
		h.deliverControl(body.UnregisterEndpointResult.GetRequestId(), env)
		return false

	case *ipcv1.Envelope_ResolveEndpointResult:
		h.deliverControl(body.ResolveEndpointResult.GetRequestId(), env)
		return false

	case *ipcv1.Envelope_Response:
		h.deliverControl(body.Response.GetRequestId(), env)
		return false

	case *ipcv1.Envelope_Ping:
		pong := &ipcv1.Envelope{Body: &ipcv1.Envelope_Pong{
			Pong: &ipcv1.Pong{Nonce: body.Ping.GetNonce()},
		}}
		if err := h.co.writeEnvelope(pong); err != nil {
			h.readErr = err
			return true
		}
		return false

	case *ipcv1.Envelope_Pong:
		return false

	case *ipcv1.Envelope_EndpointRevoked:
		h.invalidateEndpoint(body.EndpointRevoked.GetEndpointId())
		h.log.Info("sdk: endpoint invalidated by server",
			"endpoint_id", body.EndpointRevoked.GetEndpointId(), "body", "EndpointRevoked")
		return false

	case *ipcv1.Envelope_EndpointDied:
		h.invalidateEndpoint(body.EndpointDied.GetEndpointId())
		h.log.Info("sdk: endpoint invalidated by server",
			"endpoint_id", body.EndpointDied.GetEndpointId(), "body", "EndpointDied")
		return false

	default:
		h.readErr = fmt.Errorf("%w: unexpected body %T on service connection", ErrProtocol, body)
		h.log.Warn("sdk: unexpected body, closing", "body", fmt.Sprintf("%T", body))
		return true
	}
}

func (h *ServiceHost) deliverControl(id uint64, env *ipcv1.Envelope) {
	h.pendingMu.Lock()
	waiter, ok := h.pending[id]
	if ok {
		delete(h.pending, id)
	}
	h.pendingMu.Unlock()
	if !ok {
		h.log.Debug("sdk: dropping unmatched control result", "request_id", id)
		return
	}
	if waiter.beforeDeliver != nil {
		waiter.beforeDeliver(env)
	}
	waiter.ch <- env
}

func (h *ServiceHost) startDispatch(d *ipcv1.Dispatch) bool {
	if d == nil || d.GetRouteId() == 0 || !validExecutionContext(d.GetExecutionContext()) {
		return false
	}
	// remaining_ms is the remaining budget. When it's 0 no deadline is set: in the protocol 0 means "use the default",
	// not "already timed out", and treating it as a 0-second deadline would make every such call fail at once.
	ctx := context.Background()
	var cancel context.CancelFunc
	if ms := d.GetRemainingMs(); ms > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	call := &inflightCall{endpointID: d.GetEndpointId(), cancel: cancel}

	h.inflightMu.Lock()
	if _, duplicate := h.inflight[d.GetRouteId()]; duplicate {
		h.inflightMu.Unlock()
		cancel()
		return false
	}
	h.inflight[d.GetRouteId()] = call
	h.inflightMu.Unlock()

	// One slow method shouldn't block other calls on the same Service. The concurrency limit is controlled at the source by nervud's
	// max_inflight_requests, so no second queue layer is added here.
	go h.dispatch(d, ctx, call)
	return true
}

func (h *ServiceHost) cancelDispatch(routeID uint64) {
	h.inflightMu.Lock()
	call := h.inflight[routeID]
	h.inflightMu.Unlock()
	if call == nil {
		h.log.Debug("sdk: dropping cancel for unknown route", "route_id", routeID)
		return
	}
	call.cancel()
}

func (h *ServiceHost) finishDispatch(routeID uint64, call *inflightCall) {
	h.inflightMu.Lock()
	if h.inflight[routeID] == call {
		delete(h.inflight, routeID)
	}
	h.inflightMu.Unlock()
	call.cancel()
}

// dispatch runs one method call and replies with a DispatchResult.
func (h *ServiceHost) dispatch(d *ipcv1.Dispatch, ctx context.Context, call *inflightCall) {
	routeID := d.GetRouteId()
	defer h.finishDispatch(routeID, call)

	h.mu.RLock()
	fn, ok := h.handlers[handlerKey{endpointID: d.GetEndpointId(), methodID: d.GetMethodId()}]
	h.mu.RUnlock()

	if !ok {
		// Fail closed: no handler means not implemented, never a silent success. A silent success would make
		// the caller think the action ran. The reason only goes to the local log (public_message isn't written by Providers)
		h.log.Warn("sdk: no handler for method", "method_id", d.GetMethodId(), "route_id", routeID)
		h.replyFailure(routeID, &StatusError{Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND})
		return
	}

	cc := CallContext{
		Ctx:        ctx,
		MethodID:   d.GetMethodId(),
		RouteID:    routeID,
		EndpointID: d.GetEndpointId(),
		Caller:     d.GetCaller(),
		Execution:  d.GetExecutionContext(),
	}

	payload, err := h.safeCall(fn, cc, d.GetPayload())
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			h.replyFailure(routeID, se)
			return
		}
		if errors.Is(err, context.Canceled) {
			h.replyFailure(routeID, &StatusError{Code: ipcv1.StatusCode_STATUS_CODE_CANCELLED})
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			h.replyFailure(routeID, &StatusError{Code: ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED})
			return
		}
		// Anything that isn't a StatusError is normalised to INTERNAL, with the original text only in the local log, so internal
		// implementation details (paths/stacks/dependency errors) don't leak across the process boundary.
		h.log.Error("sdk: handler failed", "method_id", d.GetMethodId(), "err", err)
		h.replyFailure(routeID, &StatusError{Code: ipcv1.StatusCode_STATUS_CODE_INTERNAL})
		return
	}

	env := &ipcv1.Envelope{Body: &ipcv1.Envelope_DispatchResult{
		DispatchResult: &ipcv1.DispatchResult{
			RouteId: routeID,
			Outcome: &ipcv1.DispatchResult_Success{Success: &ipcv1.Success{
				Code:    ipcv1.StatusCode_STATUS_CODE_OK,
				Payload: payload,
			}},
		},
	}}
	if err := h.co.writeEnvelope(env); err != nil {
		h.log.Debug("sdk: send DispatchResult failed", "route_id", routeID, "err", err)
	}
}

func validExecutionContext(ec *ipcv1.ExecutionContext) bool {
	if ec == nil || ec.GetDeadlineNanos() <= 0 {
		return false
	}
	hasResource := ec.GetResourceHandle() != ""
	if hasResource != (ec.GetResourceGeneration() != 0) {
		return false
	}
	if ec.GetLeaseId() == 0 {
		return ec.GetControllerClass() == ipcv1.ControllerClass_CONTROLLER_CLASS_UNSPECIFIED &&
			ec.GetMotionEpoch() == 0 && ec.GetCommandSequence() == 0
	}
	if !hasResource || ec.GetMotionEpoch() == 0 || ec.GetCommandSequence() == 0 {
		return false
	}
	switch ec.GetControllerClass() {
	case ipcv1.ControllerClass_CONTROLLER_CLASS_HUMAN,
		ipcv1.ControllerClass_CONTROLLER_CLASS_AI:
		return true
	default:
		return false
	}
}

// safeCall runs the handler and turns a panic into an error.
//
// One Provider handler panicking shouldn't take down the whole Service process: that would make in-flight calls of other endpoints
// in the same process vanish too, and nervud would only see a dropped connection, unable to attribute it to a specific
// method. Turning it into INTERNAL gives the caller a clear failure and leaves the full picture in the log.
func (h *ServiceHost) safeCall(fn Handler, cc CallContext, payload []byte) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("sdk: handler panicked", "method_id", cc.MethodID, "panic", r)
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	return fn(cc, payload)
}

// replyFailure replies with a failed DispatchResult.
//
// **public_message is deliberately not sent**: status.proto says it may only be generated by nervud from audited templates
// and sanitised structured fields, and Provider free text must never be passed through as is. The only legitimate channel for a Provider to express
// a distinguishable cause is error_detail (the interface's own typed detail);
// nervud decodes and checks it by the authoritative schema and re-encodes it after dropping unknown fields before forwarding.
// So StatusError.PublicMessage is ignored on the provider side and only used for the local log.
func (h *ServiceHost) replyFailure(routeID uint64, se *StatusError) {
	env := &ipcv1.Envelope{Body: &ipcv1.Envelope_DispatchResult{
		DispatchResult: &ipcv1.DispatchResult{
			RouteId: routeID,
			Outcome: &ipcv1.DispatchResult_Failure{Failure: &ipcv1.Failure{
				Code:        se.Code,
				ErrorDetail: se.Detail,
			}},
		},
	}}
	if err := h.co.writeEnvelope(env); err != nil {
		h.log.Debug("sdk: send failure DispatchResult failed", "route_id", routeID, "err", err)
	}
}

func (h *ServiceHost) failAllPending() {
	h.subs.closeAll(h.closedErr())

	h.pendingMu.Lock()
	m := h.pending
	h.pending = make(map[uint64]*controlWaiter)
	h.pendingMu.Unlock()
	for _, waiter := range m {
		close(waiter.ch)
	}

	h.inflightMu.Lock()
	calls := make([]*inflightCall, 0, len(h.inflight))
	for _, call := range h.inflight {
		calls = append(calls, call)
	}
	h.inflight = make(map[uint64]*inflightCall)
	h.inflightMu.Unlock()
	for _, call := range calls {
		call.cancel()
	}
}

func (h *ServiceHost) closedErr() error {
	if h.readErr != nil {
		return fmt.Errorf("%w: %v", ErrClosed, h.readErr)
	}
	return ErrClosed
}

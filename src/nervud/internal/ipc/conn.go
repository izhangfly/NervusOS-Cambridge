// This file is the per-connection Envelope state machine: the first-frame Hello/HelloAck handshake,
// then dispatch by body. Framing is in frame.go, frame -> Envelope well-formedness checks in envelope.go,
// and connection admission and the frame pump in ipc.go
//
// Write-back contract: each connection owns a bounded outbound queue (outbox.go) + a
// separate writer goroutine (runWriter), the only place that really calls co.c.Write,
// meeting frame.go's "one writer per connection" constraint. Any goroutine, such as the frame pump or handleRequest,
// only queues Envelopes through enqueue and never touches the socket directly. That's also why
// Dispatch forwarding (route.go/dispatch.go) can safely write an Envelope into another
// connection: the target connection's own writer actually performs that write
package ipc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"google.golang.org/protobuf/proto"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/control"
	"github.com/nervus-os/nervud/internal/endpoint"
	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/operation"
	"github.com/nervus-os/nervud/internal/pkgregistry"
	"github.com/nervus-os/nervud/internal/protocheck"
)

// The control-plane protocol version nervud implements.
//
// nervud implements only this one major; minor only ever increases, and the handshake takes the intersection with the range the client declares.
// Major incompatibilities raise the major, and connections that can't negotiate are refused
const (
	// protocolMajor = 2: v2 isn't backward compatible with v1.
	//
	// What breaks the wire is the removal of two implicit defaults: an empty selector in ResolveEndpoint and AcquireControl
	// no longer implicitly means {nervus.resource.motion.base, main}. A v1 client
	// that leaves the selector empty to request the base's lease would get "can't resolve" on v2 instead of the base.
	// That kind of silent change of meaning is far more dangerous than refusing the connection, so the major has to refuse it.
	protocolMajor    = 2
	protocolMinorMax = 0
)

// Cases found during handshake/dispatch that need the connection closed. They're split into two kinds of sentinel so offline audit rules
// can tell protocol violations / potential attacks from capability gaps this build hasn't implemented yet; mixed
// into one Action, real signs of attack would be buried in the noise of unimplemented paths
var (
	// errHandshakeExpectedHello: a body other than Hello arrived before the handshake (the first frame
	// must be Hello). An illegal handshake state, closed as a protocol violation
	errHandshakeExpectedHello = errors.New("ipc: first frame must be Hello")

	// errDuplicateHello: another Hello arrived after the handshake completed. Also an illegal handshake state
	errDuplicateHello = errors.New("ipc: Hello received after handshake completed")

	// errUnexpectedBody: the client sent a response/push body that only the server should produce
	//  (Response, Event, *Result and so on). A protocol violation
	errUnexpectedBody = errors.New("ipc: server-originated body not valid inbound from client")

	// errUnsupportedBody: a control/call body the client legitimately sent but this build hasn't implemented yet.
	// Not a violation; a capability gap
	errUnsupportedBody = errors.New("ipc: body not implemented in this build")

	// errZeroRequestID: a request with a correlation ID used the reserved value 0.
	// The protocol explicitly requires Request, AcquireControl and LaunchComponent to start at 1.
	errZeroRequestID = errors.New("ipc: request uses reserved request_id 0")

	// errDispatchResultMismatch: the route_id a DispatchResult carries exists, but the connection sending the
	// result isn't the registered target connection. There's no legitimate explanation: no Service is ever told a
	// route_id pointing at another connection, so this isn't a capability gap but potential impersonation
	errDispatchResultMismatch = errors.New("ipc: DispatchResult route_id targets a different connection")
)

// phase is the stage of the connection state machine
type phase uint8

const (
	// phaseHandshake waits for the first-frame Hello. Only Hello is accepted in this stage; any other body is closed as
	// an illegal handshake state
	phaseHandshake phase = iota
	// phaseReady: the handshake is complete; dispatch by body
	phaseReady
)

// conn is the Envelope state machine of an admitted connection.
//
// It doesn't own the socket's lifecycle (ipc.go's admit/release manage that); it's called once each time the frame pump
// decodes an Envelope
type conn struct {
	s   *Server
	c   net.Conn
	w   *bufio.Writer
	log *slog.Logger

	// caller is the trusted identity resolved when the connection was set up. ComponentID is filled in after a successful handshake
	//  (the Component nervud confirmed after verifying the claim, not the self-reported value)
	caller identity.Caller
	// componentType comes from service.Manager's verified unit mapping, never
	// from Hello. It gates Service-only protocol directions.
	componentType pkgregistry.ComponentType

	phase phase

	// negMajor/negMinor label the protocol version of outbound Envelopes. Before the handshake they use the version nervud supports,
	// so a Failure HelloAck from a failed negotiation can still tell the client which version this side speaks;
	// after a successful handshake they're updated to the chosen version
	negMajor uint32
	negMinor uint32

	// outbox is this connection's bounded outbound queue, with runWriter as its only consumer,
	// so several goroutines writing concurrently can't corrupt frame boundaries
	outbox *outboundQueue
	// writerDone is closed when runWriter exits, for serve's cleanup to wait on: only after the writer
	// has really stopped touching the socket can the outer layer safely close the underlying connection
	writerDone chan struct{}

	// requestMu guards the unified in-flight request tracker. Dispatch and
	// builtin calls both reserve one entry and their canonical payload bytes.
	requestMu    sync.Mutex
	requests     map[uint64]int
	requestBytes int64

	// connID is this connection's identifier in the control module. control's ConnID is a uint64,
	// while endpoint uses ConnHandle (interface{}) taking the *conn pointer directly. There are two because
	// control's lease table must be queryable by value in RevokeConn, and a pointer as a key passed across modules can easily
	// end up holding a connection that was already released by accident.
	connID control.ConnID

	// leaseMu protects the two lease handle mapping tables below.
	//
	// Why a mapping: the lease_id on the wire is a uint64, while control.ID is a [16]byte.
	// Truncating the internal ID to uint64 would collide; a [16]byte doesn't fit into a uint64.
	// So outward handles are allocated monotonically per connection, and internal IDs never leave the process, the same principle as endpoint_id:
	// the lookup key is (connection, handle), and the same number elsewhere isn't the same thing.
	leaseMu   sync.Mutex
	nextLease uint64
	leases    map[uint64]control.ID
	// leaseHandles preserves the wire handle across an idempotent renewal.
	// control returns the same internal ID for that case, and the protocol
	// requires lease_id to remain stable until the lease ends.
	leaseHandles map[control.ID]uint64
}

func newConn(s *Server, c net.Conn, caller identity.Caller, log *slog.Logger) *conn {
	return &conn{
		s:            s,
		c:            c,
		w:            bufio.NewWriter(c),
		log:          log,
		caller:       caller,
		phase:        phaseHandshake,
		negMajor:     protocolMajor,
		negMinor:     protocolMinorMax,
		outbox:       newOutboundQueue(maxOutboundQueueBytes),
		writerDone:   make(chan struct{}),
		connID:       control.ConnID(s.nextConnID.Add(1)),
		leases:       make(map[uint64]control.ID),
		leaseHandles: make(map[control.ID]uint64),
		requests:     make(map[uint64]int),
	}
}

func (co *conn) reserveRequest(requestID uint64, payloadBytes int) ipcv1.StatusCode {
	co.requestMu.Lock()
	defer co.requestMu.Unlock()
	if _, exists := co.requests[requestID]; exists {
		return ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT
	}
	if len(co.requests) >= maxInflightRequests ||
		co.requestBytes+int64(payloadBytes) > maxInflightPayloadBytes {
		return ipcv1.StatusCode_STATUS_CODE_RESOURCE_EXHAUSTED
	}
	co.requests[requestID] = payloadBytes
	co.requestBytes += int64(payloadBytes)
	return ipcv1.StatusCode_STATUS_CODE_UNSPECIFIED
}

func (co *conn) releaseRequest(requestID uint64) {
	co.requestMu.Lock()
	if payloadBytes, ok := co.requests[requestID]; ok {
		delete(co.requests, requestID)
		co.requestBytes -= int64(payloadBytes)
	}
	co.requestMu.Unlock()
}

// leaseConnID returns this connection's identifier in the control module.
func (co *conn) leaseConnID() control.ConnID { return co.connID }

// registerLease allocates a connection-scoped outward handle for a newly issued lease. An idempotent renewal of the same internal lease
// reuses its handle; different leases never reuse an old number, even when their lifetimes don't overlap.
func (co *conn) registerLease(l control.Lease) uint64 {
	co.leaseMu.Lock()
	defer co.leaseMu.Unlock()
	if handle, ok := co.leaseHandles[l.ID]; ok {
		return handle
	}
	if co.leases == nil {
		co.leases = make(map[uint64]control.ID)
	}
	if co.leaseHandles == nil {
		co.leaseHandles = make(map[control.ID]uint64)
	}
	co.nextLease++
	co.leases[co.nextLease] = l.ID
	co.leaseHandles[l.ID] = co.nextLease
	return co.nextLease
}

// lookupLease maps an outward handle back to the internal control.ID.
func (co *conn) lookupLease(handle uint64) (control.ID, bool) {
	co.leaseMu.Lock()
	defer co.leaseMu.Unlock()
	id, ok := co.leases[handle]
	return id, ok
}

// wireLeaseHandle returns the connection-scoped wire handle for an internal
// lease proof. A valid control lease without a registered handle is not safe to
// dispatch: the Provider must see the same lease_id the caller received.
func (co *conn) wireLeaseHandle(id control.ID) (uint64, bool) {
	co.leaseMu.Lock()
	defer co.leaseMu.Unlock()
	handle, ok := co.leaseHandles[id]
	return handle, ok && handle != 0
}

// forgetLease unregisters a released handle.
//
// Handles aren't reused: the next lease on the same connection gets a new number. Reuse would let a late
// ReleaseControl release a newly issued lease: both would have the same lease_id on the same connection,
// and the receiver would have no way to tell them apart. The same reason request_id doesn't wrap around.
func (co *conn) forgetLease(handle uint64) {
	co.leaseMu.Lock()
	co.forgetLeaseHandleLocked(handle)
	co.leaseMu.Unlock()
}

// forgetLeaseID removes only the wire handle derived from the exact internal
// lease that ended. Terminal notifications can arrive after timeout,
// preemption, Safety revocation, or permission changes; deleting by resource
// would risk removing a newer lease for the same connection and resource.
func (co *conn) forgetLeaseID(id control.ID) {
	co.leaseMu.Lock()
	if handle, ok := co.leaseHandles[id]; ok {
		co.forgetLeaseHandleLocked(handle)
	}
	co.leaseMu.Unlock()
}

func (co *conn) forgetLeaseHandleLocked(handle uint64) {
	if id, ok := co.leases[handle]; ok && co.leaseHandles[id] == handle {
		delete(co.leaseHandles, id)
	}
	delete(co.leases, handle)
}

// runWriter is the only goroutine on this connection that really calls co.c.Write: it loops taking Envelopes
// from the outbox and serialising them out. When a write fails (the socket is broken) it closes the outbox (blocking further
// enqueues, so concurrent callers discover at once "this connection is dead" instead of piling up silently) and the underlying connection,
// then exits; it exits normally when the outbox is closed and drained.
//
// Its lifecycle is folded into the Kernel's existing shutdown wait protocol by serve with s.wg; for the exact cleanup order see
// serve in ipc.go
func (co *conn) runWriter() {
	defer co.s.wg.Done()
	defer close(co.writerDone)

	for {
		env, ok := co.outbox.pop()
		if !ok {
			return
		}
		if err := co.writeEnvelope(env); err != nil {
			co.log.Debug("ipc: writer failed, closing connection", "err", err)
			co.outbox.close()
			_ = co.c.Close()
			return
		}
	}
}

// enqueue queues env in this connection's outbound queue for runWriter to write asynchronously. When the queue is full
// or closed nothing is dropped silently; it's handled as a slow consumer: close the connection, rate-limit the audit, and return false
// to tell the caller "this connection is dead" (the caller may be the goroutine of another connection forwarding a Dispatch,
// or this connection's own frame pump)
func (co *conn) enqueue(env *ipcv1.Envelope) bool {
	if co.outbox.push(env) {
		return true
	}
	co.closeAsSlowConsumer()
	return false
}

// Deliver implements subscription.Sink: try to queue one Event, without closing the connection when the queue is full.
//
// The difference from enqueue is deliberate. enqueue is for request-response paths: there, failing to deliver means
// the peer can't even take what it asked for, and closing is right. Subscriptions are different: one subscription falling behind doesn't mean
// the whole connection is dead, and the same subscriber may have other subscriptions it can keep up with.
//
// So this only reports success or failure, and subscription.Registry decides by delivery_class whether to
// merge, drop, or close only this subscription.
func (co *conn) Deliver(env *ipcv1.Envelope) bool {
	return co.outbox.push(env)
}

// closeAsSlowConsumer handles "this connection can't keep up" cases such as a full/closed outbound queue:
// close the connection and rate-limit the audit. It uses a separate Action to distinguish it from ProtocolViolation/UnsupportedBody:
// this is a resource condition, not an attack signal (it reuses violationLog's token bucket for the same reason as auditUnsupported:
// both end by closing the connection)
func (co *conn) closeAsSlowConsumer() {
	co.outbox.close()
	_ = co.c.Close()
	if !co.s.violationLog.allow() {
		return
	}
	co.s.auditor.Record(context.Background(), audit.Event{
		Action:  "ipc.SlowConsumerDisconnect",
		Subject: co.caller.String(),
		Denied:  true,
	})
}

// readDeadline gives the longest wait for the next frame in the current phase.
//
// The handshake window is much shorter than the steady-state idle window: connecting and saying nothing (or saying something other than Hello) shouldn't hold
// a connection slot for long. After the handshake it moves to the idle window kept alive by Ping/Pong
func (co *conn) readDeadline() time.Duration {
	if co.phase == phaseHandshake {
		return co.s.limits.HandshakeTimeout
	}
	return co.s.limits.IdleTimeout
}

// handle processes one well-formed Envelope and returns whether to keep reading this connection.
// Returning false means the connection should be closed; audit and logging before closing happen inside handle
func (co *conn) handle(env *ipcv1.Envelope) bool {
	if co.phase == phaseHandshake {
		return co.handleHandshake(env)
	}
	return co.handleReady(env)
}

// handleHandshake performs the Hello/HelloAck handshake
func (co *conn) handleHandshake(env *ipcv1.Envelope) bool {
	hello := env.GetHello()
	if hello == nil {
		// The first frame must be Hello. An illegal handshake state is closed as a protocol violation.
		// No HelloAck is sent here: the peer never went through the handshake, so a handshake result would mean nothing
		co.log.Warn("ipc: first frame is not Hello, closing", "body", bodyName(env))
		co.s.auditViolation(co.caller, errHandshakeExpectedHello)
		return false
	}

	major, minor, ok := negotiateVersion(hello, protocolMajor, protocolMinorMax)
	if !ok {
		// When versions can't be agreed, send a Failure HelloAck first and then close; don't just close.
		// A bare close leaves the client unable to tell "version incompatible" from "socket broken", and the
		// right reactions are opposite: the former shouldn't blindly reconnect, the latter should
		co.log.Warn("ipc: protocol version negotiation failed",
			"client_min_major", hello.GetMinProtocolMajor(),
			"client_max_major", hello.GetMaxProtocolMajor(),
			"client_max_minor", hello.GetMaxProtocolMinor(),
			"server_major", protocolMajor, "server_minor_max", protocolMinorMax)
		co.sendHelloFailure(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)
		return false
	}

	// Verify the claim rather than believe it. The Component the client self-reports in declared_component_id
	// is only a lead to be verified: it must be checked against kernel facts through the peer's cgroup -> unit -> Component,
	// and the handshake completes only if that passes. co.c is always a *net.UnixConn underneath
	//  (accepted from a UDS listener), used for SO_PEERPIDFD
	uc, _ := co.c.(*net.UnixConn)
	instance, err := co.s.verifyComponent(uc, co.caller, hello.GetDeclaredComponentId())
	if err != nil {
		// Both kinds of failure close with UNAUTHENTICATED, but audit distinguishes them:
		//  - errComponentMismatch (verification found a mismatch): potential impersonation, audited as a violation
		//  - others (capability gaps: Components not wired / the peer not in a managed cgroup / kernel too old):
		//    not audited as violations, so capability gaps don't flood security alerts
		if errors.Is(err, errComponentMismatch) {
			co.log.Warn("ipc: component impersonation suspected, rejecting handshake",
				"declared", hello.GetDeclaredComponentId(), "err", err)
			co.s.auditViolation(co.caller, err)
		} else {
			co.log.Warn("ipc: component not verifiable, rejecting handshake",
				"declared", hello.GetDeclaredComponentId(), "err", err)
		}
		co.sendHelloFailure(ipcv1.StatusCode_STATUS_CODE_UNAUTHENTICATED)
		return false
	}

	co.caller.ComponentID = instance.ComponentID
	co.componentType = instance.Type
	co.negMajor, co.negMinor = major, minor

	// HelloAck success: fill in the verified package_id/component_id (this doesn't grant an
	// identity; the identity was settled at SO_PEERCRED, and echoing it only lets the SDK spot configuration mismatches early),
	// and send the ConnectionLimits in effect for this connection (their wire projection)
	ack := &ipcv1.Envelope{Body: &ipcv1.Envelope_HelloAck{HelloAck: &ipcv1.HelloAck{
		Outcome: &ipcv1.HelloAck_Success{Success: &ipcv1.HelloAckSuccess{
			ProtocolMajor: major,
			ProtocolMinor: minor,
			PackageId:     co.caller.PackageID,
			ComponentId:   instance.ComponentID,
			Limits:        co.s.connectionLimits(),
		}},
	}}}
	if !co.enqueue(ack) {
		// Queue full / connection dead: nothing to audit (the audit due when enqueue fails has already been written),
		// a normal wrap-up
		co.log.Debug("ipc: enqueue HelloAck failed, closing")
		return false
	}

	co.phase = phaseReady
	co.log.Debug("ipc: handshake complete", "major", major, "minor", minor)
	return true
}

// handleReady dispatches by body after the handshake.
//
// Dispatch looks at direction first, then at whether it's implemented: the wrong direction is a violation outright, the right direction but unimplemented is a capability gap:
//   - Ping -> Pong, Pong accepted and ignored, Request -> Route -> Dispatch forwarding (handleRequest),
//     DispatchResult -> table lookup (handleDispatchResult) are all wired
//   - bodies in the nervud -> peer direction (responses/pushes/Dispatches sent to Services) sent by the peer ->
//     protocol violation, closed and audited
//   - bodies legal in the peer -> nervud direction but not implemented in this build -> closed and audited as UnsupportedBody
//
// None of the three are dropped silently, so protocol violations aren't masked by capability gaps
func (co *conn) handleReady(env *ipcv1.Envelope) bool {
	switch body := env.GetBody().(type) {
	case *ipcv1.Envelope_Ping:
		// Keep-alive: either side may send Ping, and the server answers Pong
		return co.handlePing(body.Ping)

	case *ipcv1.Envelope_Pong:
		// Keep-alive reply. The protocol lets either side send Ping, so nervud is also a legitimate Pong receiver.
		// nervud doesn't send Pings itself yet, so every Pong received is unexpected; but a Pong carries no request
		// and needs no reply, so it's accepted and ignored rather than closed as a violation: it isn't a body that only the server should
		// send. Once nervud sends Pings and records nonces, this can be tightened to "unmatched means violation"
		co.log.Debug("ipc: unsolicited Pong ignored")
		return true

	case *ipcv1.Envelope_Request:
		return co.handleRequest(body.Request)

	case *ipcv1.Envelope_DispatchResult:
		if !co.componentType.CanProvideInterfaces() {
			co.log.Warn("ipc: component type may not send DispatchResult, closing")
			co.s.auditViolation(co.caller, errUnexpectedBody)
			return false
		}
		return co.handleDispatchResult(body.DispatchResult)

	case *ipcv1.Envelope_ResolveEndpoint:
		return co.handleResolveEndpoint(env, body.ResolveEndpoint)

	case *ipcv1.Envelope_RegisterEndpoint:
		return co.handleRegisterEndpoint(env, body.RegisterEndpoint)

	case *ipcv1.Envelope_UnregisterEndpoint:
		return co.handleUnregisterEndpoint(env, body.UnregisterEndpoint)

	case *ipcv1.Envelope_AcquireControl:
		return co.handleAcquireControl(body.AcquireControl)

	case *ipcv1.Envelope_ReleaseControl:
		return co.handleReleaseControl(body.ReleaseControl)

	case *ipcv1.Envelope_LaunchComponent:
		return co.handleLaunchComponent(body.LaunchComponent)

	case *ipcv1.Envelope_Subscribe:
		return co.handleSubscribe(body.Subscribe)

	case *ipcv1.Envelope_Unsubscribe:
		return co.handleUnsubscribe(body.Unsubscribe)

	case *ipcv1.Envelope_BindEventScope:
		return co.handleBindEventScope(body.BindEventScope)

	case *ipcv1.Envelope_PublishEvent:
		return co.handlePublishEvent(body.PublishEvent)

	case *ipcv1.Envelope_Hello:
		// The handshake has completed; another Hello is an illegal handshake state
		co.log.Warn("ipc: duplicate Hello after handshake, closing")
		co.s.auditViolation(co.caller, errDuplicateHello)
		return false

	case *ipcv1.Envelope_HelloAck,
		*ipcv1.Envelope_Response,
		*ipcv1.Envelope_ResolveEndpointResult,
		*ipcv1.Envelope_RegisterEndpointResult,
		*ipcv1.Envelope_UnregisterEndpointResult,
		*ipcv1.Envelope_SubscribeResult,
		*ipcv1.Envelope_UnsubscribeResult,
		*ipcv1.Envelope_Event,
		*ipcv1.Envelope_SubscriptionClosed,
		*ipcv1.Envelope_EndpointDied,
		*ipcv1.Envelope_EndpointRevoked,
		*ipcv1.Envelope_Dispatch,
		*ipcv1.Envelope_CancelDispatch,
		*ipcv1.Envelope_AcquireControlResult,
		*ipcv1.Envelope_LaunchComponentResult,
		*ipcv1.Envelope_ReleaseControlResult:
		// All bodies in the nervud -> peer direction: responses (HelloAck/*Result/Response), pushes
		//  (Event/EndpointDied/EndpointRevoked/SubscriptionClosed), and the Dispatch/CancelDispatch nervud sends
		// to Services (only nervud may send these to Services).
		// nervud never receives them; receiving one is a protocol violation
		co.log.Warn("ipc: server-originated body received from peer, closing", "body", bodyName(env))
		co.s.auditViolation(co.caller, errUnexpectedBody)
		return false

	case *ipcv1.Envelope_Cancel:
		// Legal in the peer -> nervud direction, but not implemented in this build. Cancel needs a dedicated reply and
		// tracking of in-flight calls, and inventing a reply would make the caller think the cancel succeeded, so close and audit as
		// UnsupportedBody, kept apart from protocol violations so it neither pollutes the security signal nor misjudges future
		// integrations as attacks.
		//
		// Subscribe/Unsubscribe moved out of this group when internal/subscription landed.
		return co.unsupported(env)

	default:
		// parseEnvelope already blocks unknown bodies (proto3 puts oneof branches it doesn't know into unknown
		// fields, leaving Body nil). Getting here means the schema added a known body this switch doesn't
		// cover: fail closed by treating it as unimplemented, rather than letting it through silently
		return co.unsupported(env)
	}
}

// handlePing answers Pong, echoing the nonce (keep-alive)
func (co *conn) handlePing(ping *ipcv1.Ping) bool {
	pong := &ipcv1.Envelope{Body: &ipcv1.Envelope_Pong{Pong: &ipcv1.Pong{Nonce: ping.GetNonce()}}}
	return co.enqueue(pong)
}

// handleResolveEndpoint forwards to the injected EndpointResolver. When the dependency isn't injected it returns
// unsupported, so IPC never guesses binding results without an authoritative router
func (co *conn) handleResolveEndpoint(env *ipcv1.Envelope, req *ipcv1.ResolveEndpoint) bool {
	if co.s.endpoints == nil {
		return co.unsupported(env)
	}
	result := co.s.endpoints.ResolveEndpoint(co, co.caller, req)
	return co.writeResultEnvelope(&ipcv1.Envelope{
		Body: &ipcv1.Envelope_ResolveEndpointResult{ResolveEndpointResult: result},
	})
}

// handleRegisterEndpoint forwards to internal/endpoint (with the same nil degradation as above)
func (co *conn) handleRegisterEndpoint(env *ipcv1.Envelope, req *ipcv1.RegisterEndpoint) bool {
	if co.s.endpoints == nil {
		return co.unsupported(env)
	}
	if !co.componentType.CanProvideInterfaces() {
		result := &ipcv1.RegisterEndpointResult{
			RequestId: req.GetRequestId(),
			Outcome: &ipcv1.RegisterEndpointResult_Failure{Failure: &ipcv1.Failure{
				Code: ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED,
			}},
		}
		return co.writeResultEnvelope(&ipcv1.Envelope{
			Body: &ipcv1.Envelope_RegisterEndpointResult{RegisterEndpointResult: result},
		})
	}
	result := co.s.endpoints.RegisterEndpoint(co, co.caller, req)
	return co.writeResultEnvelope(&ipcv1.Envelope{
		Body: &ipcv1.Envelope_RegisterEndpointResult{RegisterEndpointResult: result},
	})
}

// handleUnregisterEndpoint forwards to internal/endpoint (with the same nil degradation as above)
func (co *conn) handleUnregisterEndpoint(env *ipcv1.Envelope, req *ipcv1.UnregisterEndpoint) bool {
	if co.s.endpoints == nil {
		return co.unsupported(env)
	}
	if !co.componentType.CanProvideInterfaces() {
		result := &ipcv1.UnregisterEndpointResult{
			RequestId: req.GetRequestId(),
			Outcome: &ipcv1.UnregisterEndpointResult_Failure{Failure: &ipcv1.Failure{
				Code: ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED,
			}},
		}
		return co.writeResultEnvelope(&ipcv1.Envelope{
			Body: &ipcv1.Envelope_UnregisterEndpointResult{UnregisterEndpointResult: result},
		})
	}
	result := co.s.endpoints.UnregisterEndpoint(co, req)
	if result.GetSuccess() != nil {
		co.s.revokeEndpoint(co, req.GetEndpointId(), 0)
	}
	return co.writeResultEnvelope(&ipcv1.Envelope{
		Body: &ipcv1.Envelope_UnregisterEndpointResult{UnregisterEndpointResult: result},
	})
}

// writeResultEnvelope queues a response/result Envelope in the outbox; when the queue is full/closed
// the connection is closed (handled inside enqueue)
func (co *conn) writeResultEnvelope(env *ipcv1.Envelope) bool {
	return co.enqueue(env)
}

// handleRequest processes one Request (the request pipeline: Route -> Dispatch).
//
// When co.s.endpoints is nil it keeps the existing degraded behaviour: after checking request_id it always returns UNAVAILABLE,
// neither dropping silently nor closing the connection bare. When non-nil, it first looks up Route (which really calls
// permission.Allowed internally), then on success turns the Request into a Dispatch queued in the target Service connection's
// outbox, records the related state in the route table, and returns true. This function no longer produces the final
// Response in place; the real Response arrives through this connection's outbox at some later moment from one of handleDispatchResult / the timeout sweeper / the target connection
// disconnecting (dispatch.go)
func (co *conn) handleRequest(req *ipcv1.Request) bool {
	// request_id 0 is permanently reserved (legal requests start at 1). Close as a protocol violation
	// before producing any Response: replying with a request_id=0 Response would acknowledge a correlation key that shouldn't
	// exist, and the SDK side never registers anything pending for 0
	if req.GetRequestId() == 0 {
		co.log.Warn("ipc: Request with reserved request_id 0, closing")
		co.s.auditViolation(co.caller, errZeroRequestID)
		return false
	}

	if co.s.endpoints == nil {
		// The degraded path is unchanged: always UNAVAILABLE when Endpoints isn't wired, neither dropping silently nor closing bare
		return co.enqueue(responseEnvelope(failureResponse(
			req.GetRequestId(), ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE)))
	}

	// Sample the revocation epoch before consulting endpoint/catalog authority.
	// Publishing the route later is conditional on this value remaining stable.
	dispatchEpoch := co.s.dispatch.snapshotEpoch()
	route, rerr := co.s.endpoints.Route(co, req.GetEndpointId(), req.GetMethodId())
	if rerr.Code != ipcv1.StatusCode_STATUS_CODE_UNSPECIFIED {
		// This must be logged: Route's nearly ten failure paths all look the same on the wire,
		// STATUS_CODE_NOT_FOUND, and the message the caller sees points at no specific cause. Without logging,
		// a "call failed" leaves no trace at all on the kernel side; you can only read the code and guess which path it was,
		// and a wrong guess costs a whole round of compile, flash and reproduce.
		//
		// It goes to the log rather than audit: it isn't a security decision (the real refusals are the two allowedAt ones,
		// which already write audit), but diagnostic information, and possibly frequent.
		co.log.Warn("ipc: route failed",
			"pkg", co.caller.PackageID,
			"comp", co.caller.ComponentID,
			"endpoint_id", req.GetEndpointId(),
			"method_id", req.GetMethodId(),
			"code", rerr.Code.String(),
			"reason", rerr.Reason)
		return co.enqueue(responseEnvelope(failureResponse(req.GetRequestId(), rerr.Code)))
	}
	if err := protocheck.GateSupport(route.Method.Meta, co.s.operations != nil); err != nil {
		co.s.recordMethodGateFailure(co.caller, route, req.GetMethodId(), err)
		return co.enqueue(responseEnvelope(failureResponse(
			req.GetRequestId(), methodGateCode(err))))
	}
	// Methods needing user confirmation: only the system confirmation UI may call them itself, having just asked the user.
	// Other callers go through it to call on their behalf (see protocheck.GateUserConfirmation)
	if err := protocheck.GateUserConfirmation(
		route.Method.Meta, co.callerIsConfirmationUI()); err != nil {
		co.s.recordMethodGateFailure(co.caller, route, req.GetMethodId(), err)
		return co.enqueue(responseEnvelope(failureResponse(
			req.GetRequestId(), methodGateCode(err))))
	}
	payload, err := protocheck.ValidateRequest(
		route.Method.Meta, route.Method.Request, req.GetPayload())
	if err != nil {
		co.s.recordMethodGateFailure(co.caller, route, req.GetMethodId(), err)
		return co.enqueue(responseEnvelope(failureResponse(
			req.GetRequestId(), requestValidationCode(err))))
	}
	requiresControl := methodRequiresControl(route.Method.Meta)
	var leaseProof control.LeaseProof
	if requiresControl {
		if route.ResourceHandle == "" || co.s.leases == nil {
			return co.enqueue(responseEnvelope(failureResponse(
				req.GetRequestId(), ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)))
		}
		leaseProof, err = co.s.leases.CheckResource(
			co.connID, route.ResourceHandle, route.ResourceGeneration,
		)
		if err != nil {
			return co.enqueue(responseEnvelope(failureResponse(
				req.GetRequestId(), ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)))
		}
	}
	if code := co.reserveRequest(req.GetRequestId(), len(payload)); code != ipcv1.StatusCode_STATUS_CODE_UNSPECIFIED {
		return co.enqueue(responseEnvelope(failureResponse(req.GetRequestId(), code)))
	}

	now := time.Now()
	deadline := now.Add(methodTimeout(req.GetTimeoutMs(), route.Method.Meta))
	if requiresControl {
		if !leaseProof.Deadline.After(now) {
			co.releaseRequest(req.GetRequestId())
			return co.enqueue(responseEnvelope(failureResponse(
				req.GetRequestId(), ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)))
		}
		if leaseProof.Deadline.Before(deadline) {
			deadline = leaseProof.Deadline
		}
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		co.releaseRequest(req.GetRequestId())
		return co.enqueue(responseEnvelope(failureResponse(
			req.GetRequestId(), ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED)))
	}

	// A built-in endpoint: the target is nervud itself, so run it in place without a Dispatch.
	//
	// Builtin must be checked before TargetConn. Built-ins have no connection, so TargetConn is always nil,
	// and the other order would treat it as "routed successfully but with no forwarding target" and return UNAVAILABLE.
	if route.Builtin != nil {
		return co.handleBuiltinRequest(req, route, payload, deadline)
	}

	target, ok := route.TargetConn.(*conn)
	if !ok || target == nil {
		// Route reported success, but there's no real connection to forward to (a wiring gap, or a test double deliberately
		// left empty). There's no workaround: treat it as unavailable, rather than pretending the forward succeeded
		co.releaseRequest(req.GetRequestId())
		return co.enqueue(responseEnvelope(failureResponse(
			req.GetRequestId(), ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE)))
	}
	// Every Dispatch carries an ExecutionContext, unconditionally.
	//
	// v1 decided whether to include it by the negotiated minor, with an extra "refuse if the minor is too low" branch on control methods.
	// v2 includes it from day one, and both branches were removed together: a field that's "sometimes there and sometimes not"
	// forces Providers to write two code paths, and a missing one only shows up with particular peer versions.
	if (route.ResourceHandle == "") != (route.ResourceGeneration == 0) {
		co.releaseRequest(req.GetRequestId())
		return co.enqueue(responseEnvelope(failureResponse(
			req.GetRequestId(), ipcv1.StatusCode_STATUS_CODE_INTERNAL)))
	}
	deadlineNanos, deadlineErr := co.s.monotonicDeadlineNanos(deadline)
	if deadlineErr != nil {
		co.log.Error("ipc: project Dispatch monotonic deadline", "err", deadlineErr)
		co.releaseRequest(req.GetRequestId())
		return co.enqueue(responseEnvelope(failureResponse(
			req.GetRequestId(), ipcv1.StatusCode_STATUS_CODE_INTERNAL)))
	}
	execution := &ipcv1.ExecutionContext{
		DeadlineNanos:      deadlineNanos,
		ResourceHandle:     route.ResourceHandle,
		ResourceGeneration: route.ResourceGeneration,
	}
	if requiresControl {
		leaseID, found := co.wireLeaseHandle(leaseProof.ID)
		controllerClass, validClass := classToWire(leaseProof.Class)
		if !found || !validClass || leaseProof.Epoch == 0 ||
			leaseProof.Resource != route.ResourceHandle ||
			leaseProof.ResourceGeneration != route.ResourceGeneration {
			co.releaseRequest(req.GetRequestId())
			return co.enqueue(responseEnvelope(failureResponse(
				req.GetRequestId(), ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)))
		}
		execution.LeaseId = leaseID
		execution.ControllerClass = controllerClass
		execution.MotionEpoch = leaseProof.Epoch
	}

	// The Operation is created before the Dispatch. This order means that when the Provider receives the Dispatch the
	// operation_id is already valid, so it can answer DispatchResult{ACCEPTED} straight away;
	// that code requires "the Operation must already exist".
	//
	// The other way round (Dispatch first, then create) leaves a window: the Provider has started moving while
	// the operation doesn't exist yet, and its first ReportProgress would be refused.
	var operationID uint64
	if route.Method.Meta.GetReturnsOperation() {
		id, code := co.createOperation(route, target, deadline, execution)
		if code != ipcv1.StatusCode_STATUS_CODE_ACCEPTED {
			co.releaseRequest(req.GetRequestId())
			return co.enqueue(responseEnvelope(failureResponse(req.GetRequestId(), code)))
		}
		operationID = id
		execution.OperationId = id
	}

	routeID, publishStatus := co.s.dispatch.publishDispatchAtEpoch(
		dispatchEpoch,
		co,
		req.GetRequestId(),
		target,
		deadline,
		route,
		req.GetMethodId(),
		remainingMillis(remaining),
		payload,
		callerContext(co.caller, route.RequiredPermissions),
		execution,
	)
	// All three failure paths must converge the operation already created.
	//
	// The consequence of not converging: a PENDING operation hangs there, and the Provider never
	// received the Dispatch, so nobody will ever Accept or Complete it. It would stay
	// until its deadline expired, while the caller already got a failure response and has no idea it exists.
	failOperation := func(code ipcv1.StatusCode) {
		if operationID == 0 || co.s.operations == nil {
			return
		}
		if err := co.s.operations.Fail(operationID, code, nil); err != nil {
			co.log.Warn("ipc: converge undispatched operation",
				"operation_id", operationID, "err", err)
		}
	}

	switch publishStatus {
	case dispatchPublishEpochChanged:
		failOperation(ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE)
		co.releaseRequest(req.GetRequestId())
		return co.enqueue(responseEnvelope(failureResponse(
			req.GetRequestId(), ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE)))
	case dispatchPublishTargetUnavailable:
		// The table already removed the unpublished route and closed its token.
		// Do connection teardown only after releasing dispatch.mu.
		if target.outbox != nil {
			target.closeAsSlowConsumer()
		}
		co.s.transfer.CloseRoute(routeID)
		failOperation(ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE)
		co.releaseRequest(req.GetRequestId())
		return co.enqueue(responseEnvelope(failureResponse(
			req.GetRequestId(), ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE)))
	case dispatchPublishSequenceExhausted:
		failOperation(ipcv1.StatusCode_STATUS_CODE_INTERNAL)
		co.releaseRequest(req.GetRequestId())
		return co.enqueue(responseEnvelope(failureResponse(
			req.GetRequestId(), ipcv1.StatusCode_STATUS_CODE_INTERNAL)))
	case dispatchPublishOK:
		return true
	default:
		panic("ipc: unknown dispatch publish status")
	}
}

// createOperation creates the operation for one returns_operation call.
//
// Returning ACCEPTED means it was created; other codes go back to the caller unchanged: Create's precondition failures
// (invalid resource, expired lease, stale epoch) are already distinguishable reasons and need no extra wrapping.
func (co *conn) createOperation(
	route endpoint.RouteInfo,
	target *conn,
	deadline time.Time,
	execution *ipcv1.ExecutionContext,
) (uint64, ipcv1.StatusCode) {
	if co.s.operations == nil {
		// GateSupport already blocked this; getting here means the wiring disagrees in two places.
		return 0, ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE
	}

	// Resource set: in v1 an operation binds one resource (see package operation).
	// An interface without a resource gives an empty set. Create accepts that for
	// non-motion long tasks (no lease), such as package install, and rejects it
	// for motion, which must name the resource its lease covers.
	var resources []string
	if route.ResourceHandle != "" {
		resources = []string{route.ResourceHandle}
	}

	return co.s.operations.Create(
		co,     // caller connection: converged on disconnect
		target, // executing connection: the ownership credential on the reporting side
		co.caller,
		operation.OriginBinding{
			InterfaceID: route.InterfaceID,
			IfaceMajor:  route.InterfaceMajor,
			IfaceMinor:  route.InterfaceMinor,
			MethodID:    route.Method.MethodID,
			SchemaHash:  route.InterfaceSchemaHash,
		},
		resources,
		execution.GetLeaseId(),
		execution.GetMotionEpoch(),
		deadline,
	)
}

func methodRequiresControl(meta *ipcv1.MethodMeta) bool {
	return meta != nil && (meta.GetRequiresControlLease() || meta.GetIsMotion())
}

// handleDispatchResult handles the DispatchResult a Service sends back: only a matching table lookup
// produces the final Response for the corresponding caller. An unknown/finished route_id is an expected, benign
// race (the sweeper or another result finished first, or disconnect cleanup already removed it): drop it but keep the connection
// alive. A target mismatch (the route_id exists but the connection sending the result isn't the registered target) has no legitimate
// explanation, so close as a protocol violation
func (co *conn) handleDispatchResult(dr *ipcv1.DispatchResult) bool {
	e, status := co.s.dispatch.complete(dr.GetRouteId(), co, time.Now())
	switch status {
	case completeMismatch:
		co.log.Warn("ipc: DispatchResult route_id targets a different connection, closing",
			"route_id", dr.GetRouteId())
		co.s.auditViolation(co.caller, errDispatchResultMismatch)
		return false
	case completeNotFound:
		co.s.auditDispatchRace(co.caller.String(), dr.GetRouteId())
		return true
	case completeExpired:
		co.s.transfer.CloseRoute(e.routeID)
		return resolveRoute(e, failureResponse(
			e.sourceRequestID, ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED))
	case completeOK:
	}

	resp, valid := co.s.validateDispatchResult(e, dr)
	if valid {
		co.s.convergeSynchronousOperation(e, resp)
	}
	sourceOK := resolveRoute(e, resp)
	if !valid {
		co.log.Warn("ipc: Provider violated method result contract, closing",
			"route_id", dr.GetRouteId(),
			"interface", e.route.InterfaceID,
			"method_id", e.methodID)
		return false
	}
	return sourceOK
}

// methodTimeout applies the method-declared default and maximum, then the
// connection-wide kernel ceiling. Zero never means unlimited.
func methodTimeout(requested uint32, meta *ipcv1.MethodMeta) time.Duration {
	defaultMS := uint32(defaultMethodTimeoutMs)
	maxMS := uint32(maxMethodTimeoutMs)
	if meta != nil {
		if meta.GetDefaultTimeoutMs() != 0 {
			defaultMS = meta.GetDefaultTimeoutMs()
		}
		if declared := meta.GetMaxTimeoutMs(); declared != 0 && declared < maxMS {
			maxMS = declared
		}
	}
	if defaultMS > maxMS {
		defaultMS = maxMS
	}
	if requested == 0 {
		requested = defaultMS
	}
	if requested > maxMS {
		requested = maxMS
	}
	if requested == 0 {
		requested = 1
	}
	return time.Duration(requested) * time.Millisecond
}

// callerContext projects the identity.Caller the kernel has already verified into a CallerContext that can be passed out to Services
// (Services can read it but can't use it to bypass the Policy nervud already enforced, still less
// invent identities or permission decisions). granted contains only the permissions endpoint.Route checked one by one for this call
// on the same catalog snapshot, not the Package's full grant set.
func callerContext(c identity.Caller, granted []string) *ipcv1.CallerContext {
	return &ipcv1.CallerContext{
		PackageId:          c.PackageID,
		ComponentId:        c.ComponentID,
		Uid:                c.UID,
		Gid:                c.GID,
		Pid:                c.PID,
		TrustProfile:       trustProfileWire(c.Trust),
		GrantedPermissions: append([]string(nil), granted...),
	}
}

// trustProfileWire maps identity.TrustProfile to ipcv1.TrustProfile explicitly.
// Their values currently correspond one to one, but an explicit switch is used rather than a bare type conversion: the two packages'
// enums evolve independently, and a bare conversion would silently mis-map without error when either adds or reorders values
func trustProfileWire(t identity.TrustProfile) ipcv1.TrustProfile {
	switch t {
	case identity.TrustOrdinary:
		return ipcv1.TrustProfile_TRUST_PROFILE_ORDINARY
	case identity.TrustOEM:
		return ipcv1.TrustProfile_TRUST_PROFILE_OEM
	case identity.TrustPlatform:
		return ipcv1.TrustProfile_TRUST_PROFILE_PLATFORM
	default:
		return ipcv1.TrustProfile_TRUST_PROFILE_UNSPECIFIED
	}
}

func internalResponse(reqID uint64) *ipcv1.Response {
	return failureResponse(reqID, ipcv1.StatusCode_STATUS_CODE_INTERNAL)
}

// failureResponse/responseEnvelope are two shared helpers for building final Responses,
// so each failure branch doesn't repeat the outcome assembly boilerplate
func failureResponse(reqID uint64, code ipcv1.StatusCode) *ipcv1.Response {
	return &ipcv1.Response{RequestId: reqID, Outcome: &ipcv1.Response_Failure{
		Failure: &ipcv1.Failure{Code: code},
	}}
}

func responseEnvelope(resp *ipcv1.Response) *ipcv1.Envelope {
	return &ipcv1.Envelope{Body: &ipcv1.Envelope_Response{Response: resp}}
}

// unsupported closes the connection and audits it as UnsupportedBody
func (co *conn) unsupported(env *ipcv1.Envelope) bool {
	co.log.Warn("ipc: unsupported body in this build, closing", "body", bodyName(env))
	co.s.auditUnsupported(co.caller, errUnsupportedBody)
	return false
}

// sendHelloFailure sends a HelloAck carrying a Failure (it doesn't close the connection; the caller decides that)
func (co *conn) sendHelloFailure(code ipcv1.StatusCode) {
	fail := &ipcv1.Envelope{Body: &ipcv1.Envelope_HelloAck{HelloAck: &ipcv1.HelloAck{
		Outcome: &ipcv1.HelloAck_Failure{Failure: &ipcv1.Failure{Code: code}},
	}}}
	// The caller (handleHandshake) returns false and closes the connection right after, whether or not enqueue succeeded,
	// so the return value needs no extra handling here
	co.enqueue(fail)
}

// writeEnvelope serialises and writes out an Envelope, with a write deadline.
//
// Outbound Envelopes are always labelled with the protocol version this connection negotiated. Writing goes through the conn's own buffered writer,
// so length + body merge into one syscall (as frame.go recommends)
func (co *conn) writeEnvelope(env *ipcv1.Envelope) error {
	env.ProtocolMajor = co.negMajor
	env.ProtocolMinor = co.negMinor

	b, err := proto.Marshal(env)
	if err != nil {
		// An unserialisable Envelope built by this side is a nervud bug and isn't sent out
		return fmt.Errorf("ipc: marshal outbound envelope: %w", err)
	}

	// Writes must have a finite deadline, otherwise a slow consumer that never reads could hang
	// the frame pump goroutine on Write forever. Control frames are all small, so FrameBodyTimeout
	// is reused as the write window, with the same meaning: bytes of an announced length must land quickly
	if err := co.c.SetWriteDeadline(time.Now().Add(co.s.limits.FrameBodyTimeout)); err != nil {
		return err
	}
	if err := WriteFrame(co.w, b); err != nil {
		return err
	}
	return co.w.Flush()
}

// negotiateVersion intersects the versions the server supports (srvMajor, with srvMinorMax as the minor ceiling for that major)
// with the range the client's Hello declares, and picks the (major, minor) that takes effect immediately. With no intersection it returns
// ok=false.
//
// srvMajor/srvMinorMax are parameters rather than read from constants directly, so the out-of-range logic can be unit-tested properly
func negotiateVersion(h *ipcv1.Hello, srvMajor, srvMinorMax uint32) (major, minor uint32, ok bool) {
	// The server implements only the one major srvMajor, which must fall inside the client's closed range
	// [min_protocol_major, max_protocol_major], or there's nothing to negotiate. When the range itself is inverted
	//  (max < min) that check naturally fails and also lands in "no intersection"
	if h.GetMinProtocolMajor() > srvMajor || h.GetMaxProtocolMajor() < srvMajor {
		return 0, 0, false
	}
	major = srvMajor

	if h.GetMaxProtocolMajor() == srvMajor {
		// The chosen major is exactly the client's highest major: Hello's max_protocol_minor is
		// the client's minor ceiling for this major, so take the smaller of it and the server's ceiling
		minor = srvMinorMax
		if cm := h.GetMaxProtocolMinor(); cm < minor {
			minor = cm
		}
		return major, minor, true
	}

	// The chosen major is lower than the client's highest: Hello only declared a minor for the highest major, and has no minor
	// information for lower majors. Minor 0 is the guaranteed floor any implementation offers for a major, so
	// only 0 can be given: you can't assume that a client supporting a higher major also supports higher
	// minors of ours, which would pick a version the client never declared support for once the server's minor rises
	return major, 0, true
}

// bodyName returns the concrete type name of the Envelope body, only for diagnostic logs
func bodyName(env *ipcv1.Envelope) string {
	return fmt.Sprintf("%T", env.GetBody())
}

// handleBuiltinRequest runs one built-in endpoint call in place and returns a Response.
//
// Two differences from the forwarding path:
//
//  1. It takes no route table entry. Built-ins have no "waiting for the peer's result": once run, there's a result,
//     and no late, duplicate or post-revocation DispatchResult can arrive.
//  2. Panics must be caught on the spot. Built-in handlers run inside the kernel process, and a panic would take down
//     the whole of nervud, which is far worse than any single call failing. The forwarding path doesn't have this risk,
//     because Providers run in other processes.
func (co *conn) handleBuiltinRequest(
	req *ipcv1.Request,
	route endpoint.RouteInfo,
	payload []byte,
	deadline time.Time,
) bool {
	// A goroutine rather than running synchronously: the read loop mustn't be stuck behind a slow handler, or later
	// Pings/Cancels on this connection couldn't be read, and the client would wrongly conclude it's lost.
	go func() {
		defer co.releaseRequest(req.GetRequestId())
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()

		resultCh := make(chan endpoint.BuiltinResult, 1)
		go func() {
			resultCh <- co.s.callBuiltin(endpoint.BuiltinCall{
				Context:  ctx,
				Conn:     co,
				Caller:   co.caller,
				MethodID: req.GetMethodId(),
				Payload:  payload,
			}, route.Builtin)
		}()

		var result endpoint.BuiltinResult
		select {
		case result = <-resultCh:
		case <-ctx.Done():
			co.enqueue(responseEnvelope(failureResponse(
				req.GetRequestId(), ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED)))
			return
		}

		resp, valid := co.s.validateBuiltinResult(req.GetRequestId(), route, result)
		if !valid {
			co.s.auditor.Record(context.Background(), audit.Event{
				Action:  "ipc.BuiltinContractViolation",
				Subject: co.caller.String(),
				Denied:  true,
				Detail: fmt.Sprintf(
					"interface=%s major=%d method_id=%d",
					route.InterfaceID, route.InterfaceMajor, req.GetMethodId()),
			})
		}
		co.enqueue(responseEnvelope(resp))
	}()
	return true
}

// callBuiltin runs the handler and turns a panic into INTERNAL.
//
// Built-in handlers run inside the kernel process, and a panic would take down the whole of nervud: every App's
// connections, in-flight calls and the Safety supervision chain would vanish together. By comparison, letting this one call fail is
// clearly the smaller cost.
func (s *Server) callBuiltin(
	call endpoint.BuiltinCall,
	handler endpoint.BuiltinHandler,
) (result endpoint.BuiltinResult) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("ipc: builtin handler panicked",
				"method_id", call.MethodID,
				"caller", call.Caller.PackageID,
				"panic", r)
			result = endpoint.BuiltinResult{
				Code: ipcv1.StatusCode_STATUS_CODE_INTERNAL,
			}
		}
	}()
	return handler(call)
}

func remainingMillis(remaining time.Duration) uint32 {
	if remaining <= 0 {
		return 0
	}
	ms := (remaining + time.Millisecond - 1) / time.Millisecond
	if ms > time.Duration(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(ms)
}

// This file implements the route table: when nervud forwards a Request to a Service connection, it uses a
// route_id to record the source/target connections, an authoritative snapshot of the request and method, the deadline, the registration
// generation and the Transfer RouteToken, consumed together by normal results, revocation, timeouts and disconnects.
//
// The core of the design: whoever successfully deletes this entry under the table lock is the only producer of this call's final
// Response. All three paths use the same "look up and delete" semantics, which naturally guarantees each Request
// at most one final Response, so no extra atomic completion flag is needed.
//
// Revocation and publishing share the table lock and the epoch: a request samples the epoch before looking up endpoint/catalog, and only succeeds
// if the epoch is still the same when publishing the route. Revocation increments the epoch first, then removes entries and closes RouteTokens,
// so a revocation between lookup and publishing can't leave an old route that could BeginTransfer afterwards.
package ipc

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/endpoint"
	"github.com/nervus-os/nervud/internal/transfer"
)

// routeEntry is the state of one in-flight Dispatch
type routeEntry struct {
	routeID         uint64
	source          *conn
	sourceRequestID uint64
	target          *conn
	deadline        time.Time
	methodID        uint32
	route           endpoint.RouteInfo
	execution       *ipcv1.ExecutionContext
	token           *transfer.RouteToken
}

// completeStatus classifies the result of dispatchTable.complete
type completeStatus int

const (
	// completeOK: found and the target matches; the entry has been deleted and the caller is the only completer
	completeOK completeStatus = iota
	// completeNotFound: the route_id doesn't exist. A benign race (the sweeper / another result /
	// disconnect cleanup finished first), or a forged route_id; the two can't be told apart here
	completeNotFound
	// completeMismatch: the route_id exists, but the connection sending the result isn't the registered target connection.
	// There's no legitimate explanation: no Service is ever told a route_id pointing at another connection
	completeMismatch
	// completeExpired: target matched, but the result arrived after the exact
	// route deadline. The entry is removed and must resolve as DEADLINE_EXCEEDED.
	completeExpired
)

// dispatchTable is the single authority for route_id -> in-flight Dispatch
type dispatchTable struct {
	mu      sync.Mutex
	entries map[uint64]*routeEntry
	// epoch changes at every authorization revocation. Request routing samples
	// it before consulting endpoint/catalog state and must present the same
	// value when publishing a route, closing the lookup-to-publish race.
	epoch uint64

	// beforePublishEnqueue is only populated by package tests to pause the
	// publish critical section at the former revoke-before-Dispatch race.
	beforePublishEnqueue func()

	// nextID starts at 1, and 0 counts as never assigned, echoing the existing request_id convention
	// (route_id's own proto comment doesn't explicitly reserve 0; this is a convention this implementation introduces)
	nextID atomic.Uint64
	// nextCommandSequence is global within this kernel boot. A global sequence
	// is also strictly monotonic for every resource/generation subsequence while
	// avoiding an unbounded map of retired catalog generations.
	nextCommandSequence uint64
}

type dispatchPublishStatus uint8

const (
	dispatchPublishOK dispatchPublishStatus = iota
	dispatchPublishEpochChanged
	dispatchPublishTargetUnavailable
	dispatchPublishSequenceExhausted
)

func newDispatchTable() *dispatchTable {
	return &dispatchTable{entries: make(map[uint64]*routeEntry)}
}

// create allocates a new route_id and registers the entry
func (t *dispatchTable) create(
	source *conn,
	sourceReqID uint64,
	target *conn,
	deadline time.Time,
	route endpoint.RouteInfo,
	methodID uint32,
) uint64 {
	epoch := t.snapshotEpoch()
	id, _ := t.createAtEpoch(
		epoch, source, sourceReqID, target, deadline, route, methodID)
	return id
}

// createAtEpoch publishes a route only if no authorization revocation raced
// with the endpoint/catalog checks that preceded it.
func (t *dispatchTable) createAtEpoch(
	expectedEpoch uint64,
	source *conn,
	sourceReqID uint64,
	target *conn,
	deadline time.Time,
	route endpoint.RouteInfo,
	methodID uint32,
) (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.epoch != expectedEpoch {
		return 0, false
	}
	id := t.nextID.Add(1)
	t.entries[id] = &routeEntry{
		routeID:         id,
		source:          source,
		sourceRequestID: sourceReqID,
		target:          target,
		deadline:        deadline,
		methodID:        methodID,
		route:           route,
		token:           transfer.NewRouteToken(),
	}
	return id, true
}

// publishDispatchAtEpoch atomically publishes route authority and appends the
// corresponding Dispatch to the Provider's in-memory outbox. revoke uses the
// same table lock, so the Provider can observe only one of these orderings:
//
//   - Dispatch is queued first, then a matching CancelDispatch; or
//   - revocation advances epoch first and no Dispatch is queued.
//
// target.outbox.push is non-blocking and performs no socket I/O. Slow-consumer
// teardown is deliberately left to the caller after this lock is released.
func (t *dispatchTable) publishDispatchAtEpoch(
	expectedEpoch uint64,
	source *conn,
	sourceReqID uint64,
	target *conn,
	deadline time.Time,
	route endpoint.RouteInfo,
	methodID uint32,
	remainingMS uint32,
	payload []byte,
	caller *ipcv1.CallerContext,
	execution *ipcv1.ExecutionContext,
) (uint64, dispatchPublishStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.epoch != expectedEpoch {
		return 0, dispatchPublishEpochChanged
	}

	var executionSnapshot *ipcv1.ExecutionContext
	if execution != nil {
		// Copied field by field, not proto.Clone: this snapshot is frozen into the route table,
		// and listing the fields makes "added a field but forgot to carry it over" visible in code review.
		//
		// The cost is remembering to change this when adding fields; forgetting shows up as the Provider receiving an
		// ExecutionContext missing a field, with neither side reporting an error.
		executionSnapshot = &ipcv1.ExecutionContext{
			LeaseId:            execution.GetLeaseId(),
			ControllerClass:    execution.GetControllerClass(),
			MotionEpoch:        execution.GetMotionEpoch(),
			DeadlineNanos:      execution.GetDeadlineNanos(),
			ResourceHandle:     execution.GetResourceHandle(),
			ResourceGeneration: execution.GetResourceGeneration(),
			OperationId:        execution.GetOperationId(),
		}
		if executionSnapshot.GetLeaseId() != 0 {
			if t.nextCommandSequence == ^uint64(0) {
				return 0, dispatchPublishSequenceExhausted
			}
			t.nextCommandSequence++
			executionSnapshot.CommandSequence = t.nextCommandSequence
		}
	}

	id := t.nextID.Add(1)
	entry := &routeEntry{
		routeID:         id,
		source:          source,
		sourceRequestID: sourceReqID,
		target:          target,
		deadline:        deadline,
		methodID:        methodID,
		route:           route,
		execution:       executionSnapshot,
		token:           transfer.NewRouteToken(),
	}
	t.entries[id] = entry
	if t.beforePublishEnqueue != nil {
		t.beforePublishEnqueue()
	}
	if target == nil || target.outbox == nil || !target.outbox.push(&ipcv1.Envelope{
		Body: &ipcv1.Envelope_Dispatch{Dispatch: &ipcv1.Dispatch{
			RouteId:          id,
			EndpointId:       route.ServiceEndpointID,
			MethodId:         methodID,
			RemainingMs:      remainingMS,
			Payload:          payload,
			Caller:           caller,
			ExecutionContext: executionSnapshot,
		}},
	}) {
		delete(t.entries, id)
		entry.token.Close()
		return id, dispatchPublishTargetUnavailable
	}
	return id, dispatchPublishOK
}

func (t *dispatchTable) snapshotEpoch() uint64 {
	t.mu.Lock()
	epoch := t.epoch
	t.mu.Unlock()
	return epoch
}

// revoke closes matching route tokens under the same lock that publishes
// routes. Incrementing epoch even when no entry matches rejects a Route lookup
// that started before this revocation but has not published yet.
func (t *dispatchTable) revoke(match func(*routeEntry) bool) []*routeEntry {
	t.mu.Lock()
	t.epoch++
	var revoked []*routeEntry
	for id, entry := range t.entries {
		if !match(entry) {
			continue
		}
		delete(t.entries, id)
		entry.token.Close()
		revoked = append(revoked, entry)
	}
	t.mu.Unlock()
	return revoked
}

func (t *dispatchTable) revokeEndpoint(
	provider *conn,
	endpointID, generation uint64,
) []*routeEntry {
	return t.revoke(func(entry *routeEntry) bool {
		return entry.target == provider &&
			entry.route.ServiceEndpointID == endpointID &&
			(generation == 0 || entry.route.RegistrationGeneration == generation)
	})
}

func (t *dispatchTable) revokePackage(packageID string) []*routeEntry {
	return t.revoke(func(entry *routeEntry) bool {
		return entry.source.caller.PackageID == packageID ||
			entry.target.caller.PackageID == packageID
	})
}

func (t *dispatchTable) revokePermission(packageID, permission string) []*routeEntry {
	return t.revoke(func(entry *routeEntry) bool {
		return entry.source.caller.PackageID == packageID &&
			slices.Contains(entry.route.RequiredPermissions, permission)
	})
}

func (t *dispatchTable) revokeResource(resource string, generation uint64) []*routeEntry {
	return t.revoke(func(entry *routeEntry) bool {
		return entry.route.ResourceHandle == resource &&
			entry.route.ResourceGeneration == generation
	})
}

func (t *dispatchTable) revokeControl(
	caller transfer.ConnID,
	resource string,
) []*routeEntry {
	return t.revoke(func(entry *routeEntry) bool {
		return transfer.ConnID(entry.source.connID) == caller &&
			entry.route.ResourceHandle == resource &&
			methodRequiresControl(entry.route.Method.Meta)
	})
}

// complete tries to complete a route, requiring the result to come from the registered target connection itself
func (t *dispatchTable) complete(
	routeID uint64,
	target *conn,
	nowValues ...time.Time,
) (*routeEntry, completeStatus) {
	now := time.Now()
	if len(nowValues) != 0 {
		now = nowValues[0]
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.entries[routeID]
	if !ok {
		return nil, completeNotFound
	}
	if e.target != target {
		return nil, completeMismatch
	}
	delete(t.entries, routeID)
	e.token.Close()
	if !now.Before(e.deadline) {
		return e, completeExpired
	}
	return e, completeOK
}

// completeAny completes a route unconditionally (without checking the source), for the sweeper and
// handleRequest's own "found the target connection dead right after creating it" path. Both have already
// confirmed through other channels that this route should end, and don't need to check who is ending it
func (t *dispatchTable) completeAny(routeID uint64) (*routeEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.entries[routeID]
	if ok {
		delete(t.entries, routeID)
		e.token.Close()
	}
	return e, ok
}

// origin returns the immutable authorization snapshot for a live route only
// when the querying connection is that route's Provider target. The route
// token closes the race with completion immediately after this lookup.
func (t *dispatchTable) origin(routeID uint64, target *conn) (*routeEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.entries[routeID]
	if !ok || e.target != target || !e.token.Open() {
		return nil, false
	}
	return e, true
}

// connClosed removes every entry with c as target or source and returns them classified, for the caller to handle after releasing
// the table lock. The table lock only protects this map itself, not connection I/O such as sending results into another connection's
// outbox (holding the lock across connections' I/O is explicitly forbidden)
func (t *dispatchTable) connClosed(c *conn) (asTarget, asSource []*routeEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for id, e := range t.entries {
		switch c {
		case e.target:
			asTarget = append(asTarget, e)
			delete(t.entries, id)
			e.token.Close()
		case e.source:
			asSource = append(asSource, e)
			delete(t.entries, id)
			e.token.Close()
		}
	}
	return asTarget, asSource
}

// reap removes every expired entry (deadline passed), called periodically by the sweeper
func (t *dispatchTable) reap(now time.Time) []*routeEntry {
	var expired []*routeEntry

	t.mu.Lock()
	for id, e := range t.entries {
		if now.After(e.deadline) {
			expired = append(expired, e)
			delete(t.entries, id)
			e.token.Close()
		}
	}
	t.mu.Unlock()

	return expired
}

// resolveRoute is the common tail of every path that completes a route: give back the source's in-flight
// count, and send the final Response into the source's outbox. The return value comes from enqueue, and only
// handleRequest's synchronous path cares about it (to decide whether to keep reading the source connection)
func resolveRoute(e *routeEntry, resp *ipcv1.Response) bool {
	e.source.releaseRequest(e.sourceRequestID)
	return e.source.enqueue(responseEnvelope(resp))
}

// dispatchConnClosed handles the effect of a connection disconnecting on the route table:
//   - the connection is the target of some routes: callers are waiting on it and shouldn't only find out at the timeout,
//     so synthesise UNAVAILABLE straight away and send it back to each source
//   - the connection is the source of some routes: there's nowhere to deliver, so drop them (no Response is produced,
//     no connection would read it), and make a best effort to tell the matching targets to stop in-flight work, so an action
//     nobody is waiting for any more doesn't keep holding actuators/resources
func (s *Server) dispatchConnClosed(c *conn) {
	asTarget, asSource := s.dispatch.connClosed(c)

	for _, e := range asTarget {
		s.transfer.CloseRoute(e.routeID)
		resolveRoute(e, failureResponse(e.sourceRequestID, ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE))
	}
	for _, e := range asSource {
		s.transfer.CloseRoute(e.routeID)
		e.source.releaseRequest(e.sourceRequestID)
		if e.target != nil {
			e.target.enqueue(&ipcv1.Envelope{Body: &ipcv1.Envelope_CancelDispatch{
				CancelDispatch: &ipcv1.CancelDispatch{
					RouteId: e.routeID,
					Reason:  ipcv1.CancelDispatchReason_CANCEL_DISPATCH_REASON_CLIENT_GONE,
				},
			}})
		}
	}
}

// dispatchReapInterval is the sweeper's scan period. A coarse trade-off: a request may be cleaned up at most one period
// after its real deadline, in exchange for keeping table lock contention very low.
// Against the magnitudes of defaultMethodTimeoutMs=5000/maxMethodTimeoutMs=30000,
// 250ms of delay is negligible
const dispatchReapInterval = 250 * time.Millisecond

// runDispatchReaper periodically cleans up routes still unfinished past their deadline, so a Service that never
// answers can't leave a caller's request hanging forever. Its lifecycle mirrors acceptLoop: it joins the existing
// start/stop protocol with s.wg/s.quit rather than inventing a new mechanism
func (s *Server) runDispatchReaper() {
	defer s.wg.Done()

	ticker := time.NewTicker(dispatchReapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.quit:
			return
		case now := <-ticker.C:
			for _, e := range s.dispatch.reap(now) {
				s.transfer.CloseRoute(e.routeID)
				resolveRoute(e, failureResponse(e.sourceRequestID, ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED))
				if e.target != nil {
					e.target.enqueue(&ipcv1.Envelope{Body: &ipcv1.Envelope_CancelDispatch{
						CancelDispatch: &ipcv1.CancelDispatch{
							RouteId: e.routeID,
							Reason:  ipcv1.CancelDispatchReason_CANCEL_DISPATCH_REASON_DEADLINE_EXCEEDED,
						},
					}})
				}
			}
		}
	}
}

// auditDispatchRace records an audit entry for a dropped DispatchResult with a late/unknown route_id,
// using a rate-limit bucket separate from violationLog: this is an expected, normal race (the sweeper or another result
// finished first), and shouldn't compete with real protocol violation signals for the same audit budget
func (s *Server) auditDispatchRace(subject string, routeID uint64) {
	if !s.dispatchRaceLog.allow() {
		return
	}
	s.auditor.Record(context.Background(), audit.Event{
		Action:  "ipc.DispatchResultUnmatched",
		Subject: subject,
		Detail:  fmt.Sprintf("route_id=%d", routeID),
	})
}

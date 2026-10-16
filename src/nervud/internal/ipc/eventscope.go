// This file is the event instance ownership table: who is entitled to subscribe to which instance on an endpoint.
//
// # The gap it fills
//
// Subscriptions are set up per (endpoint, event_id). One camera can have several streams open at once,
// and without separating instances, A subscribing to cam.front would get the state changes of B's stream:
// when it opened, how long it ran, when it dropped.
//
// And only the Provider knows "did you open this stream". This table is where it hands that knowledge
// to nervud.
//
// # Why registration in advance rather than asking the Provider at subscription time
//
// Asking costs a round trip each time, and Subscribe runs in the connection's read loop: while waiting, the connection's
// request responses, Pings and even other subscriptions can't be read. One slow Provider could hold up the whole connection.
//
// Registering in advance moves that round trip to the moment the instance is created, where a call is already being handled,
// so one more sentence adds no wait at all.
//
// # Ownership is proved by the route, not self-reported
//
// What the Provider says is "this scope belongs to the caller of the call I'm handling right now", and nervud itself knows
// who made that call (route -> source conn). The same trick as BeginTransfer proving identity with
// origin_route_id.
package ipc

import "sync"

// scopeKey uniquely identifies an event instance.
//
// It uses the Provider side's (connection, endpoint_id): the same source as the subscription fan-out key; if the two don't line up you get
// "registered but can't subscribe" or the reverse.
type scopeKey struct {
	providerConn *conn
	endpointID   uint64
	scope        uint64
}

// eventScopes records instance ownership.
//
// BindEventScope and Subscribe come from the read loop goroutines of different connections,
// so it must be locked.
type eventScopes struct {
	mu sync.Mutex
	// owner is the connection entitled to subscribe to the instance.
	owner map[scopeKey]*conn
	// byProvider / byOwner are two reverse indexes, for bulk cleanup on disconnect.
	//
	// Both are needed: when a Provider dies, every instance it registered stops existing; when a caller dies,
	// its instances vanish with it (streams are bound to connections). Without either one, that kind of disconnect would
	// leave entries that can never be cleared.
	byProvider map[*conn]map[scopeKey]struct{}
	byOwner    map[*conn]map[scopeKey]struct{}
}

func newEventScopes() *eventScopes {
	return &eventScopes{
		owner:      make(map[scopeKey]*conn),
		byProvider: make(map[*conn]map[scopeKey]struct{}),
		byOwner:    make(map[*conn]map[scopeKey]struct{}),
	}
}

// bind registers one ownership. Registering the same key again overwrites it.
//
// Overwrite rather than refuse: a Provider reusing a scope number is its own business (closing and reopening the same
// stream), and refusing would force it to revoke first and then register, with subscriptions failing in between. Overwriting means
// "this scope now belongs to this one", consistent with the latest facts.
func (s *eventScopes) bind(provider *conn, endpointID, scope uint64, owner *conn) {
	key := scopeKey{providerConn: provider, endpointID: endpointID, scope: scope}

	s.mu.Lock()
	defer s.mu.Unlock()

	if previous, exists := s.owner[key]; exists && previous != owner {
		s.detachOwnerLocked(previous, key)
	}
	s.owner[key] = owner
	addScopeIndex(s.byProvider, provider, key)
	addScopeIndex(s.byOwner, owner, key)
}

// release revokes one registration.
func (s *eventScopes) release(provider *conn, endpointID, scope uint64) {
	key := scopeKey{providerConn: provider, endpointID: endpointID, scope: scope}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(key)
}

// allows reports whether subscriber is entitled to subscribe to this instance.
//
// Anything unregistered is refused. The fail-closed direction matters especially here: letting through an unregistered scope
// would mean going back to broadcast without instances, which is exactly the problem this table solves.
func (s *eventScopes) allows(provider *conn, endpointID, scope uint64, subscriber *conn) bool {
	if provider == nil || subscriber == nil || scope == 0 {
		return false
	}
	key := scopeKey{providerConn: provider, endpointID: endpointID, scope: scope}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner[key] == subscriber
}

// closeProvider clears every registration under a Provider connection.
func (s *eventScopes) closeProvider(provider *conn) {
	s.closeIndex(s.byProvider, provider)
}

// closeOwner clears every registration under a subscriber connection.
func (s *eventScopes) closeOwner(owner *conn) {
	s.closeIndex(s.byOwner, owner)
}

// closeEndpoint clears every registration on one Provider endpoint.
//
// Called when the endpoint is withdrawn: after that none of its instances exist. It can't just wait for the connection to drop:
// a long-running Provider repeatedly registering and withdrawing endpoints would make registrations pile up without bound.
func (s *eventScopes) closeEndpoint(provider *conn, endpointID uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.byProvider[provider] {
		if key.endpointID == endpointID {
			s.removeLocked(key)
		}
	}
}

func (s *eventScopes) closeIndex(index map[*conn]map[scopeKey]struct{}, c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Take a snapshot first: removeLocked changes this index, and deleting while iterating is undefined behaviour.
	keys := make([]scopeKey, 0, len(index[c]))
	for key := range index[c] {
		keys = append(keys, key)
	}
	for _, key := range keys {
		s.removeLocked(key)
	}
}

// removeLocked removes one registration and its traces in both indexes.
func (s *eventScopes) removeLocked(key scopeKey) {
	owner, exists := s.owner[key]
	if !exists {
		return
	}
	delete(s.owner, key)
	s.detachOwnerLocked(owner, key)
	if set := s.byProvider[key.providerConn]; set != nil {
		delete(set, key)
		if len(set) == 0 {
			delete(s.byProvider, key.providerConn)
		}
	}
}

func (s *eventScopes) detachOwnerLocked(owner *conn, key scopeKey) {
	set := s.byOwner[owner]
	if set == nil {
		return
	}
	delete(set, key)
	if len(set) == 0 {
		delete(s.byOwner, owner)
	}
}

func addScopeIndex(index map[*conn]map[scopeKey]struct{}, c *conn, key scopeKey) {
	set := index[c]
	if set == nil {
		set = make(map[scopeKey]struct{})
		index[c] = set
	}
	set[key] = struct{}{}
}

// len is for diagnostics and tests.
func (s *eventScopes) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.owner)
}

// This file is the consumer-side Client: connect to nervud, Resolve logical interfaces into routing handles, then call.
//
// Typical use:
//
//	c, err := sdk.Connect(sdk.Config{ComponentID: "main"})
//	defer c.Close()
//	ep, err := c.ResolveEndpoint(ctx, sdk.ResolveRequest{
//	    InterfaceID: "nervus.interface.motion.base", MinMajor: 1, MaxMajor: 1,
//	})
//	respBytes, err := c.Call(ctx, ep.EndpointID, methodSetVelocity, reqBytes, time.Second)
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

// Config holds the shared construction parameters of Client / ServiceHost.
type Config struct {
	// SockPath is the control-plane UDS. Empty = DefaultSockPath.
	SockPath string
	// ComponentID is the Component ID this process claims, which nervud checks against kernel facts.
	// Required: leaving it empty stops nervud narrowing the check, and the handshake is refused.
	ComponentID string
	// SDKName / SDKVersion are only for diagnostic statistics and take no part in decisions. Defaults are used when empty.
	SDKName    string
	SDKVersion string
	// DialTimeout / HandshakeTimeout use defaults when empty.
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration
	// Log uses slog.Default() when nil.
	Log *slog.Logger
}

const (
	defaultSDKName          = "nervus-ipc-go"
	defaultSDKVersion       = "0.1.0"
	defaultDialTimeout      = 5 * time.Second
	defaultHandshakeTimeout = 5 * time.Second
)

func (cfg *Config) applyDefaults() {
	if cfg.SDKName == "" {
		cfg.SDKName = defaultSDKName
	}
	if cfg.SDKVersion == "" {
		cfg.SDKVersion = defaultSDKVersion
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = defaultHandshakeTimeout
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
}

// Client is the consumer side of a control-plane connection. Its methods can be called concurrently.
type Client struct {
	co   *conn
	ids  *requestIDGen
	pend *pendingMap
	log  *slog.Logger

	// resolves registers in-flight ResolveEndpoints. They're separate from Requests because, although the two
	// request_id spaces share a generator, their response types differ, and putting them in one map would need type assertions.
	resolveMu sync.Mutex
	resolves  map[uint64]chan *ipcv1.ResolveEndpointResult

	// subs is the subscription state machine; see subscribe.go.
	subs *subscriber

	readErr  error
	readDone chan struct{}
}

// writeEnvelope / nextRequestID / closedErr let Client satisfy eventTransport.
func (c *Client) writeEnvelope(env *ipcv1.Envelope) error { return c.co.writeEnvelope(env) }
func (c *Client) nextRequestID() (uint64, error)          { return c.ids.nextID() }

// Connect sets up the connection and completes the handshake.
func Connect(cfg Config) (*Client, error) {
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

	c := &Client{
		co:       co,
		ids:      newRequestIDGen(),
		pend:     newPendingMap(),
		resolves: make(map[uint64]chan *ipcv1.ResolveEndpointResult),
		log:      cfg.Log,
		readDone: make(chan struct{}),
	}
	c.subs = newSubscriber(c, cfg.Log)
	go c.readLoop()
	return c, nil
}

// Subscribe subscribes to events on an endpoint.
//
// The returned Subscription's C() is closed when the subscription ends, so it can be ranged over directly; for why it ended
// see Err(). **It must be consumed**: a subscription nobody reads is merged, dropped,
// or (RELIABLE) ended by local backpressure according to its DeliveryClass.
func (c *Client) Subscribe(ctx context.Context, req SubscribeRequest) (*Subscription, error) {
	return c.subs.subscribe(ctx, req)
}

// Unsubscribe takes down a subscription and closes its event channel.
//
// Idempotence comes from the server: unsubscribing again returns a *StatusError with NOT_FOUND.
func (c *Client) Unsubscribe(ctx context.Context, sub *Subscription) error {
	return c.subs.unsubscribe(ctx, sub)
}

// Identity returns the identity nervud verified at the handshake.
func (c *Client) Identity() Identity { return c.co.Identity() }

// Limits returns the budgets in effect for this connection (a guide for SDK self-discipline, not a server promise).
func (c *Client) Limits() *ipcv1.ConnectionLimits { return c.co.Limits() }

// Close closes the connection. Every in-flight request fails with ErrClosed. Idempotent.
func (c *Client) Close() error {
	c.co.close()
	<-c.readDone
	return nil
}

// readLoop is this connection's only reader goroutine.
func (c *Client) readLoop() {
	defer close(c.readDone)
	defer c.failAllPending()

	for {
		env, err := c.co.readEnvelope()
		if err != nil {
			select {
			case <-c.co.closed:
				// A read failure caused by this side's own Close isn't an anomaly
			default:
				c.readErr = err
				c.log.Debug("sdk: read loop ended", "err", err)
			}
			c.co.close()
			return
		}

		// The four subscription-related bodies mean the same in both roles, so they go to the shared state machine.
		if c.subs.handleSubscriptionBody(env) {
			continue
		}

		switch body := env.GetBody().(type) {
		case *ipcv1.Envelope_Response:
			r := body.Response
			if !c.pend.deliver(r.GetRequestId(), r) {
				// Late/duplicate/unknown request_id: drop it and note it, **without** closing the connection.
				// A request that already timed out getting a late response is normal timing, and treating it as a violation would kick connections off often.
				c.log.Debug("sdk: dropping unmatched Response", "request_id", r.GetRequestId())
			}

		case *ipcv1.Envelope_ResolveEndpointResult:
			c.deliverResolve(body.ResolveEndpointResult)

		case *ipcv1.Envelope_Ping:
			// The protocol lets either side send Ping, and the server Pings clients too. Echo the nonce unchanged.
			pong := &ipcv1.Envelope{Body: &ipcv1.Envelope_Pong{
				Pong: &ipcv1.Pong{Nonce: body.Ping.GetNonce()},
			}}
			if err := c.co.writeEnvelope(pong); err != nil {
				c.log.Debug("sdk: send Pong failed", "err", err)
				return
			}

		case *ipcv1.Envelope_Pong:
			// This SDK doesn't send Pings itself yet, so receiving a Pong is unexpected. Accept and ignore it rather than closing
			// the connection as a violation: it carries no request and needs no reply, so it's harmless.

		case *ipcv1.Envelope_EndpointDied, *ipcv1.Envelope_EndpointRevoked:
			// A binding invalidation notice pushed by the server. The caller's next Call gets NOT_FOUND
			// and takes the NeedsReResolve recovery path, so this only logs.
			// TODO: add a callback here when active notification is needed (no wire change).
			c.log.Info("sdk: endpoint invalidated by server", "body", fmt.Sprintf("%T", body))

		default:
			// Bodies reaching here are either ones nervud never sends on its own (Cancel, the ControlLease group;
			// see the implementation status notes in wire.go), or ones that should only appear on Service connections
			// (Dispatch). Receiving one is a protocol violation.
			c.readErr = fmt.Errorf("%w: unexpected body %T on client connection", ErrProtocol, body)
			c.log.Warn("sdk: unexpected body, closing", "body", fmt.Sprintf("%T", body))
			c.co.close()
			return
		}
	}
}

func (c *Client) failAllPending() {
	c.pend.failAll()
	c.subs.closeAll(c.closedErr())
	c.resolveMu.Lock()
	m := c.resolves
	c.resolves = make(map[uint64]chan *ipcv1.ResolveEndpointResult)
	c.resolveMu.Unlock()
	for _, ch := range m {
		close(ch)
	}
}

func (c *Client) deliverResolve(r *ipcv1.ResolveEndpointResult) {
	id := r.GetRequestId()
	c.resolveMu.Lock()
	ch, ok := c.resolves[id]
	if ok {
		delete(c.resolves, id)
	}
	c.resolveMu.Unlock()
	if !ok {
		c.log.Debug("sdk: dropping unmatched ResolveEndpointResult", "request_id", id)
		return
	}
	ch <- r
}

// ResolveRequest is one interface resolution request.
type ResolveRequest struct {
	// InterfaceID is the logical interface ID, such as "nervus.interface.motion.base".
	InterfaceID string
	// MinMajor / MaxMajor are the closed range of acceptable interface major versions.
	MinMajor, MaxMajor uint32
	// ResourceType / ResourceRole choose the target Resource, and can only express "an exact type +
	// role". To choose devices by labels or specify a multi-candidate policy, use Selector.
	//
	// **Leaving both empty no longer has an implicit default**: v1 treated an empty selector as
	// {nervus.resource.motion.base, main}, and v2 removed that rule. Interfaces binding physical resources
	// fail to resolve when it's left empty, and must say explicitly which device they want.
	ResourceType, ResourceRole string

	// Selector is the full resource selector: it supports label filtering and multi-candidate policies.
	//
	// When non-empty it **completely replaces** the two fields above, with no merging, avoiding semantics like "filled in three places,
	// and which one wins" that you'd need to read the implementation to know.
	//
	// Choosing devices by labels is the recommended approach: stable_role is a product of board configuration (whether the front camera on this board
	// is called cam.front or camera0), and depending on it means changing code when the board changes.
	//
	//	Selector: &ipcv1.ResourceSelector{
	//	    Type:   "nervus.resource.camera",
	//	    Labels: map[string]string{"nervus.camera.facing": "front"},
	//	}
	//
	// **Not specifying Policy means REQUIRE_UNIQUE**: several matches fail outright, rather than handing out any one.
	// To have nervud choose for you, write RESOURCE_SELECTION_POLICY_SYSTEM_PREFERRED explicitly.
	Selector *ipcv1.ResourceSelector
}

// selector gives the ResourceSelector this request finally sends.
func (r ResolveRequest) selector() *ipcv1.ResourceSelector {
	if r.Selector != nil {
		return r.Selector
	}
	return selectorOf(r.ResourceType, r.ResourceRole)
}

// Endpoint is the result of one successful resolution.
type Endpoint struct {
	// EndpointID is the routing handle **scoped to this connection**. All are invalid after a disconnect, and a reconnect must
	// Resolve again. It doesn't get its safety from "others can't guess the number": the lookup key is (connection, endpoint_id).
	EndpointID uint64
	// Major / Minor are the interface version chosen atomically.
	Major, Minor uint32
	// SchemaHash is the deterministic identifier of the chosen interface schema. Callers should check it matches their local generated code;
	// a mismatch means the two sides' schemas drifted, an error that must surface before the first call,
	// rather than when some field gets silently dropped.
	SchemaHash []byte
	// ResourceHandle is the resolved public Resource handle. It doesn't include internal information such as the Provider PID / device node /
	// serial number.
	ResourceHandle string
}

// ResolveEndpoint resolves a logical interface into a routing handle on this connection.
func (c *Client) ResolveEndpoint(ctx context.Context, req ResolveRequest) (Endpoint, error) {
	id, err := c.ids.nextID()
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

	ch := make(chan *ipcv1.ResolveEndpointResult, 1)
	c.resolveMu.Lock()
	c.resolves[id] = ch
	c.resolveMu.Unlock()

	if err := c.co.writeEnvelope(env); err != nil {
		c.resolveMu.Lock()
		delete(c.resolves, id)
		c.resolveMu.Unlock()
		return Endpoint{}, err
	}

	select {
	case <-ctx.Done():
		c.resolveMu.Lock()
		delete(c.resolves, id)
		c.resolveMu.Unlock()
		return Endpoint{}, ctx.Err()

	case res, ok := <-ch:
		if !ok {
			return Endpoint{}, c.closedErr()
		}
		if f := res.GetFailure(); f != nil {
			// The failure error_detail is always ResolveEndpointErrorDetail; callers can use
			// StatusError.UnmarshalDetail to get distinguishable reasons such as INTERFACE_NOT_FOUND /
			// RESOURCE_NOT_FOUND / VERSION_MISMATCH.
			return Endpoint{}, statusErrorFrom(f)
		}
		s := res.GetSuccess()
		if s == nil {
			return Endpoint{}, fmt.Errorf("%w: ResolveEndpointResult has neither outcome", ErrProtocol)
		}
		return Endpoint{
			EndpointID:     s.GetEndpointId(),
			Major:          s.GetInterfaceMajor(),
			Minor:          s.GetInterfaceMinor(),
			SchemaHash:     s.GetInterfaceSchemaHash(),
			ResourceHandle: s.GetResourceHandle(),
		}, nil
	}
}

// selectorOf builds the ResourceSelector. Returns nil when both fields are empty.
//
// In v2, nil means "this interface binds no resource", no longer "take the default device". Sending an empty selector
// means the same as not sending one, and not sending saves a few bytes. Interfaces binding resources fail to resolve on nil, which is exactly
// what we want: "forgot to fill it in" surfaces at the first Resolve, rather than routing to some device that happens to exist.
func selectorOf(typ, role string) *ipcv1.ResourceSelector {
	if typ == "" && role == "" {
		return nil
	}
	return &ipcv1.ResourceSelector{Type: typ, Role: role}
}

// Call makes one method call and blocks until the final response, a timeout or a disconnect.
//
// timeout is **what the caller wants**: nervud tightens it to the method maximum, taking the smaller. Passing 0 means
// use the method default, and does **not** mean unlimited.
//
// The payload returned is the serialised bytes of the method-specific response, typed uniquely by
// (endpoint_id, interface version, method_id); the caller decodes it from known context.
func (c *Client) Call(ctx context.Context, endpointID uint64, methodID uint32, payload []byte, timeout time.Duration) ([]byte, error) {
	id, err := c.ids.nextID()
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

	ch := c.pend.add(id)
	if err := c.co.writeEnvelope(env); err != nil {
		c.pend.remove(id)
		return nil, err
	}

	select {
	case <-ctx.Done():
		// This side gives up waiting. **Note** this doesn't cancel the other side's work: Cancel(32) isn't implemented in the kernel,
		// so the Service keeps computing until its own deadline. Side effects (motors that already moved) aren't rolled back
		// because this side gave up either. See the "Implementation status" table in the repository README.
		c.pend.remove(id)
		return nil, ctx.Err()

	case resp, ok := <-ch:
		if !ok {
			return nil, c.closedErr()
		}
		if f := resp.GetFailure(); f != nil {
			return nil, statusErrorFrom(f)
		}
		s := resp.GetSuccess()
		if s == nil {
			return nil, fmt.Errorf("%w: Response has neither outcome", ErrProtocol)
		}
		return s.GetPayload(), nil
	}
}

// closedErr gives why the connection dropped: with a read error, include it; otherwise it's a normal close.
func (c *Client) closedErr() error {
	if c.readErr != nil {
		return fmt.Errorf("%w: %v", ErrClosed, c.readErr)
	}
	return ErrClosed
}

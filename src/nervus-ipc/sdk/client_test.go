package sdk

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// quietConfig is a config pointing at the test double, with logging turned down to Error to avoid noise.
func quietConfig(sock string) Config {
	return Config{
		SockPath:    sock,
		ComponentID: testComponentID,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestConnect_RequiresComponentID(t *testing.T) {
	// Leaving it empty stops nervud narrowing the check to one comparison, so the handshake is always refused. Rather than wait for a network round trip
	// to fail, stop it at construction.
	if _, err := Connect(Config{SockPath: "/nonexistent"}); err == nil {
		t.Fatal("empty ComponentID must be rejected before dialing")
	}
}

func TestConnect_HandshakeSuccess(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(nil))

	c, err := Connect(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	id := c.Identity()
	if id.PackageID != testPackageID || id.ComponentID != testComponentID {
		t.Errorf("identity = %+v, want %s/%s", id, testPackageID, testComponentID)
	}
	// The budget must be kept: the SDK uses it to police itself (e.g. not sending requests over max_frame_bytes);
	// dropping it would leave "getting disconnected" as the only way to find the limit.
	if c.Limits().GetMaxFrameBytes() != MaxFrameBytes {
		t.Errorf("limits not captured: %+v", c.Limits())
	}
}

func TestConnect_FirstFrameIsHelloWithVersionRange(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(nil))

	c, err := Connect(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	f.mu.Lock()
	first := f.received[0]
	f.mu.Unlock()

	h := first.GetHello()
	if h == nil {
		t.Fatalf("first frame is %T, want Hello", first.GetBody())
	}
	// **Both lower and upper bounds are 2**: v2 isn't backward compatible with v1. Declaring min=1 would let a v1 server
	// complete the handshake with this SDK, then quietly disagree on empty selector semantics, far more dangerous than
	// failing to connect. This assertion stops anyone moving the lower bound back "for compatibility with old devices".
	if h.GetMinProtocolMajor() != 2 || h.GetMaxProtocolMajor() != 2 {
		t.Errorf("major range = [%d,%d], want [2,2]", h.GetMinProtocolMajor(), h.GetMaxProtocolMajor())
	}
	if h.GetDeclaredComponentId() != testComponentID {
		t.Errorf("declared component = %q", h.GetDeclaredComponentId())
	}
	// Handshake frames **must** carry the version: protocol_major/minor only mean anything here in the whole protocol.
	if first.GetProtocolMajor() != 2 {
		t.Errorf("Hello envelope protocol_major = %d, want 2", first.GetProtocolMajor())
	}
}

func TestConnect_HandshakeRejected(t *testing.T) {
	// When identity verification fails, nervud sends a Failure HelloAck before closing the connection, never a bare close:
	// a bare close would leave the client unable to tell "wrong version/identity" from "broken socket", and the right
	// responses to those two are opposite. This asserts the SDK really carries the reason out.
	f := startFakeNervud(t)
	f.setHandler(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if env.GetHello() != nil {
			return []*ipcv1.Envelope{helloAckFailure(ipcv1.StatusCode_STATUS_CODE_UNAUTHENTICATED)}
		}
		return nil
	})

	_, err := Connect(quietConfig(f.sockPath()))
	if !errors.Is(err, ErrHandshakeFailed) {
		t.Fatalf("err = %v, want ErrHandshakeFailed", err)
	}
	if !contains(err.Error(), "UNAUTHENTICATED") {
		t.Errorf("error should carry the status code, got %v", err)
	}
}

func TestResolveEndpoint_Success(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		r := env.GetResolveEndpoint()
		if r == nil {
			return nil
		}
		return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_ResolveEndpointResult{
			ResolveEndpointResult: &ipcv1.ResolveEndpointResult{
				RequestId: r.GetRequestId(),
				Outcome: &ipcv1.ResolveEndpointResult_Success{
					Success: &ipcv1.ResolveEndpointSuccess{
						EndpointId:     42,
						InterfaceMajor: 1,
						ResourceHandle: "base.main",
					},
				},
			},
		}}}
	}))

	c, err := Connect(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	ep, err := c.ResolveEndpoint(context.Background(), ResolveRequest{
		InterfaceID: "nervus.interface.motion.base", MinMajor: 1, MaxMajor: 1,
	})
	if err != nil {
		t.Fatalf("ResolveEndpoint: %v", err)
	}
	if ep.EndpointID != 42 || ep.ResourceHandle != "base.main" {
		t.Fatalf("endpoint = %+v", ep)
	}
}

func TestResolveEndpoint_EmptySelectorIsOmitted(t *testing.T) {
	// An empty selector and no selector mean the same thing (nervud implicitly takes motion.base/main),
	// but sending none saves a few bytes. This asserts the SDK doesn't pointlessly stuff in an empty message.
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		r := env.GetResolveEndpoint()
		if r == nil {
			return nil
		}
		if r.GetSelector() != nil {
			t.Errorf("selector should be omitted when both fields empty, got %+v", r.GetSelector())
		}
		return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_ResolveEndpointResult{
			ResolveEndpointResult: &ipcv1.ResolveEndpointResult{
				RequestId: r.GetRequestId(),
				Outcome: &ipcv1.ResolveEndpointResult_Success{
					Success: &ipcv1.ResolveEndpointSuccess{EndpointId: 1},
				},
			},
		}}}
	}))

	c, _ := Connect(quietConfig(f.sockPath()))
	defer func() { _ = c.Close() }()
	if _, err := c.ResolveEndpoint(context.Background(), ResolveRequest{
		InterfaceID: "x", MinMajor: 1, MaxMajor: 1,
	}); err != nil {
		t.Fatalf("ResolveEndpoint: %v", err)
	}
}

func TestResolveEndpoint_TypedErrorDetail(t *testing.T) {
	// Failures must carry a typed detail: the caller needs to tell "interface doesn't exist" (don't retry) from
	// "resource temporarily unavailable" (back off and retry), and a blanket FAILED_PRECONDITION isn't enough.
	detail := &ipcv1.ResolveEndpointErrorDetail{
		Reason: ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_INTERFACE_NOT_FOUND,
	}
	detailBytes := mustMarshal(t, detail)

	f := startFakeNervud(t)
	f.setHandler(autoHandshake(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		r := env.GetResolveEndpoint()
		if r == nil {
			return nil
		}
		return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_ResolveEndpointResult{
			ResolveEndpointResult: &ipcv1.ResolveEndpointResult{
				RequestId: r.GetRequestId(),
				Outcome: &ipcv1.ResolveEndpointResult_Failure{Failure: &ipcv1.Failure{
					Code:        ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
					ErrorDetail: detailBytes,
				}},
			},
		}}}
	}))

	c, _ := Connect(quietConfig(f.sockPath()))
	defer func() { _ = c.Close() }()

	_, err := c.ResolveEndpoint(context.Background(), ResolveRequest{InterfaceID: "nope", MinMajor: 1, MaxMajor: 1})
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StatusError", err)
	}
	if se.Code != ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION {
		t.Errorf("code = %v", se.Code)
	}
	got := &ipcv1.ResolveEndpointErrorDetail{}
	ok, derr := se.UnmarshalDetail(got)
	if derr != nil || !ok {
		t.Fatalf("UnmarshalDetail: ok=%v err=%v", ok, derr)
	}
	if got.GetReason() != ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_INTERFACE_NOT_FOUND {
		t.Errorf("reason = %v", got.GetReason())
	}
}

func TestCall_SuccessAndFailure(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		r := env.GetRequest()
		if r == nil {
			return nil
		}
		if r.GetMethodId() == 99 {
			return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_Response{Response: &ipcv1.Response{
				RequestId: r.GetRequestId(),
				Outcome: &ipcv1.Response_Failure{Failure: &ipcv1.Failure{
					Code: ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED,
				}},
			}}}}
		}
		return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_Response{Response: &ipcv1.Response{
			RequestId: r.GetRequestId(),
			Outcome: &ipcv1.Response_Success{Success: &ipcv1.Success{
				Code:    ipcv1.StatusCode_STATUS_CODE_OK,
				Payload: append([]byte("echo:"), r.GetPayload()...),
			}},
		}}}}
	}))

	c, _ := Connect(quietConfig(f.sockPath()))
	defer func() { _ = c.Close() }()

	out, err := c.Call(context.Background(), 1, 7, []byte("hi"), time.Second)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(out) != "echo:hi" {
		t.Fatalf("payload = %q", out)
	}

	_, err = c.Call(context.Background(), 1, 99, nil, time.Second)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED {
		t.Fatalf("err = %v, want PERMISSION_DENIED StatusError", err)
	}
	// A permission refusal should lead to "Resolve again", not "retry in place"
	if IsRetryable(err) {
		t.Error("PERMISSION_DENIED must not be retryable")
	}
	if !NeedsReResolve(err) {
		t.Error("PERMISSION_DENIED should trigger re-resolve")
	}
}

func TestCall_OutOfOrderResponses(t *testing.T) {
	// nervud doesn't guarantee replies in request order. Three concurrent calls, the server deliberately answers in reverse,
	// and every caller must get its own.
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		r := env.GetRequest()
		if r == nil {
			return nil
		}
		// Use method_id as recognisable echo content
		return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_Response{Response: &ipcv1.Response{
			RequestId: r.GetRequestId(),
			Outcome: &ipcv1.Response_Success{Success: &ipcv1.Success{
				Payload: []byte{byte(r.GetMethodId())},
			}},
		}}}}
	}))

	c, _ := Connect(quietConfig(f.sockPath()))
	defer func() { _ = c.Close() }()

	type res struct {
		method byte
		out    []byte
		err    error
	}
	ch := make(chan res, 3)
	for _, m := range []uint32{11, 22, 33} {
		go func(m uint32) {
			out, err := c.Call(context.Background(), 1, m, nil, time.Second)
			ch <- res{byte(m), out, err}
		}(m)
	}
	for i := 0; i < 3; i++ {
		r := <-ch
		if r.err != nil {
			t.Fatalf("call %d: %v", r.method, r.err)
		}
		if len(r.out) != 1 || r.out[0] != r.method {
			t.Fatalf("call %d got payload % x (crossed wires)", r.method, r.out)
		}
	}
}

func TestCall_LateResponseDoesNotKillConnection(t *testing.T) {
	// The server sends the response back after the caller timed out and left. The SDK must drop it and keep the connection usable:
	// a timed-out request receiving a late response is normal timing, and treating it as a protocol violation would kick connections off often.
	f := startFakeNervud(t)
	var held *ipcv1.Request
	f.setHandler(autoHandshake(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if r := env.GetRequest(); r != nil {
			if held == nil {
				held = r // first request: hold it without replying
				return nil
			}
			// When the second request arrives, release the held late response first, then reply to this one normally
			return []*ipcv1.Envelope{
				{Body: &ipcv1.Envelope_Response{Response: &ipcv1.Response{
					RequestId: held.GetRequestId(),
					Outcome:   &ipcv1.Response_Success{Success: &ipcv1.Success{}},
				}}},
				{Body: &ipcv1.Envelope_Response{Response: &ipcv1.Response{
					RequestId: r.GetRequestId(),
					Outcome: &ipcv1.Response_Success{Success: &ipcv1.Success{
						Payload: []byte("second"),
					}},
				}}},
			}
		}
		return nil
	}))

	c, _ := Connect(quietConfig(f.sockPath()))
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Call(ctx, 1, 1, nil, time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first call err = %v, want DeadlineExceeded", err)
	}

	out, err := c.Call(context.Background(), 1, 2, nil, time.Second)
	if err != nil {
		t.Fatalf("connection unusable after late response: %v", err)
	}
	if string(out) != "second" {
		t.Fatalf("payload = %q", out)
	}
}

func TestCall_ServerDisconnectWakesWaiters(t *testing.T) {
	// A dropped connection must wake every in-flight request, or callers hang forever (goroutine leak).
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if env.GetRequest() != nil {
			_ = c.Close() // disconnect on receiving the request, without replying
		}
		return nil
	}))

	c, _ := Connect(quietConfig(f.sockPath()))
	defer func() { _ = c.Close() }()

	done := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), 1, 1, nil, time.Second)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Call did not return after server disconnect")
	}
}

func TestClient_RespondsToServerPing(t *testing.T) {
	// The protocol lets either side send Ping. The client must echo the nonce unchanged, or the server will
	// judge it lost and disconnect: keepalive goes both ways.
	f := startFakeNervud(t)
	pong := make(chan uint64, 1)
	f.setHandler(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if env.GetHello() != nil {
			return []*ipcv1.Envelope{
				helloAckOK(),
				{Body: &ipcv1.Envelope_Ping{Ping: &ipcv1.Ping{Nonce: 0xDEADBEEF}}},
			}
		}
		if p := env.GetPong(); p != nil {
			select {
			case pong <- p.GetNonce():
			default:
			}
		}
		return nil
	})

	c, err := Connect(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	select {
	case n := <-pong:
		if n != 0xDEADBEEF {
			t.Fatalf("pong nonce = %#x, want 0xDEADBEEF", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not answer server Ping")
	}
}

func TestClient_UnexpectedBodyClosesConnection(t *testing.T) {
	// Dispatch should only appear on Service connections. A client receiving it means nervud's state machine is confused
	// or the peer isn't nervud; it must close the connection, not silently ignore it.
	f := startFakeNervud(t)
	f.setHandler(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if env.GetHello() != nil {
			return []*ipcv1.Envelope{
				helloAckOK(),
				{Body: &ipcv1.Envelope_Dispatch{Dispatch: &ipcv1.Dispatch{RouteId: 1}}},
			}
		}
		return nil
	})

	c, err := Connect(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	deadline := time.After(3 * time.Second)
	for {
		_, err := c.Call(context.Background(), 1, 1, nil, 200*time.Millisecond)
		if errors.Is(err, ErrClosed) {
			return // expected: the connection has been closed
		}
		select {
		case <-deadline:
			t.Fatalf("connection stayed open after unexpected body, last err = %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

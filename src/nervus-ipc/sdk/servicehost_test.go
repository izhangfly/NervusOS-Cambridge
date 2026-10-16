package sdk

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func testExecutionContext() *ipcv1.ExecutionContext {
	return &ipcv1.ExecutionContext{DeadlineNanos: 1}
}

func TestServiceHost_RegisterEndpoint(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		r := env.GetRegisterEndpoint()
		if r == nil {
			return nil
		}
		if r.GetInterfaceId() != "nervus.interface.motion.base" {
			t.Errorf("interface_id = %q", r.GetInterfaceId())
		}
		if r.GetResourceHandle() != "base.main" {
			t.Errorf("resource_handle = %q", r.GetResourceHandle())
		}
		return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_RegisterEndpointResult{
			RegisterEndpointResult: &ipcv1.RegisterEndpointResult{
				RequestId: r.GetRequestId(),
				Outcome: &ipcv1.RegisterEndpointResult_Success{
					Success: &ipcv1.RegisterEndpointSuccess{EndpointId: 3},
				},
			},
		}}}
	}))

	h, err := NewServiceHost(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("NewServiceHost: %v", err)
	}
	defer func() { _ = h.Close() }()

	epID, err := h.RegisterEndpoint(context.Background(), RegisterRequest{
		InterfaceID: "nervus.interface.motion.base", Major: 1, ResourceHandle: "base.main",
	})
	if err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}
	if epID != 3 {
		t.Fatalf("endpoint id = %d, want 3", epID)
	}
}

// **A v1 server must be refused**, not connected to in a degraded mode.
//
// This replaces the old minor gate (1.0's Dispatch carried no ExecutionContext, so Providers
// couldn't authorise, and >=1.1 was required). From v2 on ExecutionContext is always carried, and that gate went away with
// the major, but the "don't talk to v1" constraint got stricter: v2 removed the implicit default of the empty selector,
// so talking to a v1 server would quietly give different results in resource resolution.
func TestServiceHost_RejectsV1Server(t *testing.T) {
	f := startFakeNervud(t)
	f.setHandler(func(_ net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if env.GetHello() == nil {
			return nil
		}
		ack := helloAckOK()
		ack.GetHelloAck().GetSuccess().ProtocolMajor = 1
		ack.GetHelloAck().GetSuccess().ProtocolMinor = 1
		return []*ipcv1.Envelope{ack}
	})

	h, err := NewServiceHost(quietConfig(f.sockPath()))
	if h != nil {
		_ = h.Close()
		t.Fatal("ServiceHost accepted a v1 server")
	}
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("NewServiceHost error = %v, want ErrProtocol", err)
	}
}

// registerThen answers the handshake and registration, then hands the remaining Envelopes to next.
func registerThen(next func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope) func(net.Conn, *ipcv1.Envelope) []*ipcv1.Envelope {
	return autoHandshake(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if r := env.GetRegisterEndpoint(); r != nil {
			return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_RegisterEndpointResult{
				RegisterEndpointResult: &ipcv1.RegisterEndpointResult{
					RequestId: r.GetRequestId(),
					Outcome: &ipcv1.RegisterEndpointResult_Success{
						Success: &ipcv1.RegisterEndpointSuccess{EndpointId: 1},
					},
				},
			}}}
		}
		if next == nil {
			return nil
		}
		return next(c, env)
	})
}

func TestServiceHost_DispatchSuccess(t *testing.T) {
	f := startFakeNervud(t)
	results := make(chan *ipcv1.DispatchResult, 1)
	f.setHandler(registerThen(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if dr := env.GetDispatchResult(); dr != nil {
			results <- dr
			return nil
		}
		return nil
	}))

	h, err := NewServiceHost(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("NewServiceHost: %v", err)
	}
	defer func() { _ = h.Close() }()

	var gotCaller *ipcv1.CallerContext
	var gotExecution *ipcv1.ExecutionContext
	h.Handle(5, func(cc CallContext, payload []byte) ([]byte, error) {
		gotCaller = cc.Caller
		gotExecution = cc.Execution
		return append([]byte("ok:"), payload...), nil
	})

	if _, err := h.RegisterEndpoint(context.Background(), RegisterRequest{InterfaceID: "x", Major: 1}); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}

	// The server delivers a Dispatch. It's injected by the next setHandler call, and the trigger is
	// the UnregisterEndpoint round trip.
	f.setHandler(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if dr := env.GetDispatchResult(); dr != nil {
			results <- dr
			return nil
		}
		if u := env.GetUnregisterEndpoint(); u != nil {
			return []*ipcv1.Envelope{
				{Body: &ipcv1.Envelope_UnregisterEndpointResult{
					UnregisterEndpointResult: &ipcv1.UnregisterEndpointResult{
						RequestId: u.GetRequestId(),
						Outcome: &ipcv1.UnregisterEndpointResult_Success{
							Success: &ipcv1.UnregisterEndpointSuccess{},
						},
					},
				}},
				{Body: &ipcv1.Envelope_Dispatch{Dispatch: &ipcv1.Dispatch{
					RouteId:     77,
					EndpointId:  1,
					MethodId:    5,
					RemainingMs: 2000,
					Payload:     []byte("hi"),
					Caller: &ipcv1.CallerContext{
						PackageId:          "com.caller",
						Uid:                20001,
						TrustProfile:       ipcv1.TrustProfile_TRUST_PROFILE_ORDINARY,
						GrantedPermissions: []string{"perm.motion.control"},
					},
					ExecutionContext: &ipcv1.ExecutionContext{
						LeaseId:            9,
						ControllerClass:    ipcv1.ControllerClass_CONTROLLER_CLASS_HUMAN,
						MotionEpoch:        12,
						DeadlineNanos:      123456,
						CommandSequence:    21,
						ResourceHandle:     "base.main",
						ResourceGeneration: 4,
					},
				}}},
			}
		}
		return nil
	})

	// Use a control round trip unrelated to the endpoint under test to trigger the fake's push. After endpoint 1 is withdrawn,
	// dispatching to 1 is itself an illegal sequence, and the new endpoint isolation correctly removes the handler.
	if err := h.UnregisterEndpoint(context.Background(), 99, false); err != nil {
		t.Fatalf("UnregisterEndpoint: %v", err)
	}

	select {
	case dr := <-results:
		if dr.GetRouteId() != 77 {
			t.Errorf("route_id = %d, want 77 (must echo, not invent)", dr.GetRouteId())
		}
		s := dr.GetSuccess()
		if s == nil {
			t.Fatalf("outcome = %v, want success", dr.GetOutcome())
		}
		if string(s.GetPayload()) != "ok:hi" {
			t.Errorf("payload = %q", s.GetPayload())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no DispatchResult")
	}

	// CallerContext must reach the handler unchanged: the Service relies on it for its second fail-closed check
	if gotCaller.GetPackageId() != "com.caller" || gotCaller.GetUid() != 20001 {
		t.Errorf("caller = %+v", gotCaller)
	}
	if len(gotCaller.GetGrantedPermissions()) != 1 {
		t.Errorf("granted permissions not passed through: %+v", gotCaller.GetGrantedPermissions())
	}
	if gotExecution.GetLeaseId() != 9 || gotExecution.GetMotionEpoch() != 12 ||
		gotExecution.GetCommandSequence() != 21 {
		t.Errorf("execution context not passed through: %+v", gotExecution)
	}
}

func TestValidExecutionContext(t *testing.T) {
	plain := &ipcv1.ExecutionContext{DeadlineNanos: 1}
	controlled := &ipcv1.ExecutionContext{
		LeaseId:            1,
		ControllerClass:    ipcv1.ControllerClass_CONTROLLER_CLASS_AI,
		MotionEpoch:        2,
		DeadlineNanos:      3,
		CommandSequence:    4,
		ResourceHandle:     "arm.main",
		ResourceGeneration: 5,
	}
	tests := []struct {
		name string
		ec   *ipcv1.ExecutionContext
		want bool
	}{
		{name: "plain", ec: plain, want: true},
		{name: "controlled", ec: controlled, want: true},
		{name: "missing", ec: nil},
		{name: "deadline", ec: &ipcv1.ExecutionContext{}},
		{name: "partial resource", ec: &ipcv1.ExecutionContext{DeadlineNanos: 1, ResourceHandle: "arm.main"}},
		{name: "lease without proof", ec: &ipcv1.ExecutionContext{DeadlineNanos: 1, LeaseId: 1}},
		{name: "plain with class", ec: &ipcv1.ExecutionContext{
			DeadlineNanos: 1, ControllerClass: ipcv1.ControllerClass_CONTROLLER_CLASS_HUMAN,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validExecutionContext(tc.ec); got != tc.want {
				t.Fatalf("validExecutionContext() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestServiceHost_UnknownMethodFailsClosed(t *testing.T) {
	// No handler means not implemented. A silent success would let the caller think the action ran:
	// for a robot that means "thinking the dog stopped when it didn't".
	dr := dispatchViaUnregister(t, nil, &ipcv1.Dispatch{
		RouteId: 5, MethodId: 12345, RemainingMs: 1000,
	})
	f := dr.GetFailure()
	if f == nil {
		t.Fatalf("outcome = %v, want failure", dr.GetOutcome())
	}
	if f.GetCode() != ipcv1.StatusCode_STATUS_CODE_NOT_FOUND {
		t.Errorf("code = %v, want NOT_FOUND", f.GetCode())
	}
}

func TestServiceHost_HandlerErrorNormalizedToInternal(t *testing.T) {
	// An arbitrary Go error's string must not cross the process boundary: it would leak paths, dependencies and stack information.
	dr := dispatchViaUnregister(t, func(h *ServiceHost) {
		h.Handle(1, func(cc CallContext, p []byte) ([]byte, error) {
			return nil, errors.New("internal detail: /etc/secret not found")
		})
	}, &ipcv1.Dispatch{RouteId: 6, MethodId: 1, RemainingMs: 1000})

	f := dr.GetFailure()
	if f == nil || f.GetCode() != ipcv1.StatusCode_STATUS_CODE_INTERNAL {
		t.Fatalf("failure = %v, want INTERNAL", f)
	}
	if contains(f.GetPublicMessage(), "secret") {
		t.Fatalf("internal detail leaked to wire: %q", f.GetPublicMessage())
	}
}

func TestServiceHost_StatusErrorPassesCodeAndDetail(t *testing.T) {
	detail := mustMarshal(t, &ipcv1.ControlLeaseErrorDetail{
		Reason: ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_HELD_BY_HUMAN,
	})
	dr := dispatchViaUnregister(t, func(h *ServiceHost) {
		h.Handle(1, func(cc CallContext, p []byte) ([]byte, error) {
			return nil, &StatusError{
				Code:          ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
				PublicMessage: "should not reach the wire",
				Detail:        detail,
			}
		})
	}, &ipcv1.Dispatch{RouteId: 7, MethodId: 1, RemainingMs: 1000})

	f := dr.GetFailure()
	if f == nil || f.GetCode() != ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION {
		t.Fatalf("failure = %v", f)
	}
	// The typed detail must be carried through unchanged: it's the caller's only channel for telling causes apart
	if len(f.GetErrorDetail()) == 0 {
		t.Fatal("error_detail dropped")
	}
	// But public_message **can't** be written by the Provider: the protocol says only nervud may generate it from
	// audited templates, and passing through a Provider's free text is forbidden
	if f.GetPublicMessage() != "" {
		t.Errorf("provider must not set public_message, got %q", f.GetPublicMessage())
	}
}

func TestServiceHost_HandlerPanicBecomesInternal(t *testing.T) {
	// One handler panicking shouldn't take down the whole Service process: in-flight calls to other endpoints in the same process
	// would vanish with it, and nervud would only see a disconnect, unable to attribute it to a specific method.
	dr := dispatchViaUnregister(t, func(h *ServiceHost) {
		h.Handle(1, func(cc CallContext, p []byte) ([]byte, error) {
			panic("boom")
		})
	}, &ipcv1.Dispatch{RouteId: 8, MethodId: 1, RemainingMs: 1000})

	f := dr.GetFailure()
	if f == nil || f.GetCode() != ipcv1.StatusCode_STATUS_CODE_INTERNAL {
		t.Fatalf("panic should become INTERNAL, got %v", f)
	}
}

func TestServiceHost_EndpointScopedHandlers(t *testing.T) {
	results := make(chan *ipcv1.DispatchResult, 2)
	var nextEndpointID uint64
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if r := env.GetRegisterEndpoint(); r != nil {
			nextEndpointID++
			return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_RegisterEndpointResult{
				RegisterEndpointResult: &ipcv1.RegisterEndpointResult{
					RequestId: r.GetRequestId(),
					Outcome: &ipcv1.RegisterEndpointResult_Success{
						Success: &ipcv1.RegisterEndpointSuccess{EndpointId: nextEndpointID},
					},
				},
			}}}
		}
		if u := env.GetUnregisterEndpoint(); u != nil {
			return []*ipcv1.Envelope{
				{Body: &ipcv1.Envelope_UnregisterEndpointResult{
					UnregisterEndpointResult: &ipcv1.UnregisterEndpointResult{
						RequestId: u.GetRequestId(),
						Outcome: &ipcv1.UnregisterEndpointResult_Success{
							Success: &ipcv1.UnregisterEndpointSuccess{},
						},
					},
				}},
				{Body: &ipcv1.Envelope_Dispatch{Dispatch: &ipcv1.Dispatch{
					RouteId: 101, EndpointId: 1, MethodId: 1, RemainingMs: 1000,
					ExecutionContext: testExecutionContext(),
				}}},
				{Body: &ipcv1.Envelope_Dispatch{Dispatch: &ipcv1.Dispatch{
					RouteId: 102, EndpointId: 2, MethodId: 1, RemainingMs: 1000,
					ExecutionContext: testExecutionContext(),
				}}},
			}
		}
		if dr := env.GetDispatchResult(); dr != nil {
			results <- dr
		}
		return nil
	}))

	h, err := NewServiceHost(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("NewServiceHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	camera := h.NewEndpoint(RegisterRequest{InterfaceID: "camera", Major: 1})
	if err := camera.Handle(1, func(cc CallContext, payload []byte) ([]byte, error) {
		if cc.RouteID != 101 || cc.EndpointID != 1 {
			t.Errorf("camera context = route %d endpoint %d", cc.RouteID, cc.EndpointID)
		}
		return []byte("camera"), nil
	}); err != nil {
		t.Fatalf("camera.Handle: %v", err)
	}
	microphone := h.NewEndpoint(RegisterRequest{InterfaceID: "microphone", Major: 1})
	if err := microphone.Handle(1, func(cc CallContext, payload []byte) ([]byte, error) {
		if cc.RouteID != 102 || cc.EndpointID != 2 {
			t.Errorf("microphone context = route %d endpoint %d", cc.RouteID, cc.EndpointID)
		}
		return []byte("microphone"), nil
	}); err != nil {
		t.Fatalf("microphone.Handle: %v", err)
	}
	if _, err := camera.Register(context.Background()); err != nil {
		t.Fatalf("camera.Register: %v", err)
	}
	if _, err := microphone.Register(context.Background()); err != nil {
		t.Fatalf("microphone.Register: %v", err)
	}

	if err := h.UnregisterEndpoint(context.Background(), 99, false); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	got := make(map[uint64]string)
	for len(got) < 2 {
		select {
		case dr := <-results:
			if dr.GetSuccess() == nil {
				t.Fatalf("route %d failed: %v", dr.GetRouteId(), dr.GetFailure())
			}
			got[dr.GetRouteId()] = string(dr.GetSuccess().GetPayload())
		case <-time.After(5 * time.Second):
			t.Fatalf("only received %v", got)
		}
	}
	if got[101] != "camera" || got[102] != "microphone" {
		t.Fatalf("endpoint handlers crossed: %v", got)
	}
}

func TestServiceHost_RegisterInstallsHandlersBeforeFirstDispatch(t *testing.T) {
	results := make(chan *ipcv1.DispatchResult, 1)
	f := startFakeNervud(t)
	f.setHandler(autoHandshake(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if r := env.GetRegisterEndpoint(); r != nil {
			return []*ipcv1.Envelope{
				{Body: &ipcv1.Envelope_RegisterEndpointResult{
					RegisterEndpointResult: &ipcv1.RegisterEndpointResult{
						RequestId: r.GetRequestId(),
						Outcome: &ipcv1.RegisterEndpointResult_Success{
							Success: &ipcv1.RegisterEndpointSuccess{EndpointId: 7},
						},
					},
				}},
				{Body: &ipcv1.Envelope_Dispatch{Dispatch: &ipcv1.Dispatch{
					RouteId: 77, EndpointId: 7, MethodId: 1, RemainingMs: 1000,
					ExecutionContext: testExecutionContext(),
				}}},
			}
		}
		if dr := env.GetDispatchResult(); dr != nil {
			results <- dr
		}
		return nil
	}))

	h, err := NewServiceHost(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("NewServiceHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	endpoint := h.NewEndpoint(RegisterRequest{InterfaceID: "camera", Major: 1})
	if err := endpoint.Handle(1, func(cc CallContext, payload []byte) ([]byte, error) {
		return []byte("ready"), nil
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := endpoint.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	select {
	case dr := <-results:
		if string(dr.GetSuccess().GetPayload()) != "ready" {
			t.Fatalf("first dispatch outcome = %v", dr.GetOutcome())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Dispatch was not handled")
	}
}

func TestServiceHost_HandlerCanCallSystemInterfaceOnSameConnection(t *testing.T) {
	results := make(chan *ipcv1.DispatchResult, 1)
	f := startFakeNervud(t)
	f.setHandler(registerThen(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if u := env.GetUnregisterEndpoint(); u != nil {
			return []*ipcv1.Envelope{
				{Body: &ipcv1.Envelope_UnregisterEndpointResult{
					UnregisterEndpointResult: &ipcv1.UnregisterEndpointResult{
						RequestId: u.GetRequestId(),
						Outcome: &ipcv1.UnregisterEndpointResult_Success{
							Success: &ipcv1.UnregisterEndpointSuccess{},
						},
					},
				}},
				{Body: &ipcv1.Envelope_Dispatch{Dispatch: &ipcv1.Dispatch{
					RouteId: 42, EndpointId: 1, MethodId: 1, RemainingMs: 2000,
					ExecutionContext: testExecutionContext(),
				}}},
			}
		}
		if r := env.GetResolveEndpoint(); r != nil {
			if r.GetInterfaceId() != "nervus.interface.transfer.control" {
				t.Errorf("resolved interface = %q", r.GetInterfaceId())
			}
			return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_ResolveEndpointResult{
				ResolveEndpointResult: &ipcv1.ResolveEndpointResult{
					RequestId: r.GetRequestId(),
					Outcome: &ipcv1.ResolveEndpointResult_Success{
						Success: &ipcv1.ResolveEndpointSuccess{
							EndpointId: 55, InterfaceMajor: 1,
						},
					},
				},
			}}}
		}
		if r := env.GetRequest(); r != nil {
			if r.GetEndpointId() != 55 || r.GetMethodId() != 1 || string(r.GetPayload()) != "begin:42" {
				t.Errorf("provider callback = endpoint %d method %d payload %q",
					r.GetEndpointId(), r.GetMethodId(), r.GetPayload())
			}
			return []*ipcv1.Envelope{{Body: &ipcv1.Envelope_Response{
				Response: &ipcv1.Response{
					RequestId: r.GetRequestId(),
					Outcome: &ipcv1.Response_Success{
						Success: &ipcv1.Success{
							Code: ipcv1.StatusCode_STATUS_CODE_OK, Payload: []byte("transfer-ready"),
						},
					},
				},
			}}}
		}
		if dr := env.GetDispatchResult(); dr != nil {
			results <- dr
		}
		return nil
	}))

	h, err := NewServiceHost(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("NewServiceHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	h.Handle(1, func(cc CallContext, payload []byte) ([]byte, error) {
		endpoint, err := h.ResolveEndpoint(cc.Ctx, ResolveRequest{
			InterfaceID: "nervus.interface.transfer.control",
			MinMajor:    1,
			MaxMajor:    1,
		})
		if err != nil {
			return nil, err
		}
		return h.Call(cc.Ctx, endpoint.EndpointID, 1, []byte("begin:42"), time.Second)
	})
	if _, err := h.RegisterEndpoint(context.Background(), RegisterRequest{InterfaceID: "camera", Major: 1}); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}
	if err := h.UnregisterEndpoint(context.Background(), 99, false); err != nil {
		t.Fatalf("trigger: %v", err)
	}

	select {
	case dr := <-results:
		if string(dr.GetSuccess().GetPayload()) != "transfer-ready" {
			t.Fatalf("provider callback outcome = %v", dr.GetOutcome())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider callback deadlocked with ServiceHost reader")
	}
}

func TestServiceHost_CancelDispatchCancelsHandler(t *testing.T) {
	results := make(chan *ipcv1.DispatchResult, 1)
	f := startFakeNervud(t)
	f.setHandler(registerThen(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if u := env.GetUnregisterEndpoint(); u != nil {
			return []*ipcv1.Envelope{
				{Body: &ipcv1.Envelope_UnregisterEndpointResult{
					UnregisterEndpointResult: &ipcv1.UnregisterEndpointResult{
						RequestId: u.GetRequestId(),
						Outcome: &ipcv1.UnregisterEndpointResult_Success{
							Success: &ipcv1.UnregisterEndpointSuccess{},
						},
					},
				}},
				{Body: &ipcv1.Envelope_Dispatch{Dispatch: &ipcv1.Dispatch{
					RouteId: 88, EndpointId: 1, MethodId: 1,
					ExecutionContext: testExecutionContext(),
				}}},
				{Body: &ipcv1.Envelope_CancelDispatch{CancelDispatch: &ipcv1.CancelDispatch{
					RouteId: 88,
					Reason:  ipcv1.CancelDispatchReason_CANCEL_DISPATCH_REASON_CLIENT_CANCELLED,
				}}},
			}
		}
		if dr := env.GetDispatchResult(); dr != nil {
			results <- dr
		}
		return nil
	}))

	h, err := NewServiceHost(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("NewServiceHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	h.Handle(1, func(cc CallContext, payload []byte) ([]byte, error) {
		<-cc.Ctx.Done()
		return nil, cc.Ctx.Err()
	})
	if _, err := h.RegisterEndpoint(context.Background(), RegisterRequest{InterfaceID: "x", Major: 1}); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}
	if err := h.UnregisterEndpoint(context.Background(), 99, false); err != nil {
		t.Fatalf("trigger: %v", err)
	}

	select {
	case dr := <-results:
		if dr.GetFailure().GetCode() != ipcv1.StatusCode_STATUS_CODE_CANCELLED {
			t.Fatalf("cancel outcome = %v", dr.GetOutcome())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled Dispatch did not finish")
	}
	select {
	case <-h.readDone:
		t.Fatalf("CancelDispatch closed the service connection: %v", h.readErr)
	default:
	}
}

// dispatchViaUnregister is the shared fixture for these cases: start the host, register the handler, register the endpoint,
// then use one UnregisterEndpoint round trip to get the server to push the Dispatch, and collect the DispatchResult.
//
// It borrows a round trip rather than having fakeNervud push on its own, because the test double's handler is "reply on receipt"
// request-response, with no independent moment to push. The real nervud can push any time; the difference doesn't affect
// the logic under test (ServiceHost's read loop makes no assumption about when a Dispatch arrives).
func dispatchViaUnregister(t *testing.T, register func(*ServiceHost), d *ipcv1.Dispatch) *ipcv1.DispatchResult {
	t.Helper()

	results := make(chan *ipcv1.DispatchResult, 1)
	f := startFakeNervud(t)
	f.setHandler(registerThen(nil))

	h, err := NewServiceHost(quietConfig(f.sockPath()))
	if err != nil {
		t.Fatalf("NewServiceHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	if register != nil {
		register(h)
	}
	if _, err := h.RegisterEndpoint(context.Background(), RegisterRequest{InterfaceID: "x", Major: 1}); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}
	if d.GetEndpointId() == 0 {
		d.EndpointId = 1
	}
	if d.GetExecutionContext() == nil {
		d.ExecutionContext = &ipcv1.ExecutionContext{DeadlineNanos: 1}
	}

	f.setHandler(func(c net.Conn, env *ipcv1.Envelope) []*ipcv1.Envelope {
		if dr := env.GetDispatchResult(); dr != nil {
			select {
			case results <- dr:
			default:
			}
			return nil
		}
		if u := env.GetUnregisterEndpoint(); u != nil {
			return []*ipcv1.Envelope{
				{Body: &ipcv1.Envelope_UnregisterEndpointResult{
					UnregisterEndpointResult: &ipcv1.UnregisterEndpointResult{
						RequestId: u.GetRequestId(),
						Outcome: &ipcv1.UnregisterEndpointResult_Success{
							Success: &ipcv1.UnregisterEndpointSuccess{},
						},
					},
				}},
				{Body: &ipcv1.Envelope_Dispatch{Dispatch: d}},
			}
		}
		return nil
	})

	if err := h.UnregisterEndpoint(context.Background(), 99, false); err != nil {
		t.Fatalf("UnregisterEndpoint: %v", err)
	}

	select {
	case dr := <-results:
		return dr
	case <-time.After(5 * time.Second):
		t.Fatal("no DispatchResult received")
		return nil
	}
}

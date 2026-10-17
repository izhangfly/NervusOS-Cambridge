package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nervus-os/nervus-gazebo-provider/internal/contract"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"github.com/nervus-os/nervus-ipc/sdk"
)

type fakePublishedEvent struct {
	id        uint32
	payload   []byte
	timestamp uint64
}

type fakeEndpoint struct {
	mu               sync.Mutex
	req              sdk.RegisterRequest
	handlers         map[uint32]sdk.Handler
	expectedHandlers int
	registered       bool
	registerCount    int
	unregisterCount  int
	unregisterDrains []bool
	published        []fakePublishedEvent
	host             *fakeHost
	id               uint64
}

func (e *fakeEndpoint) Handle(methodID uint32, handler sdk.Handler) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.registered {
		return errors.New("fake endpoint: handler installed after registration")
	}
	if methodID == 0 || handler == nil {
		return errors.New("fake endpoint: invalid handler")
	}
	if _, duplicate := e.handlers[methodID]; duplicate {
		return fmt.Errorf("fake endpoint: duplicate method %d", methodID)
	}
	e.handlers[methodID] = handler
	return nil
}

func (e *fakeEndpoint) Register(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.registered {
		return 0, errors.New("fake endpoint: already registered")
	}
	if len(e.handlers) != e.expectedHandlers {
		return 0, fmt.Errorf(
			"fake endpoint: registered with %d handlers, want %d",
			len(e.handlers),
			e.expectedHandlers,
		)
	}
	e.registered = true
	e.registerCount++
	e.host.record(e.req.InterfaceID + ".register")
	return e.id, nil
}

func (e *fakeEndpoint) Unregister(ctx context.Context, drain bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.registered {
		return errors.New("fake endpoint: not registered")
	}
	e.registered = false
	e.unregisterCount++
	e.unregisterDrains = append(e.unregisterDrains, drain)
	e.host.record(e.req.InterfaceID + ".unregister")
	return nil
}

func (e *fakeEndpoint) PublishEventAt(id uint32, payload []byte, timestamp uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.registered {
		return errors.New("fake endpoint: event published while unregistered")
	}
	e.published = append(e.published, fakePublishedEvent{
		id:        id,
		payload:   append([]byte(nil), payload...),
		timestamp: timestamp,
	})
	return nil
}

func (e *fakeEndpoint) invoke(
	methodID uint32,
	cc sdk.CallContext,
	request []byte,
) ([]byte, error) {
	e.mu.Lock()
	handler := e.handlers[methodID]
	e.mu.Unlock()
	if handler == nil {
		return nil, fmt.Errorf("fake endpoint: method %d is not installed", methodID)
	}
	return handler(cc, request)
}

func (e *fakeEndpoint) snapshot() (registered bool, registers, unregisters int, events []fakePublishedEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.registered, e.registerCount, e.unregisterCount, append([]fakePublishedEvent(nil), e.published...)
}

type fakeHost struct {
	mu        sync.Mutex
	endpoints map[string]*fakeEndpoint
	nextID    uint64
	order     []string
	serveDone chan struct{}
	serveErr  error
	closeOnce sync.Once
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		endpoints: make(map[string]*fakeEndpoint),
		nextID:    100,
		serveDone: make(chan struct{}),
	}
}

func (h *fakeHost) NewEndpoint(req sdk.RegisterRequest) Endpoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	want := 0
	switch req.InterfaceID {
	case contract.ManagerInterface:
		want = 8
	case contract.ControlInterface:
		want = 6
	}
	ep := &fakeEndpoint{
		req:              req,
		handlers:         make(map[uint32]sdk.Handler),
		expectedHandlers: want,
		host:             h,
		id:               h.nextID,
	}
	h.endpoints[req.InterfaceID] = ep
	return ep
}

func (h *fakeHost) Serve() error {
	<-h.serveDone
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.serveErr
}

func (h *fakeHost) Close() error {
	h.record("host.close")
	h.closeOnce.Do(func() { close(h.serveDone) })
	return nil
}

func (h *fakeHost) endpoint(interfaceID string) *fakeEndpoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.endpoints[interfaceID]
}

func (h *fakeHost) record(value string) {
	h.mu.Lock()
	h.order = append(h.order, value)
	h.mu.Unlock()
}

func (h *fakeHost) orderSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.order...)
}

type fakeBackend struct {
	mu sync.Mutex

	status       simulation.Status
	statusErr    error
	capabilities simulation.Capabilities
	models       simulation.ModelList
	modelsErr    error
	joints       simulation.JointList
	jointsErr    error
	jointState   simulation.JointState
	jointErr     error
	bindings     []simulation.MotionBinding
	bindingsErr  error

	startStatus simulation.Status
	startErr    error
	stopStatus  simulation.Status
	stopErr     error
	loadStatus  simulation.Status
	loadErr     error

	controlStatus simulation.Status
	controlErr    error
	receipt       simulation.CommandReceipt
	receiptErr    error

	lastStart       simulation.StartOptions
	lastLoadedWorld string
	lastControl     simulation.ControlContext
	lastStep        uint32
	lastReset       simulation.ResetMode
	lastCommands    []simulation.JointCommand
	callCounts      map[string]int

	events chan simulation.Event
	closed bool
	host   *fakeHost
}

func newFakeBackend(status simulation.Status, host *fakeHost) *fakeBackend {
	return &fakeBackend{
		status:        status,
		startStatus:   status,
		stopStatus:    status,
		loadStatus:    status,
		controlStatus: status,
		receipt: simulation.CommandReceipt{
			SubmittedCommands: 1,
			FeedbackAvailable: true,
		},
		callCounts: make(map[string]int),
		events:     make(chan simulation.Event, 32),
		host:       host,
	}
}

func (b *fakeBackend) called(name string) {
	b.callCounts[name]++
}

func (b *fakeBackend) Status(context.Context) (simulation.Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.called("status")
	return b.status, b.statusErr
}

func (b *fakeBackend) Capabilities(context.Context) (simulation.Capabilities, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.called("capabilities")
	return b.capabilities, nil
}

func (b *fakeBackend) ListModels(context.Context, uint64) (simulation.ModelList, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.called("list_models")
	return b.models, b.modelsErr
}

func (b *fakeBackend) ListJoints(context.Context, string, uint64) (simulation.JointList, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.called("list_joints")
	return b.joints, b.jointsErr
}

func (b *fakeBackend) JointState(context.Context, simulation.JointQuery) (simulation.JointState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.called("joint_state")
	return b.jointState, b.jointErr
}

func (b *fakeBackend) MotionBindings(context.Context) ([]simulation.MotionBinding, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]simulation.MotionBinding(nil), b.bindings...), b.bindingsErr
}

func (b *fakeBackend) Start(_ context.Context, options simulation.StartOptions) (simulation.Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.called("start")
	b.lastStart = options
	return b.startStatus, b.startErr
}

func (b *fakeBackend) Stop(context.Context) (simulation.Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.called("stop")
	return b.stopStatus, b.stopErr
}

func (b *fakeBackend) LoadWorld(_ context.Context, world string) (simulation.Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.called("load")
	b.lastLoadedWorld = world
	return b.loadStatus, b.loadErr
}

func (b *fakeBackend) Pause(_ context.Context, execution simulation.ControlContext) (simulation.Status, error) {
	return b.recordControl("pause", execution, 0, simulation.ResetUnspecified)
}

func (b *fakeBackend) Resume(_ context.Context, execution simulation.ControlContext) (simulation.Status, error) {
	return b.recordControl("resume", execution, 0, simulation.ResetUnspecified)
}

func (b *fakeBackend) Step(
	_ context.Context,
	execution simulation.ControlContext,
	iterations uint32,
) (simulation.Status, error) {
	return b.recordControl("step", execution, iterations, simulation.ResetUnspecified)
}

func (b *fakeBackend) ResetWorld(
	_ context.Context,
	execution simulation.ControlContext,
	mode simulation.ResetMode,
) (simulation.Status, error) {
	return b.recordControl("reset", execution, 0, mode)
}

func (b *fakeBackend) recordControl(
	name string,
	execution simulation.ControlContext,
	iterations uint32,
	mode simulation.ResetMode,
) (simulation.Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.called(name)
	b.lastControl = execution
	b.lastStep = iterations
	b.lastReset = mode
	return b.controlStatus, b.controlErr
}

func (b *fakeBackend) SetJointPositions(
	_ context.Context,
	execution simulation.ControlContext,
	commands []simulation.JointCommand,
) (simulation.CommandReceipt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.called("set_joints")
	b.lastControl = execution
	b.lastCommands = append([]simulation.JointCommand(nil), commands...)
	receipt := b.receipt
	if b.receiptErr == nil {
		receipt.SubmittedCommands = uint32(len(commands))
	}
	return receipt, b.receiptErr
}

func (b *fakeBackend) Events() <-chan simulation.Event { return b.events }

func (b *fakeBackend) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	if b.host != nil {
		b.host.record("backend.close")
	}
	return nil
}

func (b *fakeBackend) calls(name string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.callCounts[name]
}

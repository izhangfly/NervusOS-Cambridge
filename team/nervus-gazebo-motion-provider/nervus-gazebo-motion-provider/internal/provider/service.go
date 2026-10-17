package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	simulationv1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/provider/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/contract"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"github.com/nervus-os/nervus-ipc/sdk"
)

type Service struct {
	host    Host
	backend simulation.Backend
	config  Config
	log     *slog.Logger

	manager Endpoint
	control Endpoint
	base    Endpoint

	lifecycleMu       sync.Mutex
	managerRegistered bool
	controlRegistered bool
	baseRegistered    bool
	runStarted        bool

	callsMu sync.Mutex
	closing bool
	calls   sync.WaitGroup

	controlGate chan struct{}
	sequenceMu  sync.Mutex
	sequences   map[string]sequenceState
}

type sequenceState struct{ generation, epoch, sequence uint64 }

func New(host Host, backend simulation.Backend, config Config, log *slog.Logger) (*Service, error) {
	if host == nil {
		return nil, errors.New("provider: nil ServiceHost")
	}
	if backend == nil {
		return nil, errors.New("provider: nil simulation backend")
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}

	managerHash, controlHash, err := contract.SchemaHashes()
	if err != nil {
		return nil, err
	}
	baseHash, _, err := contract.StandardSchemaHashes()
	if err != nil {
		return nil, err
	}
	s := &Service{
		host:        host,
		backend:     backend,
		config:      normalized,
		log:         log,
		controlGate: make(chan struct{}, 1),
		sequences:   make(map[string]sequenceState),
	}
	s.manager = host.NewEndpoint(sdk.RegisterRequest{
		InterfaceID: contract.ManagerInterface,
		Major:       contract.InterfaceMajor,
		SchemaHash:  managerHash,
	})
	s.control = host.NewEndpoint(sdk.RegisterRequest{
		InterfaceID:    contract.ControlInterface,
		Major:          contract.InterfaceMajor,
		SchemaHash:     controlHash,
		ResourceHandle: contract.WorldResourceRole,
	})
	s.base = host.NewEndpoint(sdk.RegisterRequest{
		InterfaceID:    contract.BaseInterface,
		Major:          contract.InterfaceMajor,
		SchemaHash:     baseHash,
		ResourceHandle: contract.BaseResourceRole,
	})
	if err := s.installManagerHandlers(); err != nil {
		return nil, err
	}
	if err := s.installControlHandlers(); err != nil {
		return nil, err
	}
	if err := s.installBaseHandlers(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) installManagerHandlers() error {
	handlers := map[simulationv1.ManagerMethod]sdk.Handler{
		simulationv1.ManagerMethod_MANAGER_METHOD_GET_STATUS:       s.getStatus,
		simulationv1.ManagerMethod_MANAGER_METHOD_LIST_MODELS:      s.listModels,
		simulationv1.ManagerMethod_MANAGER_METHOD_LIST_JOINTS:      s.listJoints,
		simulationv1.ManagerMethod_MANAGER_METHOD_GET_JOINT_STATE:  s.getJointState,
		simulationv1.ManagerMethod_MANAGER_METHOD_START_SIMULATION: s.startSimulation,
		simulationv1.ManagerMethod_MANAGER_METHOD_STOP_SIMULATION:  s.stopSimulation,
		simulationv1.ManagerMethod_MANAGER_METHOD_LOAD_WORLD:       s.loadWorld,
		simulationv1.ManagerMethod_MANAGER_METHOD_GET_CAPABILITIES: s.getCapabilities,
	}
	for method, handler := range handlers {
		if err := s.manager.Handle(uint32(method), s.track(handler)); err != nil {
			return fmt.Errorf("provider: install manager method %s: %w", method, err)
		}
	}
	return nil
}

func (s *Service) installControlHandlers() error {
	handlers := map[simulationv1.ControlMethod]sdk.Handler{
		simulationv1.ControlMethod_CONTROL_METHOD_PAUSE:                s.pause,
		simulationv1.ControlMethod_CONTROL_METHOD_RESUME:               s.resume,
		simulationv1.ControlMethod_CONTROL_METHOD_STEP:                 s.step,
		simulationv1.ControlMethod_CONTROL_METHOD_SET_JOINT_POSITION:   s.setJointPosition,
		simulationv1.ControlMethod_CONTROL_METHOD_SET_JOINT_POSITIONS:  s.setJointPositions,
		simulationv1.ControlMethod_CONTROL_METHOD_RESET_WORLD:          s.resetWorld,
		simulationv1.ControlMethod_CONTROL_METHOD_SET_JOINT_TRAJECTORY: s.setJointTrajectory,
		simulationv1.ControlMethod_CONTROL_METHOD_GO_HOME:              s.goHome,
		simulationv1.ControlMethod_CONTROL_METHOD_STOP_MOTION:          s.stopJointMotion,
		simulationv1.ControlMethod_CONTROL_METHOD_STOP_ALL_MOTION:      s.stopAllMotion,
	}
	for method, handler := range handlers {
		if err := s.control.Handle(uint32(method), s.track(handler)); err != nil {
			return fmt.Errorf("provider: install control method %s: %w", method, err)
		}
	}
	return nil
}

func (s *Service) track(handler sdk.Handler) sdk.Handler {
	return func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		s.callsMu.Lock()
		if s.closing {
			s.callsMu.Unlock()
			return nil, statusError(
				simulation.ReasonProviderNotReady,
				"service.shutting_down",
				0,
				true,
			)
		}
		s.calls.Add(1)
		s.callsMu.Unlock()
		defer s.calls.Done()
		return handler(cc, payload)
	}
}

// Run registers the manager immediately, conditionally exposes control when
// the backend is ready, and blocks until shutdown or ServiceHost failure.
func (s *Service) Run(ctx context.Context) (runErr error) {
	if ctx == nil {
		return errors.New("provider: nil run context")
	}
	s.lifecycleMu.Lock()
	if s.runStarted {
		s.lifecycleMu.Unlock()
		return errors.New("provider: service already started")
	}
	s.runStarted = true
	s.lifecycleMu.Unlock()

	defer func() {
		if err := s.shutdown(); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()

	registerCtx, cancel := context.WithTimeout(ctx, s.config.EndpointTimeout)
	_, err := s.manager.Register(registerCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("provider: register manager endpoint: %w", err)
	}
	s.lifecycleMu.Lock()
	s.managerRegistered = true
	s.lifecycleMu.Unlock()

	statusCtx, cancel := context.WithTimeout(ctx, s.config.EndpointTimeout)
	status, err := s.backend.Status(statusCtx)
	cancel()
	if err != nil {
		s.log.Warn("gazebo provider: initial status unavailable", "err", err)
	} else if err := s.applyStatus(ctx, status, 0); err != nil {
		return err
	}

	served := make(chan error, 1)
	go func() { served <- s.host.Serve() }()
	events := s.backend.Events()
	for {
		select {
		case <-ctx.Done():
			s.log.Info("gazebo provider: shutdown requested")
			return nil
		case err := <-served:
			if err == nil || errors.Is(err, sdk.ErrClosed) {
				return nil
			}
			return fmt.Errorf("provider: ServiceHost disconnected: %w", err)
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if err := s.handleEvent(ctx, event); err != nil {
				return err
			}
		}
	}
}

func (s *Service) applyStatus(ctx context.Context, status simulation.Status, timestamp uint64) error {
	if err := s.validateMotionBindings(ctx); err != nil {
		return err
	}
	if err := s.reconcileControl(ctx, status.ReadyForControl()); err != nil {
		return err
	}
	if err := s.reconcileBase(ctx, status.ReadyForControl()); err != nil {
		return err
	}
	payload, err := marshalStatus(status)
	if err != nil {
		return err
	}
	if err := s.publishManager(
		uint32(simulationv1.ManagerEvent_MANAGER_EVENT_STATUS_CHANGED),
		payload,
		timestamp,
	); err != nil {
		s.log.Debug("gazebo provider: publish status failed", "err", err)
	}
	return nil
}

func (s *Service) validateMotionBindings(ctx context.Context) error {
	backend, ok := s.backend.(simulation.MotionBindingProvider)
	if !ok {
		return nil
	}
	bindings, err := backend.MotionBindings(ctx)
	if err != nil {
		return fmt.Errorf("provider: query motion bindings: %w", err)
	}
	allowed := contract.MotionProfileAllowlist()
	for _, binding := range bindings {
		if binding.ResourceRole != "" {
			slot, exists := allowed.Slots[binding.ResourceRole]
			if !exists || slot.InterfaceID != binding.InterfaceID {
				return fmt.Errorf(
					"provider: motion binding %s/%s is outside signed descriptor",
					binding.ProfileID, binding.CapabilityRole,
				)
			}
		}
		if binding.State == simulation.MotionBindingReady && binding.InterfaceID == contract.BaseInterface {
			if _, supported := s.backend.(simulation.BaseMotionBackend); !supported {
				return fmt.Errorf("provider: standard BaseMotion binding %s/%s has no backend implementation", binding.ProfileID, binding.CapabilityRole)
			}
		}
		if binding.State == simulation.MotionBindingReady && binding.InterfaceID == contract.ManipulatorInterface {
			// Task 7 signs finite standard Resource slots, but no standard
			// endpoint may be registered until its complete semantic handler
			// set exists. A backend claiming READY early is a startup error.
			return fmt.Errorf(
				"provider: standard motion binding %s/%s became ready without an endpoint implementation",
				binding.ProfileID, binding.CapabilityRole,
			)
		}
	}
	return nil
}

func (s *Service) reconcileBase(ctx context.Context, ready bool) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	eligible := false
	if ready {
		if _, supported := s.backend.(simulation.BaseMotionBackend); supported {
			if provider, ok := s.backend.(simulation.MotionBindingProvider); ok {
				bindings, err := provider.MotionBindings(ctx)
				if err != nil {
					return fmt.Errorf("provider: query BaseMotion bindings: %w", err)
				}
				count := 0
				for _, binding := range bindings {
					if binding.ResourceRole == contract.BaseResourceRole && binding.InterfaceID == contract.BaseInterface && binding.State == simulation.MotionBindingReady {
						count++
					}
				}
				eligible = count == 1
			}
		}
	}
	if eligible == s.baseRegistered {
		return nil
	}
	opCtx, cancel := boundedContext(ctx, s.config.EndpointTimeout)
	defer cancel()
	if eligible {
		if _, err := s.base.Register(opCtx); err != nil {
			return fmt.Errorf("provider: register BaseMotion endpoint: %w", err)
		}
		s.baseRegistered = true
		s.log.Info("gazebo provider: BaseMotion endpoint registered")
		return nil
	}
	if err := s.base.Unregister(opCtx, true); err != nil {
		return fmt.Errorf("provider: unregister BaseMotion endpoint: %w", err)
	}
	s.baseRegistered = false
	s.resetSequenceFor(contract.BaseResourceRole)
	s.log.Info("gazebo provider: BaseMotion endpoint unregistered")
	return nil
}

func (s *Service) reconcileControl(ctx context.Context, ready bool) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if ready == s.controlRegistered {
		return nil
	}
	opCtx, cancel := boundedContext(ctx, s.config.EndpointTimeout)
	defer cancel()
	if ready {
		endpointID, err := s.control.Register(opCtx)
		if err != nil {
			return fmt.Errorf("provider: register control endpoint: %w", err)
		}
		s.controlRegistered = true
		s.log.Info("gazebo provider: control endpoint registered", "endpoint_id", endpointID)
		return nil
	}
	if err := s.control.Unregister(opCtx, true); err != nil {
		return fmt.Errorf("provider: unregister control endpoint: %w", err)
	}
	s.controlRegistered = false
	s.resetSequence()
	s.log.Info("gazebo provider: control endpoint unregistered")
	return nil
}

func (s *Service) publishManager(eventID uint32, payload []byte, timestamp uint64) error {
	s.lifecycleMu.Lock()
	registered := s.managerRegistered
	s.lifecycleMu.Unlock()
	if !registered {
		return errors.New("provider: manager endpoint is not registered")
	}
	return s.manager.PublishEventAt(eventID, payload, timestamp)
}

func (s *Service) publishControl(eventID uint32, payload []byte, timestamp uint64) error {
	s.lifecycleMu.Lock()
	registered := s.controlRegistered
	s.lifecycleMu.Unlock()
	if !registered {
		return errors.New("provider: control endpoint is not registered")
	}
	return s.control.PublishEventAt(eventID, payload, timestamp)
}

func (s *Service) shutdown() error {
	s.callsMu.Lock()
	s.closing = true
	s.callsMu.Unlock()

	var result error
	s.lifecycleMu.Lock()
	if s.baseRegistered {
		ctx, cancel := context.WithTimeout(context.Background(), s.config.ShutdownTimeout)
		if err := s.base.Unregister(ctx, true); err != nil {
			result = errors.Join(result, fmt.Errorf("provider: unregister BaseMotion during shutdown: %w", err))
		}
		cancel()
		s.baseRegistered = false
	}
	if s.controlRegistered {
		ctx, cancel := context.WithTimeout(context.Background(), s.config.ShutdownTimeout)
		if err := s.control.Unregister(ctx, true); err != nil {
			result = errors.Join(result, fmt.Errorf("provider: unregister control during shutdown: %w", err))
		}
		cancel()
		s.controlRegistered = false
	}
	s.lifecycleMu.Unlock()

	if err := s.waitForCalls(); err != nil {
		result = errors.Join(result, err)
	}
	if err := s.backend.Close(); err != nil {
		result = errors.Join(result, fmt.Errorf("provider: close simulation backend: %w", err))
	}

	s.lifecycleMu.Lock()
	if s.managerRegistered {
		ctx, cancel := context.WithTimeout(context.Background(), s.config.ShutdownTimeout)
		if err := s.manager.Unregister(ctx, true); err != nil {
			result = errors.Join(result, fmt.Errorf("provider: unregister manager during shutdown: %w", err))
		}
		cancel()
		s.managerRegistered = false
	}
	s.lifecycleMu.Unlock()

	if err := s.host.Close(); err != nil && !errors.Is(err, sdk.ErrClosed) {
		result = errors.Join(result, fmt.Errorf("provider: close ServiceHost: %w", err))
	}
	return result
}

func (s *Service) waitForCalls() error {
	done := make(chan struct{})
	go func() {
		s.calls.Wait()
		close(done)
	}()
	timer := time.NewTimer(s.config.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return errors.New("provider: timed out waiting for handlers to stop")
	}
}

func boundedContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, timeout)
}

func (s *Service) resetSequence() {
	s.resetSequenceFor(contract.WorldResourceRole)
}

func (s *Service) resetSequenceFor(resource string) {
	s.sequenceMu.Lock()
	delete(s.sequences, resource)
	s.sequenceMu.Unlock()
}

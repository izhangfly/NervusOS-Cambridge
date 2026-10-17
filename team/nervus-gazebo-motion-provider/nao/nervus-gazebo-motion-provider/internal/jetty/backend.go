package jetty

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simprocess"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

type controller struct {
	topic string
	joint simulation.Joint
}

type sceneSnapshot struct {
	models       []simulation.Model
	joints       map[string][]simulation.Joint
	controllers  map[string]controller
	stateSources []stateSource
	bindings     []simulation.MotionBinding
}

type stateSource struct {
	topic       string
	messageType string
	model       string
	optional    bool
}

type subscriptionKind uint8

const (
	subscriptionUnknown subscriptionKind = iota
	subscriptionStats
	subscriptionJointState
)

type subscription struct {
	kind  subscriptionKind
	model string
}

type Backend struct {
	client  gztransport.Client
	config  Config
	log     *slog.Logger
	process simulation.ProcessController
	wslg    simulation.WSLgProbe

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu                   sync.RWMutex
	status               simulation.Status
	lastStatsAt          time.Time
	statsSequence        uint64
	signal               chan struct{}
	scene                sceneSnapshot
	jointStates          map[string]simulation.JointState
	pendingFeedback      map[string]uint64
	subscriptions        map[gztransport.SubscriptionID]subscription
	feedbackModels       map[string]bool
	targetWorld          string
	cycleCancel          context.CancelFunc
	processWake          chan struct{}
	activeWSLgFaults     map[simulation.WSLgFaultKind]struct{}
	transportFaultActive bool
	transportFaultReason simulation.Reason

	events chan simulation.Event

	closeOnce sync.Once
	closeErr  error
}

func New(config Config, client gztransport.Client, log *slog.Logger) (*Backend, error) {
	world := config.World
	if world == "" {
		world = defaultWorld
	}
	process := simprocess.NewAttach([]string{world})
	return NewWithLifecycle(config, client, process, nil, log)
}

func NewWithLifecycle(
	config Config,
	client gztransport.Client,
	process simulation.ProcessController,
	wslgProbe simulation.WSLgProbe,
	log *slog.Logger,
) (*Backend, error) {
	if client == nil {
		return nil, errors.New("jetty: nil Gazebo Transport client")
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	if process == nil {
		return nil, errors.New("jetty: nil process controller")
	}
	if process.Mode() != normalized.Mode {
		return nil, errors.New("jetty: process controller mode does not match config")
	}

	ctx, cancel := context.WithCancel(context.Background())
	versions := client.Versions()
	processStatus := process.Snapshot()
	b := &Backend{
		client:  client,
		config:  normalized,
		log:     log,
		process: process,
		wslg:    wslgProbe,
		ctx:     ctx,
		cancel:  cancel,
		status: simulation.Status{
			Mode:             normalized.Mode,
			Process:          processStatus.State,
			Transport:        simulation.TransportDisconnected,
			Run:              simulation.RunUnspecified,
			ProcessOwned:     processStatus.Owned,
			ManagedProcessID: processStatus.ProcessID,
			HasLastExitCode:  processStatus.HasLastExitCode,
			LastExitCode:     processStatus.LastExitCode,
			LastFault:        simulation.ReasonProviderNotReady,
			Versions: simulation.Versions{
				GazeboTransport: versions.Transport,
				GazeboMessages:  versions.Messages,
				Adapter:         fmt.Sprintf("c-abi/%d", versions.ABI),
			},
		},
		signal:           make(chan struct{}),
		jointStates:      make(map[string]simulation.JointState),
		pendingFeedback:  make(map[string]uint64),
		subscriptions:    make(map[gztransport.SubscriptionID]subscription),
		feedbackModels:   make(map[string]bool),
		events:           make(chan simulation.Event, normalized.EventBuffer),
		targetWorld:      normalized.World,
		processWake:      make(chan struct{}, 1),
		activeWSLgFaults: make(map[simulation.WSLgFaultKind]struct{}),
	}
	b.wg.Add(2)
	go b.run()
	go b.watchProcess()
	if b.wslg != nil {
		b.wg.Add(1)
		go b.monitorWSLg()
	}
	return b, nil
}

func (b *Backend) Status(context.Context) (simulation.Status, error) {
	b.mu.RLock()
	status := b.status
	lastStatsAt := b.lastStatsAt
	b.mu.RUnlock()
	if status.Statistics.Valid && !lastStatsAt.IsZero() {
		age := time.Since(lastStatsAt)
		if age < 0 {
			age = 0
		}
		status.Statistics.SampleAgeMS = uint64(age / time.Millisecond)
	}
	return status, nil
}

func (b *Backend) Capabilities(context.Context) (simulation.Capabilities, error) {
	b.mu.RLock()
	status := b.status
	controllerCount := len(b.scene.controllers)
	feedback := len(b.feedbackModels) > 0
	b.mu.RUnlock()
	ready := status.ReadyForControl()
	return simulation.Capabilities{
		Mode:                       b.config.Mode,
		CanManageProcess:           b.config.Mode == simulation.ModeManaged,
		CanLoadWorld:               b.config.Mode == simulation.ModeManaged,
		CanResetWorld:              ready,
		CanControlWorld:            ready,
		CanQueryScene:              status.Transport == simulation.TransportReady,
		CanCommandJoints:           ready && controllerCount > 0,
		JointStateFeedback:         ready && feedback,
		SensorTopics:               false,
		AllowedWorlds:              b.process.AllowedWorlds(),
		MaxStepIterations:          b.config.MaxStepIterations,
		MaxJointCommandsPerRequest: b.config.MaxJointCommands,
	}, nil
}

func (b *Backend) Events() <-chan simulation.Event { return b.events }

func (b *Backend) Close() error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		b.cancel()
		if err := b.process.Close(); err != nil {
			b.closeErr = errors.Join(b.closeErr, err)
		}
		b.wg.Wait()

		b.mu.RLock()
		ids := make([]gztransport.SubscriptionID, 0, len(b.subscriptions))
		for id := range b.subscriptions {
			ids = append(ids, id)
		}
		b.mu.RUnlock()
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := b.client.Unsubscribe(ctx, id)
			cancel()
			if err != nil && !errors.Is(err, gztransport.ErrNotFound) &&
				!errors.Is(err, gztransport.ErrClosed) {
				b.closeErr = errors.Join(b.closeErr, fmt.Errorf("unsubscribe %d: %w", id, err))
			}
		}
		if err := b.client.Close(); err != nil && !errors.Is(err, gztransport.ErrClosed) {
			b.closeErr = errors.Join(b.closeErr, err)
		}
		close(b.events)
	})
	return b.closeErr
}

func (b *Backend) snapshot() (simulation.Status, <-chan struct{}) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.status, b.signal
}

func (b *Backend) notifyLocked() {
	close(b.signal)
	b.signal = make(chan struct{})
}

func (b *Backend) emitStatus(status simulation.Status) {
	event := simulation.Event{Kind: simulation.EventStatusChanged, Status: status}
	select {
	case b.events <- event:
	default:
		b.log.Debug("jetty: coalesced status event because Provider queue is full")
	}
}

func (b *Backend) emitFault(event simulation.Event) {
	select {
	case b.events <- event:
	case <-b.ctx.Done():
	}
}

var _ simulation.Backend = (*Backend)(nil)

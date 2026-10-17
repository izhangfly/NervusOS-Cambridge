// Package simulation defines the pure-Go domain boundary used by the Nervus
// Provider. Implementations may use Gazebo Transport internally, but handlers
// never see C or Gazebo C++ types.
package simulation

import "context"

type Mode uint8

const (
	ModeUnspecified Mode = iota
	ModeAttach
	ModeManaged
)

type ProcessState uint8

const (
	ProcessUnspecified ProcessState = iota
	ProcessExternal
	ProcessStopped
	ProcessStarting
	ProcessRunning
	ProcessStopping
	ProcessExited
)

type TransportState uint8

const (
	TransportUnspecified TransportState = iota
	TransportDisconnected
	TransportDiscovering
	TransportReady
	TransportDegraded
)

type RunState uint8

const (
	RunUnspecified RunState = iota
	RunRunning
	RunPaused
)

type MotionBindingState uint8

const (
	MotionBindingUnspecified MotionBindingState = iota
	MotionBindingDiscovering
	MotionBindingReady
	MotionBindingDegraded
	MotionBindingUnsupported
)

type MotionBinding struct {
	ProfileID       string
	Model           string
	CapabilityRole  string
	ResourceRole    string
	InterfaceID     string
	Adapter         string
	State           MotionBindingState
	Reason          string
	WorldGeneration uint64
}

// MotionBindingProvider is an optional extension used to reconcile standard
// Nervus motion Resources independently from world readiness.
type MotionBindingProvider interface {
	MotionBindings(context.Context) ([]MotionBinding, error)
}

type ControllerClass uint8

const (
	ControllerUnspecified ControllerClass = iota
	ControllerHuman
	ControllerAI
)

type ResetMode uint8

const (
	ResetUnspecified ResetMode = iota
	ResetAll
	ResetTimeOnly
	ResetModelsOnly
)

type Reason uint8

const (
	ReasonUnspecified Reason = iota
	ReasonProviderNotReady
	ReasonProcessExternallyManaged
	ReasonProcessAlreadyRunning
	ReasonProcessNotRunning
	ReasonWorldNotFound
	ReasonWorldNotReady
	ReasonWorldChanged
	ReasonTransportDisconnected
	ReasonServiceNotFound
	ReasonTopicNotFound
	ReasonTypeMismatch
	ReasonRequestTimeout
	ReasonRemoteRejected
	ReasonModelNotFound
	ReasonJointNotFound
	ReasonJointNotControllable
	ReasonAxisNotSupported
	ReasonOutOfRange
	ReasonFeedbackUnavailable
	ReasonQueueOverflow
	ReasonWSLgNotReady
	ReasonNativeFailure
	ReasonInvalidState
	ReasonBatchTooLarge
	ReasonProcessExited
	ReasonInternal
)

// Error is safe to project into SimulationErrorDetail. Operation must be a
// stable provider-owned token, never a path or native diagnostic string.
type Error struct {
	Reason    Reason
	Operation string
	Retryable bool
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Operation == "" {
		return "simulation operation failed"
	}
	return "simulation operation failed: " + e.Operation
}

func NewError(reason Reason, operation string, retryable bool) *Error {
	return &Error{Reason: reason, Operation: operation, Retryable: retryable}
}

type Statistics struct {
	Valid            bool
	SimulationTimeNS int64
	RealTimeNS       int64
	Iterations       uint64
	RealTimeFactor   float64
	SampleAgeMS      uint64
}

type Versions struct {
	GazeboSim       string
	GazeboTransport string
	GazeboMessages  string
	Adapter         string
}

type Status struct {
	Mode             Mode
	Process          ProcessState
	Transport        TransportState
	Run              RunState
	WorldName        string
	WorldGeneration  uint64
	ProcessOwned     bool
	ManagedProcessID uint64
	HasLastExitCode  bool
	LastExitCode     int32
	Statistics       Statistics
	LastFault        Reason
	Versions         Versions
}

// ReadyForControl reports whether the current world may expose the resource-
// bound control endpoint. TransportReady means stats, scene and world control
// discovery have all completed, not merely that a transport node exists.
func (s Status) ReadyForControl() bool {
	if s.Transport != TransportReady || s.WorldName == "" || s.WorldGeneration == 0 {
		return false
	}
	if s.Process != ProcessRunning && s.Process != ProcessExternal {
		return false
	}
	return s.Run == RunRunning || s.Run == RunPaused
}

type Capabilities struct {
	Mode                       Mode
	CanManageProcess           bool
	CanLoadWorld               bool
	CanResetWorld              bool
	CanControlWorld            bool
	CanQueryScene              bool
	CanCommandJoints           bool
	JointStateFeedback         bool
	SensorTopics               bool
	AllowedWorlds              []string
	MaxStepIterations          uint32
	MaxJointCommandsPerRequest uint32
}

type Model struct {
	Name          string
	EntityID      uint64
	Static        bool
	CanonicalLink string
}

type ModelList struct {
	WorldName       string
	WorldGeneration uint64
	Models          []Model
}

type JointAxis struct {
	Axis              uint32
	HasPositionLimits bool
	LowerPosition     float64
	UpperPosition     float64
}

type Joint struct {
	Name         string
	Type         string
	Axes         []JointAxis
	Controllable bool
}

type JointList struct {
	WorldName       string
	WorldGeneration uint64
	Model           string
	Joints          []Joint
}

type JointState struct {
	WorldName              string
	WorldGeneration        uint64
	Model                  string
	Joint                  string
	Axis                   uint32
	HasPosition            bool
	Position               float64
	HasVelocity            bool
	Velocity               float64
	SampleSimulationTimeNS int64
	Sequence               uint64
}

type StartOptions struct {
	World string
}

// ProcessSnapshot is the process-controller-owned portion of Status. The
// Transport backend combines it with Gazebo discovery and statistics state.
type ProcessSnapshot struct {
	State           ProcessState
	Owned           bool
	ProcessID       uint64
	HasLastExitCode bool
	LastExitCode    int32
	World           string
}

type ProcessEvent struct {
	Snapshot ProcessSnapshot
	Exit     ProcessExitEvent
	HasExit  bool
}

// ProcessController owns only process lifecycle. Implementations must never
// use a shell to interpolate request data and must not stop an unowned process.
type ProcessController interface {
	Mode() Mode
	AllowedWorlds() []string
	Snapshot() ProcessSnapshot
	Start(context.Context, string) (ProcessSnapshot, error)
	Stop(context.Context) (ProcessSnapshot, error)
	Events() <-chan ProcessEvent
	Close() error
}

// WSLgProbe performs read-only environment checks. Repairs remain the
// responsibility of the existing Windows / WSL launcher.
type WSLgProbe interface {
	Check(context.Context) []WSLgFaultEvent
}

type JointQuery struct {
	WorldGeneration uint64
	Model           string
	Joint           string
	Axis            uint32
}

// ControlContext is the trusted execution snapshot projected by nervud plus
// the caller's optimistic world generation. It is not a transferable token.
type ControlContext struct {
	WorldGeneration    uint64
	ResourceHandle     string
	ResourceGeneration uint64
	LeaseID            uint64
	MotionEpoch        uint64
	DeadlineNanos      int64
	CommandSequence    uint64
	Controller         ControllerClass
}

type JointCommand struct {
	Model    string
	Joint    string
	Axis     uint32
	Position float64
}

type CommandReceipt struct {
	SubmittedCommands uint32
	FeedbackAvailable bool
}

// Backend is the complete domain surface consumed by the Nervus handlers.
// Implementations must honor ctx and must not retry mutating Gazebo requests.
type Backend interface {
	Status(context.Context) (Status, error)
	Capabilities(context.Context) (Capabilities, error)
	ListModels(context.Context, uint64) (ModelList, error)
	ListJoints(context.Context, string, uint64) (JointList, error)
	JointState(context.Context, JointQuery) (JointState, error)

	Start(context.Context, StartOptions) (Status, error)
	Stop(context.Context) (Status, error)
	LoadWorld(context.Context, string) (Status, error)

	Pause(context.Context, ControlContext) (Status, error)
	Resume(context.Context, ControlContext) (Status, error)
	Step(context.Context, ControlContext, uint32) (Status, error)
	ResetWorld(context.Context, ControlContext, ResetMode) (Status, error)
	SetJointPositions(context.Context, ControlContext, []JointCommand) (CommandReceipt, error)

	Events() <-chan Event
	Close() error
}

type EventKind uint8

const (
	EventUnspecified EventKind = iota
	EventStatusChanged
	EventProcessExited
	EventTransportFault
	EventWSLgFault
	EventWorldChanged
	EventJointStateChanged
	EventCommandRejected
)

type TransportFaultKind uint8

const (
	TransportFaultUnspecified TransportFaultKind = iota
	TransportFaultDisconnected
	TransportFaultDiscoveryFailed
	TransportFaultTypeMismatch
	TransportFaultRequestTimeout
	TransportFaultRemoteRejected
	TransportFaultQueueOverflow
	TransportFaultNativeFailure
)

type WSLgFaultKind uint8

const (
	WSLgFaultUnspecified WSLgFaultKind = iota
	WSLgFaultKeepaliveMissing
	WSLgFaultCopyMode
	WSLgFaultDisplayUnavailable
	WSLgFaultSharedMemoryUnavailable
)

type ProcessExitEvent struct {
	ProcessID       uint64
	ExitCode        int32
	Expected        bool
	WorldGeneration uint64
}

type TransportFaultEvent struct {
	Kind            TransportFaultKind
	Reason          Reason
	Retryable       bool
	WorldGeneration uint64
	DroppedEvents   uint64
}

type WSLgFaultEvent struct {
	Kind            WSLgFaultKind
	Retryable       bool
	WorldGeneration uint64
}

type WorldChangedEvent struct {
	PreviousWorld      string
	PreviousGeneration uint64
	CurrentWorld       string
	CurrentGeneration  uint64
}

type CommandRejectedEvent struct {
	WorldGeneration uint64
	CommandSequence uint64
	CommandIndex    uint32
	Reason          Reason
	Model           string
	Joint           string
	Axis            uint32
}

// Event carries exactly one payload selected by Kind. MonotonicNanos is zero
// when the backend has no authoritative source timestamp.
type Event struct {
	Kind           EventKind
	MonotonicNanos uint64

	Status          Status
	ProcessExit     ProcessExitEvent
	TransportFault  TransportFaultEvent
	WSLgFault       WSLgFaultEvent
	WorldChanged    WorldChangedEvent
	JointState      JointState
	CommandRejected CommandRejectedEvent
}

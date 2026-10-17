package simulation

import "context"

// UnavailableBackend keeps the manager endpoint honest before the Gazebo
// domain implementation is attached. It never exposes a control endpoint and
// never pretends a command reached Gazebo.
type UnavailableBackend struct {
	mode Mode
}

func NewUnavailableBackend(mode Mode) *UnavailableBackend {
	return &UnavailableBackend{mode: mode}
}

func (b *UnavailableBackend) Status(context.Context) (Status, error) {
	return Status{
		Mode:      b.mode,
		Process:   ProcessStopped,
		Transport: TransportDisconnected,
		Run:       RunUnspecified,
		LastFault: ReasonProviderNotReady,
	}, nil
}

func (b *UnavailableBackend) Capabilities(context.Context) (Capabilities, error) {
	return Capabilities{Mode: b.mode}, nil
}

func (b *UnavailableBackend) ListModels(context.Context, uint64) (ModelList, error) {
	return ModelList{}, unavailable("manager.list_models")
}

func (b *UnavailableBackend) ListJoints(context.Context, string, uint64) (JointList, error) {
	return JointList{}, unavailable("manager.list_joints")
}

func (b *UnavailableBackend) JointState(context.Context, JointQuery) (JointState, error) {
	return JointState{}, unavailable("manager.get_joint_state")
}

func (b *UnavailableBackend) Start(context.Context, StartOptions) (Status, error) {
	if b.mode == ModeAttach {
		return Status{}, NewError(ReasonProcessExternallyManaged, "manager.start", false)
	}
	return Status{}, unavailable("manager.start")
}

func (b *UnavailableBackend) Stop(context.Context) (Status, error) {
	if b.mode == ModeAttach {
		return Status{}, NewError(ReasonProcessExternallyManaged, "manager.stop", false)
	}
	return Status{}, unavailable("manager.stop")
}

func (b *UnavailableBackend) LoadWorld(context.Context, string) (Status, error) {
	if b.mode == ModeAttach {
		return Status{}, NewError(ReasonProcessExternallyManaged, "manager.load_world", false)
	}
	return Status{}, unavailable("manager.load_world")
}

func (b *UnavailableBackend) Pause(context.Context, ControlContext) (Status, error) {
	return Status{}, unavailable("control.pause")
}

func (b *UnavailableBackend) Resume(context.Context, ControlContext) (Status, error) {
	return Status{}, unavailable("control.resume")
}

func (b *UnavailableBackend) Step(context.Context, ControlContext, uint32) (Status, error) {
	return Status{}, unavailable("control.step")
}

func (b *UnavailableBackend) ResetWorld(context.Context, ControlContext, ResetMode) (Status, error) {
	return Status{}, unavailable("control.reset_world")
}

func (b *UnavailableBackend) SetJointPositions(
	context.Context,
	ControlContext,
	[]JointCommand,
) (CommandReceipt, error) {
	return CommandReceipt{}, unavailable("control.set_joint_positions")
}

func (*UnavailableBackend) Events() <-chan Event { return nil }

func (*UnavailableBackend) Close() error { return nil }

func unavailable(operation string) error {
	return NewError(ReasonProviderNotReady, operation, true)
}

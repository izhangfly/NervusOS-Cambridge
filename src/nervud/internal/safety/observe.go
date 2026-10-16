package safety

import "github.com/nervus-os/nervud/internal/motiongate"

// Snapshot is a consistent read-only snapshot of the safety state, for developer-facing observation surfaces.
type Snapshot struct {
	State     motiongate.State
	Epoch     uint64
	StopPhase StopPhase
}

// Observer lets upper layers (future IPC events, UI) read the safety state. v1 only provides snapshot reads;
// push subscriptions (event streams like EndpointDied) come once IPC dispatch lands.
//
// *Module implements this interface.
type Observer interface {
	SafetySnapshot() Snapshot
}

// Controller lets authorised callers request a software emergency stop. Permissions are decided by nervud at the IPC layer; this interface
// only expresses that a capability to request an emergency stop exists, not that anyone can call it (only HUMAN/UI may, and with authorisation).
//
// *Module implements this interface (RequestStop is Trip).
type Controller interface {
	RequestStop(reason ReasonCode)
}

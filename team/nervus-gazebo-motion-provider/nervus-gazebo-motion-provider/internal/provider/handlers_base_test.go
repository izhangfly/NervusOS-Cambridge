package provider

import (
	"math"
	"testing"

	"github.com/nervus-os/nervus-gazebo-provider/internal/contract"
	basemotionv1 "github.com/nervus-os/nervus-ipc/protocol/interface/basemotionv1"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"
)

func baseCallContext(sequence, generation uint64) sdk.CallContext {
	cc := callContext("perm.motion.control")
	cc.Execution = &ipcv1.ExecutionContext{
		LeaseId: 10, ControllerClass: ipcv1.ControllerClass_CONTROLLER_CLASS_AI,
		MotionEpoch: 20, DeadlineNanos: math.MaxInt64, CommandSequence: sequence,
		ResourceHandle: contract.BaseResourceRole, ResourceGeneration: generation,
	}
	return cc
}

func TestBaseMotionHandlersForwardNativeSemantics(t *testing.T) {
	host := newFakeHost()
	backend := newFakeBackend(readyStatus(4), host)
	service, err := New(host, backend, DefaultConfig(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	_ = service
	base := host.endpoint(contract.BaseInterface)

	statePayload, err := proto.Marshal(&basemotionv1.GetMotionStateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	stateWire, err := base.invoke(3, callContext(), statePayload)
	if err != nil {
		t.Fatalf("GetMotionState: %v", err)
	}
	var state basemotionv1.MotionState
	if err := proto.Unmarshal(stateWire, &state); err != nil {
		t.Fatal(err)
	}
	if state.GetReadiness() != basemotionv1.Readiness_READINESS_READY || state.GetSupportsLinearY() || state.GetVelocityLimits().GetMaxLinear() != 1 {
		t.Fatalf("unexpected base state: readiness=%v supports_linear_y=%v max_linear=%v",
			state.GetReadiness(), state.GetSupportsLinearY(), state.GetVelocityLimits().GetMaxLinear())
	}

	velocityPayload, _ := proto.Marshal(&basemotionv1.SetVelocityRequest{LinearX: .4, AngularZ: .2})
	if _, err := base.invoke(1, baseCallContext(1, 9), velocityPayload); err != nil {
		t.Fatalf("SetVelocity: %v", err)
	}
	backend.mu.Lock()
	gotVelocity, gotExecution := backend.lastBase, backend.lastControl
	backend.mu.Unlock()
	if gotVelocity.LinearX != .4 || gotVelocity.AngularZ != .2 || gotExecution.ResourceHandle != contract.BaseResourceRole {
		t.Fatalf("unexpected base command: %+v %+v", gotVelocity, gotExecution)
	}

	stopPayload, _ := proto.Marshal(&basemotionv1.StopRequest{})
	if _, err := base.invoke(2, baseCallContext(2, 9), stopPayload); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := backend.calls("base_stop"); got != 1 {
		t.Fatalf("base stop calls = %d, want 1", got)
	}
}

func TestBaseMotionRejectsReplayPerResource(t *testing.T) {
	host := newFakeHost()
	backend := newFakeBackend(readyStatus(1), host)
	service, err := New(host, backend, DefaultConfig(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	_ = service
	base := host.endpoint(contract.BaseInterface)
	payload, _ := proto.Marshal(&basemotionv1.SetVelocityRequest{LinearX: .1})
	if _, err := base.invoke(1, baseCallContext(2, 3), payload); err != nil {
		t.Fatal(err)
	}
	if _, err := base.invoke(1, baseCallContext(2, 3), payload); err == nil {
		t.Fatal("replayed BaseMotion command was accepted")
	}
	if got := backend.calls("base_set_velocity"); got != 1 {
		t.Fatalf("base command calls = %d, want 1", got)
	}
}

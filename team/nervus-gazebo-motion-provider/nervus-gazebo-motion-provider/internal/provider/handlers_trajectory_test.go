package provider

import (
	"testing"
	"time"

	simulationv1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/provider/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/contract"
	"google.golang.org/protobuf/proto"
)

func TestJointTrajectoryHomeStopAndWholeMachineStop(t *testing.T) {
	host := newFakeHost()
	backend := newFakeBackend(readyStatus(4), host)
	service, err := New(host, backend, DefaultConfig(), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = service
	control := host.endpoint(contract.ControlInterface)

	trajectory, _ := proto.Marshal(&simulationv1.SetJointTrajectoryRequest{
		WorldGeneration: 4,
		Model:           "nervus_mobile_g1_dex3",
		JointGroup:      "left_arm",
		Points: []*simulationv1.JointTrajectoryPoint{
			{Positions: []float64{0, 0.1}, TimeFromStartNanos: int64(500 * time.Millisecond)},
			{Positions: []float64{0.2, 0}, TimeFromStartNanos: int64(time.Second)},
		},
	})
	wire, err := control.invoke(
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_SET_JOINT_TRAJECTORY),
		controlCallContext(1, 30),
		trajectory,
	)
	if err != nil {
		t.Fatalf("SetJointTrajectory: %v", err)
	}
	var receipt simulationv1.JointMotionReceipt
	if err := proto.Unmarshal(wire, &receipt); err != nil {
		t.Fatalf("unmarshal trajectory receipt: %v", err)
	}
	if receipt.GetMotionId() != 77 || !receipt.GetConverged() || receipt.GetCommandSequence() != 1 {
		t.Fatalf("trajectory receipt = %+v", &receipt)
	}

	home, _ := proto.Marshal(&simulationv1.GoHomeRequest{
		WorldGeneration: 4, Model: "nervus_mobile_g1_dex3", JointGroup: "left_arm",
	})
	if _, err := control.invoke(
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_GO_HOME),
		controlCallContext(2, 30),
		home,
	); err != nil {
		t.Fatalf("GoHome: %v", err)
	}

	stop, _ := proto.Marshal(&simulationv1.StopMotionRequest{
		WorldGeneration: 4, Model: "nervus_mobile_g1_dex3", JointGroup: "left_arm",
	})
	if _, err := control.invoke(
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_STOP_MOTION),
		controlCallContext(3, 30),
		stop,
	); err != nil {
		t.Fatalf("StopMotion: %v", err)
	}

	stopAll, _ := proto.Marshal(&simulationv1.StopAllMotionRequest{WorldGeneration: 4})
	if _, err := control.invoke(
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_STOP_ALL_MOTION),
		controlCallContext(4, 30),
		stopAll,
	); err != nil {
		t.Fatalf("StopAllMotion: %v", err)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.callCounts["start_joint_trajectory"] != 1 ||
		backend.callCounts["start_joint_home"] != 1 ||
		backend.callCounts["cancel_joint_motion"] != 1 ||
		backend.callCounts["stop_all_motion"] != 1 || backend.lastHome != "home" {
		t.Fatalf("joint motion calls = %+v, home = %q", backend.callCounts, backend.lastHome)
	}
}

func TestJointTrajectoryRejectsMalformedTimingBeforeBackend(t *testing.T) {
	host := newFakeHost()
	backend := newFakeBackend(readyStatus(1), host)
	service, err := New(host, backend, DefaultConfig(), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = service
	control := host.endpoint(contract.ControlInterface)
	wire, _ := proto.Marshal(&simulationv1.SetJointTrajectoryRequest{
		WorldGeneration: 1, Model: "robot", JointGroup: "arm",
		Points: []*simulationv1.JointTrajectoryPoint{
			{Positions: []float64{0}, TimeFromStartNanos: int64(time.Second)},
			{Positions: []float64{0}, TimeFromStartNanos: int64(time.Second)},
		},
	})
	if _, err := control.invoke(
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_SET_JOINT_TRAJECTORY),
		controlCallContext(1, 1),
		wire,
	); err == nil {
		t.Fatal("non-increasing trajectory timing was accepted")
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.callCounts["start_joint_trajectory"] != 0 {
		t.Fatal("malformed trajectory reached backend")
	}
}

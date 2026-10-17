package jetty

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/nervus-os/nervus-gazebo-provider/internal/gzmsgs"
	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"google.golang.org/protobuf/proto"
)

func TestBaseMotionPublishesTwistAndPromotesOnOdometry(t *testing.T) {
	client := newFakeClient(false)
	backend := &Backend{
		client: client, config: DefaultConfig(), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		status: simulation.Status{Transport: simulation.TransportReady, Process: simulation.ProcessExternal, Run: simulation.RunRunning, WorldName: "test", WorldGeneration: 4},
		signal: make(chan struct{}), scene: sceneSnapshot{baseControllers: map[string]baseController{"base.main": {
			model: "robot", commandTopic: "/model/robot/cmd_vel", maxLinear: 1, maxAngular: 2, hasFeedback: true,
		}}, bindings: []simulation.MotionBinding{{ResourceRole: "base.main", Adapter: "diff_drive", State: simulation.MotionBindingDiscovering}}},
	}
	execution := simulation.ControlContext{WorldGeneration: 4, ResourceHandle: "base.main", MotionEpoch: 1, CommandSequence: 1}
	if err := backend.SetBaseVelocity(context.Background(), execution, simulation.BaseVelocity{LinearX: .25, AngularZ: .5}); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	if len(client.published) != 1 {
		t.Fatalf("published = %d, want 1", len(client.published))
	}
	var twist gzmsgs.Twist
	if err := proto.Unmarshal(client.published[0].Data, &twist); err != nil {
		t.Fatal(err)
	}
	client.mu.Unlock()
	if twist.GetLinear().GetX() != .25 || twist.GetAngular().GetZ() != .5 {
		t.Fatalf("unexpected Twist: linear_x=%v angular_z=%v", twist.GetLinear().GetX(), twist.GetAngular().GetZ())
	}

	odom, _ := proto.Marshal(&gzmsgs.Odometry{Twist: &gzmsgs.Twist{Linear: &gzmsgs.Vector3D{}, Angular: &gzmsgs.Vector3D{}}})
	if err := backend.applyBaseOdometry(gztransport.Event{Data: odom}, "base.main"); err != nil {
		t.Fatal(err)
	}
	bindings, _ := backend.MotionBindings(context.Background())
	if len(bindings) != 1 || bindings[0].State != simulation.MotionBindingReady {
		t.Fatalf("odometry did not promote binding: %+v", bindings)
	}
}

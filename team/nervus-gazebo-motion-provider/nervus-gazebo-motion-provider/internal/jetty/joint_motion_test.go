package jetty

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

func TestJointTrajectoryOwnsCommandSourceAndCancelsToHold(t *testing.T) {
	backend, client := newTestBackend(t, true)
	client.enqueueJointState(0)
	status, err := backend.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	execution := simulation.ControlContext{
		WorldGeneration:    status.WorldGeneration,
		ResourceHandle:     "simulation.world.main",
		ResourceGeneration: 1,
		LeaseID:            1,
		MotionEpoch:        1,
		DeadlineNanos:      1,
		CommandSequence:    1,
		Controller:         simulation.ControllerHuman,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	receipt, err := backend.StartJointTrajectory(ctx, execution, "nervus_nao", "all", []simulation.JointTrajectoryPoint{{
		Positions: []float64{0.2}, TimeFromStart: int64(time.Second),
	}})
	if err != nil {
		t.Fatalf("StartJointTrajectory: %v", err)
	}
	if receipt.MotionID == 0 || receipt.ControlledJoints != 1 || !receipt.FeedbackAvailable {
		t.Fatalf("start receipt = %+v", receipt)
	}
	if _, err := backend.SetJointPositions(ctx, execution, []simulation.JointCommand{{
		Model: "nervus_nao", Joint: "HeadYaw", Axis: 0, Position: 0.1,
	}}); err == nil {
		t.Fatal("direct command competed with an active trajectory")
	} else {
		var domain *simulation.Error
		if !errors.As(err, &domain) || domain.Reason != simulation.ReasonInvalidState {
			t.Fatalf("competing command error = %v", err)
		}
	}
	if _, err := backend.CancelJointMotion(ctx, execution, "nervus_nao", "all"); err != nil {
		t.Fatalf("CancelJointMotion: %v", err)
	}
	completed, err := backend.WaitJointMotion(ctx, receipt.MotionID)
	if !errors.Is(err, context.Canceled) || completed.Converged {
		t.Fatalf("cancelled completion = %+v, %v", completed, err)
	}
}

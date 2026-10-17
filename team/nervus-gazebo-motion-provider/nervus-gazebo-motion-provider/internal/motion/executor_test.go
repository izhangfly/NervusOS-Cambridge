package motion

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestExecutorInterpolatesConvergesAndOwnsJoints(t *testing.T) {
	executor := NewExecutor(context.Background())
	joints := []JointRef{{Model: "robot", Joint: "a"}, {Model: "robot", Joint: "b"}}
	var mu sync.Mutex
	actual := []float64{0, 0}
	plan := Plan{
		Generation: 1,
		Joints:     joints,
		Initial:    []float64{0, 0},
		Points:     []TrajectoryPoint{{Positions: []float64{0.1, -0.1}, TimeFromStart: 80 * time.Millisecond}},
		Limits: []JointLimit{
			{HasPosition: true, Lower: -1, Upper: 1, Tolerance: 0.001, MaxVelocity: 2},
			{HasPosition: true, Lower: -1, Upper: 1, Tolerance: 0.001, MaxVelocity: 2},
		},
		Interval: 5 * time.Millisecond,
		Settle:   15 * time.Millisecond,
		Publish: func(_ context.Context, positions []float64) error {
			mu.Lock()
			copy(actual, positions)
			mu.Unlock()
			return nil
		},
		Read: func(context.Context) ([]float64, error) {
			mu.Lock()
			defer mu.Unlock()
			return append([]float64(nil), actual...), nil
		},
	}
	id, err := executor.Start(plan)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !executor.AnyOwned(joints) {
		t.Fatal("trajectory joints were not owned")
	}
	if _, err := executor.Start(plan); !errors.Is(err, ErrJointBusy) {
		t.Fatalf("overlapping Start error = %v, want ErrJointBusy", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := executor.Wait(ctx, id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.Err != nil || !result.Converged || result.ControlledJoints != 2 {
		t.Fatalf("result = %+v", result)
	}
	if executor.AnyOwned(joints) {
		t.Fatal("trajectory ownership was not released")
	}
}

func TestExecutorCancellationReleasesOwnership(t *testing.T) {
	executor := NewExecutor(context.Background())
	joints := []JointRef{{Model: "robot", Joint: "a"}}
	plan := Plan{
		Generation: 1,
		Joints:     joints,
		Initial:    []float64{0},
		Points:     []TrajectoryPoint{{Positions: []float64{0.5}, TimeFromStart: time.Second}},
		Limits:     []JointLimit{{HasPosition: true, Lower: -1, Upper: 1, Tolerance: 0.01, MaxVelocity: 1}},
		Interval:   5 * time.Millisecond,
		Settle:     10 * time.Millisecond,
		Publish:    func(context.Context, []float64) error { return nil },
		Read:       func(context.Context) ([]float64, error) { return []float64{0}, nil },
	}
	id, err := executor.Start(plan)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := executor.CancelJoints(joints); got != 1 {
		t.Fatalf("CancelJoints = %d, want 1", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := executor.Wait(ctx, id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !errors.Is(result.Err, context.Canceled) || result.Converged {
		t.Fatalf("cancelled result = %+v", result)
	}
	if executor.AnyOwned(joints) {
		t.Fatal("cancelled trajectory retained ownership")
	}
}

package motion

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func TestTrajectoryValidationInterpolationAndConvergence(t *testing.T) {
	limits := []JointLimit{
		{HasPosition: true, Lower: -1, Upper: 1, Tolerance: 0.01},
		{HasPosition: true, Lower: -2, Upper: 2, Tolerance: 0.02},
	}
	points := []TrajectoryPoint{
		{Positions: []float64{0, 0}, TimeFromStart: time.Second},
		{Positions: []float64{1, -2}, TimeFromStart: 3 * time.Second},
	}
	if err := ValidateTrajectory(points, limits, 4); err != nil {
		t.Fatalf("ValidateTrajectory: %v", err)
	}
	middle, err := Interpolate(points[0], points[1], 2*time.Second)
	if err != nil || math.Abs(middle[0]-0.5) > 1e-9 || math.Abs(middle[1]+1) > 1e-9 {
		t.Fatalf("Interpolate = %v, %v", middle, err)
	}
	if !Converged([]float64{0.995, -1.985}, points[1].Positions, limits) {
		t.Fatal("actual feedback inside tolerance did not converge")
	}
	if Converged([]float64{0.98, -1.985}, points[1].Positions, limits) {
		t.Fatal("actual feedback outside tolerance converged")
	}
}

func TestTrajectoryRejectsBadTimeLimitsAndNonFiniteValues(t *testing.T) {
	limits := []JointLimit{{HasPosition: true, Lower: -1, Upper: 1, Tolerance: 0.01}}
	tests := [][]TrajectoryPoint{
		{{Positions: []float64{0}, TimeFromStart: 0}},
		{{Positions: []float64{0}, TimeFromStart: time.Second}, {Positions: []float64{0.5}, TimeFromStart: time.Second}},
		{{Positions: []float64{2}, TimeFromStart: time.Second}},
		{{Positions: []float64{math.NaN()}, TimeFromStart: time.Second}},
		{{Positions: []float64{0, 1}, TimeFromStart: time.Second}},
	}
	for index, points := range tests {
		if err := ValidateTrajectory(points, limits, 8); err == nil {
			t.Errorf("invalid trajectory %d was accepted", index)
		}
	}
}

func TestWaitForConvergenceRequiresFeedbackAndHonorsCancellation(t *testing.T) {
	limits := []JointLimit{{Tolerance: 0.01}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := WaitForConvergence(ctx, time.Millisecond, func(context.Context) ([]float64, error) {
		return []float64{0}, nil
	}, []float64{1}, limits)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForConvergence error = %v", err)
	}

	reads := 0
	err = WaitForConvergence(context.Background(), time.Millisecond,
		func(context.Context) ([]float64, error) {
			reads++
			if reads < 2 {
				return []float64{0}, nil
			}
			return []float64{1}, nil
		}, []float64{1}, limits)
	if err != nil || reads != 2 {
		t.Fatalf("convergence = %v after %d reads", err, reads)
	}
}

func TestLifecycleInvalidatesOldGeneration(t *testing.T) {
	var lifecycle Lifecycle
	if err := lifecycle.BeginBind(Binding{WorldGeneration: 4}); err != nil {
		t.Fatal(err)
	}
	lifecycle.MarkReady()
	if err := lifecycle.RequireReady(4); err != nil {
		t.Fatalf("generation 4 not ready: %v", err)
	}
	lifecycle.Invalidate(5)
	if !errors.Is(lifecycle.RequireReady(4), ErrNotReady) {
		t.Fatalf("old generation remained executable")
	}
	if lifecycle.State() != StateDegraded {
		t.Fatalf("state = %v, want degraded", lifecycle.State())
	}
}

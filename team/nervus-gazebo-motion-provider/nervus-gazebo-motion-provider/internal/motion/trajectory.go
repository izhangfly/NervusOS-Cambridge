package motion

import (
	"context"
	"fmt"
	"math"
	"time"
)

type JointLimit struct {
	HasPosition bool
	Lower       float64
	Upper       float64
	Tolerance   float64
	MaxVelocity float64
}

type TrajectoryPoint struct {
	Positions     []float64
	TimeFromStart time.Duration
}

func ValidateTrajectory(points []TrajectoryPoint, limits []JointLimit, maxPoints int) error {
	if len(points) == 0 || maxPoints < 1 || len(points) > maxPoints || len(limits) == 0 {
		return fmt.Errorf("motion: invalid trajectory size")
	}
	var previous time.Duration
	for index, point := range points {
		if len(point.Positions) != len(limits) || point.TimeFromStart <= 0 ||
			(index > 0 && point.TimeFromStart <= previous) {
			return fmt.Errorf("motion: invalid trajectory point %d", index)
		}
		for joint, value := range point.Positions {
			if !finite(value) || (limits[joint].HasPosition &&
				(value < limits[joint].Lower || value > limits[joint].Upper)) {
				return fmt.Errorf("motion: trajectory joint %d is outside limits", joint)
			}
			if limits[joint].MaxVelocity < 0 || !finite(limits[joint].MaxVelocity) {
				return fmt.Errorf("motion: trajectory joint %d has an invalid velocity limit", joint)
			}
			if index > 0 && limits[joint].MaxVelocity > 0 {
				velocity := math.Abs(value-points[index-1].Positions[joint]) /
					(point.TimeFromStart - previous).Seconds()
				if velocity > limits[joint].MaxVelocity+1e-9 {
					return fmt.Errorf("motion: trajectory joint %d exceeds its velocity limit", joint)
				}
			}
		}
		previous = point.TimeFromStart
	}
	return nil
}

// WaitForSettledConvergence requires the feedback to remain within tolerance
// for settle before it accepts the target. A single transient sample is not
// sufficient evidence that a physical controller has stopped at the target.
func WaitForSettledConvergence(
	ctx context.Context,
	interval, settle time.Duration,
	read func(context.Context) ([]float64, error),
	target []float64,
	limits []JointLimit,
) error {
	if ctx == nil || interval <= 0 || settle <= 0 || read == nil {
		return fmt.Errorf("motion: invalid settled convergence check")
	}
	var convergedAt time.Time
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		actual, err := read(ctx)
		if err != nil {
			return err
		}
		now := time.Now()
		if Converged(actual, target, limits) {
			if convergedAt.IsZero() {
				convergedAt = now
			}
			if now.Sub(convergedAt) >= settle {
				return nil
			}
		} else {
			convergedAt = time.Time{}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func Interpolate(left, right TrajectoryPoint, at time.Duration) ([]float64, error) {
	if len(left.Positions) == 0 || len(left.Positions) != len(right.Positions) ||
		right.TimeFromStart <= left.TimeFromStart || at < left.TimeFromStart ||
		at > right.TimeFromStart {
		return nil, fmt.Errorf("motion: invalid interpolation interval")
	}
	ratio := float64(at-left.TimeFromStart) / float64(right.TimeFromStart-left.TimeFromStart)
	out := make([]float64, len(left.Positions))
	for index := range out {
		out[index] = left.Positions[index] + ratio*(right.Positions[index]-left.Positions[index])
	}
	return out, nil
}

func Converged(actual, target []float64, limits []JointLimit) bool {
	if len(actual) == 0 || len(actual) != len(target) || len(actual) != len(limits) {
		return false
	}
	for index := range actual {
		tolerance := limits[index].Tolerance
		if !finite(tolerance) || tolerance <= 0 || !finite(actual[index]) ||
			!finite(target[index]) || math.Abs(actual[index]-target[index]) > tolerance {
			return false
		}
	}
	return true
}

// WaitForConvergence requires actual feedback after trajectory time has
// elapsed. Time progress alone is never accepted as target convergence.
func WaitForConvergence(
	ctx context.Context,
	interval time.Duration,
	read func(context.Context) ([]float64, error),
	target []float64,
	limits []JointLimit,
) error {
	if ctx == nil || interval <= 0 || read == nil {
		return fmt.Errorf("motion: invalid convergence check")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		actual, err := read(ctx)
		if err != nil {
			return err
		}
		if Converged(actual, target, limits) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

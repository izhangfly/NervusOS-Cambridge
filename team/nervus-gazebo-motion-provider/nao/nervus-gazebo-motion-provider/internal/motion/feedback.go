package motion

import "context"

type JointSample struct {
	Generation          uint64
	SimulationTimeNanos int64
	Positions           []float64
	Velocities          []float64
}

type RootPose struct {
	Frame      string
	X, Y, Z    float64
	QX, QY, QZ float64
	QW         float64
}

// JointStateSource and RootPoseSource are internal closed-loop dependencies.
// They are intentionally not Nervus sensor Resources or Transfer producers.
type JointStateSource interface {
	JointState(context.Context, uint64) (JointSample, error)
}

type RootPoseSource interface {
	RootPose(context.Context, uint64) (RootPose, error)
}

// Kinematics is injected by a complete manipulator adapter. Profile loading
// cannot select arbitrary code or provide an implementation at runtime.
type Kinematics interface {
	Forward(context.Context, []float64, string, string) (RootPose, error)
	Inverse(context.Context, RootPose, []float64) ([]float64, error)
}

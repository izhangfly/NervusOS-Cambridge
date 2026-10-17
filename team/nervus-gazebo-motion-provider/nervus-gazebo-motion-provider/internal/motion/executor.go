package motion

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrJointBusy = errors.New("motion: joint already has an active command source")

type JointRef struct {
	Model string
	Joint string
	Axis  uint32
}

type Plan struct {
	Generation uint64
	Joints     []JointRef
	Initial    []float64
	Points     []TrajectoryPoint
	Limits     []JointLimit
	Interval   time.Duration
	Settle     time.Duration
	Publish    func(context.Context, []float64) error
	Read       func(context.Context) ([]float64, error)
}

type ExecutionResult struct {
	MotionID         uint64
	ControlledJoints int
	Converged        bool
	Err              error
}

type activeExecution struct {
	id     uint64
	joints []JointRef
	cancel context.CancelFunc
	done   chan struct{}
	result ExecutionResult
}

// Executor serializes command-source ownership per joint while allowing
// disjoint arm and hand groups to move concurrently.
type Executor struct {
	ctx context.Context

	mu     sync.Mutex
	nextID uint64
	owners map[JointRef]uint64
	active map[uint64]*activeExecution
}

func NewExecutor(ctx context.Context) *Executor {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Executor{
		ctx: ctx, owners: make(map[JointRef]uint64), active: make(map[uint64]*activeExecution),
	}
}

func (e *Executor) Start(plan Plan) (uint64, error) {
	if err := validatePlan(plan); err != nil {
		return 0, err
	}
	e.mu.Lock()
	for _, joint := range plan.Joints {
		if e.owners[joint] != 0 {
			e.mu.Unlock()
			return 0, ErrJointBusy
		}
	}
	e.nextID++
	if e.nextID == 0 {
		e.mu.Unlock()
		return 0, fmt.Errorf("motion: motion id exhausted")
	}
	id := e.nextID
	ctx, cancel := context.WithCancel(e.ctx)
	active := &activeExecution{
		id: id, joints: append([]JointRef(nil), plan.Joints...), cancel: cancel,
		done: make(chan struct{}),
	}
	for _, joint := range active.joints {
		e.owners[joint] = id
	}
	e.active[id] = active
	e.mu.Unlock()

	go e.run(ctx, active, plan)
	return id, nil
}

func (e *Executor) Wait(ctx context.Context, id uint64) (ExecutionResult, error) {
	e.mu.Lock()
	active := e.active[id]
	e.mu.Unlock()
	if active == nil {
		return ExecutionResult{}, fmt.Errorf("motion: motion id is not active")
	}
	select {
	case <-active.done:
		result := active.result
		e.mu.Lock()
		if e.active[id] == active {
			delete(e.active, id)
		}
		e.mu.Unlock()
		return result, nil
	case <-ctx.Done():
		return ExecutionResult{}, ctx.Err()
	}
}

func (e *Executor) Cancel(id uint64) bool {
	e.mu.Lock()
	active := e.active[id]
	e.mu.Unlock()
	if active == nil {
		return false
	}
	active.cancel()
	return true
}

func (e *Executor) CancelJoints(joints []JointRef) int {
	active := e.executionsForJoints(joints)
	for _, item := range active {
		item.cancel()
	}
	return len(active)
}

func (e *Executor) CancelJointsAndWait(ctx context.Context, joints []JointRef) (int, error) {
	active := e.executionsForJoints(joints)
	for _, item := range active {
		item.cancel()
	}
	return len(active), waitExecutions(ctx, active)
}

func (e *Executor) executionsForJoints(joints []JointRef) []*activeExecution {
	e.mu.Lock()
	ids := make(map[uint64]struct{})
	for _, joint := range joints {
		if id := e.owners[joint]; id != 0 {
			ids[id] = struct{}{}
		}
	}
	active := make([]*activeExecution, 0, len(ids))
	for id := range ids {
		if item := e.active[id]; item != nil {
			active = append(active, item)
		}
	}
	e.mu.Unlock()
	return active
}

func (e *Executor) CancelAll() int {
	active := e.allExecutions()
	for _, item := range active {
		item.cancel()
	}
	return len(active)
}

func (e *Executor) CancelAllAndWait(ctx context.Context) (int, error) {
	active := e.allExecutions()
	for _, item := range active {
		item.cancel()
	}
	return len(active), waitExecutions(ctx, active)
}

func (e *Executor) allExecutions() []*activeExecution {
	e.mu.Lock()
	active := make([]*activeExecution, 0, len(e.active))
	for _, item := range e.active {
		select {
		case <-item.done:
		default:
			active = append(active, item)
		}
	}
	e.mu.Unlock()
	return active
}

func waitExecutions(ctx context.Context, active []*activeExecution) error {
	for _, item := range active {
		select {
		case <-item.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (e *Executor) AnyOwned(joints []JointRef) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, joint := range joints {
		if e.owners[joint] != 0 {
			return true
		}
	}
	return false
}

func (e *Executor) run(ctx context.Context, active *activeExecution, plan Plan) {
	result := ExecutionResult{MotionID: active.id, ControlledJoints: len(active.joints)}
	result.Err = executePlan(ctx, plan)
	result.Converged = result.Err == nil

	e.mu.Lock()
	for _, joint := range active.joints {
		if e.owners[joint] == active.id {
			delete(e.owners, joint)
		}
	}
	active.result = result
	close(active.done)
	// Keep the terminal result reachable until one waiter collects it. The
	// bounded Provider call is that waiter in normal operation.
	e.mu.Unlock()
}

func validatePlan(plan Plan) error {
	if plan.Generation == 0 || len(plan.Joints) == 0 || len(plan.Initial) != len(plan.Joints) ||
		len(plan.Limits) != len(plan.Joints) || plan.Interval <= 0 || plan.Settle <= 0 ||
		plan.Publish == nil || plan.Read == nil {
		return fmt.Errorf("motion: invalid execution plan")
	}
	seen := make(map[JointRef]struct{}, len(plan.Joints))
	for _, joint := range plan.Joints {
		if joint.Model == "" || joint.Joint == "" {
			return fmt.Errorf("motion: invalid joint reference")
		}
		if _, duplicate := seen[joint]; duplicate {
			return fmt.Errorf("motion: duplicate joint reference")
		}
		seen[joint] = struct{}{}
	}
	if err := ValidateTrajectory(plan.Points, plan.Limits, 4096); err != nil {
		return err
	}
	for index, limit := range plan.Limits {
		if limit.MaxVelocity <= 0 {
			continue
		}
		first := plan.Points[0]
		velocity := abs(first.Positions[index]-plan.Initial[index]) / first.TimeFromStart.Seconds()
		if velocity > limit.MaxVelocity+1e-9 {
			return fmt.Errorf("motion: initial trajectory segment exceeds joint %d velocity limit", index)
		}
	}
	return nil
}

func executePlan(ctx context.Context, plan Plan) error {
	start := time.Now()
	left := TrajectoryPoint{Positions: append([]float64(nil), plan.Initial...)}
	pointIndex := 0
	ticker := time.NewTicker(plan.Interval)
	defer ticker.Stop()
	for {
		elapsed := time.Since(start)
		for pointIndex < len(plan.Points)-1 && elapsed >= plan.Points[pointIndex].TimeFromStart {
			left = plan.Points[pointIndex]
			pointIndex++
		}
		right := plan.Points[pointIndex]
		at := elapsed
		if at > right.TimeFromStart {
			at = right.TimeFromStart
		}
		positions, err := Interpolate(left, right, at)
		if err != nil {
			return err
		}
		if err := plan.Publish(ctx, positions); err != nil {
			return err
		}
		if elapsed >= plan.Points[len(plan.Points)-1].TimeFromStart {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	target := plan.Points[len(plan.Points)-1].Positions
	return WaitForSettledConvergence(ctx, plan.Interval, plan.Settle, plan.Read, target, plan.Limits)
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

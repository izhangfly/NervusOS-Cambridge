package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	manipulatorv1 "github.com/nervus-os/nervus-ipc/protocol/interface/manipulatorv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
)

// Method IDs come from the generated enum: a local copy would quietly go stale.
var (
	methodGetArmState      = uint32(manipulatorv1.ManipulatorMethod_MANIPULATOR_METHOD_GET_ARM_STATE)
	methodMoveToPose       = uint32(manipulatorv1.ManipulatorMethod_MANIPULATOR_METHOD_MOVE_TO_POSE)
	methodMoveTrajectory   = uint32(manipulatorv1.ManipulatorMethod_MANIPULATOR_METHOD_MOVE_JOINT_TRAJECTORY)
	methodGoHome           = uint32(manipulatorv1.ManipulatorMethod_MANIPULATOR_METHOD_GO_HOME)
	methodSetGripper       = uint32(manipulatorv1.ManipulatorMethod_MANIPULATOR_METHOD_SET_GRIPPER)
	methodStop             = uint32(manipulatorv1.ManipulatorMethod_MANIPULATOR_METHOD_STOP)
	methodServoToPose      = uint32(manipulatorv1.ManipulatorMethod_MANIPULATOR_METHOD_SERVO_TO_POSE)
	defaultGripperMaxMM    = 70.0
	reachToleranceMM       = 3.0
	reachToleranceDeg      = 3.0
	jointToleranceDeg      = 1.5
	moveTimeout            = 25 * time.Second
	enableSettle           = 1300 * time.Millisecond
	servoRefreshPeriod     = 100 * time.Millisecond
	stateCacheTTL          = 80 * time.Millisecond
	maxTrajectoryPoints    = 2000
	trajectoryStreamPeriod = 66 * time.Millisecond
)

// Failures carry a StatusCode only. nervud doesn't forward Provider error details yet (it treats one
// as a contract violation and answers INTERNAL), so the reason a caller can act on lives in
// GetArmState's readiness instead: NOT_READY (motors off, drag-teaching, another client), FAULT.
func fail(code ipcv1.StatusCode) error { return &sdk.StatusError{Code: code} }

var (
	errInvalid     = fail(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	errPrecond     = fail(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)
	errUnavailable = fail(ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE)
	errDeadline    = fail(ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED)
)

// Service implements nervus.interface.manipulator.arm@1 for the AgileX PiPER.
type Service struct {
	drv *driver
	log *slog.Logger

	mu        sync.Mutex
	epoch     uint64 // highest motion epoch seen on a lease-gated command
	sequence  uint64 // highest command sequence within that epoch
	cached    *driverState
	cachedAt  time.Time
	moveMu    sync.Mutex // one long move at a time
	moveEpoch uint64
}

func newService(drv *driver, log *slog.Logger) *Service {
	return &Service{drv: drv, log: log}
}

// admit is the last staleness check before touching the arm, as ExecutionContext asks of every
// lease-gated Provider: a command from a revoked epoch is refused, and one that arrives behind a
// newer command of the same lease is dropped (latest wins). Returns ok=false for a silent drop.
func (s *Service) admit(cc sdk.CallContext) (ok bool, err error) {
	ec := cc.Execution
	if ec == nil || ec.GetMotionEpoch() == 0 {
		// The kernel always attaches one to lease-gated methods; a missing one is a kernel bug,
		// and moving without it would ignore the lease entirely.
		return false, errPrecond
	}
	if cc.Ctx.Err() != nil {
		return false, errDeadline
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case ec.GetMotionEpoch() < s.epoch:
		return false, errPrecond
	case ec.GetMotionEpoch() > s.epoch:
		s.epoch = ec.GetMotionEpoch()
		s.sequence = ec.GetCommandSequence()
		return true, nil
	case ec.GetCommandSequence() <= s.sequence:
		return false, nil
	}
	s.sequence = ec.GetCommandSequence()
	return true, nil
}

func (s *Service) currentEpoch() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

func (s *Service) state(ctx context.Context, fresh bool) (*driverState, error) {
	s.mu.Lock()
	if !fresh && s.cached != nil && time.Since(s.cachedAt) < stateCacheTTL {
		st := s.cached
		s.mu.Unlock()
		return st, nil
	}
	s.mu.Unlock()
	st, err := s.drv.state(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cached, s.cachedAt = st, time.Now()
	s.mu.Unlock()
	return st, nil
}

// readiness maps the driver's view to Manipulator@1's.
func readiness(st *driverState) manipulatorv1.ArmReadiness {
	switch {
	case st == nil || !st.FeedbackFresh:
		return manipulatorv1.ArmReadiness_ARM_READINESS_NOT_READY
	case st.Estopped || (st.ArmStatus != "NORMAL" && !st.SoftFault):
		return manipulatorv1.ArmReadiness_ARM_READINESS_FAULT
	case st.Teaching || !st.Enabled:
		return manipulatorv1.ArmReadiness_ARM_READINESS_NOT_READY
	case st.Owner != nil && st.Owner.Client != clientName:
		// Another local client (the Robot Arm app, the ACT engine) is moving the arm.
		return manipulatorv1.ArmReadiness_ARM_READINESS_NOT_READY
	}
	return manipulatorv1.ArmReadiness_ARM_READINESS_READY
}

// ensureEnabled turns the motors on the first time a lease holder moves the arm. Enabling holds the
// measured pose, so it doesn't move anything by itself. It refuses while the arm is being
// drag-taught (enabling would pull it out of teaching mode under someone's hands), after an
// e-stop or hard fault (that takes an explicit Recover), and when feedback isn't flowing.
func (s *Service) ensureEnabled(ctx context.Context) error {
	st, err := s.state(ctx, true)
	if err != nil {
		return errUnavailable
	}
	switch {
	case !st.FeedbackFresh:
		return errUnavailable
	case st.Teaching, st.Estopped, st.ArmStatus != "NORMAL" && !st.SoftFault:
		return errPrecond
	case st.Enabled:
		return nil
	}
	s.log.Info("piperd: enabling the arm motors for a lease holder")
	if err := s.drv.post(ctx, "/api/enable", map[string]any{"on": true}, nil); err != nil {
		s.log.Warn("piperd: enable failed", "err", err)
		return errUnavailable
	}
	select {
	case <-time.After(enableSettle):
	case <-ctx.Done():
		return errDeadline
	}
	return nil
}

// mapDriverErr turns a driver refusal into a StatusCode.
func mapDriverErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errBusy), errors.Is(err, errNotEnabled):
		return errPrecond
	case errors.Is(err, errStale):
		return errUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		return errDeadline
	}
	return errUnavailable
}

// speedFor maps max_vel_scale (0..1, 0 = default) to the driver's speed percentage.
func speedFor(scale float64) (int, bool) {
	if scale <= 0 {
		return 0, false
	}
	return int(math.Round(5 + 95*scale)), true
}

// ---------------------------------------------------------------- read-only

func (s *Service) GetArmState(ctx context.Context) (*manipulatorv1.ArmState, error) {
	st, err := s.state(ctx, false)
	if err != nil {
		return &manipulatorv1.ArmState{Readiness: manipulatorv1.ArmReadiness_ARM_READINESS_NOT_READY}, nil
	}
	out := &manipulatorv1.ArmState{
		Readiness:         readiness(st),
		Joints:            &manipulatorv1.JointState{},
		ActiveMotionEpoch: s.currentEpoch(),
	}
	if st.FeedbackFresh {
		for _, d := range st.Current {
			out.Joints.PositionRad = append(out.Joints.PositionRad, d*deg)
		}
		out.TcpPose = toProtoPose(st.PoseCurrent)
	}
	for j := 1; j <= 6; j++ {
		lim := st.Limits[string(rune('0'+j))]
		if len(lim) == 2 {
			out.JointLowerRad = append(out.JointLowerRad, lim[0]*deg)
			out.JointUpperRad = append(out.JointUpperRad, lim[1]*deg)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- short commands

func (s *Service) ServoToPose(cc sdk.CallContext, req *manipulatorv1.ServoToPoseRequest) (*manipulatorv1.ServoToPoseResult, error) {
	pose, err := fromProtoPose(req.GetTarget())
	if err != nil {
		return nil, errInvalid
	}
	if v := req.GetMaxVelScale(); math.IsNaN(v) || v < 0 || v > 1 {
		return nil, errInvalid
	}
	ok, err := s.admit(cc)
	if err != nil {
		return nil, err
	}
	if !ok {
		// Overtaken by a newer target from the same lease: nothing to do, and not an error.
		return &manipulatorv1.ServoToPoseResult{AcceptedTarget: req.GetTarget()}, nil
	}
	body := map[string]any{"pose": pose}
	if sp, set := speedFor(req.GetMaxVelScale()); set {
		body["speed"] = sp
	}
	var reply servoReply
	err = s.drv.post(cc.Ctx, "/api/servo", body, &reply)
	if errors.Is(err, errNotEnabled) {
		if err := s.ensureEnabled(cc.Ctx); err != nil {
			return nil, err
		}
		err = s.drv.post(cc.Ctx, "/api/servo", body, &reply)
	}
	if err != nil {
		return nil, mapDriverErr(err)
	}
	accepted := req.GetTarget()
	if reply.Clamped && len(reply.Goal) == 6 {
		// Keep the requested orientation exactly; only the position was clamped.
		accepted = &manipulatorv1.Pose{
			Frame: baseFrame,
			X:     reply.Goal[0] / 1000, Y: reply.Goal[1] / 1000, Z: reply.Goal[2] / 1000,
			Qx: req.GetTarget().GetQx(), Qy: req.GetTarget().GetQy(),
			Qz: req.GetTarget().GetQz(), Qw: req.GetTarget().GetQw(),
		}
	}
	return &manipulatorv1.ServoToPoseResult{AcceptedTarget: accepted, Clamped: reply.Clamped}, nil
}

func (s *Service) SetGripper(cc sdk.CallContext, req *manipulatorv1.SetGripperRequest) error {
	o := req.GetOpening()
	if math.IsNaN(o) || o < 0 || o > 1 || math.IsNaN(req.GetMaxEffort()) || req.GetMaxEffort() < 0 {
		return errInvalid
	}
	ok, err := s.admit(cc)
	if err != nil || !ok {
		return err
	}
	st, err := s.state(cc.Ctx, false)
	if err != nil {
		return errUnavailable
	}
	maxMM := defaultGripperMaxMM
	if len(st.GripperRange) == 2 && st.GripperRange[1] > 0 && st.GripperRange[1] < maxMM {
		maxMM = st.GripperRange[1]
	}
	body := map[string]any{"mm": o * maxMM}
	if e := req.GetMaxEffort(); e > 0 {
		// max_effort is in N*m; the driver takes 0.001 N*m, 0..5000
		body["effort"] = int(math.Min(5000, math.Round(e*1000)))
	}
	if !st.Enabled {
		if err := s.ensureEnabled(cc.Ctx); err != nil {
			return err
		}
	}
	return mapDriverErr(s.drv.post(cc.Ctx, "/api/gripper", body, nil))
}

func (s *Service) Stop(cc sdk.CallContext) error {
	// Stop is always honoured, whatever the sequence: a late Stop is still a Stop.
	if cc.Execution == nil {
		return errPrecond
	}
	return mapDriverErr(s.drv.post(cc.Ctx, "/api/stop", nil, nil))
}

// ---------------------------------------------------------------- long tasks
//
// These answer synchronously within the call's deadline: nervud closes the operation from the final
// result (see convergeSynchronousOperation). Each keeps the driver's deadman fed while it runs and
// gives up if a newer epoch arrives (the lease was released, pre-empted or revoked by Safety).

func (s *Service) beginMove(cc sdk.CallContext) (func(), error) {
	ok, err := s.admit(cc)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errPrecond
	}
	if err := s.ensureEnabled(cc.Ctx); err != nil {
		return nil, err
	}
	s.moveMu.Lock()
	s.mu.Lock()
	s.moveEpoch = cc.Execution.GetMotionEpoch()
	s.mu.Unlock()
	return s.moveMu.Unlock, nil
}

func (s *Service) superseded(epoch uint64) bool {
	return s.currentEpoch() != epoch
}

func (s *Service) GoHome(cc sdk.CallContext) (*manipulatorv1.ManipulatorMoveResult, error) {
	done, err := s.beginMove(cc)
	if err != nil {
		return nil, err
	}
	defer done()
	if err := s.drv.post(cc.Ctx, "/api/home", nil, nil); err != nil {
		return nil, mapDriverErr(err)
	}
	return s.waitJoints(cc, nil)
}

func (s *Service) MoveToPose(cc sdk.CallContext, req *manipulatorv1.MoveToPoseRequest) (*manipulatorv1.ManipulatorMoveResult, error) {
	pose, err := fromProtoPose(req.GetTarget())
	if err != nil {
		return nil, errInvalid
	}
	if v := req.GetMaxVelScale(); math.IsNaN(v) || v < 0 || v > 1 {
		return nil, errInvalid
	}
	done, err := s.beginMove(cc)
	if err != nil {
		return nil, err
	}
	defer done()
	epoch := cc.Execution.GetMotionEpoch()
	body := map[string]any{"pose": pose}
	if sp, set := speedFor(req.GetMaxVelScale()); set {
		body["speed"] = sp
	}
	deadline := time.Now().Add(moveTimeout)
	tick := time.NewTicker(servoRefreshPeriod)
	defer tick.Stop()
	for {
		var reply servoReply
		if err := s.drv.post(cc.Ctx, "/api/servo", body, &reply); err != nil {
			return nil, mapDriverErr(err)
		}
		if len(reply.Goal) == 6 {
			pose = reply.Goal // aim at the clamped goal, or "reached" never happens
		}
		st, err := s.state(cc.Ctx, true)
		if err == nil && st.FeedbackFresh && poseReached(st.PoseCurrent, pose) {
			_ = s.drv.post(context.Background(), "/api/stop", nil, nil)
			return &manipulatorv1.ManipulatorMoveResult{FinalPose: toProtoPose(st.PoseCurrent)}, nil
		}
		select {
		case <-cc.Ctx.Done():
			_ = s.drv.post(context.Background(), "/api/stop", nil, nil)
			return nil, errDeadline
		case <-tick.C:
		}
		if s.superseded(epoch) || time.Now().After(deadline) {
			_ = s.drv.post(context.Background(), "/api/stop", nil, nil)
			return nil, errPrecond
		}
	}
}

func (s *Service) MoveJointTrajectory(cc sdk.CallContext, req *manipulatorv1.MoveJointTrajectoryRequest) (*manipulatorv1.ManipulatorMoveResult, error) {
	pts := req.GetPoints()
	if len(pts) == 0 || len(pts) > maxTrajectoryPoints {
		return nil, errInvalid
	}
	var last int64 = -1
	for _, p := range pts {
		if len(p.GetPositionRad()) != 6 || p.GetTimeFromStartNanos() <= last {
			return nil, errInvalid
		}
		for _, v := range p.GetPositionRad() {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, errInvalid
			}
		}
		last = p.GetTimeFromStartNanos()
	}
	done, err := s.beginMove(cc)
	if err != nil {
		return nil, err
	}
	defer done()
	epoch := cc.Execution.GetMotionEpoch()
	st, err := s.state(cc.Ctx, true)
	if err != nil || !st.FeedbackFresh || len(st.Current) != 6 {
		return nil, errUnavailable
	}
	start := time.Now()
	from := append([]float64(nil), st.Current...)
	var fromT int64
	tick := time.NewTicker(trajectoryStreamPeriod)
	defer tick.Stop()
	for i := 0; i < len(pts); {
		elapsed := time.Since(start).Nanoseconds()
		for i < len(pts) && elapsed >= pts[i].GetTimeFromStartNanos() {
			from = radToDeg(pts[i].GetPositionRad())
			fromT = pts[i].GetTimeFromStartNanos()
			i++
		}
		target := from
		if i < len(pts) {
			// Linear interpolation toward the next waypoint.
			next := radToDeg(pts[i].GetPositionRad())
			span := float64(pts[i].GetTimeFromStartNanos() - fromT)
			a := 0.0
			if span > 0 {
				a = math.Min(1, float64(elapsed-fromT)/span)
			}
			target = make([]float64, 6)
			for j := range target {
				target[j] = from[j] + a*(next[j]-from[j])
			}
		}
		if err := s.drv.post(cc.Ctx, "/api/stream", map[string]any{"joints": target}, nil); err != nil {
			return nil, mapDriverErr(err)
		}
		select {
		case <-cc.Ctx.Done():
			_ = s.drv.post(context.Background(), "/api/stop", nil, nil)
			return nil, errDeadline
		case <-tick.C:
		}
		if s.superseded(epoch) {
			_ = s.drv.post(context.Background(), "/api/stop", nil, nil)
			return nil, errPrecond
		}
	}
	return s.waitJoints(cc, from)
}

// waitJoints waits for the arm to reach target joints (deg), or the driver's own joint targets
// when target is nil, sending heartbeats so the driver keeps streaming the move.
func (s *Service) waitJoints(cc sdk.CallContext, target []float64) (*manipulatorv1.ManipulatorMoveResult, error) {
	epoch := cc.Execution.GetMotionEpoch()
	deadline := time.Now().Add(moveTimeout)
	tick := time.NewTicker(servoRefreshPeriod)
	defer tick.Stop()
	for {
		_ = s.drv.post(cc.Ctx, "/api/heartbeat", nil, nil)
		st, err := s.state(cc.Ctx, true)
		if err == nil && st.FeedbackFresh {
			want := target
			if want == nil {
				want = st.ReadyPose
			}
			if jointsReached(st.Current, want) {
				return &manipulatorv1.ManipulatorMoveResult{FinalPose: toProtoPose(st.PoseCurrent)}, nil
			}
		}
		select {
		case <-cc.Ctx.Done():
			_ = s.drv.post(context.Background(), "/api/stop", nil, nil)
			return nil, errDeadline
		case <-tick.C:
		}
		if s.superseded(epoch) || time.Now().After(deadline) {
			_ = s.drv.post(context.Background(), "/api/stop", nil, nil)
			return nil, errPrecond
		}
	}
}

func radToDeg(r []float64) []float64 {
	out := make([]float64, len(r))
	for i, v := range r {
		out[i] = v / deg
	}
	return out
}

func poseReached(cur, goal []float64) bool {
	if len(cur) < 6 || len(goal) < 6 {
		return false
	}
	for ax := 0; ax < 3; ax++ {
		if math.Abs(cur[ax]-goal[ax]) > reachToleranceMM {
			return false
		}
	}
	for ax := 3; ax < 6; ax++ {
		d := math.Mod(cur[ax]-goal[ax]+540, 360) - 180
		if math.Abs(d) > reachToleranceDeg {
			return false
		}
	}
	return true
}

func jointsReached(cur, goal []float64) bool {
	if len(cur) < 6 || len(goal) < 6 {
		return false
	}
	for j := 0; j < 6; j++ {
		if math.Abs(cur[j]-goal[j]) > jointToleranceDeg {
			return false
		}
	}
	return true
}

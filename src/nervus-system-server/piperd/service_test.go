package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	manipulatorv1 "github.com/nervus-os/nervus-ipc/protocol/interface/manipulatorv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
)

// fakeDriver is a minimal stand-in for piper_webctl.py's API.
type fakeDriver struct {
	mu       sync.Mutex
	state    driverState
	calls    []string
	bodies   []map[string]any
	servoErr string // when set, /api/servo answers 409 with this error
}

func (f *fakeDriver) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.URL.Path)
		var body map[string]any
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			f.bodies = append(f.bodies, body)
		}
		switch r.URL.Path {
		case "/api/state":
			_ = json.NewEncoder(w).Encode(f.state)
		case "/api/enable":
			f.state.Enabled = true
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/servo":
			if f.servoErr != "" {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": f.servoErr})
				return
			}
			if !f.state.Enabled {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"motors are not enabled"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "goal": body["pose"], "clamped": false})
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	})
}

func (f *fakeDriver) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == path {
			n++
		}
	}
	return n
}

func newTestService(t *testing.T, st driverState) (*Service, *fakeDriver) {
	t.Helper()
	fd := &fakeDriver{state: st}
	srv := httptest.NewServer(fd.handler())
	t.Cleanup(srv.Close)
	return newService(newDriver(srv.URL), slog.New(slog.NewTextHandler(io.Discard, nil))), fd
}

func healthy() driverState {
	return driverState{
		Current:       []float64{0, 45, -60, 0, 45, 0},
		PoseCurrent:   []float64{138, 0, 418, 180, 65, 180},
		Enabled:       true,
		FeedbackFresh: true,
		ArmStatus:     "NORMAL",
		GripperRange:  []float64{0, 95},
		ReadyPose:     []float64{0, 45, -60, 0, 45, 0},
		Limits:        map[string][]float64{"1": {-154, 154}, "2": {0, 195}, "3": {-175, 0}, "4": {-106, 106}, "5": {-75, 75}, "6": {-100, 100}},
	}
}

func callCtx(epoch, seq uint64) sdk.CallContext {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = cancel
	return sdk.CallContext{Ctx: ctx, Execution: &ipcv1.ExecutionContext{MotionEpoch: epoch, CommandSequence: seq}}
}

func TestEulerQuatRoundTrip(t *testing.T) {
	for _, e := range [][3]float64{{180, 65, 180}, {-162.5, 63, -158.6}, {10, -20, 30}, {0, 0, 0}, {45, 89, 0}} {
		x, y, z, w := eulerToQuat(e[0]*deg, e[1]*deg, e[2]*deg)
		if n := math.Sqrt(x*x + y*y + z*z + w*w); math.Abs(n-1) > 1e-9 {
			t.Fatalf("quaternion for %v not unit: %v", e, n)
		}
		rx, ry, rz := quatToEuler(x, y, z, w)
		// Compare rotations, not angles: (180,65,180) and (0,115,0) are the same orientation.
		x2, y2, z2, w2 := eulerToQuat(rx, ry, rz)
		dot := math.Abs(x*x2 + y*y2 + z*z2 + w*w2)
		if math.Abs(dot-1) > 1e-9 {
			t.Fatalf("round trip of %v changed the rotation (dot %v)", e, dot)
		}
	}
}

// The convention must be static XYZ (R = Rz*Ry*Rx), as piper_sdk reports: a pure yaw of 90 deg
// turns the x axis onto y.
func TestEulerConventionIsStaticXYZ(t *testing.T) {
	x, y, z, w := eulerToQuat(0, 0, 90*deg)
	// rotate (1,0,0) by q
	vx := 1 - 2*(y*y+z*z)
	vy := 2 * (x*y + z*w)
	if math.Abs(vx) > 1e-9 || math.Abs(vy-1) > 1e-9 {
		t.Fatalf("yaw 90 maps x to (%v,%v), want (0,1)", vx, vy)
	}
}

func TestFromProtoPoseRejectsBadInput(t *testing.T) {
	good := toProtoPose([]float64{200, 0, 300, 180, 60, 180})
	if _, err := fromProtoPose(good); err != nil {
		t.Fatalf("valid pose refused: %v", err)
	}
	for name, p := range map[string]*manipulatorv1.Pose{
		"nil":            nil,
		"empty frame":    {X: 0.2, Qw: 1},
		"other frame":    {Frame: "tool0", X: 0.2, Qw: 1},
		"zero quat":      {Frame: baseFrame, X: 0.2},
		"nan":            {Frame: baseFrame, X: math.NaN(), Qw: 1},
		"huge quat norm": {Frame: baseFrame, X: 0.2, Qw: 3},
	} {
		if _, err := fromProtoPose(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAdmitDropsReorderedAndRefusesStaleEpochs(t *testing.T) {
	s, _ := newTestService(t, healthy())
	if ok, err := s.admit(callCtx(5, 10)); !ok || err != nil {
		t.Fatalf("first command: ok=%v err=%v", ok, err)
	}
	if ok, err := s.admit(callCtx(5, 9)); ok || err != nil {
		t.Fatalf("older sequence should be dropped silently: ok=%v err=%v", ok, err)
	}
	if ok, err := s.admit(callCtx(5, 11)); !ok || err != nil {
		t.Fatalf("newer sequence: ok=%v err=%v", ok, err)
	}
	if ok, err := s.admit(callCtx(6, 1)); !ok || err != nil {
		t.Fatalf("new epoch starts over: ok=%v err=%v", ok, err)
	}
	if _, err := s.admit(callCtx(5, 99)); !isCode(err, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION) {
		t.Fatalf("revoked epoch: err=%v, want FAILED_PRECONDITION", err)
	}
	if _, err := s.admit(sdk.CallContext{Ctx: context.Background()}); err == nil {
		t.Fatal("a lease-gated command without ExecutionContext was admitted")
	}
}

func TestReadiness(t *testing.T) {
	st := healthy()
	if r := readiness(&st); r != manipulatorv1.ArmReadiness_ARM_READINESS_READY {
		t.Fatalf("healthy = %v", r)
	}
	cases := map[string]func(*driverState){
		"stale":    func(s *driverState) { s.FeedbackFresh = false },
		"disabled": func(s *driverState) { s.Enabled = false },
		"teaching": func(s *driverState) { s.Teaching = true },
		"other":    func(s *driverState) { s.Owner = &driverOwner{Client: "act", Kind: "stream"} },
	}
	for name, mut := range cases {
		s := healthy()
		mut(&s)
		if r := readiness(&s); r != manipulatorv1.ArmReadiness_ARM_READINESS_NOT_READY {
			t.Errorf("%s = %v, want NOT_READY", name, r)
		}
	}
	s := healthy()
	s.ArmStatus = "COLLISION_DETECTED"
	if r := readiness(&s); r != manipulatorv1.ArmReadiness_ARM_READINESS_FAULT {
		t.Errorf("collision = %v, want FAULT", r)
	}
	s = healthy()
	s.ArmStatus, s.SoftFault = "TARGET_POS_EXCEEDS_LIMIT", true
	if r := readiness(&s); r != manipulatorv1.ArmReadiness_ARM_READINESS_READY {
		t.Errorf("soft fault = %v, want READY (it clears itself)", r)
	}
}

func TestServoEnablesMotorsOnFirstUse(t *testing.T) {
	st := healthy()
	st.Enabled = false
	s, fd := newTestService(t, st)
	req := &manipulatorv1.ServoToPoseRequest{Target: toProtoPose([]float64{200, 0, 300, 180, 60, 180}), MaxVelScale: 0.5}
	res, err := s.ServoToPose(callCtx(1, 1), req)
	if err != nil {
		t.Fatalf("servo: %v", err)
	}
	if fd.count("/api/enable") != 1 || fd.count("/api/servo") != 2 {
		t.Fatalf("calls = %v, want one enable and a retried servo", fd.calls)
	}
	if res.GetAcceptedTarget() == nil {
		t.Fatal("no accepted target")
	}
	fd.mu.Lock()
	last := fd.bodies[len(fd.bodies)-1]
	fd.mu.Unlock()
	if last["client"] != clientName || last["speed"] == nil {
		t.Fatalf("servo body = %v, want client=%s and a speed", last, clientName)
	}
}

func TestServoNeverPullsTheArmOutOfTeaching(t *testing.T) {
	st := healthy()
	st.Enabled, st.Teaching = false, true
	s, fd := newTestService(t, st)
	req := &manipulatorv1.ServoToPoseRequest{Target: toProtoPose([]float64{200, 0, 300, 180, 60, 180})}
	if _, err := s.ServoToPose(callCtx(1, 1), req); !isCode(err, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION) {
		t.Fatalf("err = %v, want FAILED_PRECONDITION while drag-teaching", err)
	}
	if fd.count("/api/enable") != 0 {
		t.Fatal("enabled the motors while someone was drag-teaching the arm")
	}
}

func TestServoBusyMapsToPrecondition(t *testing.T) {
	s, fd := newTestService(t, healthy())
	fd.servoErr = "busy"
	req := &manipulatorv1.ServoToPoseRequest{Target: toProtoPose([]float64{200, 0, 300, 180, 60, 180})}
	if _, err := s.ServoToPose(callCtx(1, 1), req); !isCode(err, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION) {
		t.Fatalf("err = %v, want FAILED_PRECONDITION when another client holds the arm", err)
	}
}

func TestGetArmStateUnits(t *testing.T) {
	s, _ := newTestService(t, healthy())
	st, err := s.GetArmState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.GetJoints().GetPositionRad()) != 6 || math.Abs(st.GetJoints().GetPositionRad()[1]-45*deg) > 1e-9 {
		t.Fatalf("joints = %v", st.GetJoints().GetPositionRad())
	}
	if p := st.GetTcpPose(); p.GetFrame() != baseFrame || math.Abs(p.GetX()-0.138) > 1e-9 || math.Abs(p.GetZ()-0.418) > 1e-9 {
		t.Fatalf("tcp pose = %+v (metres in base_link expected)", p)
	}
	if len(st.GetJointUpperRad()) != 6 || math.Abs(st.GetJointUpperRad()[1]-195*deg) > 1e-9 {
		t.Fatalf("limits = %v", st.GetJointUpperRad())
	}
}

func isCode(err error, code ipcv1.StatusCode) bool {
	var se *sdk.StatusError
	return errors.As(err, &se) && se.Code == code
}

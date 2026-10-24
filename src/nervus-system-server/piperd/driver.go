package main

// The client side of the PiPER driver's loopback HTTP API (hardware/piper/piper_webctl.py,
// nervus-piper.service). piperd never touches the CAN bus: the driver owns can0, and every other
// arm client (the Robot Arm app, the ACT engine) goes through it too, so the bus has one owner.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// clientName identifies piperd to the driver's one-controller-at-a-time arbitration.
const clientName = "piperd"

// errBusy means another client (the Robot Arm app, the ACT engine) controls the arm right now.
var errBusy = errors.New("piperd: the arm is controlled by another client")

// errNotEnabled means the motors are off.
var errNotEnabled = errors.New("piperd: motors are not enabled")

// errStale means the arm's feedback stopped flowing (unpowered, unplugged, or in standby).
var errStale = errors.New("piperd: arm feedback is stale")

// errGuard means the driver's IK flip guard just held the arm and refuses goals for a moment.
var errGuard = errors.New("piperd: the driver's IK guard is holding the arm")

// driverState is the subset of GET /api/state piperd reads.
type driverState struct {
	Current       []float64            `json:"current"`      // joint angles, deg
	PoseCurrent   []float64            `json:"pose_current"` // x,y,z mm; rx,ry,rz deg
	Enabled       bool                 `json:"enabled"`
	Estopped      bool                 `json:"estopped"`
	FeedbackFresh bool                 `json:"feedback_fresh"`
	Teaching      bool                 `json:"teaching"`
	SoftFault     bool                 `json:"soft_fault"`
	ArmStatus     string               `json:"arm_status"`
	Limits        map[string][]float64 `json:"limits"`
	GripperRange  []float64            `json:"gripper_range"`
	GripperActual *float64             `json:"gripper_actual_mm"`
	ReadyPose     []float64            `json:"ready_pose"`
	MotorsEnabled []bool               `json:"motors_enabled"`
	Owner         *driverOwner         `json:"owner"`
	Status        map[string]any       `json:"status"`
	FeedbackAgeMs *float64             `json:"feedback_age_ms"`
	ServoGoal     []float64            `json:"servo_goal"`
	WorkspaceMm   map[string][]float64 `json:"workspace_mm"`
	Speed         int                  `json:"speed"`
}

type driverOwner struct {
	Client string `json:"client"`
	Kind   string `json:"kind"`
}

type servoReply struct {
	OK      bool      `json:"ok"`
	Goal    []float64 `json:"goal"`
	Clamped bool      `json:"clamped"`
	Error   string    `json:"error"`
	Owner   string    `json:"owner"`
}

// driver talks to the arm driver over HTTP. Every request has a short timeout: a teleop stream at
// 20 Hz must never queue behind a hung request.
type driver struct {
	base string
	http *http.Client
}

func newDriver(base string) *driver {
	return &driver{
		base: base,
		http: &http.Client{Timeout: 2 * time.Second},
	}
}

func (d *driver) state(ctx context.Context) (*driverState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/api/state?lite=1", nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("piperd: driver unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("piperd: driver state: HTTP %d", resp.StatusCode)
	}
	var st driverState
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return nil, fmt.Errorf("piperd: decode driver state: %w", err)
	}
	return &st, nil
}

// post sends a JSON command. A 409 is translated into errBusy / errNotEnabled / errStale so the
// handlers can map it to a typed Manipulator reason.
func (d *driver) post(ctx context.Context, path string, body map[string]any, out any) error {
	if body == nil {
		body = map[string]any{}
	}
	body["client"] = clientName
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("piperd: driver unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusConflict {
		var e struct {
			Error string `json:"error"`
			Guard bool   `json:"guard"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Guard {
			return fmt.Errorf("%w: %s", errGuard, e.Error)
		}
		switch e.Error {
		case "busy":
			return errBusy
		case "motors are not enabled":
			return errNotEnabled
		case "arm feedback is stale":
			return errStale
		}
		return fmt.Errorf("piperd: driver refused %s: %s", path, e.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("piperd: driver %s: HTTP %d: %s", path, resp.StatusCode, bytes.TrimSpace(data))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("piperd: decode %s reply: %w", path, err)
		}
	}
	return nil
}

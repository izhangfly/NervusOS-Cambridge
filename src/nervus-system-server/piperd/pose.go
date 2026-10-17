package main

// Pose conversion between Manipulator@1 (metres + unit quaternion, explicit frame) and the
// PiPER's own representation (millimetres + RX/RY/RZ degrees).
//
// The PiPER reports and accepts static-XYZ Euler angles ("sxyz"): R = Rz(rz) * Ry(ry) * Rx(rx),
// the convention of piper_sdk's forward kinematics and its utils/tf.py. Getting it wrong would
// still round-trip (a held orientation stays held), but any app that composes rotations would
// steer the wrist the wrong way.

import (
	"errors"
	"math"

	manipulatorv1 "github.com/nervus-os/nervus-ipc/protocol/interface/manipulatorv1"
)

// baseFrame is the only frame piperd accepts. The interface forbids implicit frames.
const baseFrame = "base_link"

const deg = math.Pi / 180.0

var errBadPose = errors.New("piperd: invalid pose")

// eulerToQuat converts sxyz Euler angles (radians) to a unit quaternion (x, y, z, w).
func eulerToQuat(rx, ry, rz float64) (x, y, z, w float64) {
	cr, sr := math.Cos(rx/2), math.Sin(rx/2)
	cp, sp := math.Cos(ry/2), math.Sin(ry/2)
	cy, sy := math.Cos(rz/2), math.Sin(rz/2)
	w = cr*cp*cy + sr*sp*sy
	x = sr*cp*cy - cr*sp*sy
	y = cr*sp*cy + sr*cp*sy
	z = cr*cp*sy - sr*sp*cy
	return x, y, z, w
}

// quatToEuler converts a quaternion to sxyz Euler angles (radians). The quaternion is normalised
// first; near gimbal lock (pitch at +-90 deg) yaw is folded into roll, as piper_sdk does.
func quatToEuler(x, y, z, w float64) (rx, ry, rz float64) {
	n := math.Sqrt(x*x + y*y + z*z + w*w)
	x, y, z, w = x/n, y/n, z/n, w/n
	r00 := 1 - 2*(y*y+z*z)
	r10 := 2 * (x*y + z*w)
	r20 := 2 * (x*z - y*w)
	r21 := 2 * (y*z + x*w)
	r22 := 1 - 2*(x*x+y*y)
	cy := math.Hypot(r00, r10)
	if cy > 1e-9 {
		rx = math.Atan2(r21, r22)
		ry = math.Atan2(-r20, cy)
		rz = math.Atan2(r10, r00)
		return rx, ry, rz
	}
	r11 := 1 - 2*(x*x+z*z)
	r12 := 2 * (y*z - x*w)
	rx = math.Atan2(-r12, r11)
	ry = math.Atan2(-r20, cy)
	rz = 0
	return rx, ry, rz
}

// toProtoPose converts the driver's [x,y,z mm, rx,ry,rz deg] to a Manipulator pose.
func toProtoPose(p []float64) *manipulatorv1.Pose {
	if len(p) < 6 {
		return nil
	}
	qx, qy, qz, qw := eulerToQuat(p[3]*deg, p[4]*deg, p[5]*deg)
	return &manipulatorv1.Pose{
		Frame: baseFrame,
		X:     p[0] / 1000, Y: p[1] / 1000, Z: p[2] / 1000,
		Qx: qx, Qy: qy, Qz: qz, Qw: qw,
	}
}

// fromProtoPose validates a Manipulator pose and converts it to the driver's units.
func fromProtoPose(p *manipulatorv1.Pose) ([]float64, error) {
	if p == nil || p.GetFrame() != baseFrame {
		return nil, errBadPose
	}
	vals := []float64{p.GetX(), p.GetY(), p.GetZ(), p.GetQx(), p.GetQy(), p.GetQz(), p.GetQw()}
	for _, v := range vals {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, errBadPose
		}
	}
	// A quaternion far from unit length isn't a rotation that slipped a little; it's garbage.
	n := math.Sqrt(p.GetQx()*p.GetQx() + p.GetQy()*p.GetQy() + p.GetQz()*p.GetQz() + p.GetQw()*p.GetQw())
	if n < 0.5 || n > 1.5 {
		return nil, errBadPose
	}
	rx, ry, rz := quatToEuler(p.GetQx(), p.GetQy(), p.GetQz(), p.GetQw())
	return []float64{p.GetX() * 1000, p.GetY() * 1000, p.GetZ() * 1000, rx / deg, ry / deg, rz / deg}, nil
}

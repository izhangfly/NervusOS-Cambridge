// This file is the handshake: the first round trip after the connection is set up (Hello → HelloAck).
//
// Before the handshake nervud accepts no other body, and the handshake has its own short deadline: connecting and saying nothing
// would otherwise occupy a connection slot for free. So every step in this file has a timeout.
package sdk

import (
	"fmt"
	"time"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// Identity is the **verified** identity nervud echoes back after the handshake.
//
// It isn't an identity nervud grants: the identity was settled at SO_PEERCRED (the PID/UID/GID the kernel gives).
// Echoing it lets the SDK spot configuration mismatches early ("I thought I was com.foo.a,
// the kernel says I'm com.foo.b"), which would otherwise only surface at the first permission refusal.
type Identity struct {
	PackageID   string
	ComponentID string
}

// handshake sends Hello and waits for HelloAck, filling in the conn's negotiation result on success.
//
// declaredComponentID is the Component ID this process **claims**. It's a lead to be verified, not an
// identity statement: nervud checks it through the peer PID → cgroup → systemd unit → launch record,
// and refuses the handshake on a mismatch. The client still provides it because several Components of the same Package
// share a UID, and having the claimed value first narrows the check from "walking every Component of the Package" down to
// one comparison. Getting it wrong won't let you impersonate anyone; it will only stop you connecting.
func (co *conn) handshake(sdkName, sdkVersion, declaredComponentID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if err := co.c.SetDeadline(deadline); err != nil {
		return fmt.Errorf("sdk: set handshake deadline: %w", err)
	}
	// Clear the handshake deadline whether it succeeded or failed: it only covers the handshake, and later reads and writes manage
	// their own timeouts. Forgetting would make the first ordinary request fail mysteriously when the deadline expires.
	defer func() { _ = co.c.SetDeadline(time.Time{}) }()

	hello := &ipcv1.Envelope{
		// Handshake frames **must** carry the version: these are the only two places in the whole protocol where it means anything.
		ProtocolMajor: protocolMajorMax,
		ProtocolMinor: protocolMinorMax,
		Body: &ipcv1.Envelope_Hello{Hello: &ipcv1.Hello{
			MinProtocolMajor: protocolMajorMin,
			MaxProtocolMajor: protocolMajorMax,
			MaxProtocolMinor: protocolMinorMax,
			// sdk_name / sdk_version are only for diagnostics and compatibility statistics, **never used in any decision**.
			// Permissions come from Package signatures/install origin/manifest, not from "which SDK is installed".
			SdkName:             sdkName,
			SdkVersion:          sdkVersion,
			DeclaredComponentId: declaredComponentID,
		}},
	}
	if err := co.writeEnvelope(hello); err != nil {
		return fmt.Errorf("sdk: send Hello: %w", err)
	}

	env, err := co.readEnvelope()
	if err != nil {
		return fmt.Errorf("sdk: read HelloAck: %w", err)
	}
	ack := env.GetHelloAck()
	if ack == nil {
		// Only HelloAck can arrive during the handshake. Anything else means the peer isn't nervud, or
		// the state machine is confused; neither case should continue.
		return fmt.Errorf("%w: expected HelloAck, got %T", ErrProtocol, env.GetBody())
	}

	if f := ack.GetFailure(); f != nil {
		// When versions can't be agreed or identity verification fails, nervud **sends a Failure HelloAck first, then closes**,
		// never a bare close. So the exact reason is available here: UNAUTHENTICATED usually means
		// declared_component_id disagrees with kernel facts, and FAILED_PRECONDITION means no version intersection.
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, statusErrorFrom(f))
	}

	ok := ack.GetSuccess()
	if ok == nil {
		return fmt.Errorf("%w: HelloAck has neither success nor failure", ErrProtocol)
	}

	co.negMajor = ok.GetProtocolMajor()
	co.negMinor = ok.GetProtocolMinor()
	co.packageID = ok.GetPackageId()
	co.componentID = ok.GetComponentId()
	co.limits = ok.GetLimits()

	if co.negMajor != protocolMajorMax {
		// The server chose a major we didn't declare support for. The protocol says it must choose within the closed range we gave,
		// so going out of range means the peer's implementation is broken; carrying on would only quietly disagree on some field.
		return fmt.Errorf("%w: server chose unsupported major %d", ErrProtocol, co.negMajor)
	}
	return nil
}

// Identity returns the identity verified at the handshake. Returns the zero value before the handshake.
func (co *conn) Identity() Identity {
	return Identity{PackageID: co.packageID, ComponentID: co.componentID}
}

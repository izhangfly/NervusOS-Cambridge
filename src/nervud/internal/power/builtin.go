// This package wraps whole-machine power actions (orderly reboot / power off) as a built-in endpoint handler,
// for nervud to register during setup (endpoint.RegisterBuiltin).
//
// # Why a built-in endpoint rather than a new Envelope body
//
// The rule in envelope.proto: "before adding a new body, ask whether it is one of the eight things: establish a connection / discover
// endpoints / make a call / return a result / cancel / subscribe / push events / keep alive;
// if not, it should be a method on some Interface." Powering off is none of them.
//
// Yet no external Provider could offer it: the Authority Gate does the work, and only nervud
// has that power. So it's built in: callers use the completely standard Resolve + Request,
// without knowing or needing to know whether the other end is the kernel or a Provider. Same shape as safety.control.
//
// # Why the method IDs are hand-written constants
//
// safety takes its method_ids from the proto-generated enum and deliberately doesn't copy them locally, since a copy goes
// stale quietly. That isn't possible here: this interface has no .proto yet.
//
// So the trade-off is hand-written values cross-referenced on both sides: the constants in this file are the only definition, and the Kotlin side
//
//	(PowerControl.kt in nervus-system-software's ui-common) points back here in its comments.
//
// Once v2 adds power_control.proto, switch this to the generated enum straight away,
// and move the Kotlin side over too. Until then, changing either side means changing the other,
// or the symptom is "I pressed it and nothing happened": the call is routed to a method that doesn't exist and gets NOT_FOUND.
package power

import (
	"context"
	"fmt"
	"log/slog"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/authority"
	"github.com/nervus-os/nervud/internal/endpoint"
	"github.com/nervus-os/nervud/internal/identity"
)

// BuiltinInterfaceID is this built-in interface's ID.
//
// It must match two places: the entry in the catalog bootstrap (which sets the threshold)
// and the interfaces declaration in callers' manifests
const BuiltinInterfaceID = "nervus.interface.power"

// Method IDs. See the top of the file for why
const (
	// MethodReboot: orderly reboot. Empty request and response payloads
	MethodReboot uint32 = 1
	// MethodPowerOff: orderly power off. Empty request and response payloads
	MethodPowerOff uint32 = 2
)

// Gate is this package's narrow dependency on authority
type Gate interface {
	Power(ctx context.Context, subj authority.Subject, req authority.PowerRequest) error
}

// Module holds the minimal dependencies for performing power actions
type Module struct {
	gate Gate
	log  *slog.Logger
}

// New constructs a Module. With a nil gate the handler always fails closed with UNAVAILABLE
func New(gate Gate, log *slog.Logger) *Module {
	return &Module{gate: gate, log: log}
}

// BuiltinHandler returns a handler that can be passed straight to endpoint.RegisterBuiltin.
func (m *Module) BuiltinHandler() endpoint.BuiltinHandler {
	return func(call endpoint.BuiltinCall) endpoint.BuiltinResult {
		var payload []byte
		var code ipcv1.StatusCode
		switch call.MethodID {
		case MethodReboot:
			payload, code = m.act(call.Context, call.Caller, authority.PowerActionReboot)
		case MethodPowerOff:
			payload, code = m.act(call.Context, call.Caller, authority.PowerActionPowerOff)
		default:
			// Fail closed: an unimplemented method is a method that doesn't exist. Silently succeeding would make the caller think
			// the machine is shutting down, then wait forever for a shutdown that never happens
			code = ipcv1.StatusCode_STATUS_CODE_NOT_FOUND
		}
		return endpoint.BuiltinResult{Payload: payload, Code: code}
	}
}

func (m *Module) act(ctx context.Context, caller identity.Caller, action authority.PowerAction) ([]byte, ipcv1.StatusCode) {
	if m == nil || m.gate == nil {
		return nil, ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE
	}

	// Subject is the caller rather than kernel: the audit has to answer "who shut the machine down".
	// The Gate's osPower records its own intent entry with Subject=kernel, which answers
	// "the shutdown really started"; each covers one side, and both are needed
	subj := authority.Subject{PackageID: caller.PackageID, UID: caller.UID}
	req := authority.PowerRequest{
		Action: action,
		// Reason is required by Validate. On this path the reason is "a user started it from some package",
		// so the package ID goes in, and afterwards you can match whether it came from Settings or elsewhere
		Reason: fmt.Sprintf("requested by %s", subj.String()),
	}

	if err := m.gate.Power(ctx, subj, req); err != nil {
		if m.log != nil {
			m.log.Warn("power: action failed", "action", action.String(), "caller", subj.String(), "err", err)
		}
		return nil, ipcv1.StatusCode_STATUS_CODE_INTERNAL
	}

	// Getting here usually means systemd has taken over the shutdown, and this process may get SIGTERM at any moment.
	// Still return OK: the caller may receive it in a tiny window, and if it does it should show "shutting down"
	return nil, ipcv1.StatusCode_STATUS_CODE_OK
}

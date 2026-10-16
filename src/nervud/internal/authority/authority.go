// This file implements the Gate and the four-step pipeline do; request types are in request.go,
// platform implementations in the ops*.go files
package authority

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/authority/systemd"
)

// UnitManager is authority's narrow interface dependency on the systemd subpackage: start/stop/wait for a transient unit.
// *systemd.Conn satisfies it implicitly. authority takes it as an injected interface rather than calling new systemd.Conn directly, so
// tests that don't start processes can pass nil (StartSandboxedProcess fails closed), and unit tests can inject a
// fake that runs through Validate/audit without touching real D-Bus
type UnitManager interface {
	StartTransientUnit(ctx context.Context, spec systemd.UnitSpec) error
	StopUnit(ctx context.Context, name string) error
	WaitUnit(ctx context.Context, name string) (systemd.ExitInfo, error)
}

// PowerManager is an optional extension of UnitManager: orderly whole-machine power actions.
// *systemd.Conn satisfies it implicitly (see systemd/power.go).
//
// Why it isn't merged into UnitManager: starting processes and shutting down are two capabilities with completely different blast radiuses,
// and merging them would force every test that wants to inject a fake spawner to implement shutdown methods, which are exactly
// what should never be implemented casually. An optional interface + type assertion fails closed when it isn't there
// (osPower returns ErrUnsupportedPlatform), neither widening the responsibilities of existing test doubles
// nor leaving any room for "quietly falling back to a hard reboot without real systemd".
type PowerManager interface {
	Reboot(ctx context.Context) error
	PowerOff(ctx context.Context) error
}

// Gate is the single entry point for all of NSOS's Linux privileged operations.
//
// It's a code architecture and audit boundary, not process-level isolation.
// If nervud's execution is completely hijacked, the whole NSOS kernel should be considered lost.
// The Gate's value is in making privileged operations enumerable, auditable and reviewable,
// not in stopping an attacker who already has execution.
//
// The Gate is infrastructure, not a kernel.Module: it has no background loop to start or stop.
// It's built directly by assemble and injected into modules, with the same lifetime as the process.
// When injecting, give each consumer the narrow interface it needs; don't pass the whole Gate down.
type Gate struct {
	auditor audit.Recorder
	inv     *Invariants
	log     *slog.Logger
	// spawner is the systemd unit management backend; nil means no process-starting capability is configured, and
	// StartSandboxedProcess/StopProcess/WaitProcess return ErrUnsupportedPlatform
	spawner UnitManager
}

type Config struct {
	Auditor    audit.Recorder
	Invariants *Invariants // nil means DefaultInvariants
	Log        *slog.Logger
	// Spawner is the systemd unit management backend (in production main.go builds it with systemd.Dial and injects it).
	// When nil the Gate can still be used for non-process operations (install / tree deletion / setting owners / reboot)
	Spawner UnitManager
}

// New constructs the Gate.
// Note: a missing Auditor isn't allowed
func New(cfg Config) (*Gate, error) {
	if cfg.Auditor == nil {
		return nil, fmt.Errorf("authority: Auditor is required")
	}
	if cfg.Log == nil {
		return nil, fmt.Errorf("authority: Log is required")
	}
	inv := cfg.Invariants
	if inv == nil {
		inv = DefaultInvariants()
	}
	return &Gate{auditor: cfg.Auditor, inv: inv, log: cfg.Log, spawner: cfg.Spawner}, nil
}

// How it works
//
//	a module makes a request -> Authority checks the low-level invariants -> executes -> Audit records
//
// V2-TODO: a Policy decision step.
// Every caller on this path is a kernel module, part of the same TCB and equally trusted.
// Of the real threats, what actually crosses a trust boundary is App requests,
// which are handled by internal/permission's capability enforcement, not here.
// Policy's real customers are scheduler (granting RT priority) and safety (tightening OEM Safety Contracts, NRCP).
//
// Every exported operation must go through this function: "invariants were checked, audit was written" is guaranteed by structure, not by developer discipline.
// Generics only remove the repeated per-operation pipeline; Res is struct{} for operations with no return value.
//
// run is the package's unexported platform implementation and accepts no outside injection
func do[Req Request, Res any](
	ctx context.Context,
	g *Gate,
	subj Subject,
	req Req,
	run func(context.Context, Req) (Res, error),
) (Res, error) {
	var zero Res
	kind := req.Kind()
	detail := ""
	if d, ok := any(req).(interface{ Detail() string }); ok {
		detail = d.Detail()
	}

	// Policy seam: if a subject-dependent, changeable decision ever really needs to sit on the privileged path, it goes here
	//  (Subject already runs through every signature and audit, so only the check would be added, without changing interfaces); refusals use ErrPolicyDenied

	// Invariants: hard system constraints independent of the caller, which no policy may exempt
	if err := req.Validate(g.inv); err != nil {
		g.auditor.Record(ctx, audit.Event{
			Action: kind.String(), Subject: subj.String(), Denied: true, Err: err, Detail: detail,
		})
		return zero, err
	}

	// Execute the real Linux operation (only this package and scheduler may touch syscall / x/sys / os/exec directly)
	res, err := run(ctx, req)

	// Audit: record both success and failure. An audit log that only records successes is worthless as evidence
	g.auditor.Record(ctx, audit.Event{
		Action: kind.String(), Subject: subj.String(), Denied: false, Err: err, Detail: detail,
	})
	if err != nil {
		// ErrAlreadyExists is lowered to Debug: it's the expected result of an idempotent operation, not a fault.
		// Callers treat it as "already done" (see pkgregistry.provisionEntry), and
		// provisionAll runs it for every package on every start. Logged as ERROR, every boot would
		// print one line per installed package, and ops would soon learn to ignore the ERROR level.
		//
		// The audit record isn't affected: the Record above always carries err, so the evidence is still complete.
		// Only the human-facing path is lowered.
		if errors.Is(err, ErrAlreadyExists) {
			g.log.Debug("authority: target already exists (idempotent no-op)",
				"kind", kind.String(), "subject", subj.String(), "err", err)
		} else {
			g.log.Error("authority: operation failed",
				"kind", kind.String(), "subject", subj.String(), "err", err)
		}
		return zero, fmt.Errorf("authority: %s: %w", kind, err)
	}
	return res, nil
}

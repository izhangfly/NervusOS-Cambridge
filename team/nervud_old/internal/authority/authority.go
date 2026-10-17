// This file implements the Gate and its four-step pipeline do(); request types are in request.go.
// Platform implementations are in ops_linux.go / ops_other.go.
package authority

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nervus-os/nervud/internal/audit"
)

// Gate is the single entry point for every privileged Linux operation in NSOS.
//
// It is a code-architecture and audit boundary, not process-level isolation:
// if nervud's execution is fully hijacked, treat the whole NSOS kernel as lost.
// The Gate's value is that privileged operations can be enumerated, audited and reviewed,
// not that it stops an attacker who already has execution.
//
// Gate is a facility, not a kernel.Module: it has no background loop to start or stop.
// assemble() constructs it directly and injects it into the modules; it lives as long as the process.
// Inject the narrow interface each consumer needs; do not pass the whole Gate down.
type Gate struct {
	auditor audit.Recorder
	inv     *Invariants
	log     *slog.Logger
}

type Config struct {
	Auditor    audit.Recorder
	Invariants *Invariants // nil means DefaultInvariants()
	Log        *slog.Logger
}

// New constructs a Gate.
// Note: an Auditor is mandatory.
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
	return &Gate{auditor: cfg.Auditor, inv: inv, log: cfg.Log}, nil
}

// How it works:
//
//	a module makes a request -> Authority checks the low-level invariants -> runs it -> Audit records it
//
// V2-TODO: a Policy decision step.
// Every caller on this path is a kernel module, inside the same TCB and equally trusted,
// so there is no real threat here: what actually crosses a trust boundary is an App's request,
// and that is enforced by internal/permission's capability checks, not by this path.
// Policy's real clients will be the scheduler (granting RT priority) and safety (tightening the OEM Safety Contract, NRCP).
//
// Every exported operation must go through this function: "the invariants were checked and the audit was written" is guaranteed by structure, not by developer discipline.
// The generics only remove the per-operation copy of the pipeline; for operations without a result, Res is struct{}
//
// run is the package-private platform implementation and cannot be injected from outside.
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

	// The Policy seam: if a subject-dependent, changeable decision ever has to sit on the privileged path, it goes here
	// (Subject already runs through every signature and audit, so it would add a check without touching the interface); deny with ErrPolicyDenied.

	// Invariants: hard system constraints that do not depend on the caller; no policy may waive them.
	if err := req.Validate(g.inv); err != nil {
		g.auditor.Record(ctx, audit.Event{
			Action: kind.String(), Subject: subj.String(), Denied: true, Err: err, Detail: detail,
		})
		return zero, err
	}

	// Run the real Linux operation (only this package and scheduler may touch syscall / x/sys / os/exec directly).
	res, err := run(ctx, req)

	// Audit: record success and failure alike. An audit log of successes only is worthless as evidence.
	g.auditor.Record(ctx, audit.Event{
		Action: kind.String(), Subject: subj.String(), Denied: false, Err: err, Detail: detail,
	})
	if err != nil {
		g.log.Error("authority: operation failed",
			"kind", kind.String(), "subject", subj.String(), "err", err)
		return zero, fmt.Errorf("authority: %s: %w", kind, err)
	}
	return res, nil
}

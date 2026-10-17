// See doc.go for the package description
//
// audit imports no sibling module. Dependency arrows always point at audit, so an import cycle is structurally impossible
package audit

import (
	"context"
	"log/slog"
)

// Event is one security/permission audit record.
//
// Fixed fields, no `any`: otherwise it cannot be analysed offline as structured data.
//
// authority, permission, safety and other modules all use this type, so it is defined in the audit package.
type Event struct {
	Action  string // the operation code as a string, e.g. "CreatePrivateDataDirectory"
	Subject string // who asked, e.g. "pkg:com.example.app uid:20001"; "kernel" = the kernel itself
	Denied  bool   // true = stopped by policy or an invariant; the operation did not run
	Err     error  // nil means success; when Denied, the reason
	Detail  string // extra information (such as the target path). Never write keys or passwords here
}

// Recorder is the write interface of the audit. Users hold this interface, not a concrete implementation.
type Recorder interface {
	Record(ctx context.Context, ev Event)
}

// New returns the minimal implementation.
//
// TODO(rewrite): replace with a dedicated append-only audit file, with rotation and integrity protection.
// Mixed in with ordinary logs, the audit can be filtered out by log level and cannot be protected from being overwritten.
func New(log *slog.Logger) Recorder { return &slogRecorder{log: log} }

type slogRecorder struct{ log *slog.Logger }

// Record writes one audit entry at a fixed Info level: the audit is not debugging output
// and must not disappear because someone turned the log level down.
func (r *slogRecorder) Record(ctx context.Context, ev Event) {
	r.log.LogAttrs(ctx, slog.LevelInfo, "audit",
		slog.String("action", ev.Action),
		slog.String("subject", ev.Subject),
		slog.Bool("denied", ev.Denied),
		slog.Any("err", ev.Err),
		slog.String("detail", ev.Detail),
	)
}

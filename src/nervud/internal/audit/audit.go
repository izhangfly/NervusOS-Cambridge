// This file keeps only the shared types and a minimal slog implementation, so security-sensitive modules can record audits without depending on
// the file backend or a particular persistence strategy.
package audit

import (
	"context"
	"log/slog"
)

// Event is one security/permission audit record.
//
// The fields are fixed, with no any, otherwise offline structured analysis would be impossible.
//
// This type is shared by several modules (authority / permission / safety and others), so it's defined on its own in the audit package.
type Event struct {
	Action  string // the operation code as a string, such as "CreatePrivateDataDirectory"
	Subject string // the initiator, such as "pkg:com.example.app uid:20001"; "kernel" = the kernel itself
	Denied  bool   // true = stopped by policy or an invariant; the operation didn't run
	Err     error  // nil means success; the refusal reason when Denied
	Detail  string // extra information (such as the target path). Never write keys or passwords here
}

// Recorder is the audit write interface. Users hold this interface rather than a concrete implementation
type Recorder interface {
	Record(ctx context.Context, ev Event)
}

// New returns the minimal implementation that only writes to slog.
//
// Production uses NewFileRecorder: audit mixed in with ordinary logs gets filtered out by the log level,
// overwritten by the rotation policy, and nothing can prove it wasn't altered. The version in file.go covers
// those three things, and still writes to slog as well.
//
// This implementation is for tests and minimal wiring that doesn't need to keep evidence.
func New(log *slog.Logger) Recorder { return &slogRecorder{log: log} }

type slogRecorder struct{ log *slog.Logger }

// Record writes one audit entry. Always at Info level: audit isn't debug information
// and must not be lost because someone lowered the log level.
func (r *slogRecorder) Record(ctx context.Context, ev Event) {
	r.log.LogAttrs(ctx, slog.LevelInfo, "audit",
		slog.String("action", ev.Action),
		slog.String("subject", ev.Subject),
		slog.Bool("denied", ev.Denied),
		slog.Any("err", ev.Err),
		slog.String("detail", ev.Detail),
	)
}

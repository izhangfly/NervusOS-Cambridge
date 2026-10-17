// This file defines Authority's operation codes (Kind), requesters (Subject) and request contract (Request).
// The Gate and its four-step pipeline are in authority.go; invariants in invariant.go.
package authority

import "fmt"

// Kind is the fixed operation code of a privileged operation, used for audit records and offline analysis.
//
// Once assigned, a value is never reused: audit logs are kept for a long time, and reusing a value would change what history means.
// Retired operations keep their constant, marked Deprecated, in place.
type Kind uint16

const (
	// KindUnspecified: the zero value is invalid, so an uninitialised request is never taken for a real operation
	KindUnspecified Kind = 0

	KindPrepareAppIdentity     Kind = 1
	KindInstallVerifiedPackage Kind = 2
	KindCreatePrivateDataDir   Kind = 3
	KindStartSandboxedProcess  Kind = 4
	KindStopProcess            Kind = 5
	KindSetOwner               Kind = 6
	KindEnableFsVerity         Kind = 7
	KindReboot                 Kind = 8
)

func (k Kind) String() string {
	switch k {
	case KindPrepareAppIdentity:
		return "PrepareAppIdentity"
	case KindInstallVerifiedPackage:
		return "InstallVerifiedPackage"
	case KindCreatePrivateDataDir:
		return "CreatePrivateDataDirectory"
	case KindStartSandboxedProcess:
		return "StartSandboxedProcess"
	case KindStopProcess:
		return "StopProcess"
	case KindSetOwner:
		return "SetOwner"
	case KindEnableFsVerity:
		return "EnableFsVerity"
	case KindReboot:
		return "Reboot"
	default:
		return "Unspecified"
	}
}

// Subject is who made a privileged request, used to attribute it in the audit.
//
// The identity module does not exist yet, so for now the caller fills in Subject; once SO_PEERCRED is wired in,
// identity will resolve it from the connection, and clients will never be allowed to declare their own identity.
type Subject struct {
	PackageID string // empty string = the kernel itself
	UID       uint32
}

// SubjectKernel returns the Subject that stands for the nervud kernel itself.
// It gets no automatic exemption: low-level invariants are still checked and audited.
func SubjectKernel() Subject { return Subject{} }

// String builds the attribution string for the audit (the audit.Event.Subject field).
func (s Subject) String() string {
	if s.PackageID == "" {
		return "kernel"
	}
	return fmt.Sprintf("pkg:%s uid:%d", s.PackageID, s.UID)
}

// Request is the common contract of every privileged request.
//
// # Kind classifies it for the audit; Validate does the self-checks that do not depend on the caller.
//
// Optional extension: if a request type also implements interface{ Detail() string }, the result goes into
// the audit record's Detail field (such as the target path).
type Request interface {
	Kind() Kind
	Validate(inv *Invariants) error
}

// This file defines Authority's operation codes (Kind), initiator (Subject) and request contract (Request).
// The Gate and the four-step pipeline are in authority.go; invariants in invariant.go
package authority

import "fmt"

// Kind is the fixed operation code of a privileged operation, for audit storage and offline analysis.
//
// Once assigned, a value is never reused. Audit logs are kept for a long time, and reusing a value would change the meaning of history.
// Retired operations have their constant marked Deprecated and left in place
type Kind uint16

const (
	// KindUnspecified: the zero value is invalid, so uninitialised requests can't pass as legal operations
	KindUnspecified Kind = 0

	KindPrepareAppIdentity     Kind = 1
	KindInstallVerifiedPackage Kind = 2
	KindCreatePrivateDataDir   Kind = 3
	KindStartSandboxedProcess  Kind = 4
	KindStopProcess            Kind = 5
	KindSetOwner               Kind = 6
	KindEnableFsVerity         Kind = 7
	KindReboot                 Kind = 8
	KindRemovePackageTree      Kind = 9
	KindEnsureAppUser          Kind = 10
	// KindPowerAction is an orderly reboot/shutdown (going through systemd's full shutdown.target).
	// It's separate from KindReboot because they're two different things: KindReboot is a reboot(2) hard reboot
	// for fault recovery; this is a normal power action the user starts in Settings.
	// Whether it's a reboot or a shutdown is decided by PowerRequest.Action, which goes into the audit Detail
	KindPowerAction Kind = 11
	// KindSetUserDataAccess adds or removes a UID's entry in the user documents area's ACL, i.e.
	// the runtime grant and revocation of perm.storage.user. Separate from KindSetOwner:
	// that changes the owner, this changes the access control list, and audit readers care about different questions for each
	KindSetUserDataAccess Kind = 12
)

func (k Kind) String() string {
	switch k {
	case KindEnsureAppUser:
		return "EnsureAppUser"
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
	case KindRemovePackageTree:
		return "RemovePackageTree"
	case KindPowerAction:
		return "PowerAction"
	case KindSetUserDataAccess:
		return "SetUserDataAccess"
	default:
		return "Unspecified"
	}
}

// Subject is the initiator of a privileged request, for audit attribution.
//
// The identity module hasn't landed yet, so Subject is filled in by the caller for now; once SO_PEERCRED is wired,
// identity will resolve it from the connection, and external clients may not self-report identities
type Subject struct {
	PackageID string // empty string = the kernel itself
	UID       uint32
}

// SubjectKernel returns the Subject representing the nervud kernel itself.
// It gets no automatic exemption: low-level invariants are still checked and audited
func SubjectKernel() Subject { return Subject{} }

// String builds the attribution string for audit (the audit.Event.Subject field)
func (s Subject) String() string {
	if s.PackageID == "" {
		return "kernel"
	}
	return fmt.Sprintf("pkg:%s uid:%d", s.PackageID, s.UID)
}

// Request is the uniform contract of every privileged request.
//
// Kind is for audit classification; Validate does caller-independent self-checks.
//
// Optional extension: if a request type also implements interface{ Detail() string }, the return value goes into
// the audit record's Detail field (such as the target path)
type Request interface {
	Kind() Kind
	Validate(inv *Invariants) error
}

package safety

// ReportKind distinguishes the kinds of fact an OEM Provider reports back across the Safety boundary.
type ReportKind uint8

const (
	// ReportHaltAccepted: the Provider accepted the stop request (!= physically at standstill).
	ReportHaltAccepted ReportKind = iota + 1
	// ReportStopProgress: stop progress advanced to a phase.
	ReportStopProgress
	// ReportStandstillConfirmed: trustworthy standstill evidence obtained.
	ReportStandstillConfirmed
	// ReportProviderFault: a Provider fault.
	ReportProviderFault
)

// FaultCode classifies Provider faults. Values must match safety.proto's FaultCode.
type FaultCode uint32

const (
	FaultUnspecified FaultCode = 0
	FaultDeviceError FaultCode = 1
	FaultLinkLost    FaultCode = 2
)

// ProviderReport is a boundary fact from Provider -> nervud (mirroring safety.proto's HaltAccepted /
// StopProgress / StandstillConfirmed / ProviderFault). It has no pointers and can be passed by value;
// the Supervisor uses it to advance the StopProgress state machine.
//
// Epoch must match the current latch generation: reports with a stale epoch are always ignored.
type ProviderReport struct {
	Kind  ReportKind
	Epoch uint64
	Phase StopPhase // valid for ReportStopProgress
	Fault FaultCode // valid for ReportProviderFault
}

// StopProgressSource is the seam through which the Supervisor consumes Provider reports. It returns a bounded read-only channel;
// in practice ServiceHost/Dispatch feed the Provider's safety.proto messages into it (not wired this round).
type StopProgressSource interface {
	Reports() <-chan ProviderReport
}

// NopReports is the stub for v1 without a real Provider: it returns a nil channel,
// so the Supervisor's select branch never fires (i.e. there are no reports).
func NopReports() StopProgressSource { return nopReports{} }

type nopReports struct{}

func (nopReports) Reports() <-chan ProviderReport { return nil }

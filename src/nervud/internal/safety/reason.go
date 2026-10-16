package safety

// ReasonCode is the fixed numeric trigger reason code. It's used by the zero-allocation audit ring: the Safety trigger hot path
// only writes this uint32, and the normal-priority auditDrain turns it into audit text. The hot path never does string
// formatting or Protobuf encoding (audit is completed by a normal-priority thread reading the fixed event codes).
//
// Values must match the HaltReason enum in nervus-ipc's safety.proto (the proto is the single source of truth;
// the local consts are only a mirror); cross-language consistency is guarded by golden tests. 0 is always unspecified and fails closed.
type ReasonCode uint32

const (
	ReasonUnspecified          ReasonCode = 0 // never a legal trigger reason; uninitialised/unknown lands here
	ReasonOperatorEStop        ReasonCode = 1 // manual/UI software emergency stop
	ReasonDeadmanTimeout       ReasonCode = 2 // deadman / command freshness lapsed
	ReasonProviderFault        ReasonCode = 3 // triggered by a Provider-reported fault
	ReasonHeartbeatLost        ReasonCode = 4 // heartbeat with the Provider/MCU lost
	ReasonSupervisorEscalation ReasonCode = 5 // triggered by a Supervisor timeout escalation
	ReasonExternalTrip         ReasonCode = 6 // latch caused by an external report (such as the MCU / independent hardware)
)

func (r ReasonCode) String() string {
	switch r {
	case ReasonOperatorEStop:
		return "OPERATOR_ESTOP"
	case ReasonDeadmanTimeout:
		return "DEADMAN_TIMEOUT"
	case ReasonProviderFault:
		return "PROVIDER_FAULT"
	case ReasonHeartbeatLost:
		return "HEARTBEAT_LOST"
	case ReasonSupervisorEscalation:
		return "SUPERVISOR_ESCALATION"
	case ReasonExternalTrip:
		return "EXTERNAL_TRIP"
	default:
		return "UNSPECIFIED"
	}
}

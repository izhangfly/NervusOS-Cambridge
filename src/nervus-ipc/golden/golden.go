// Package golden defines the language-neutral Go↔JVM golden vectors for the
// Nervus IPC v1 wire (NRCP §22.6, end of §10.12).
//
// This file is the **single construction source** of the vectors: it builds each vector with Go generated code,
// writes it with **deterministic** serialisation as testdata/golden/<name>.binpb, and writes each vector's expected decoded
// fields into testdata/golden/vectors.json (a language-independent list).
//
// The shape of the gate (A5 §3):
//   - Go test (golden_test.go): for each vector, build → deterministic serialisation, asserting it equals the committed
//     .binpb byte for byte; then parse the committed .binpb and assert the fields equal the expected values.
//   - JVM test (GoldenVectorsTest.kt): load **the same** .binpb, decode with protobuf-java and assert the fields,
//     then toByteArray() and assert it equals the committed bytes.
//
// Because both sides assert "the bytes I built/serialised == the same committed .binpb", Go and the JVM are transitively
// guaranteed to agree byte for byte, which is exactly §22.6's hard requirement.
//
// Categories covered (A5 §3):
//   - Envelope Success / Failure outcomes + generic StatusCode mapping;
//   - typed error_detail: ResolveEndpointErrorDetail, ControlLeaseErrorDetail
//     (BaseMotion/Manipulator details to be added in the same shape once A2/A3 land);
//   - unknown reason compatibility: reason=99 in error_detail, with the outer generic code unchanged;
//   - malformed Provider details: error_detail is unparseable garbage bytes, normalised to INTERNAL outside;
//   - ControlLease wire: Acquire/Release and their Results (new in A5 §1);
//   - Transfer tickets and data plane attach results.
package golden

import (
	"google.golang.org/protobuf/proto"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// Kind identifies a vector's top-level message type, for cross-language loaders to dispatch by name.
type Kind string

const (
	KindEnvelope        Kind = "envelope"
	KindSchemaBundle    Kind = "interface_schema_bundle"
	KindSchemaBundleSet Kind = "interface_schema_bundle_set"
	KindTransferHandle  Kind = "transfer_handle"
	KindAttachResult    Kind = "attach_transfer_result"
)

// Vector is the definition of one golden vector.
type Vector struct {
	// Name is the stable vector name, also the .binpb file name (without the extension).
	Name string
	// Kind is the top-level message type.
	Kind Kind
	// Build constructs the vector's proto.Message.
	Build func() proto.Message
	// Doc is a human-readable one-line description, written into vectors.json.
	Doc string
}

// marshalDetail encodes a typed error_detail into bytes with deterministic serialisation.
// Both languages produce byte-for-byte identical encodings for these small messages without maps.
func marshalDetail(m proto.Message) []byte {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		panic(err) // vectors are fixed input, so an encoding failure is a program bug
	}
	return b
}

// resourceSelector is a small helper for building ResourceSelectors.
func resourceSelector(typ, role string) *ipcv1.ResourceSelector {
	return &ipcv1.ResourceSelector{Type: typ, Role: role}
}

// Vectors are all the golden vectors, in a stable order (which decides the order of vectors.json).
//
// Values deliberately use fixed, readable values that are unambiguous across languages (no randomness, no time),
// so the committed .binpb files stay stable for the long term.
func Vectors() []Vector {
	return []Vector{
		// --- Success / Failure outcomes + generic StatusCode ---
		{
			Name: "response_success_ok",
			Kind: KindEnvelope,
			Doc:  "Response success, code=OK, non-empty payload",
			Build: func() proto.Message {
				return &ipcv1.Envelope{Body: &ipcv1.Envelope_Response{Response: &ipcv1.Response{
					RequestId: 1,
					Outcome: &ipcv1.Response_Success{Success: &ipcv1.Success{
						Code:    ipcv1.StatusCode_STATUS_CODE_OK,
						Payload: []byte("pong"),
					}},
				}}}
			},
		},
		{
			Name: "response_failure_permission_denied",
			Kind: KindEnvelope,
			Doc:  "Response failure, code=PERMISSION_DENIED, with public_message",
			Build: func() proto.Message {
				return &ipcv1.Envelope{Body: &ipcv1.Envelope_Response{Response: &ipcv1.Response{
					RequestId: 2,
					Outcome: &ipcv1.Response_Failure{Failure: &ipcv1.Failure{
						Code:          ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED,
						PublicMessage: "denied",
					}},
				}}}
			},
		},
		{
			Name: "response_failure_unavailable",
			Kind: KindEnvelope,
			Doc:  "Response failure, code=UNAVAILABLE, no detail (disconnect/restart kind)",
			Build: func() proto.Message {
				return &ipcv1.Envelope{Body: &ipcv1.Envelope_Response{Response: &ipcv1.Response{
					RequestId: 3,
					Outcome: &ipcv1.Response_Failure{Failure: &ipcv1.Failure{
						Code: ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
					}},
				}}}
			},
		},

		// --- typed error_detail: ResolveEndpointErrorDetail ---
		{
			Name: "resolve_failure_interface_not_found",
			Kind: KindEnvelope,
			Doc:  "ResolveEndpointResult failure, NOT_FOUND + reason=INTERFACE_NOT_FOUND",
			Build: func() proto.Message {
				return &ipcv1.Envelope{Body: &ipcv1.Envelope_ResolveEndpointResult{ResolveEndpointResult: &ipcv1.ResolveEndpointResult{
					RequestId: 4,
					Outcome: &ipcv1.ResolveEndpointResult_Failure{Failure: &ipcv1.Failure{
						Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
						ErrorDetail: marshalDetail(&ipcv1.ResolveEndpointErrorDetail{
							Reason: ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_INTERFACE_NOT_FOUND,
						}),
					}},
				}}}
			},
		},
		{
			Name: "resolve_failure_version_mismatch",
			Kind: KindEnvelope,
			Doc:  "ResolveEndpointResult failure, FAILED_PRECONDITION + reason=VERSION_MISMATCH",
			Build: func() proto.Message {
				return &ipcv1.Envelope{Body: &ipcv1.Envelope_ResolveEndpointResult{ResolveEndpointResult: &ipcv1.ResolveEndpointResult{
					RequestId: 5,
					Outcome: &ipcv1.ResolveEndpointResult_Failure{Failure: &ipcv1.Failure{
						Code: ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
						ErrorDetail: marshalDetail(&ipcv1.ResolveEndpointErrorDetail{
							Reason: ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_VERSION_MISMATCH,
						}),
					}},
				}}}
			},
		},

		// --- Unknown reason compatibility: old SDKs meeting a new reason keep the generic code ---
		{
			Name: "resolve_failure_unknown_reason",
			Kind: KindEnvelope,
			Doc:  "reason=99 (unknown) in error_detail, outer NOT_FOUND unchanged; old SDKs must keep the generic code",
			Build: func() proto.Message {
				return &ipcv1.Envelope{Body: &ipcv1.Envelope_ResolveEndpointResult{ResolveEndpointResult: &ipcv1.ResolveEndpointResult{
					RequestId: 6,
					Outcome: &ipcv1.ResolveEndpointResult_Failure{Failure: &ipcv1.Failure{
						Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
						ErrorDetail: marshalDetail(&ipcv1.ResolveEndpointErrorDetail{
							Reason: ipcv1.ResolveEndpointReason(99),
						}),
					}},
				}}}
			},
		},

		// --- Malformed Provider details normalised to INTERNAL ---
		{
			Name: "failure_malformed_detail_internal",
			Kind: KindEnvelope,
			Doc:  "Response failure, INTERNAL, error_detail is unparseable garbage bytes (the result of nervud's normalisation)",
			Build: func() proto.Message {
				return &ipcv1.Envelope{Body: &ipcv1.Envelope_Response{Response: &ipcv1.Response{
					RequestId: 7,
					Outcome: &ipcv1.Response_Failure{Failure: &ipcv1.Failure{
						Code:        ipcv1.StatusCode_STATUS_CODE_INTERNAL,
						ErrorDetail: []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x0F}, // illegal tag/varint
					}},
				}}}
			},
		},

		// --- ControlLease wire (A5 §1) ---
		{
			Name: "acquire_control_human_base",
			Kind: KindEnvelope,
			Doc:  "AcquireControl: HUMAN requests base.main, wanting a 500ms lifetime",
			Build: func() proto.Message {
				return &ipcv1.Envelope{Body: &ipcv1.Envelope_AcquireControl{AcquireControl: &ipcv1.AcquireControl{
					RequestId:              8,
					ControllerClass:        ipcv1.ControllerClass_CONTROLLER_CLASS_HUMAN,
					Resource:               resourceSelector("nervus.resource.motion.base", "main"),
					RequestedDeadlineNanos: 500_000_000,
				}}}
			},
		},
		{
			Name: "acquire_control_ai_arm",
			Kind: KindEnvelope,
			Doc:  "AcquireControl: AI requests arm.main, wanting a 1s lifetime",
			Build: func() proto.Message {
				return &ipcv1.Envelope{Body: &ipcv1.Envelope_AcquireControl{AcquireControl: &ipcv1.AcquireControl{
					RequestId:              9,
					ControllerClass:        ipcv1.ControllerClass_CONTROLLER_CLASS_AI,
					Resource:               resourceSelector("nervus.resource.manipulator", "main"),
					RequestedDeadlineNanos: 1_000_000_000,
				}}}
			},
		},
		{
			Name: "acquire_control_result_success",
			Kind: KindEnvelope,
			Doc:  "AcquireControlResult success: lease_id/motion_epoch/deadline/resource_handle",
			Build: func() proto.Message {
				return &ipcv1.Envelope{Body: &ipcv1.Envelope_AcquireControlResult{AcquireControlResult: &ipcv1.AcquireControlResult{
					RequestId: 10,
					Outcome: &ipcv1.AcquireControlResult_Success{Success: &ipcv1.AcquireControlSuccess{
						LeaseId:        0xABCD,
						MotionEpoch:    3,
						DeadlineNanos:  1_234_567_890,
						ResourceHandle: "base.main",
					}},
				}}}
			},
		},
		{
			Name: "acquire_control_result_held_by_human",
			Kind: KindEnvelope,
			Doc:  "AcquireControlResult failure: FAILED_PRECONDITION + reason=HELD_BY_HUMAN (distinguishable cause)",
			Build: func() proto.Message {
				return &ipcv1.Envelope{Body: &ipcv1.Envelope_AcquireControlResult{AcquireControlResult: &ipcv1.AcquireControlResult{
					RequestId: 11,
					Outcome: &ipcv1.AcquireControlResult_Failure{Failure: &ipcv1.Failure{
						Code: ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
						ErrorDetail: marshalDetail(&ipcv1.ControlLeaseErrorDetail{
							Reason: ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_HELD_BY_HUMAN,
						}),
					}},
				}}}
			},
		},
		{
			Name: "acquire_control_result_unknown_reason",
			Kind: KindEnvelope,
			Doc:  "AcquireControlResult failure: control-lease reason=99 (unknown), outer FAILED_PRECONDITION unchanged",
			Build: func() proto.Message {
				return &ipcv1.Envelope{Body: &ipcv1.Envelope_AcquireControlResult{AcquireControlResult: &ipcv1.AcquireControlResult{
					RequestId: 12,
					Outcome: &ipcv1.AcquireControlResult_Failure{Failure: &ipcv1.Failure{
						Code: ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
						ErrorDetail: marshalDetail(&ipcv1.ControlLeaseErrorDetail{
							Reason: ipcv1.ControlLeaseErrorReason(99),
						}),
					}},
				}}}
			},
		},
		{
			Name: "release_control",
			Kind: KindEnvelope,
			Doc:  "ReleaseControl: release the lease this connection holds",
			Build: func() proto.Message {
				return &ipcv1.Envelope{Body: &ipcv1.Envelope_ReleaseControl{ReleaseControl: &ipcv1.ReleaseControl{
					RequestId: 13,
					LeaseId:   0xABCD,
				}}}
			},
		},
		{
			Name: "release_control_result_success",
			Kind: KindEnvelope,
			Doc:  "ReleaseControlResult success, code=OK",
			Build: func() proto.Message {
				return &ipcv1.Envelope{Body: &ipcv1.Envelope_ReleaseControlResult{ReleaseControlResult: &ipcv1.ReleaseControlResult{
					RequestId: 14,
					Outcome: &ipcv1.ReleaseControlResult_Success{Success: &ipcv1.Success{
						Code: ipcv1.StatusCode_STATUS_CODE_OK,
					}},
				}}}
			},
		},
		{
			Name: "dispatch_control_execution_context",
			Kind: KindEnvelope,
			Doc:  "Dispatch carrying the ControlLease execution context issued by the kernel",
			Build: func() proto.Message {
				return &ipcv1.Envelope{
					ProtocolMajor: 1,
					ProtocolMinor: 1,
					Body: &ipcv1.Envelope_Dispatch{Dispatch: &ipcv1.Dispatch{
						RouteId:     15,
						EndpointId:  23,
						MethodId:    7,
						RemainingMs: 250,
						Payload:     []byte("move"),
						ExecutionContext: &ipcv1.ExecutionContext{
							LeaseId:            0xABCD,
							ControllerClass:    ipcv1.ControllerClass_CONTROLLER_CLASS_HUMAN,
							MotionEpoch:        3,
							DeadlineNanos:      1_234_567_890,
							CommandSequence:    17,
							ResourceHandle:     "base.main",
							ResourceGeneration: 4,
						},
					}},
				}
			},
		},

		// --- Schema bundle distribution (A5 §2) ---
		{
			Name: "interface_schema_bundle",
			Kind: KindSchemaBundle,
			Doc:  "The InterfaceSchemaBundle of a single OEM private interface (interface_id/version/schema_hash/fds)",
			Build: func() proto.Message {
				return &ipcv1.InterfaceSchemaBundle{
					InterfaceId:       "com.acme.interface.gripper",
					Version:           1,
					SchemaHash:        []byte{0xDE, 0xAD, 0xBE, 0xEF},
					FileDescriptorSet: []byte{0x0A, 0x03, 'a', 'b', 'c'}, // placeholder fds bytes
					MethodEnumType:    "com.acme.gripper.v1.GripperMethod",
				}
			},
		},
		{
			Name: "interface_schema_bundle_set",
			Kind: KindSchemaBundleSet,
			Doc:  "The multi-interface bundle set one .nspkg carries",
			Build: func() proto.Message {
				return &ipcv1.InterfaceSchemaBundleSet{Bundles: []*ipcv1.InterfaceSchemaBundle{
					{
						InterfaceId:       "nervus.interface.motion.base",
						Version:           1,
						SchemaHash:        []byte{0x01, 0x02, 0x03, 0x04},
						FileDescriptorSet: []byte{0x0A, 0x01, 'x'},
						MethodEnumType:    "nervus.interface.motion.v1.BaseMotionMethod",
					},
					{
						InterfaceId:       "com.acme.interface.gripper",
						Version:           2,
						SchemaHash:        []byte{0xDE, 0xAD, 0xBE, 0xEF},
						FileDescriptorSet: []byte{0x0A, 0x01, 'y'},
						MethodEnumType:    "com.acme.gripper.v2.GripperMethod",
					},
				}}
			},
		},

		// --- The generic Transfer data plane ---
		{
			Name: "transfer_handle_peer",
			Kind: KindTransferHandle,
			Doc:  "A single-use PEER handle for a bidirectional Transfer",
			Build: func() proto.Message {
				return &ipcv1.TransferHandle{
					TransferId:              []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
					AttachTicket:            []byte("0123456789abcdef0123456789abcdef"),
					Role:                    ipcv1.TransferRole_TRANSFER_ROLE_PEER,
					Mode:                    ipcv1.TransferMode_TRANSFER_MODE_FRAMED_RELAY,
					ExpiresAtMonotonicNanos: 9_876_543_210,
					DataPlaneEndpoint:       "/run/nervus/nervud-transfer.sock",
				}
			},
		},
		{
			Name: "attach_transfer_result_success",
			Kind: KindAttachResult,
			Doc:  "Transfer attached successfully, returning the tightened packet size and rate",
			Build: func() proto.Message {
				return &ipcv1.AttachTransferResult{
					Outcome: &ipcv1.AttachTransferResult_Success{
						Success: &ipcv1.AttachTransferSuccess{
							Mode:              ipcv1.TransferMode_TRANSFER_MODE_FRAMED_RELAY,
							MaxPacketBytes:    1_048_576,
							MaxBytesPerSecond: 8_388_608,
						},
					},
				}
			},
		},
	}
}

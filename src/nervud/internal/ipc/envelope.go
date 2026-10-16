// This file is the hand-over between the framing layer and the protocol semantics layer: it decodes a frame's bytes into an Envelope and does
// the well-formedness checks Protobuf itself can't express.
//
// The connection state machine (Hello/HelloAck negotiation, body dispatch) is in conn.go. This only answers
// "is this frame a legal Envelope at all", not "is this kind of body allowed right now"
package ipc

import (
	"errors"
	"fmt"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"google.golang.org/protobuf/proto"
)

// These two are protocol violations too: the connection is closed on sight, with no Response produced.
//
// The same level as frame.go's length errors: there the boundary can't be trusted, here the boundary can be trusted but the content
// isn't an Envelope. Carrying on the conversation makes no sense in either case
var (
	ErrMalformedEnvelope = errors.New("ipc: malformed envelope")
	ErrEmptyEnvelopeBody = errors.New("ipc: envelope body is not set")
)

// parseEnvelope decodes a frame body into an Envelope.
//
// No business judgement before decoding, and after decoding only two state-independent checks:
//
//  1. It must be valid Protobuf. Malformed input disconnects rather than returning an error code: a peer that can build malformed frames
//     won't mend its ways because it received an INVALID_ARGUMENT, and replying only gives away bandwidth
//  2. A body must be set. An empty Envelope has no legitimate use, and tolerating it would leave a gap for flooding empty frames
//     to burn the budget; unknown bodies from an unknown newer version land here too (proto3 puts
//     oneof branches it doesn't know into unknown fields, leaving Body nil), and are treated as violations the same way
//
// Unknown fields and unknown bodies are handled differently on purpose: the former are kept and ignored by the proto runtime,
// so older receivers can ignore unknown fields; the latter means the peer is using semantics this side doesn't
// implement at all, and carrying on would only produce undefined behaviour
func parseEnvelope(b []byte) (*ipcv1.Envelope, error) {
	var env ipcv1.Envelope
	if err := proto.Unmarshal(b, &env); err != nil {
		// proto's raw error text isn't leaked to the peer; the full text stays only in this side's log/audit
		return nil, fmt.Errorf("%w: %v", ErrMalformedEnvelope, err)
	}
	if env.GetBody() == nil {
		return nil, ErrEmptyEnvelopeBody
	}
	return &env, nil
}

// isProtocolViolation decides whether an error means "this connection can't continue".
//
// Deciding it in one place avoids each call site writing its own errors.Is chain: missing one kind would treat a
// protocol violation as an ordinary disconnect, and it would never show up in the audit
func isProtocolViolation(err error) bool {
	return errors.Is(err, ErrZeroLength) ||
		errors.Is(err, ErrFrameTooLarge) ||
		errors.Is(err, ErrMalformedEnvelope) ||
		errors.Is(err, ErrEmptyEnvelopeBody)
}

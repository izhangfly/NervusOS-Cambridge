// Package ipc carries control-plane communication: AF_UNIX/SOCK_STREAM + a uint32 big-endian length prefix +
// a Protobuf Envelope + Nervus RPC/Event/Subscription semantics. The entry point is /run/nervus/nervud.sock.
package ipc

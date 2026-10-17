// Package control manages HUMAN/AI ControlLeases, the deadman and the motion epoch.
// A ControlLease is Resource-scoped from day one; [REWRITE-v1] the only valid Scope is {base.main}.
// SAFETY > HUMAN > AI > NONE only orders the effective sources of control; the four do not share one owner enum or Lease implementation.
package control

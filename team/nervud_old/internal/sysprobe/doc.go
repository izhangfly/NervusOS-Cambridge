// Package sysprobe gathers purely observational system calls: it asks the Linux kernel for facts without changing any system state.
//
// # Why it is not in authority
//
// The whole value of the Authority Gate is that privileged operations can be enumerated. Stuffing an operation like SO_PEERCRED,
// which any ordinary process can call on its own socket, into the Gate would drown the audit log
// in unprivileged noise and mix entries that need no decision into the Kind enum.
//
// Worse would be the opposite: an exported pass-through inside the authority package that skips do().
// authority.go already says that run is the package-private platform implementation and cannot be injected from outside;
// otherwise the Gate degenerates into "audit whatever arbitrary operation you wrote yourself". An unaudited syscall pass-through
// in the same package opens the same door from the other side, and has no structural
// constraint at all (no Kind, no Validate, no audit): what the next person adds to it is up to their discipline alone.
//
// So this package stands on its own.
//
// Admission rules
//
//  1. Pure observation: whether the call succeeds or fails, system state does not change; the caller only gets information.
//     This is the main test; the other three reinforce it.
//  2. Self-scoped: it acts only on objects the caller already holds (its own fd, its own thread),
//     never on global system state, and it does not accept any handle you care to give it.
//  3. Zero capabilities: an ordinary user process can call it just as well; needing a capability means it is
//     an exercise of power, which belongs in the Gate or in the module that owns that mechanism.
//  4. Typed, narrow signatures: no generic pass-throughs like Getsockopt(fd, level, opt int).
//     A generic pass-through exports the whole syscall table indirectly, and depguard becomes pointless.
//
// The package has no state, no constructor and no lifecycle: import it and call it. It is not assembled in main.go
// and not injected through a narrow interface either (there is no implementation to swap; injection would only add a layer for nothing).
package sysprobe

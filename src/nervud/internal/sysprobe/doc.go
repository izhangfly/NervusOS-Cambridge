//go:build linux

// Package sysprobe collects purely observational system calls: they ask the Linux kernel for facts but change no system state
//
// # The whole package compiles only on Linux
//
// There is no non-Linux fallback here. There used to be a monotonic_unsupported.go that returned
// ErrUnsupportedPlatform on non-Linux; it was deleted. nervud only runs on Linux, and
// a fake implementation that "compiles but returns an error from every call" has no users. All it does is
// turn the fact "this package depends on Linux" from a compile-time problem into a run-time one.
//
// Building on non-Linux now gives
// "build constraints exclude all Go files in internal/sysprobe", which is exactly the
// message we want. Develop and test on Linux or WSL.
//
// Why it isn't in authority
//
// The whole value of the Authority Gate is that privileged operations can be enumerated. Putting operations like SO_PEERCRED,
// which any ordinary process can call on its own socket, into the Gate floods the audit log
// with unprivileged noise and mixes entries that need no ruling into the Kind enum.
//
// Doing the opposite is more dangerous: opening an exported pass-through inside the authority package that skips do.
// authority.go already says run is the package's unexported platform implementation and accepts no outside injection;
// otherwise the Gate degrades into "audit whatever operation you wrote yourself". A same-package,
// unaudited syscall pass-through opens the same door from the other side, and it has no structural
// constraint (no Kind, no Validate, no audit), so what the next person adds to it is down to self-discipline.
//
// So this is a package of its own.
//
// Admission criteria
//
//  1. Purely observational: whether the call succeeds or fails, system state doesn't change; the caller only gets information.
//     This is the main criterion; the other three reinforce it.
//  2. Self-scoped: it acts only on objects the caller already holds (its own fd, its own thread),
//     not on global system state, and doesn't accept a handle to just any object.
//  3. Zero capabilities: an ordinary user process gets the same success; needing a capability means it's
//     an exercise of power and belongs in the Gate or in the module that owns the mechanism.
//  4. Narrow typed signatures: generic pass-throughs like Getsockopt(fd, level, opt int) are forbidden.
//     A generic pass-through indirectly exports the whole syscall table, and depguard would be pointless.
//
// The package has no state, no constructor and no lifecycle: import it and call it. It isn't wired up in main.go
// and isn't injected through a narrow interface (there's no implementation to swap, so injection would only add a layer for nothing).
package sysprobe

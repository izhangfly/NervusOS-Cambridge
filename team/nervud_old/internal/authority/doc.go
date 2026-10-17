// Package authority is the single entry point for every privileged Linux operation (the Authority Gate).
// Only this package may import os/exec, syscall and golang.org/x/sys/unix directly.
// It exposes only fixed, typed, narrow interfaces (such as StartSandboxedProcess and StopProcess).
// Arbitrary execution of the RunAsRoot/ExecuteShell kind is strictly forbidden.
package authority

// Package authority is the single entry point for all Linux privileged operations (the Authority Gate).
// Only this package may import os/exec, syscall and golang.org/x/sys/unix directly.
// It exposes only fixed, typed, narrow interfaces (such as StartSandboxedProcess, StopProcess);
// arbitrary execution capabilities like RunAsRoot/ExecuteShell are strictly forbidden.
package authority

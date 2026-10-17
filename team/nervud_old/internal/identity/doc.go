// Package identity provides trusted identity: it reads SO_PEERCRED (PID/UID/GID) from the socket
// and maps the UID to a Package ID in the Package Registry. Identity comes from the kernel; what a client says about itself is not trusted.
package identity

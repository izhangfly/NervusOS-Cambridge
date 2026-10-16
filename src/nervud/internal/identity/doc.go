// Package identity handles trusted identity: it reads SO_PEERCRED (PID/UID/GID) from the socket
// and maps the UID to a Package ID in the Package Registry. Identity comes from the kernel; what the client claims isn't trusted
package identity

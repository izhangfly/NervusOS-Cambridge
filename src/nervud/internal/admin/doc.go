// Package admin is nervud's privileged admin control plane: a root-only UDS that lets local
// ops tools (cmd/nervusctl) trigger install / uninstall / disable and enable / permission grant and revoke without being an App.
//
// Why it's separate from the App control plane (internal/ipc):
//
//   - The App control plane (/run/nervus/nervud.sock, 0666) only accepts connections whose UID is in the App range;
//     root is refused outright by Invariants.CheckUID, so ops tools (running as root) can't connect at all.
//   - Admin operations are privileged (an install turns staging into executable code on the system and changes permission state),
//     so they must be triggered through a confirmation channel the system holds, not by any ordinary App. Authority lives only in nervud;
//     dangerous actions can only be requested, and the confirmation entry point belongs to the system. Hence a separate
//     0600 channel whose SO_PEERCRED check requires the ops identity, so the two sets of admission rules don't mix.
//
// The single authoritative source of truth for the Package Registry is always
// the pkgregistry.Module inside the nervud process. nervusctl doesn't change the Registry on disk directly; it only
// delivers commands to nervud over this channel, and the same Module in nervud executes them. Signature checks, Arbitrate,
// OEM countersignatures, ABI and permission Intersect all happen in pkgregistry; this package makes no security decision
// and only does identity admission + argument/path-escape checks + forwarding + result projection.
//
// nervud controls staging (see CmdBeginStaging): nervud creates a directory under its own staging root
// and sends the path back to the CLI; the CLI unpacks into it and then triggers install. Install checks that the path really is
// a direct child of the staging root (path-escape check), then has pkgregistry re-check and commit atomically.
package admin

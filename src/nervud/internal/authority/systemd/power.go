// This file is whole-machine power actions: starting an orderly reboot/shutdown through systemd's D-Bus interface.
//
// # Why not reboot(2)
//
// ops.go's osReboot uses unix.Reboot(LINUX_REBOOT_CMD_RESTART): it takes effect immediately after sync,
// giving no upper layer a chance to flush. That's the fault recovery path: the kernel has already judged the system untrustworthy,
// and leaving the current state as fast as possible is best, at the cost of every running component being cut off hard.
//
// A user pressing "reboot" in Settings is different: the machine is healthy right now, running components should get
// SIGTERM and a chance to write to disk, and filesystems should be unmounted properly. This path has systemd go through the full
// shutdown.target, exactly the same as typing systemctl reboot in a terminal.
//
// Both paths stay, and neither can be deleted: one is "the machine is broken, start over quickly", the other "the user wants to shut down".
package systemd

import (
	"context"
	"fmt"
)

// Reboot asks systemd to reboot the whole machine in an orderly way.
//
// Normally this call doesn't return: systemd starts the shutdown as soon as it receives it, and the D-Bus connection
// drops with it. So callers shouldn't treat "returned an error" as "didn't reboot": the error from the dropped connection
// is precisely evidence that the reboot has started. See how authority.Gate.osPower handles it.
func (c *Conn) Reboot(ctx context.Context) error {
	if call := c.mgr.CallWithContext(ctx, mgrIface+".Reboot", 0); call.Err != nil {
		return fmt.Errorf("systemd: Reboot: %w", call.Err)
	}
	return nil
}

// PowerOff asks systemd for an orderly shutdown. Return semantics as for Reboot.
func (c *Conn) PowerOff(ctx context.Context) error {
	if call := c.mgr.CallWithContext(ctx, mgrIface+".PowerOff", 0); call.Err != nil {
		return fmt.Errorf("systemd: PowerOff: %w", call.Err)
	}
	return nil
}

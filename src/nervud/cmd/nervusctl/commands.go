// This file implements the subcommands: it chains argument checks, (for install) unpacking, admin channel calls and result
// display. Every security decision is on the nervud side; this only checks arguments and produces readable output.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/nervus-os/nervud/internal/adminwire"
)

// outf/outln are minimal wrappers around fmt.Fprintf/Fprintln: if writing to the user's terminal fails
//
//	(stdout/stderr gone), the CLI can't recover, so write errors are deliberately dropped. Dropping them here in one place,
//
// rather than scattering `_, _ =` across every call site, also keeps errcheck happy.
func outf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
func outln(w io.Writer, a ...any)               { _, _ = fmt.Fprintln(w, a...) }

// cmdInstall: begin-staging -> unpack the .nspkg into a directory nervud controls -> install.
//
// The optional second argument is a comma-separated list of consented permissions, equivalent to the user nodding to them
// on the install confirmation screen. This is an [ops back door, not an authorisation UI]: the real user-facing confirmation screen is
// the App Store (or nervus.permissionui). It exists so the kernel's half of the chain can be verified end to end
// without a UI, and so consent can still be given when installing on a machine with no graphical interface
func cmdInstall(c *adminwire.Client, args []string, out io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return badUsage("install requires <file.nspkg> [consented,permissions]")
	}
	var consented []string
	if len(args) == 2 && args[1] != "" {
		for _, p := range strings.Split(args[1], ",") {
			if p = strings.TrimSpace(p); p != "" {
				consented = append(consented, p)
			}
		}
	}
	nspkgPath := args[0]
	if _, err := os.Stat(nspkgPath); err != nil {
		return fmt.Errorf("open package: %w", err)
	}

	// 1) Have nervud create a staging directory it controls (same filesystem, controlled owner/permissions).
	beginResp, err := c.Do(adminwire.Request{Cmd: adminwire.CmdBeginStaging})
	if err != nil {
		return err
	}
	if !beginResp.OK || beginResp.StagingDir == "" {
		return respErr(beginResp)
	}
	staging := beginResp.StagingDir

	// 2) Unpack the .nspkg into that directory (zstd+tar, tar-slip guarded). On failure nervud's staging
	// sweep reclaims orphans on a best-effort basis, so no remote clean-up is needed here.
	if err := unpackNspkg(nspkgPath, staging); err != nil {
		return fmt.Errorf("unpack %s: %w", nspkgPath, err)
	}

	// 3) Trigger the install: nervud re-checks signature/digests/permissions and commits atomically.
	resp, err := c.Do(adminwire.Request{
		Cmd: adminwire.CmdInstall, StagingDir: staging, ConsentedPermissions: consented,
	})
	if err != nil {
		return err
	}
	if !resp.OK || resp.Package == nil {
		return respErr(resp)
	}

	p := resp.Package
	outf(out, "installed %s %s (trust=%s, source=%s)\n", p.ID, p.Version, p.Trust, p.Source)
	// Print the granted permissions so the operator can check what the install actually got
	if len(p.Granted) > 0 {
		outln(out, "  granted permissions:")
		for _, perm := range p.Granted {
			outf(out, "    - %s\n", perm)
		}
	} else {
		outln(out, "  granted permissions: (none)")
	}
	return nil
}

// cmdUninstall: uninstall a Package.
func cmdUninstall(c *adminwire.Client, args []string, out io.Writer) error {
	if len(args) != 1 {
		return badUsage("uninstall requires exactly one <package_id>")
	}
	resp, err := c.Do(adminwire.Request{Cmd: adminwire.CmdUninstall, PackageID: args[0]})
	if err != nil {
		return err
	}
	if !resp.OK {
		return respErr(resp)
	}
	outf(out, "uninstalled %s\n", args[0])
	return nil
}

// cmdList: list installed Packages (table output).
func cmdList(c *adminwire.Client, args []string, out io.Writer) error {
	if len(args) != 0 {
		return badUsage("list takes no arguments")
	}
	resp, err := c.Do(adminwire.Request{Cmd: adminwire.CmdList})
	if err != nil {
		return err
	}
	if !resp.OK {
		return respErr(resp)
	}
	if len(resp.Packages) == 0 {
		outln(out, "no packages installed")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	outln(tw, "PACKAGE\tVERSION\tTRUST\tSOURCE\tDISABLED")
	for _, p := range resp.Packages {
		disabled := "-"
		if len(p.Disabled) > 0 {
			disabled = fmt.Sprintf("%v", p.Disabled)
		}
		outf(tw, "%s\t%s\t%s\t%s\t%s\n", p.ID, p.Version, p.Trust, p.Source, disabled)
	}
	return tw.Flush()
}

// cmdSetEnabled: enable/disable a Component (enabled decides which).
func cmdSetEnabled(c *adminwire.Client, args []string, out io.Writer, enabled bool) error {
	verb := "disable"
	if enabled {
		verb = "enable"
	}
	if len(args) != 2 {
		return badUsage("%s requires <package_id> <component_id>", verb)
	}
	resp, err := c.Do(adminwire.Request{
		Cmd: adminwire.CmdSetEnabled, PackageID: args[0], ComponentID: args[1], Enabled: enabled,
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		return respErr(resp)
	}
	outf(out, "%sd %s/%s\n", verb, args[0], args[1])
	return nil
}

// cmdSetPermission: grant/revoke a runtime permission (state decides which).
func cmdSetPermission(c *adminwire.Client, args []string, out io.Writer, state string) error {
	verb, past := "revoke", "revoked"
	if state == adminwire.GrantStateGranted {
		verb, past = "grant", "granted"
	}
	if len(args) != 2 {
		return badUsage("%s requires <package_id> <permission>", verb)
	}
	resp, err := c.Do(adminwire.Request{
		Cmd: adminwire.CmdSetPermission, PackageID: args[0], Permission: args[1], GrantState: state,
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		return respErr(resp)
	}
	outf(out, "%s %s %s\n", past, args[0], args[1])
	return nil
}

// cmdLaunch: start an installed component, e.g. from an icon on the host desktop.
func cmdLaunch(c *adminwire.Client, args []string, out io.Writer) error {
	if len(args) != 2 {
		return badUsage("launch requires <package_id> <component_id>")
	}
	resp, err := c.Do(adminwire.Request{Cmd: adminwire.CmdLaunch, PackageID: args[0], ComponentID: args[1]})
	if err != nil {
		return err
	}
	if !resp.OK {
		return respErr(resp)
	}
	outf(out, "launched %s/%s\n", args[0], args[1])
	return nil
}

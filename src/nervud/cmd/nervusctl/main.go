// Command nervusctl is nervud's local privileged ops CLI: install/uninstall/list/disable-enable/grant-revoke.
//
// It isn't resident and doesn't hold the Registry's source of truth. The source of truth is always the pkgregistry inside the nervud process,
// or several writers would split the authoritative state. nervusctl only delivers commands to nervud over the privileged admin channel (internal/adminwire),
// and nervud executes and re-checks them; signature verification and decisions all happen in nervud, not in the CLI.
//
// The only heavy lifting the CLI does is unpacking a .nspkg into a staging directory (zstd+tar, tar-slip
// guarded), and the target is the directory nervud controls and sent back through begin-staging. nervud
// then re-checks the digest of every file in staging, so even a tampered unpacking step in the CLI
// gets refused on the nervud side; the CLI isn't a trust anchor.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/nervus-os/nervud/internal/adminwire"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// usage prints the command overview. In a variable so -h and argument errors can share it.
const usage = `nervusctl - nervud local privileged O&M tool

Usage:
  nervusctl [--sock PATH] <command> [arguments...]

Commands:
  install <file.nspkg> [perms]      Unpack and trigger installation. [perms] is a
                                    comma-separated list of USER_CONSENT permissions
                                    the operator consents to on the user's behalf
  uninstall <package_id>            Uninstall a Package
  list                              List installed Packages
  launch <package_id> <component>   Start a Component (as the launcher would)
  enable <package_id> <component>   Enable a Component
  disable <package_id> <component>  Disable a Component
  grant <package_id> <permission>   Grant a runtime (dangerous) permission
  revoke <package_id> <permission>  Revoke a runtime permission
  audit-verify [dir]                Verify the audit hash chain

Options:
  --sock PATH   Admin channel socket path
`

// run is the testable entry point: parse arguments, construct the client, run the subcommand. Returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nervusctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("sock", adminwire.DefaultSockPath, "admin channel socket path")
	fs.Usage = func() { outf(stderr, "%s", usage) }

	if err := fs.Parse(args); err != nil {
		return 2 // flag already printed the error
	}
	rest := fs.Args()
	if len(rest) == 0 {
		outf(stderr, "%s", usage)
		return 2
	}

	client := adminwire.NewClient(*sock)
	cmd, cmdArgs := rest[0], rest[1:]

	var err error
	switch cmd {
	case "install":
		err = cmdInstall(client, cmdArgs, stdout)
	case "uninstall":
		err = cmdUninstall(client, cmdArgs, stdout)
	case "list":
		err = cmdList(client, cmdArgs, stdout)
	case "launch":
		err = cmdLaunch(client, cmdArgs, stdout)
	case "enable":
		err = cmdSetEnabled(client, cmdArgs, stdout, true)
	case "disable":
		err = cmdSetEnabled(client, cmdArgs, stdout, false)
	case "grant":
		err = cmdSetPermission(client, cmdArgs, stdout, adminwire.GrantStateGranted)
	case "revoke":
		err = cmdSetPermission(client, cmdArgs, stdout, adminwire.GrantStateDenied)
	case "audit-verify":
		// Doesn't connect to the admin channel: it reads local files, and the audit must still be checkable precisely when nervud
		// won't start or is behaving suspiciously.
		err = cmdAuditVerify(cmdArgs, stdout)
	case "help", "-h", "--help":
		outf(stdout, "%s", usage)
		return 0
	default:
		outf(stderr, "nervusctl: unknown command %q\n\n%s", cmd, usage)
		return 2
	}

	if err != nil {
		var ue usageErr
		if errors.As(err, &ue) {
			outf(stderr, "nervusctl: %s\n\n%s", ue.msg, usage)
			return 2
		}
		outf(stderr, "nervusctl: %v\n", err)
		return 1
	}
	return 0
}

// usageErr marks an argument usage error, distinct from a run-time failure: the former exits 2 and prints usage,
// the latter exits 1.
type usageErr struct{ msg string }

func (e usageErr) Error() string { return e.msg }

func badUsage(format string, args ...any) error {
	return usageErr{msg: fmt.Sprintf(format, args...)}
}

// respErr turns a failed Response into an error (for the subcommands to handle uniformly).
func respErr(resp adminwire.Response) error {
	if resp.Message != "" {
		return fmt.Errorf("%s (%s)", resp.Message, resp.Code)
	}
	return fmt.Errorf("command failed (%s)", resp.Code)
}

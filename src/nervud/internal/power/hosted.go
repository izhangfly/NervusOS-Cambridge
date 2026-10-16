package power

// Hosted mode: NervusOS runs as a "session" on an existing desktop Linux
// (for example Ubuntu 24.04 + GNOME) instead of owning the whole machine.
//
// In that deployment, when the user presses "power off" on the Nervus desktop or in Settings they mean "leave NervusOS
// and go back to Ubuntu", and "restart" means "restart NervusOS". Actually shutting the laptop down through systemd
// would be the wrong action, and would also close any unsaved work the user has on the host desktop.
//
// So hosted mode routes the two power methods to an external hook program:
//
//	<hook> poweroff   end the NervusOS session (stop the kernel and every component, close the Nervus display)
//	<hook> reboot     restart the NervusOS kernel; the display stays open
//
// The deployment supplies the hook (see NervusOS-Cambridge/hosted/); the kernel only authorises and audits, then
// hands the action over. The interface ID and method IDs don't change: callers (Settings / the desktop status bar) don't
// need to know whether they're in whole-machine or hosted mode.
//
// The switch is the environment variable NERVUS_HOSTED_POWER_HOOK (set by a nervud.service drop-in).
// Unset means whole-machine mode, behaving exactly as before.

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/authority"
)

// HostedHookEnv is the name of the environment variable that turns on hosted mode; its value is the hook's absolute path
const HostedHookEnv = "NERVUS_HOSTED_POWER_HOOK"

// HostedGate hands power actions to an external hook and satisfies the Gate interface
type HostedGate struct {
	hook string
	aud  audit.Recorder
	log  *slog.Logger
	// start can be replaced in tests; by default it starts the hook with exec without waiting for it to finish
	start func(name string, args ...string) error
}

// NewHostedGate constructs the hosted-mode power Gate. aud/log may be nil
func NewHostedGate(hook string, aud audit.Recorder, log *slog.Logger) *HostedGate {
	return &HostedGate{hook: hook, aud: aud, log: log, start: startDetached}
}

// Power implements Gate. It only knows two actions and refuses everything else (the same rule as whole-machine mode's Validate)
func (g *HostedGate) Power(ctx context.Context, subj authority.Subject, req authority.PowerRequest) error {
	var verb string
	switch req.Action {
	case authority.PowerActionPowerOff:
		verb = "poweroff"
	case authority.PowerActionReboot:
		verb = "reboot"
	default:
		return fmt.Errorf("hosted power: unknown action %d", req.Action)
	}

	// Once the hook starts, systemd may stop the kernel at any moment, so record the audit first
	if g.aud != nil {
		g.aud.Record(ctx, audit.Event{
			Action:  "PowerAction.hosted",
			Subject: subj.String(),
			Detail:  fmt.Sprintf("%s via %s (%s)", verb, g.hook, req.Reason),
		})
	}
	if g.log != nil {
		g.log.Info("power: hosted mode, handing off to hook", "action", verb, "hook", g.hook, "caller", subj.String())
	}
	if err := g.start(g.hook, verb); err != nil {
		return fmt.Errorf("hosted power hook %s %s: %w", g.hook, verb, err)
	}
	return nil
}

// startDetached starts the hook and reaps it in the background, without blocking the IPC call.
// The hook typically runs `systemctl stop --no-block nervud`, after which this process gets SIGTERM;
// the caller should still get its OK first, so this must never wait for the hook to finish
func startDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

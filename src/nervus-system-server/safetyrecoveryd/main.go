// Command safetyrecoveryd is Nervus OS's Safety recovery policy service.
//
// # Mechanism in the kernel, policy here
//
// The kernel's safety.Rearm() only re-checks one [unbypassable hard precondition]: stop progress must have settled,
// either the normal chain reached OUTPUT_DISABLED / STANDSTILL_CONFIRMED (output confirmed off),
// or it entered a terminal side branch such as DELIVERY_FAULT / STANDSTILL_TIMEOUT that needs a human.
// Releasing the latch while stopping is still under way would let motion resume without knowing whether output is off.
//
// "Should we re-arm now" is policy in itself: it depends on device state, the cause of the fault, whether a user is present,
// whether the condition that triggered the stop has been cleared. That will change as the product evolves and doesn't belong in the kernel.
//
// This service is that policy layer. It calls nervus.interface.safety.control through standard IPC;
// that's a [built-in endpoint] implemented by nervud itself, but called exactly like an interface any Provider
// offers.
//
// # What it does now
//
// Observes. It reads the safety state periodically and logs state transitions, especially the moment it enters REARM_REQUIRED and
// the stop phase at that time: first-hand material for reviewing afterwards "why did it stop, and how far did stopping get".
//
// # What it does [not] do yet: automatic re-arm
//
// There's no logic to clear a stop automatically, deliberately. Automatic re-arm first needs to answer
// "has the condition that triggered the stop gone away", which depends on Providers reporting device state, and
// safety.proto's five messages (SafetyHalt / HaltAccepted / StopProgress /
// StandstillConfirmed / ProviderFault) currently [have no channel to carry them]: the kernel's delivery side
// is NopPath and its report side NopReports.
//
// Writing a "wait N seconds, then re-arm" policy before the real device state is available would mean letting a
// timer decide when the machine may move again. That's more dangerous than doing nothing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	safetyv1 "github.com/nervus-os/nervus-ipc/protocol/interface/safetyv1"
	"github.com/nervus-os/nervus-ipc/sdk"
)

// componentID must match components[].id in manifest.json.
// nervud checks it via the peer's PID → cgroup → systemd unit and refuses the handshake on a mismatch.
const componentID = "main"

// interfaceID is the kernel's built-in Safety interface.
//
// Note this service [consumes] it rather than providing it: it doesn't RegisterEndpoint, it calls ResolveEndpoint.
// The opposite of pkgmanagerd, which is a Provider; this one is a Client.
const interfaceID = "nervus.interface.safety.control"

// pollInterval is the safety state polling period.
//
// [Polling rather than subscribing, for a different reason now]: the subscription channel (envelope 40-45) works now, but
// the built-in interface nervus.interface.safety.control [declares no EventMeta], and
// subscribing to an event_id that isn't in the contract is refused outright by RouteEvent.
//
// Switching to a subscription first needs events declared for the safety built-in interface on the kernel side; that comes after
// safety.proto's five messages get a channel to carry them (see the top of the file).
//
// 2 seconds is a trade-off: safety transitions are rare (they don't change at all in normal running), so polling faster only burns
// CPU and audit entries; but after a stop, ops people watch the UI for the update, and too slow looks unresponsive.
const pollInterval = 2 * time.Second

func main() {
	sockPath := flag.String("sock", sdk.DefaultSockPath, "nervud control-plane UDS")
	logLevel := flag.String("log-level", "info", "log level debug/info/warn/error")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))
	slog.SetDefault(log)

	if err := run(*sockPath, log); err != nil {
		log.Error("safetyrecoveryd exited", "err", err)
		os.Exit(1)
	}
	log.Info("safetyrecoveryd stopped")
}

func parseLevel(s string) slog.Level {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return lv
}

func run(sockPath string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// If it can't connect, fail and exit, and let nervud's supervisor restart with exponential backoff.
	// A retry loop here would hide the fact that "nervud isn't up" and make this process look healthy.
	c, err := sdk.Connect(sdk.Config{
		SockPath:    sockPath,
		ComponentID: componentID,
		SDKName:     "nervus-system-server",
		SDKVersion:  "0.1.0",
		Log:         log,
	})
	if err != nil {
		return fmt.Errorf("connect to the control plane: %w", err)
	}
	defer func() { _ = c.Close() }()

	id := c.Identity()
	log.Info("safetyrecoveryd: handshake ok",
		"package_id", id.PackageID, "component_id", id.ComponentID)

	ep, err := resolveSafety(ctx, c)
	if err != nil {
		return err
	}
	log.Info("safetyrecoveryd: resolved builtin safety endpoint",
		"endpoint_id", ep.EndpointID, "interface", interfaceID)

	return watch(ctx, c, ep, log)
}

// resolveSafety resolves the kernel's built-in Safety endpoint.
func resolveSafety(ctx context.Context, c *sdk.Client) (sdk.Endpoint, error) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	ep, err := c.ResolveEndpoint(rctx, sdk.ResolveRequest{
		InterfaceID: interfaceID,
		MinMajor:    1,
		MaxMajor:    1,
		// No selector: Safety is whole-machine state and isn't bound to any single Resource.
		//
		// From v2 that means exactly that. In v1 an empty selector was implicitly taken as
		// {motion.base, base.main}, so this Resolve would mysteriously resolve a base handle
		// along with it; that implicit default was removed with major 2.
	})
	if err != nil {
		return sdk.Endpoint{}, fmt.Errorf("resolve %s: %w", interfaceID, err)
	}
	return ep, nil
}

// watch reads the safety state periodically and logs transitions.
func watch(ctx context.Context, c *sdk.Client, ep sdk.Endpoint, log *slog.Logger) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var last *safetyv1.SafetyState

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		st, err := getState(ctx, c, ep)
		if err != nil {
			if errors.Is(err, sdk.ErrClosed) {
				// No connection = nervud stopped or this connection was judged a slow consumer. Exit and let the
				// supervisor restart us; don't pretend to be alive and idle here.
				return fmt.Errorf("control-plane connection lost: %w", err)
			}
			if sdk.NeedsReResolve(err) {
				// The binding is stale (kernel restarted / permission revoked). Resolve once more.
				log.Warn("safetyrecoveryd: endpoint invalidated, re-resolving", "err", err)
				newEP, rerr := resolveSafety(ctx, c)
				if rerr != nil {
					return rerr
				}
				ep = newEP
				continue
			}
			log.Warn("safetyrecoveryd: GetState failed", "err", err)
			continue
		}

		if changed(last, st) {
			logTransition(log, last, st)
			last = st
		}
	}
}

func getState(ctx context.Context, c *sdk.Client, ep sdk.Endpoint) (*safetyv1.SafetyState, error) {
	req, err := proto.Marshal(&safetyv1.GetSafetyStateRequest{})
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out, err := c.Call(cctx, ep.EndpointID,
		uint32(safetyv1.SafetyControlMethod_SAFETY_CONTROL_METHOD_GET_STATE),
		req, 2*time.Second)
	if err != nil {
		return nil, err
	}
	var st safetyv1.SafetyState
	if err := proto.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("decode SafetyState: %w", err)
	}
	return &st, nil
}

// changed reports whether two snapshots differ in substance.
//
// All three fields must be compared: the epoch changing on its own (pre-emption, a Provider restart) doesn't change the top-level state,
// but it means every motion command in flight has been voided, an event that must be recorded.
func changed(a, b *safetyv1.SafetyState) bool {
	if a == nil {
		return true
	}
	return a.GetState() != b.GetState() ||
		a.GetMotionEpoch() != b.GetMotionEpoch() ||
		a.GetStopPhase() != b.GetStopPhase()
}

// logTransition records one state transition.
//
// The moment it enters REARM_REQUIRED and the stop phase at that time are first-hand material for reviewing "why did it stop, and how far
// did stopping get", so WARN level makes them stand out in the log.
func logTransition(log *slog.Logger, from, to *safetyv1.SafetyState) {
	attrs := []any{
		"state", to.GetState().String(),
		"motion_epoch", to.GetMotionEpoch(),
		"stop_phase", to.GetStopPhase().String(),
	}
	if from != nil {
		attrs = append(attrs, "from_state", from.GetState().String())
	}

	switch to.GetState() {
	case safetyv1.TopState_TOP_STATE_SAFETY_LATCHED,
		safetyv1.TopState_TOP_STATE_REARM_REQUIRED:
		log.Warn("safetyrecoveryd: safety state changed", attrs...)
	default:
		log.Info("safetyrecoveryd: safety state changed", attrs...)
	}
}

//go:build linux

// Command camerad is Nervus OS's general camera service.
//
// # It turns "a pile of /dev/videoN" into "which cameras this board has"
//
// V4L2 only answers "what formats can this node produce". It can't answer "which one faces forward"; that's a board fact
// written in the board JSON in the service directory, which this service translates into semantic resources in the Catalog.
//
// # One camera = two endpoints
//
//	nervus.interface.camera@1         capture, bound to nervus.resource.camera (shared observation)
//	nervus.interface.camera.config@1  config, bound to nervus.resource.camera.config (exclusive lease)
//
// See the top of config.go for why they're split: the two have opposite resource semantics, and kernel leases are per resource.
//
// # How vendors plug in
//
// Vendors [don't implement the standard interface]; they implement a private interface in their own namespace
// (something like vendor.acme.camerad.interface.source@1), which this service turns into the standard one.
// That way the standard semantics (facing=front) come from the platform's board config and vendors can't award themselves any; the data plane
// still goes direct, with frames never copied through this process.
//
// [That path isn't wired up yet]: only V4L2 devices are supported today. Hooking up a vendor source needs this service to
// Resolve the private interface and hand its TransferHandle to the App, which first needs a
// real vendor Provider to define the shape of the private contract.
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

	"github.com/nervus-os/nervus-ipc/sdk"

	"github.com/nervus-os/nervus-system-server/camerad/boardprofile"
)

// defaultProfilePath is where the board config lives once installed, covered by the manifest's digests.
const defaultProfilePath = "/usr/lib/nervus/system-packages/nervus.camerad/board.json"

func main() {
	profilePath := flag.String("profile", defaultProfilePath, "board camera configuration JSON")
	sockPath := flag.String("sock", sdk.DefaultSockPath, "nervud control-plane UDS")
	dryRun := flag.Bool("dry-run", false, "print the role → device mapping and exit, without connecting to the control plane")
	logLevel := flag.String("log-level", "info", "log level debug/info/warn/error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if err := run(*profilePath, *sockPath, *dryRun, log); err != nil {
		log.Error("camerad exited", "err", err)
		os.Exit(1)
	}
	log.Info("camerad stopped")
}

func run(profilePath, sockPath string, dryRun bool, log *slog.Logger) error {
	profile, err := boardprofile.Load(profilePath)
	if err != nil {
		return err
	}
	log.Info("camerad: board config loaded",
		"board", profile.Board,
		"fixed_cameras", len(profile.Cameras),
		"declared_roles", len(profile.AllRoles()))

	bindings, err := Bind(profile, listSysfsDevices)
	if err != nil {
		return err
	}
	reportBindings(profile, bindings, log)

	if dryRun {
		for _, b := range bindings {
			fmt.Printf("%-20s %-16s %s\n", b.Role, b.Device.Node, b.Device.Name)
		}
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// If it can't connect, fail and exit, and let the supervisor restart with exponential backoff. A retry loop here would
	// hide the fact that "nervud isn't up" and make this process look healthy.
	host, err := sdk.NewServiceHost(sdk.Config{
		SockPath:    sockPath,
		ComponentID: componentID,
		SDKName:     "nervus-system-server",
		SDKVersion:  "0.1.0",
		Log:         log,
	})
	if err != nil {
		return fmt.Errorf("camerad: connect to the control plane: %w", err)
	}
	defer func() { _ = host.Close() }()

	identity := host.Identity()
	log.Info("camerad: handshake ok",
		"package_id", identity.PackageID, "component_id", identity.ComponentID)

	captureHash, configHash, err := schemaHashes()
	if err != nil {
		return err
	}

	endpoints, err := installEndpoints(ctx, host, bindings, captureHash, configHash, log)
	// [Close the streams whether or not setup succeeded]: after a partial setup some devices are already open,
	// and returning directly would leak them, fds and all, until the process exits.
	defer closeAll(endpoints)
	if err != nil {
		return err
	}
	if len(endpoints) == 0 {
		// A machine with no camera plugged in. [Keep running instead of exiting]: exiting would make the supervisor
		// restart a service with nothing to do again and again, while a camera plugged in would need a full reboot to take effect.
		log.Warn("camerad: no usable camera; idling")
	}

	return serve(ctx, host, log)
}

// serve runs the read loop until a stop signal arrives or the connection drops.
func serve(ctx context.Context, host *sdk.ServiceHost, log *slog.Logger) error {
	served := make(chan error, 1)
	go func() { served <- host.Serve() }()

	select {
	case <-ctx.Done():
		log.Info("camerad: stop signal received")
		return nil
	case err := <-served:
		if err == nil || errors.Is(err, sdk.ErrClosed) {
			return nil
		}
		// A dropped connection = nervud stopped or this connection was judged a slow consumer. Exit and let the supervisor
		// restart us; don't pretend to be alive and idle here.
		return fmt.Errorf("camerad: control-plane connection lost: %w", err)
	}
}

// endpointPair is the capture and config sides of one camera.
type endpointPair struct {
	capture *cameraEndpoint
	config  *configEndpoint
}

// installEndpoints sets up two endpoints for each binding.
//
// # One camera failing doesn't take the machine down
//
// When one camera can't be set up (the device won't open, the driver reports no discrete format), log a WARN and carry on instead of
// exiting the whole service. Losing the other three of four cameras because one is broken would turn a local fault into a whole-machine
// fault, and cameras working loose is common on robots.
//
// [But a failed registration is a hard error]: it means the Catalog and this binary disagree on the contract (schema hash
// mismatch, resource not declared), every later camera would fail the same way, and carrying on would just repeat the same error
// N times.
func installEndpoints(
	ctx context.Context,
	host *sdk.ServiceHost,
	bindings []Binding,
	captureHash, configHash []byte,
	log *slog.Logger,
) ([]endpointPair, error) {
	var installed []endpointPair

	for _, b := range bindings {
		capture, err := newCameraEndpoint(host, b.Role, b.Device, log)
		if err != nil {
			log.Warn("camerad: setting up this camera failed; skipping",
				"role", b.Role, "node", b.Device.Node, "err", err)
			continue
		}
		if err := capture.install(ctx, captureHash); err != nil {
			return installed, err
		}

		config := newConfigEndpoint(host, b.Role, b.Device, log)
		if err := config.install(ctx, configHash); err != nil {
			return installed, err
		}

		installed = append(installed, endpointPair{capture: capture, config: config})
	}
	return installed, nil
}

func closeAll(pairs []endpointPair) {
	for _, pair := range pairs {
		pair.capture.closeAll()
	}
}

// reportBindings writes the mapping to the log.
//
// Roles that are declared but not plugged in get a separate WARN: the most common problem during board integration is
// "the config says front camera but the cable isn't plugged in", and the log then just has [one line missing],
// which nobody notices.
func reportBindings(profile *boardprofile.Profile, bindings []Binding, log *slog.Logger) {
	bound := make(map[string]bool, len(bindings))
	for _, b := range bindings {
		bound[b.Role] = true
		log.Info("camerad: role bound",
			"role", b.Role, "node", b.Device.Node,
			"usb_path", b.Device.USBPath, "name", b.Device.Name,
			"labels", profile.LabelsFor(b.Role))
	}
	for _, role := range profile.AllRoles() {
		if !bound[role] {
			log.Warn("camerad: declared role has no matching device", "role", role)
		}
	}
}

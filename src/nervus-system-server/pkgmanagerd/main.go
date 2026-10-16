// Command pkgmanagerd is Nervus OS's software install system service.
//
// It runs in a systemd transient unit (started by nervud through authority), with the identity of an ordinary
// Package (UID in the 20000-59999 range) and the same sandbox as every other package. Its only privilege is that the kernel
// explicitly lets it connect to the admin channel; see nervud's admin.Config.ServiceUID.
//
// Lifecycle:
//
//	nervud boot scan → finds nervus.pkgmanagerd (always-on) → starts it through systemd
//	→ this process handshakes on the control plane → RegisterEndpoint → Serve loop
//	→ when nervud stops, BindsTo=nervud.service stops this unit with it
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

	"github.com/nervus-os/nervus-ipc/sdk"
)

// componentID must match components[].id in manifest.json.
//
// It's the declared_component_id in the handshake: nervud checks it via the peer's PID → cgroup →
// systemd unit → start record, and refuses the handshake on a mismatch. Getting it wrong can't let you impersonate anyone;
// it just means you can't connect, and the error is UNAUTHENTICATED, which looks like a permission problem.
const componentID = "main"

// interfaceID is the interface this service provides.
//
// It must match components[].exports[].interface in manifest.json: the kernel's
// RegisterEndpoint step 2 looks it up in the manifest and returns PERMISSION_DENIED if it isn't there
// ("interface not declared in manifest exports").
//
// The interface is defined in nervus-ipc's proto/nervus/interface/pkgmanager/v1/pkg_manager.proto.
const interfaceID = "nervus.interface.pkg.manager"

func main() {
	sockPath := flag.String("sock", sdk.DefaultSockPath, "nervud control-plane UDS")
	adminSock := flag.String("admin-sock", DefaultSockPath, "nervud admin channel UDS")
	logLevel := flag.String("log-level", "info", "log level debug/info/warn/error")
	flag.Parse()

	log := newLogger(*logLevel)
	slog.SetDefault(log)

	if err := run(*sockPath, *adminSock, log); err != nil {
		log.Error("pkgmanagerd exited", "err", err)
		os.Exit(1)
	}
	log.Info("pkgmanagerd stopped")
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	// Write to stderr: systemd collects it into the journal, so no log file rotation to manage.
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

func run(sockPath, adminSock string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	admin := newAdminClient(adminSock)
	svc := newService(admin, log)

	// Connect to the control plane first. If that fails, exit with an error: systemd restarts with backoff,
	// and nervud's service supervision handles repeated crashes according to criticality.
	// A retry loop here would be wrong: it would hide the fact that "nervud isn't up"
	// and make this process look healthy.
	host, err := sdk.NewServiceHost(sdk.Config{
		SockPath:    sockPath,
		ComponentID: componentID,
		SDKName:     "nervus-system-server",
		SDKVersion:  "0.1.0",
		Log:         log,
	})
	if err != nil {
		return fmt.Errorf("connect to the control plane: %w", err)
	}
	defer func() { _ = host.Close() }()

	id := host.Identity()
	log.Info("pkgmanagerd: handshake ok", "package_id", id.PackageID, "component_id", id.ComponentID)

	// [Every handler must be registered before registering the endpoint]. The moment registration succeeds nervud may
	// forward a Dispatch, and a method not registered yet gets NOT_FOUND; the caller would think
	// the method doesn't exist rather than "the service isn't ready yet", an error that's very hard to trace back.
	registerHandlers(host, svc, log)

	// Compute it before registering, and fail outright if that fails: registering with an empty hash only earns
	// FAILED_PRECONDITION, and that error points at the Catalog, not here.
	hash, err := schemaHash()
	if err != nil {
		return err
	}

	regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	epID, err := host.RegisterEndpoint(regCtx, sdk.RegisterRequest{
		InterfaceID: interfaceID,
		Major:       interfaceMajor,
		// SchemaHash is required and compared byte for byte. The kernel's step 5 compares it with the Catalog's
		// and returns FAILED_PRECONDITION on a mismatch; the compatibility bridge that once let nervus.pkgmanagerd register an empty hash
		// was removed with v2, and every Provider is treated alike.
		SchemaHash: hash,
		// An empty ResourceHandle = unspecified. The kernel's RegisterEndpoint step 4 only checks, when it's non-empty,
		// that it's a known handle in the Resource Registry; this interface isn't bound to any physical resource,
		// and filling one in would get it refused with INVALID_ARGUMENT for not being in the table.
	})
	if err != nil {
		return fmt.Errorf("register endpoint: %w", err)
	}
	log.Info("pkgmanagerd: endpoint registered", "endpoint_id", epID, "interface", interfaceID)

	// Serve blocks until the connection drops; cancelling ctx closes it so it returns.
	done := make(chan error, 1)
	go func() { done <- host.Serve() }()

	select {
	case <-ctx.Done():
		log.Info("pkgmanagerd: shutting down")
		// Withdraw the endpoint gracefully, so nervud can treat it as an expected event
		// (sending EndpointDied{SERVICE_SHUTTING_DOWN} to callers holding a binding)
		// rather than lumping it in with "the process crashed".
		unregCtx, unregCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer unregCancel()
		if err := host.UnregisterEndpoint(unregCtx, epID, true); err != nil {
			log.Debug("pkgmanagerd: unregister failed", "err", err)
		}
		_ = host.Close()
		<-done
		return nil
	case err := <-done:
		if err != nil && !errors.Is(err, sdk.ErrClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	}
}

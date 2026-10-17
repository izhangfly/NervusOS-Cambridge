// Command piperd is the NervusOS Provider of nervus.interface.manipulator.arm@1 for the AgileX
// PiPER arm.
//
// It is the middle of the path an App's arm command takes:
//
//	App (App SDK)  --Request-->  nervud  --Dispatch-->  piperd  --HTTP-->  PiPER driver  --CAN-->  arm
//	                 lease, permission,                   staleness check,   piper_sdk on can0
//	                 deadman, audit                       units, workspace
//
// nervud decides who may move the arm (perm.manipulator.control + a ControlLease on arm.main) and
// freezes that decision into the ExecutionContext; piperd re-checks it against the latest motion
// epoch right before touching the device and translates the standard interface into the driver's
// commands. The driver (nervus-piper.service) owns the CAN bus.
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

	manipulatorv1 "github.com/nervus-os/nervus-ipc/protocol/interface/manipulatorv1"
	"github.com/nervus-os/nervus-ipc/sdk"
)

// Must match components[].id in manifest.json.
const componentID = "main"

const (
	interfaceID    = "nervus.interface.manipulator.arm"
	interfaceMajor = 1
	// resourceHandle is the arm resource the kernel's catalog declares (stable role "arm.main").
	// piperd binds to it rather than declaring its own: there is one arm, and one catalog entry for it.
	resourceHandle = "arm.main"
)

func main() {
	sockPath := flag.String("sock", sdk.DefaultSockPath, "nervud control-plane UDS")
	driverURL := flag.String("driver", "http://127.0.0.1:8770", "PiPER driver API (loopback)")
	logLevel := flag.String("log-level", "info", "log level debug/info/warn/error")
	flag.Parse()

	log := newLogger(*logLevel)
	slog.SetDefault(log)
	if err := run(*sockPath, *driverURL, log); err != nil {
		log.Error("piperd exited", "err", err)
		os.Exit(1)
	}
	log.Info("piperd stopped")
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

func run(sockPath, driverURL string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	svc := newService(newDriver(driverURL), log)

	host, err := sdk.NewServiceHost(sdk.Config{
		SockPath:    sockPath,
		ComponentID: componentID,
		SDKName:     "nervus-system-server",
		SDKVersion:  "0.1.0",
		Log:         log,
	})
	if err != nil {
		// Exit and let systemd restart with backoff; a retry loop here would hide "nervud isn't up".
		return fmt.Errorf("connect to the control plane: %w", err)
	}
	defer func() { _ = host.Close() }()
	id := host.Identity()
	log.Info("piperd: handshake ok", "package_id", id.PackageID, "component_id", id.ComponentID)

	// Every handler must exist before RegisterEndpoint: nervud may dispatch the moment it succeeds.
	registerHandlers(host, svc)

	hash, err := schemaHash()
	if err != nil {
		return err
	}
	regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	epID, err := host.RegisterEndpoint(regCtx, sdk.RegisterRequest{
		InterfaceID:    interfaceID,
		Major:          interfaceMajor,
		SchemaHash:     hash,
		ResourceHandle: resourceHandle,
	})
	if err != nil {
		return fmt.Errorf("register endpoint: %w", err)
	}
	log.Info("piperd: endpoint registered", "endpoint_id", epID, "interface", interfaceID, "resource", resourceHandle)

	done := make(chan error, 1)
	go func() { done <- host.Serve() }()

	select {
	case <-ctx.Done():
		log.Info("piperd: shutting down")
		unregCtx, unregCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer unregCancel()
		_ = host.UnregisterEndpoint(unregCtx, epID, true)
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

func registerHandlers(host *sdk.ServiceHost, svc *Service) {
	host.Handle(methodGetArmState, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		st, err := svc.GetArmState(cc.Ctx)
		if err != nil {
			return nil, err
		}
		return proto.Marshal(st)
	})
	host.Handle(methodServoToPose, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		var req manipulatorv1.ServoToPoseRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, errInvalid
		}
		res, err := svc.ServoToPose(cc, &req)
		if err != nil {
			return nil, err
		}
		return proto.Marshal(res)
	})
	host.Handle(methodSetGripper, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		var req manipulatorv1.SetGripperRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, errInvalid
		}
		return nil, svc.SetGripper(cc, &req)
	})
	host.Handle(methodStop, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		return nil, svc.Stop(cc)
	})
	host.Handle(methodGoHome, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		res, err := svc.GoHome(cc)
		if err != nil {
			return nil, err
		}
		return proto.Marshal(res)
	})
	host.Handle(methodMoveToPose, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		var req manipulatorv1.MoveToPoseRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, errInvalid
		}
		res, err := svc.MoveToPose(cc, &req)
		if err != nil {
			return nil, err
		}
		return proto.Marshal(res)
	})
	host.Handle(methodMoveTrajectory, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		var req manipulatorv1.MoveJointTrajectoryRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, errInvalid
		}
		res, err := svc.MoveJointTrajectory(cc, &req)
		if err != nil {
			return nil, err
		}
		return proto.Marshal(res)
	})
	// SubscribeArmState (method 2) is left unhandled: Manipulator@1 declares no event enum to carry
	// it yet, and the SDK answers an unhandled method as not implemented. Apps poll GetArmState.
}

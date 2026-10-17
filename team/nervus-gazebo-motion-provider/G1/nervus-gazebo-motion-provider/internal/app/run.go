package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nervus-os/nervus-gazebo-provider/internal/contract"
	"github.com/nervus-os/nervus-gazebo-provider/internal/provider"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"github.com/nervus-os/nervus-ipc/sdk"
)

const serviceVersion = "0.1.0"

type Config struct {
	SocketPath string
	Provider   provider.Config
}

func Run(
	ctx context.Context,
	config Config,
	backend simulation.Backend,
	log *slog.Logger,
) error {
	if ctx == nil {
		return fmt.Errorf("gazebod: nil context")
	}
	if backend == nil {
		return fmt.Errorf("gazebod: nil simulation backend")
	}
	if log == nil {
		log = slog.Default()
	}

	host, err := sdk.NewServiceHost(sdk.Config{
		SockPath:    config.SocketPath,
		ComponentID: contract.ComponentID,
		SDKName:     "nervus-gazebo-motion-provider-g1",
		SDKVersion:  serviceVersion,
		Log:         log,
	})
	if err != nil {
		return errors.Join(
			fmt.Errorf("gazebod: connect to nervud: %w", err),
			backend.Close(),
		)
	}

	identity := host.Identity()
	log.Info("gazebod: ServiceHost handshake complete",
		"package_id", identity.PackageID,
		"component_id", identity.ComponentID,
	)

	service, err := provider.New(provider.WrapServiceHost(host), backend, config.Provider, log)
	if err != nil {
		_ = host.Close()
		return errors.Join(
			fmt.Errorf("gazebod: create provider service: %w", err),
			backend.Close(),
		)
	}
	return service.Run(ctx)
}

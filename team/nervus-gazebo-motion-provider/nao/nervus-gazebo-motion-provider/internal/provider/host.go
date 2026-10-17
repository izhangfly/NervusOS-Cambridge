package provider

import (
	"context"

	"github.com/nervus-os/nervus-ipc/sdk"
)

// Host and Endpoint are the narrow ServiceHost surface used by Service. The
// adapter keeps tests on the real lifecycle contract without requiring a live
// nervud socket.
type Host interface {
	NewEndpoint(sdk.RegisterRequest) Endpoint
	Serve() error
	Close() error
}

type Endpoint interface {
	Handle(uint32, sdk.Handler) error
	Register(context.Context) (uint64, error)
	Unregister(context.Context, bool) error
	PublishEventAt(uint32, []byte, uint64) error
}

type SDKHost struct {
	host *sdk.ServiceHost
}

func WrapServiceHost(host *sdk.ServiceHost) *SDKHost {
	return &SDKHost{host: host}
}

func (h *SDKHost) NewEndpoint(req sdk.RegisterRequest) Endpoint {
	return h.host.NewEndpoint(req)
}

func (h *SDKHost) Serve() error { return h.host.Serve() }

func (h *SDKHost) Close() error { return h.host.Close() }

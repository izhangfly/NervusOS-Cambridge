//go:build linux

// This file wraps the three Transfer Control steps: Begin / Commit / Abort.
//
// # Why they must go out on the same ServiceHost connection
//
// BeginTransfer needs origin_route_id, and a route_id is a [connection-scoped] handle: it's the number
// nervud assigned to "this Dispatch on this connection". Sending BeginTransfer on a separate Client connection,
// even with exactly the same process identity, nervud can't find that route and replies NOT_FOUND.
//
// So the methods in this file hang off cameraEndpoint and use the ServiceHost it holds.
package main

import (
	"context"
	"fmt"
	"time"

	transferv1 "github.com/nervus-os/nervus-ipc/protocol/interface/transferv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"
)

// interfaceTransferControl is nervud's built-in Transfer Control interface.
const interfaceTransferControl = "nervus.interface.transfer.control"

// transferCallTimeout is the timeout for the three control methods.
//
// Matches max_timeout_ms (3000) declared in transfer_control.proto: a bigger value wouldn't take
// effect (nervud tightens it to the MethodMeta) and would only make local waiting disagree with actual behaviour.
const transferCallTimeout = 3 * time.Second

// resolveTransferControl resolves the Transfer Control endpoint lazily and caches it.
//
// [Lazily, not at start-up]: on a machine without cameras camerad never opens a stream,
// and forcing a resolve at start would only make it fail and exit over a dependency it doesn't need.
//
// The cached binding can go stale (the kernel restarted). That shows up as NOT_FOUND on Begin,
// and beginTransfer resolves once more.
func (c *cameraEndpoint) resolveTransferControl(ctx context.Context) (sdk.Endpoint, error) {
	c.mu.Lock()
	if c.transferResolved {
		ep := c.transfer
		c.mu.Unlock()
		return ep, nil
	}
	c.mu.Unlock()

	rctx, cancel := context.WithTimeout(ctx, transferCallTimeout)
	defer cancel()
	ep, err := c.host.ResolveEndpoint(rctx, sdk.ResolveRequest{
		InterfaceID: interfaceTransferControl,
		MinMajor:    1,
		MaxMajor:    1,
		// No selector: Transfer Control isn't bound to any Resource. From v2 an empty selector
		// means exactly that, and no longer implicitly points at the base.
	})
	if err != nil {
		return sdk.Endpoint{}, fmt.Errorf("camerad: resolve %s: %w", interfaceTransferControl, err)
	}

	c.mu.Lock()
	c.transfer = ep
	c.transferResolved = true
	c.mu.Unlock()
	return ep, nil
}

func (c *cameraEndpoint) invalidateTransferControl() {
	c.mu.Lock()
	c.transferResolved = false
	c.mu.Unlock()
}

// beginTransfer sets up a PREPARED shared-memory pipe for this Dispatch.
//
// maxPacketBytes is the per-frame limit: nervud sizes the shared-memory slots from it. Too small and frames don't fit,
// so every frame write fails; too large only uses more memory.
func (c *cameraEndpoint) beginTransfer(
	cc sdk.CallContext, maxPacketBytes uint32,
) (*transferv1.BeginTransferResponse, error) {
	response, err := c.callTransfer(cc, maxPacketBytes)
	if err == nil {
		return response, nil
	}
	// The binding is stale (kernel restarted / permission revoked): resolve once more and retry.
	// Without the retry, after nervud restarts this process would permanently lose the ability to open streams until it restarts too.
	if !sdk.NeedsReResolve(err) {
		return nil, err
	}
	c.log.Warn("camerad: Transfer Control binding is stale; resolving again", "err", err)
	c.invalidateTransferControl()
	return c.callTransfer(cc, maxPacketBytes)
}

func (c *cameraEndpoint) callTransfer(
	cc sdk.CallContext, maxPacketBytes uint32,
) (*transferv1.BeginTransferResponse, error) {
	ep, err := c.resolveTransferControl(cc.Ctx)
	if err != nil {
		return nil, deviceUnavailable()
	}

	req, err := proto.Marshal(&transferv1.BeginTransferRequest{
		OriginRouteId: cc.RouteID,
		Direction:     ipcv1.TransferDirection_TRANSFER_DIRECTION_PROVIDER_TO_CALLER,
		// Ask explicitly for SHARED_MEMORY_RING rather than leaving UNSPECIFIED for nervud to pick the baseline:
		// the baseline is FRAMED_RELAY, and a 1080p30 stream through it means ~180 MB/s passing through the kernel frame by frame.
		// camera.proto's allowed_modes only lists RING, and this matches it.
		PreferredMode:           ipcv1.TransferMode_TRANSFER_MODE_SHARED_MEMORY_RING,
		RequestedMaxPacketBytes: maxPacketBytes,
	})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(cc.Ctx, transferCallTimeout)
	defer cancel()
	out, err := c.host.Call(ctx, ep.EndpointID,
		uint32(transferv1.TransferControlMethod_TRANSFER_CONTROL_METHOD_BEGIN_TRANSFER),
		req, transferCallTimeout)
	if err != nil {
		return nil, err
	}

	var response transferv1.BeginTransferResponse
	if err := proto.Unmarshal(out, &response); err != nil {
		return nil, err
	}
	if response.GetProvider() == nil || response.GetCaller() == nil {
		// Half the handles means only one end of this pipe can connect, and the other end waits forever.
		return nil, fmt.Errorf("camerad: BeginTransfer returned only half of the handles")
	}
	return &response, nil
}

// commitTransfer releases the consumer end. [Only once the business response is certain to be returned]; see the top of stream.go.
func (c *cameraEndpoint) commitTransfer(ctx context.Context, transferID []byte) error {
	ep, err := c.resolveTransferControl(ctx)
	if err != nil {
		return deviceUnavailable()
	}
	req, err := proto.Marshal(&transferv1.CommitTransferRequest{TransferId: transferID})
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, transferCallTimeout)
	defer cancel()
	_, err = c.host.Call(cctx, ep.EndpointID,
		uint32(transferv1.TransferControlMethod_TRANSFER_CONTROL_METHOD_COMMIT_TRANSFER),
		req, transferCallTimeout)
	return err
}

// abortTransfer abandons a pipe that hasn't been committed.
//
// [It returns no error, only logs]: it runs in a failure path's defer, where the caller is already returning a
// more important error, and stacking another one on top would only hide the root cause.
//
// nervud also aborts automatically when the route is cancelled or the connection drops, so missing one doesn't leak forever,
// but until the connection drops that pipe keeps holding quota.
func (c *cameraEndpoint) abortTransfer(ctx context.Context, transferID []byte) {
	ep, err := c.resolveTransferControl(ctx)
	if err != nil {
		c.log.Warn("camerad: resolving Transfer Control for Abort failed", "err", err)
		return
	}
	req, err := proto.Marshal(&transferv1.AbortTransferRequest{TransferId: transferID})
	if err != nil {
		c.log.Warn("camerad: building AbortTransfer failed", "err", err)
		return
	}
	// Background rather than ctx: this path is often reached precisely because ctx timed out,
	// and aborting with a cancelled ctx is no abort at all.
	cctx, cancel := context.WithTimeout(context.Background(), transferCallTimeout)
	defer cancel()
	if _, err := c.host.Call(cctx, ep.EndpointID,
		uint32(transferv1.TransferControlMethod_TRANSFER_CONTROL_METHOD_ABORT_TRANSFER),
		req, transferCallTimeout); err != nil {
		c.log.Warn("camerad: AbortTransfer failed", "err", err)
	}
}

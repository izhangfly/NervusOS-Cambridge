package jetty

import (
	"context"
	"errors"

	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

func transportError(err error, operation string, notFound simulation.Reason) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch {
	case errors.Is(err, gztransport.ErrNotFound):
		return simulation.NewError(notFound, operation, true)
	case errors.Is(err, gztransport.ErrNotConnected), errors.Is(err, gztransport.ErrClosed):
		return simulation.NewError(simulation.ReasonTransportDisconnected, operation, true)
	case errors.Is(err, gztransport.ErrTypeMismatch):
		return simulation.NewError(simulation.ReasonTypeMismatch, operation, false)
	case errors.Is(err, gztransport.ErrTimeout):
		return simulation.NewError(simulation.ReasonRequestTimeout, operation, true)
	case errors.Is(err, gztransport.ErrRemoteRejected):
		return simulation.NewError(simulation.ReasonRemoteRejected, operation, false)
	case errors.Is(err, gztransport.ErrQueueFull):
		return simulation.NewError(simulation.ReasonQueueOverflow, operation, true)
	default:
		return simulation.NewError(simulation.ReasonNativeFailure, operation, true)
	}
}

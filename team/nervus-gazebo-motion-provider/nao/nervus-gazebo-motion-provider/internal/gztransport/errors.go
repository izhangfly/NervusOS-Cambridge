package gztransport

import (
	"errors"
	"fmt"
)

// Status is the stable error classification shared with the native C ABI.
type Status int

const (
	StatusOK Status = iota
	StatusInvalidArgument
	StatusClosed
	StatusNotFound
	StatusNotConnected
	StatusTypeMismatch
	StatusTimeout
	StatusRemoteRejected
	StatusQueueFull
	StatusAlreadyExists
	StatusNativeException
	StatusInternal
	StatusMessageTooLarge

	StatusUnavailable Status = 100
)

var (
	ErrInvalidArgument = errors.New("invalid argument")
	ErrClosed          = errors.New("closed")
	ErrNotFound        = errors.New("not found")
	ErrNotConnected    = errors.New("not connected")
	ErrTypeMismatch    = errors.New("type mismatch")
	ErrTimeout         = errors.New("timeout")
	ErrRemoteRejected  = errors.New("remote rejected")
	ErrQueueFull       = errors.New("queue full")
	ErrAlreadyExists   = errors.New("already exists")
	ErrNativeException = errors.New("native exception")
	ErrInternal        = errors.New("internal error")
	ErrMessageTooLarge = errors.New("message too large")
	ErrUnavailable     = errors.New("native Gazebo Transport unavailable")
)

// Error preserves the stable status, operation and native diagnostic text.
type Error struct {
	Status    Status
	Operation string
	Detail    string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Detail == "" {
		return fmt.Sprintf("%s: %s", e.Operation, e.Status)
	}
	return fmt.Sprintf("%s: %s: %s", e.Operation, e.Status, e.Detail)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return sentinelForStatus(e.Status)
}

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusInvalidArgument:
		return "INVALID_ARGUMENT"
	case StatusClosed:
		return "CLOSED"
	case StatusNotFound:
		return "NOT_FOUND"
	case StatusNotConnected:
		return "NOT_CONNECTED"
	case StatusTypeMismatch:
		return "TYPE_MISMATCH"
	case StatusTimeout:
		return "TIMEOUT"
	case StatusRemoteRejected:
		return "REMOTE_REJECTED"
	case StatusQueueFull:
		return "QUEUE_FULL"
	case StatusAlreadyExists:
		return "ALREADY_EXISTS"
	case StatusNativeException:
		return "NATIVE_EXCEPTION"
	case StatusInternal:
		return "INTERNAL"
	case StatusMessageTooLarge:
		return "MESSAGE_TOO_LARGE"
	case StatusUnavailable:
		return "UNAVAILABLE"
	default:
		return fmt.Sprintf("STATUS_%d", int(s))
	}
}

// StatusOf returns the stable adapter status carried by err.
func StatusOf(err error) (Status, bool) {
	var nativeError *Error
	if errors.As(err, &nativeError) {
		return nativeError.Status, true
	}
	return StatusOK, false
}

func invalidArgument(operation, detail string) error {
	return &Error{
		Status:    StatusInvalidArgument,
		Operation: operation,
		Detail:    detail,
	}
}

func sentinelForStatus(status Status) error {
	switch status {
	case StatusInvalidArgument:
		return ErrInvalidArgument
	case StatusClosed:
		return ErrClosed
	case StatusNotFound:
		return ErrNotFound
	case StatusNotConnected:
		return ErrNotConnected
	case StatusTypeMismatch:
		return ErrTypeMismatch
	case StatusTimeout:
		return ErrTimeout
	case StatusRemoteRejected:
		return ErrRemoteRejected
	case StatusQueueFull:
		return ErrQueueFull
	case StatusAlreadyExists:
		return ErrAlreadyExists
	case StatusNativeException:
		return ErrNativeException
	case StatusInternal:
		return ErrInternal
	case StatusMessageTooLarge:
		return ErrMessageTooLarge
	case StatusUnavailable:
		return ErrUnavailable
	default:
		return nil
	}
}

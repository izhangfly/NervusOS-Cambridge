package gztransport

import (
	"context"
	"errors"
	"sync"
)

const defaultGoEventBuffer = 64

// EventStream pumps native events into bounded Go channels. Backpressure is
// intentional: when the Go consumer is slow, the bounded native queue applies
// its configured coalescing and drop accounting.
type EventStream struct {
	events <-chan Event
	errors <-chan error
	cancel context.CancelFunc
	done   <-chan struct{}
	once   sync.Once
}

// NewEventStream starts one event pump. A zero buffer selects a safe default.
// Closing the stream does not close the underlying Client.
func NewEventStream(
	ctx context.Context,
	source EventSource,
	buffer int,
) (*EventStream, error) {
	if err := validateContext(ctx); err != nil {
		return nil, err
	}
	if source == nil {
		return nil, invalidArgument("start event stream", "event source must not be nil")
	}
	if buffer < 0 {
		return nil, invalidArgument("start event stream", "event buffer must not be negative")
	}
	if buffer == 0 {
		buffer = defaultGoEventBuffer
	}

	streamContext, cancel := context.WithCancel(ctx)
	events := make(chan Event, buffer)
	failures := make(chan error, 1)
	done := make(chan struct{})
	stream := &EventStream{
		events: events,
		errors: failures,
		cancel: cancel,
		done:   done,
	}

	go func() {
		defer close(done)
		defer close(events)
		defer close(failures)
		for {
			event, err := source.NextEvent(streamContext)
			if err != nil {
				if streamContext.Err() != nil || errors.Is(err, ErrClosed) {
					return
				}
				failures <- err
				return
			}
			select {
			case events <- event:
			case <-streamContext.Done():
				return
			}
		}
	}()

	return stream, nil
}

// Events returns the bounded stream of copied native events.
func (s *EventStream) Events() <-chan Event {
	if s == nil {
		return nil
	}
	return s.events
}

// Errors reports at most one unexpected pump failure and then closes.
func (s *EventStream) Errors() <-chan error {
	if s == nil {
		return nil
	}
	return s.errors
}

// Close stops the pump and waits for it to exit. It is idempotent.
func (s *EventStream) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.cancel()
		<-s.done
	})
}

package gztransport

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfigNormalizeDefaults(t *testing.T) {
	config, err := (Config{}).normalize()
	if err != nil {
		t.Fatalf("normalize default config: %v", err)
	}
	defaults := DefaultConfig()
	if config != defaults {
		t.Fatalf("default config mismatch: got %+v want %+v", config, defaults)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{name: "partition NUL", config: Config{Partition: "bad\x00partition"}},
		{name: "namespace too long", config: Config{Namespace: strings.Repeat("n", maximumTransportNameBytes+1)}},
		{name: "negative queue", config: Config{EventQueueCapacity: -1}},
		{name: "queue too large", config: Config{EventQueueCapacity: maximumEventQueueCapacity + 1}},
		{name: "negative message size", config: Config{MaxMessageBytes: -1}},
		{name: "message size too large", config: Config{MaxMessageBytes: maximumMessageBytes + 1}},
		{name: "negative publish timeout", config: Config{PublisherConnectTimeout: -time.Second}},
		{name: "negative request timeout", config: Config{DefaultRequestTimeout: -time.Second}},
		{name: "negative poll interval", config: Config{PollInterval: -time.Second}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.config.normalize()
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("normalize error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestStatusErrorMapping(t *testing.T) {
	err := &Error{
		Status:    StatusRemoteRejected,
		Operation: "request",
		Detail:    "rejected by peer",
	}
	if !errors.Is(err, ErrRemoteRejected) {
		t.Fatalf("error does not unwrap to ErrRemoteRejected: %v", err)
	}
	status, ok := StatusOf(err)
	if !ok || status != StatusRemoteRejected {
		t.Fatalf("StatusOf = %v, %v", status, ok)
	}
	if got := err.Error(); got != "request: REMOTE_REJECTED: rejected by peer" {
		t.Fatalf("unexpected error text: %q", got)
	}
}

func TestNativeTimeoutHonorsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	timeout, err := nativeTimeout(ctx, time.Second)
	if err != nil {
		t.Fatalf("nativeTimeout: %v", err)
	}
	if timeout == 0 || timeout > 25 {
		t.Fatalf("native timeout = %dms, want context-bounded value", timeout)
	}

	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := nativeTimeout(canceled, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled nativeTimeout error = %v", err)
	}
}

func TestEventStreamDeliversAndReportsFailure(t *testing.T) {
	failure := &Error{Status: StatusInternal, Operation: "source", Detail: "failed"}
	source := &scriptedEventSource{
		events: []Event{
			{Sequence: 1, Topic: "/one"},
			{Sequence: 2, Topic: "/two"},
		},
		failure: failure,
	}
	stream, err := NewEventStream(context.Background(), source, 1)
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	defer stream.Close()

	first := <-stream.Events()
	second := <-stream.Events()
	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("event order = %d, %d", first.Sequence, second.Sequence)
	}
	if err := <-stream.Errors(); !errors.Is(err, ErrInternal) {
		t.Fatalf("stream error = %v, want ErrInternal", err)
	}
	if _, open := <-stream.Events(); open {
		t.Fatal("event channel remained open")
	}
}

func TestEventStreamCloseIsIdempotent(t *testing.T) {
	source := &scriptedEventSource{block: true}
	stream, err := NewEventStream(context.Background(), source, 0)
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	stream.Close()
	stream.Close()
	if _, open := <-stream.Events(); open {
		t.Fatal("event channel remained open after close")
	}
}

func TestEventStreamValidation(t *testing.T) {
	if _, err := NewEventStream(nil, &scriptedEventSource{}, 1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil context error = %v", err)
	}
	if _, err := NewEventStream(context.Background(), nil, 1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil source error = %v", err)
	}
	if _, err := NewEventStream(context.Background(), &scriptedEventSource{}, -1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative buffer error = %v", err)
	}
}

type scriptedEventSource struct {
	mu      sync.Mutex
	events  []Event
	failure error
	block   bool
}

func (s *scriptedEventSource) NextEvent(ctx context.Context) (Event, error) {
	s.mu.Lock()
	if len(s.events) > 0 {
		event := s.events[0]
		s.events = s.events[1:]
		s.mu.Unlock()
		return event, nil
	}
	if s.failure != nil {
		err := s.failure
		s.failure = nil
		s.mu.Unlock()
		return Event{}, err
	}
	s.mu.Unlock()
	<-ctx.Done()
	return Event{}, ctx.Err()
}

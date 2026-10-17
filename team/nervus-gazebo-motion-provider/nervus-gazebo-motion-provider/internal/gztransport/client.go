package gztransport

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	defaultEventQueueCapacity      = 256
	defaultMaxMessageBytes         = 16 * 1024 * 1024
	defaultPublisherConnectTimeout = time.Second
	defaultRequestTimeout          = 5 * time.Second
	defaultPollInterval            = 50 * time.Millisecond
	maximumEventQueueCapacity      = 65536
	maximumMessageBytes            = 1024 * 1024 * 1024
	maximumTransportNameBytes      = 4096
	maximumMessageTypeBytes        = 512
	maximumNativeTimeout           = time.Duration(^uint32(0)) * time.Millisecond
)

// Config controls the native Gazebo Transport client. Zero values select safe
// defaults. Partition and Namespace are optional.
type Config struct {
	Partition               string
	Namespace               string
	EventQueueCapacity      int
	MaxMessageBytes         int
	PublisherConnectTimeout time.Duration
	DefaultRequestTimeout   time.Duration
	PollInterval            time.Duration
}

// DefaultConfig returns the configuration used by Open for zero-valued fields.
func DefaultConfig() Config {
	return Config{
		EventQueueCapacity:      defaultEventQueueCapacity,
		MaxMessageBytes:         defaultMaxMessageBytes,
		PublisherConnectTimeout: defaultPublisherConnectTimeout,
		DefaultRequestTimeout:   defaultRequestTimeout,
		PollInterval:            defaultPollInterval,
	}
}

func (c Config) normalize() (Config, error) {
	defaults := DefaultConfig()
	if c.EventQueueCapacity == 0 {
		c.EventQueueCapacity = defaults.EventQueueCapacity
	}
	if c.MaxMessageBytes == 0 {
		c.MaxMessageBytes = defaults.MaxMessageBytes
	}
	if c.PublisherConnectTimeout == 0 {
		c.PublisherConnectTimeout = defaults.PublisherConnectTimeout
	}
	if c.DefaultRequestTimeout == 0 {
		c.DefaultRequestTimeout = defaults.DefaultRequestTimeout
	}
	if c.PollInterval == 0 {
		c.PollInterval = defaults.PollInterval
	}

	if err := validateOptionalName("partition", c.Partition); err != nil {
		return Config{}, err
	}
	if err := validateOptionalName("namespace", c.Namespace); err != nil {
		return Config{}, err
	}
	if c.EventQueueCapacity < 1 || c.EventQueueCapacity > maximumEventQueueCapacity {
		return Config{}, invalidArgument(
			"normalize config",
			fmt.Sprintf("event queue capacity must be between 1 and %d", maximumEventQueueCapacity),
		)
	}
	if c.MaxMessageBytes < 1 || c.MaxMessageBytes > maximumMessageBytes {
		return Config{}, invalidArgument(
			"normalize config",
			fmt.Sprintf("maximum message bytes must be between 1 and %d", maximumMessageBytes),
		)
	}
	for name, value := range map[string]time.Duration{
		"publisher connect timeout": c.PublisherConnectTimeout,
		"default request timeout":   c.DefaultRequestTimeout,
		"poll interval":             c.PollInterval,
	} {
		if value <= 0 || value > maximumNativeTimeout {
			return Config{}, invalidArgument(
				"normalize config",
				fmt.Sprintf("%s must be greater than zero and no more than %s", name, maximumNativeTimeout),
			)
		}
	}

	return c, nil
}

// Versions reports the native ABI and linked Gazebo library versions.
type Versions struct {
	ABI       uint32
	Transport string
	Messages  string
}

// Message is a serialized protobuf message addressed to a Gazebo topic.
type Message struct {
	Topic string
	Type  string
	Data  []byte
}

// PublishOptions controls connection waiting for a publish operation.
type PublishOptions struct {
	RequireConnection bool
	ConnectionTimeout time.Duration
}

// ServiceRequest is a serialized Gazebo Transport request.
type ServiceRequest struct {
	Service      string
	RequestType  string
	ResponseType string
	Data         []byte
	Timeout      time.Duration
}

// TopicInfo is the currently discovered metadata for a topic.
type TopicInfo struct {
	PublisherCount  int
	SubscriberCount int
	TypeConflict    bool
	MessageType     string
}

// ServiceInfo is the currently discovered metadata for a service.
type ServiceInfo struct {
	ProviderCount int
	TypeConflict  bool
	RequestType   string
	ResponseType  string
}

// SubscriptionID identifies one native raw subscription.
type SubscriptionID uint64

// SubscriptionOptions controls Transport throttling and native queue behavior.
type SubscriptionOptions struct {
	MaxMessagesPerSecond uint64
	CoalesceLatest       bool
}

// Event is one serialized message copied out of the bounded native queue.
type Event struct {
	SubscriptionID SubscriptionID
	Sequence       uint64
	DroppedBefore  uint64
	Topic          string
	MessageType    string
	Data           []byte
}

// EventSource is the minimal interface consumed by EventStream.
type EventSource interface {
	NextEvent(context.Context) (Event, error)
}

// Client is the Go-native view of Gazebo Transport. It exposes no C or Gazebo
// C++ types.
type Client interface {
	EventSource

	Versions() Versions
	ListTopics(context.Context) ([]string, error)
	ListServices(context.Context) ([]string, error)
	TopicInfo(context.Context, string) (TopicInfo, error)
	ServiceInfo(context.Context, string) (ServiceInfo, error)
	WaitForTopic(context.Context, string, string) error
	WaitForSubscribers(context.Context, string, string) error
	WaitForService(context.Context, string, string, string) error
	Publish(context.Context, Message, PublishOptions) error
	Request(context.Context, ServiceRequest) ([]byte, error)
	Subscribe(context.Context, string, string, SubscriptionOptions) (SubscriptionID, error)
	Unsubscribe(context.Context, SubscriptionID) error
	Close() error
}

// Open creates an in-process Gazebo Transport client.
func Open(config Config) (Client, error) {
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return openNative(normalized)
}

// NativeVersions reports the versions compiled into the platform adapter.
func NativeVersions() (Versions, error) {
	return nativeVersions()
}

func validateOptionalName(field, value string) error {
	if value == "" {
		return nil
	}
	return validateString(field, value, maximumTransportNameBytes)
}

func validateTransportName(field, value string) error {
	if value == "" {
		return invalidArgument("validate "+field, field+" must not be empty")
	}
	return validateString(field, value, maximumTransportNameBytes)
}

func validateMessageType(field, value string) error {
	if value == "" {
		return invalidArgument("validate "+field, field+" must not be empty")
	}
	return validateString(field, value, maximumMessageTypeBytes)
}

func validateOptionalMessageType(field, value string) error {
	if value == "" {
		return nil
	}
	return validateMessageType(field, value)
}

func validateString(field, value string, maximum int) error {
	if strings.IndexByte(value, 0) >= 0 {
		return invalidArgument("validate "+field, field+" contains NUL")
	}
	if len(value) > maximum {
		return invalidArgument(
			"validate "+field,
			fmt.Sprintf("%s exceeds %d bytes", field, maximum),
		)
	}
	return nil
}

func validateContext(ctx context.Context) error {
	if ctx == nil {
		return invalidArgument("validate context", "context must not be nil")
	}
	return ctx.Err()
}

func nativeTimeout(ctx context.Context, maximum time.Duration) (uint32, error) {
	if err := validateContext(ctx); err != nil {
		return 0, err
	}
	if maximum <= 0 {
		return 0, invalidArgument("calculate timeout", "timeout must be greater than zero")
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, context.DeadlineExceeded
		}
		if remaining < maximum {
			maximum = remaining
		}
	}
	if maximum > maximumNativeTimeout {
		maximum = maximumNativeTimeout
	}
	milliseconds := (maximum + time.Millisecond - 1) / time.Millisecond
	if milliseconds < 1 {
		milliseconds = 1
	}
	return uint32(milliseconds), nil
}

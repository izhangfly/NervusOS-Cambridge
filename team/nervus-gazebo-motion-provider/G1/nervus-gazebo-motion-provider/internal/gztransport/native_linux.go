//go:build linux && cgo

package gztransport

/*
#cgo CFLAGS: -I${SRCDIR}/../../native/include
#cgo CXXFLAGS: -I${SRCDIR}/../../native/include -I/usr/include/gz/transport15 -I/usr/include/gz/msgs12 -I/usr/include/gz/utils4 -I/usr/include/gz/math9 -std=c++17 -Wall -Wextra -Wpedantic
#cgo LDFLAGS: -lgz-transport -luuid -lgz-msgs -lprotobuf -lgz-math -lgz-utils -ltinyxml2 -lstdc++

#include <stdlib.h>
#include "../../native/include/nervus/gazebo/adapter.h"
*/
import "C"

import (
	"context"
	"errors"
	"sync"
	"time"
	"unsafe"
)

const maximumGoBytes = 1<<31 - 1

type nativeClient struct {
	mu      sync.RWMutex
	eventMu sync.Mutex
	handle  *C.gz_adapter_t
	config  Config
	version Versions
}

func openNative(config Config) (Client, error) {
	var nativeConfig C.gz_adapter_config_t
	C.gz_adapter_config_init(&nativeConfig)

	partition, freePartition := optionalCString(config.Partition)
	defer freePartition()
	nameSpace, freeNamespace := optionalCString(config.Namespace)
	defer freeNamespace()

	nativeConfig.partition = partition
	nativeConfig.name_space = nameSpace
	nativeConfig.event_queue_capacity = C.size_t(config.EventQueueCapacity)
	nativeConfig.max_message_bytes = C.size_t(config.MaxMessageBytes)
	publisherTimeout, err := durationMilliseconds(config.PublisherConnectTimeout)
	if err != nil {
		return nil, err
	}
	nativeConfig.publisher_connect_timeout_ms = C.uint32_t(publisherTimeout)

	var handle *C.gz_adapter_t
	var nativeError C.gz_adapter_error_t
	status := C.gz_adapter_create(&nativeConfig, &handle, &nativeError)
	if callErr := nativeStatusError("create adapter", status, &nativeError); callErr != nil {
		if handle != nil {
			C.gz_adapter_destroy(handle)
		}
		return nil, callErr
	}
	if handle == nil {
		return nil, &Error{
			Status:    StatusInternal,
			Operation: "create adapter",
			Detail:    "native adapter returned a null handle",
		}
	}

	versions, err := nativeVersions()
	if err != nil {
		C.gz_adapter_destroy(handle)
		return nil, err
	}

	return &nativeClient{
		handle:  handle,
		config:  config,
		version: versions,
	}, nil
}

func nativeVersions() (Versions, error) {
	versions := C.gz_adapter_versions()
	if versions == nil {
		return Versions{}, &Error{
			Status:    StatusInternal,
			Operation: "read native versions",
			Detail:    "native adapter returned null version metadata",
		}
	}
	return Versions{
		ABI:       uint32(versions.abi_version),
		Transport: C.GoString(versions.transport_version),
		Messages:  C.GoString(versions.msgs_version),
	}, nil
}

func (c *nativeClient) Versions() Versions {
	if c == nil {
		return Versions{}
	}
	return c.version
}

func (c *nativeClient) ListTopics(ctx context.Context) ([]string, error) {
	return c.list(ctx, "list topics", false)
}

func (c *nativeClient) ListServices(ctx context.Context) ([]string, error) {
	return c.list(ctx, "list services", true)
}

func (c *nativeClient) list(
	ctx context.Context,
	operation string,
	services bool,
) ([]string, error) {
	if err := validateContext(ctx); err != nil {
		return nil, err
	}
	handle, unlock, err := c.lockHandle(operation)
	if err != nil {
		return nil, err
	}
	var output C.gz_adapter_string_list_t
	var nativeError C.gz_adapter_error_t
	var status C.gz_adapter_status_t
	if services {
		status = C.gz_adapter_list_services(handle, &output, &nativeError)
	} else {
		status = C.gz_adapter_list_topics(handle, &output, &nativeError)
	}
	unlock()
	defer C.gz_adapter_string_list_clear(&output)
	if err := preferContextError(ctx, nativeStatusError(operation, status, &nativeError)); err != nil {
		return nil, err
	}

	count := int(output.count)
	if count < 0 || C.size_t(count) != output.count {
		return nil, &Error{Status: StatusInternal, Operation: operation, Detail: "native list is too large"}
	}
	if count == 0 {
		return []string{}, nil
	}
	if output.items == nil {
		return nil, &Error{Status: StatusInternal, Operation: operation, Detail: "native list data is null"}
	}
	items := unsafe.Slice((**C.char)(unsafe.Pointer(output.items)), count)
	result := make([]string, count)
	for index, item := range items {
		if item == nil {
			return nil, &Error{Status: StatusInternal, Operation: operation, Detail: "native list contains a null item"}
		}
		result[index] = C.GoString(item)
	}
	return result, nil
}

func (c *nativeClient) TopicInfo(ctx context.Context, topic string) (TopicInfo, error) {
	if err := validateContext(ctx); err != nil {
		return TopicInfo{}, err
	}
	if err := validateTransportName("topic", topic); err != nil {
		return TopicInfo{}, err
	}
	nativeTopic := C.CString(topic)
	defer C.free(unsafe.Pointer(nativeTopic))

	handle, unlock, err := c.lockHandle("query topic info")
	if err != nil {
		return TopicInfo{}, err
	}
	var output C.gz_adapter_topic_info_t
	var nativeError C.gz_adapter_error_t
	status := C.gz_adapter_topic_info(handle, nativeTopic, &output, &nativeError)
	unlock()
	defer C.gz_adapter_topic_info_clear(&output)
	if err := preferContextError(ctx, nativeStatusError("query topic info", status, &nativeError)); err != nil {
		return TopicInfo{}, err
	}

	publishers, err := nativeCount("query topic info", output.publisher_count)
	if err != nil {
		return TopicInfo{}, err
	}
	subscribers, err := nativeCount("query topic info", output.subscriber_count)
	if err != nil {
		return TopicInfo{}, err
	}
	return TopicInfo{
		PublisherCount:  publishers,
		SubscriberCount: subscribers,
		TypeConflict:    output.type_conflict != 0,
		MessageType:     goString(output.message_type),
	}, nil
}

func (c *nativeClient) ServiceInfo(ctx context.Context, service string) (ServiceInfo, error) {
	if err := validateContext(ctx); err != nil {
		return ServiceInfo{}, err
	}
	if err := validateTransportName("service", service); err != nil {
		return ServiceInfo{}, err
	}
	nativeService := C.CString(service)
	defer C.free(unsafe.Pointer(nativeService))

	handle, unlock, err := c.lockHandle("query service info")
	if err != nil {
		return ServiceInfo{}, err
	}
	var output C.gz_adapter_service_info_t
	var nativeError C.gz_adapter_error_t
	status := C.gz_adapter_service_info(handle, nativeService, &output, &nativeError)
	unlock()
	defer C.gz_adapter_service_info_clear(&output)
	if err := preferContextError(ctx, nativeStatusError("query service info", status, &nativeError)); err != nil {
		return ServiceInfo{}, err
	}

	providers, err := nativeCount("query service info", output.provider_count)
	if err != nil {
		return ServiceInfo{}, err
	}
	return ServiceInfo{
		ProviderCount: providers,
		TypeConflict:  output.type_conflict != 0,
		RequestType:   goString(output.request_type),
		ResponseType:  goString(output.response_type),
	}, nil
}

func (c *nativeClient) WaitForTopic(
	ctx context.Context,
	topic string,
	expectedType string,
) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if err := validateTransportName("topic", topic); err != nil {
		return err
	}
	if err := validateOptionalMessageType("expected message type", expectedType); err != nil {
		return err
	}
	nativeTopic := C.CString(topic)
	defer C.free(unsafe.Pointer(nativeTopic))
	nativeType, freeType := optionalCString(expectedType)
	defer freeType()

	for {
		timeout, err := nativeTimeout(ctx, c.config.PollInterval)
		if err != nil {
			return err
		}
		handle, unlock, err := c.lockHandle("wait for topic")
		if err != nil {
			return err
		}
		var nativeError C.gz_adapter_error_t
		status := C.gz_adapter_wait_for_topic(handle, nativeTopic, nativeType, C.uint32_t(timeout), &nativeError)
		unlock()
		callErr := preferContextError(ctx, nativeStatusError("wait for topic", status, &nativeError))
		if callErr == nil {
			return nil
		}
		if !errors.Is(callErr, ErrTimeout) {
			return callErr
		}
	}
}

func (c *nativeClient) WaitForService(
	ctx context.Context,
	service string,
	expectedRequestType string,
	expectedResponseType string,
) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if err := validateTransportName("service", service); err != nil {
		return err
	}
	if err := validateOptionalMessageType("expected request type", expectedRequestType); err != nil {
		return err
	}
	if err := validateOptionalMessageType("expected response type", expectedResponseType); err != nil {
		return err
	}
	nativeService := C.CString(service)
	defer C.free(unsafe.Pointer(nativeService))
	nativeRequestType, freeRequestType := optionalCString(expectedRequestType)
	defer freeRequestType()
	nativeResponseType, freeResponseType := optionalCString(expectedResponseType)
	defer freeResponseType()

	for {
		timeout, err := nativeTimeout(ctx, c.config.PollInterval)
		if err != nil {
			return err
		}
		handle, unlock, err := c.lockHandle("wait for service")
		if err != nil {
			return err
		}
		var nativeError C.gz_adapter_error_t
		status := C.gz_adapter_wait_for_service(
			handle,
			nativeService,
			nativeRequestType,
			nativeResponseType,
			C.uint32_t(timeout),
			&nativeError,
		)
		unlock()
		callErr := preferContextError(ctx, nativeStatusError("wait for service", status, &nativeError))
		if callErr == nil {
			return nil
		}
		if !errors.Is(callErr, ErrTimeout) {
			return callErr
		}
	}
}

func (c *nativeClient) Publish(
	ctx context.Context,
	message Message,
	options PublishOptions,
) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if err := validateTransportName("topic", message.Topic); err != nil {
		return err
	}
	if err := validateMessageType("message type", message.Type); err != nil {
		return err
	}
	if len(message.Data) > c.config.MaxMessageBytes {
		return &Error{Status: StatusMessageTooLarge, Operation: "publish", Detail: "message exceeds configured size limit"}
	}
	if options.ConnectionTimeout < 0 || options.ConnectionTimeout > maximumNativeTimeout {
		return invalidArgument("publish", "connection timeout is outside the supported range")
	}

	nativeTopic := C.CString(message.Topic)
	defer C.free(unsafe.Pointer(nativeTopic))
	nativeType := C.CString(message.Type)
	defer C.free(unsafe.Pointer(nativeType))

	if !options.RequireConnection {
		return c.publishOnce(ctx, nativeTopic, nativeType, message.Data, false, 0)
	}
	total := options.ConnectionTimeout
	if total == 0 {
		total = c.config.PublisherConnectTimeout
	}
	deadline := time.Now().Add(total)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return &Error{Status: StatusNotConnected, Operation: "publish", Detail: "connection timeout expired"}
		}
		attempt := c.config.PollInterval
		if remaining < attempt {
			attempt = remaining
		}
		timeout, err := nativeTimeout(ctx, attempt)
		if err != nil {
			return err
		}
		err = c.publishOnce(ctx, nativeTopic, nativeType, message.Data, true, timeout)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrNotConnected) {
			return err
		}
	}
}

func (c *nativeClient) publishOnce(
	ctx context.Context,
	topic *C.char,
	messageType *C.char,
	data []byte,
	requireConnection bool,
	timeout uint32,
) error {
	handle, unlock, err := c.lockHandle("publish")
	if err != nil {
		return err
	}
	var nativeError C.gz_adapter_error_t
	status := C.gz_adapter_publish(
		handle,
		topic,
		messageType,
		bytePointer(data),
		C.size_t(len(data)),
		boolInt(requireConnection),
		C.uint32_t(timeout),
		&nativeError,
	)
	unlock()
	return preferContextError(ctx, nativeStatusError("publish", status, &nativeError))
}

func (c *nativeClient) Request(ctx context.Context, request ServiceRequest) ([]byte, error) {
	if err := validateContext(ctx); err != nil {
		return nil, err
	}
	if err := validateTransportName("service", request.Service); err != nil {
		return nil, err
	}
	if err := validateMessageType("request type", request.RequestType); err != nil {
		return nil, err
	}
	if err := validateMessageType("response type", request.ResponseType); err != nil {
		return nil, err
	}
	if len(request.Data) > c.config.MaxMessageBytes {
		return nil, &Error{Status: StatusMessageTooLarge, Operation: "request", Detail: "request exceeds configured size limit"}
	}
	timeoutDuration := request.Timeout
	if timeoutDuration == 0 {
		timeoutDuration = c.config.DefaultRequestTimeout
	}
	if timeoutDuration < 0 || timeoutDuration > maximumNativeTimeout {
		return nil, invalidArgument("request", "timeout is outside the supported range")
	}
	timeout, err := nativeTimeout(ctx, timeoutDuration)
	if err != nil {
		return nil, err
	}

	nativeService := C.CString(request.Service)
	defer C.free(unsafe.Pointer(nativeService))
	nativeRequestType := C.CString(request.RequestType)
	defer C.free(unsafe.Pointer(nativeRequestType))
	nativeResponseType := C.CString(request.ResponseType)
	defer C.free(unsafe.Pointer(nativeResponseType))

	handle, unlock, err := c.lockHandle("request")
	if err != nil {
		return nil, err
	}
	var output C.gz_adapter_buffer_t
	var nativeError C.gz_adapter_error_t
	status := C.gz_adapter_request(
		handle,
		nativeService,
		nativeRequestType,
		nativeResponseType,
		bytePointer(request.Data),
		C.size_t(len(request.Data)),
		C.uint32_t(timeout),
		&output,
		&nativeError,
	)
	unlock()
	defer C.gz_adapter_buffer_clear(&output)
	if err := preferContextError(ctx, nativeStatusError("request", status, &nativeError)); err != nil {
		return nil, err
	}
	return copyNativeBytes("request", output.data, output.size)
}

func (c *nativeClient) Subscribe(
	ctx context.Context,
	topic string,
	messageType string,
	options SubscriptionOptions,
) (SubscriptionID, error) {
	if err := validateContext(ctx); err != nil {
		return 0, err
	}
	if err := validateTransportName("topic", topic); err != nil {
		return 0, err
	}
	if err := validateMessageType("message type", messageType); err != nil {
		return 0, err
	}
	nativeTopic := C.CString(topic)
	defer C.free(unsafe.Pointer(nativeTopic))
	nativeType := C.CString(messageType)
	defer C.free(unsafe.Pointer(nativeType))

	handle, unlock, err := c.lockHandle("subscribe")
	if err != nil {
		return 0, err
	}
	var id C.uint64_t
	var nativeError C.gz_adapter_error_t
	status := C.gz_adapter_subscribe(
		handle,
		nativeTopic,
		nativeType,
		C.uint64_t(options.MaxMessagesPerSecond),
		boolInt(options.CoalesceLatest),
		&id,
		&nativeError,
	)
	unlock()
	if err := preferContextError(ctx, nativeStatusError("subscribe", status, &nativeError)); err != nil {
		return 0, err
	}
	if id == 0 {
		return 0, &Error{Status: StatusInternal, Operation: "subscribe", Detail: "native adapter returned subscription ID zero"}
	}
	return SubscriptionID(id), nil
}

func (c *nativeClient) Unsubscribe(ctx context.Context, id SubscriptionID) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if id == 0 {
		return invalidArgument("unsubscribe", "subscription ID zero is reserved")
	}
	handle, unlock, err := c.lockHandle("unsubscribe")
	if err != nil {
		return err
	}
	var nativeError C.gz_adapter_error_t
	status := C.gz_adapter_unsubscribe(handle, C.uint64_t(id), &nativeError)
	unlock()
	return preferContextError(ctx, nativeStatusError("unsubscribe", status, &nativeError))
}

func (c *nativeClient) NextEvent(ctx context.Context) (Event, error) {
	if err := validateContext(ctx); err != nil {
		return Event{}, err
	}
	c.eventMu.Lock()
	defer c.eventMu.Unlock()

	for {
		timeout, err := nativeTimeout(ctx, c.config.PollInterval)
		if err != nil {
			return Event{}, err
		}
		handle, unlock, err := c.lockHandle("read event")
		if err != nil {
			return Event{}, err
		}
		var output C.gz_adapter_event_t
		var nativeError C.gz_adapter_error_t
		status := C.gz_adapter_next_event(handle, C.uint32_t(timeout), &output, &nativeError)
		unlock()
		callErr := preferContextError(ctx, nativeStatusError("read event", status, &nativeError))
		if callErr != nil {
			C.gz_adapter_event_clear(&output)
			if errors.Is(callErr, ErrTimeout) {
				continue
			}
			return Event{}, callErr
		}
		data, err := copyNativeBytes("read event", output.data, output.size)
		event := Event{
			SubscriptionID: SubscriptionID(output.subscription_id),
			Sequence:       uint64(output.sequence),
			DroppedBefore:  uint64(output.dropped_before),
			Topic:          goString(output.topic),
			MessageType:    goString(output.message_type),
			Data:           data,
		}
		C.gz_adapter_event_clear(&output)
		if err != nil {
			return Event{}, err
		}
		return event, nil
	}
}

func (c *nativeClient) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handle == nil {
		return nil
	}

	var nativeError C.gz_adapter_error_t
	status := C.gz_adapter_close(c.handle, &nativeError)
	callErr := nativeStatusError("close adapter", status, &nativeError)
	C.gz_adapter_destroy(c.handle)
	c.handle = nil
	return callErr
}

func (c *nativeClient) lockHandle(operation string) (*C.gz_adapter_t, func(), error) {
	if c == nil {
		return nil, func() {}, &Error{Status: StatusClosed, Operation: operation, Detail: "client is nil"}
	}
	c.mu.RLock()
	if c.handle == nil {
		c.mu.RUnlock()
		return nil, func() {}, &Error{Status: StatusClosed, Operation: operation, Detail: "client is closed"}
	}
	return c.handle, c.mu.RUnlock, nil
}

func nativeStatusError(
	operation string,
	status C.gz_adapter_status_t,
	nativeError *C.gz_adapter_error_t,
) error {
	if status == C.GZ_ADAPTER_STATUS_OK {
		return nil
	}
	detail := ""
	if nativeError != nil {
		detail = C.GoString((*C.char)(unsafe.Pointer(&nativeError.message[0])))
	}
	return &Error{Status: Status(status), Operation: operation, Detail: detail}
}

func preferContextError(ctx context.Context, callErr error) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return callErr
}

func optionalCString(value string) (*C.char, func()) {
	if value == "" {
		return nil, func() {}
	}
	result := C.CString(value)
	return result, func() {
		C.free(unsafe.Pointer(result))
	}
}

func bytePointer(data []byte) *C.uint8_t {
	if len(data) == 0 {
		return nil
	}
	return (*C.uint8_t)(unsafe.Pointer(unsafe.SliceData(data)))
}

func boolInt(value bool) C.int {
	if value {
		return 1
	}
	return 0
}

func goString(value *C.char) string {
	if value == nil {
		return ""
	}
	return C.GoString(value)
}

func nativeCount(operation string, count C.size_t) (int, error) {
	result := int(count)
	if result < 0 || C.size_t(result) != count {
		return 0, &Error{Status: StatusInternal, Operation: operation, Detail: "native count exceeds Go int"}
	}
	return result, nil
}

func copyNativeBytes(operation string, data *C.uint8_t, size C.size_t) ([]byte, error) {
	if size == 0 {
		return []byte{}, nil
	}
	if data == nil {
		return nil, &Error{Status: StatusInternal, Operation: operation, Detail: "native buffer data is null"}
	}
	if uint64(size) > maximumGoBytes {
		return nil, &Error{Status: StatusInternal, Operation: operation, Detail: "native buffer exceeds Go copy limit"}
	}
	return C.GoBytes(unsafe.Pointer(data), C.int(size)), nil
}

func durationMilliseconds(value time.Duration) (uint32, error) {
	if value <= 0 || value > maximumNativeTimeout {
		return 0, invalidArgument("convert timeout", "timeout is outside the native range")
	}
	milliseconds := (value + time.Millisecond - 1) / time.Millisecond
	return uint32(milliseconds), nil
}

var _ Client = (*nativeClient)(nil)

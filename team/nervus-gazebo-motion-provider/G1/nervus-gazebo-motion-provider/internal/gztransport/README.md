# Nervus Gazebo Motion Provider Go Transport API

`gztransport` is a pure Go interface used internally by the system service. Callers only handle `context.Context`, Go structures, `[]byte`, and typed errors, without touching C handle, C buffer, Gazebo C++ objects, or protobuf C++ objects.

This is the internal transport boundary of Motion Provider, not a general-purpose Gazebo gateway. The business layer can only use it via signed motion Profile and compiled internal semantic Adapter; Nervus callers cannot provide arbitrary topic, service, or message types. This service does not subscribe or forward sensor streams such as camera, depth, audio, radar, and tactile via this means.

## Build Forms

Linux + cgo builds embed `native/src/adapter.cc` into the final Go binary via `adapter_embed_linux.cc`, then dynamically link Jetty's `libgz-transport.so.15`, `libgz-msgs.so.12`, and standard C++ runtime libraries. Adapter is not a separate process and does not require a runtime sidecar.

The current build target is fixed to Gazebo Jetty on Ubuntu 24.04: Transport 15, Msgs 12, Utils 4, and Math 9. Header file versions are explicitly constrained in cgo compilation parameters, and runtime versions can be queried via `NativeVersions` or `Client.Versions`.

Non-Linux or `CGO_ENABLED=0` builds use fail-closed stubs. `Open` and `NativeVersions` return `ErrUnavailable` with `StatusUnavailable`, without degrading to shell commands or silent empty implementations.

## API

`Open(Config)` returns the `Client` interface. Zero-value configuration uses bounded defaults:

- native event queue: 256;
- maximum message size per message: 16 MiB;
- publisher connection timeout: 1 second;
- request timeout: 5 seconds;
- context polling interval: 50 milliseconds.

The interface provides topic/service lists and information, waiting for discovery, serialized publish/request, raw subscribe/unsubscribe, event reading, and closing. The type-structured Adapter in the business layer is responsible for selecting the fixed gz-msgs type and parsing the serialized protobuf; the Transport layer does not understand world, model, joint, motion Resource, or sensor semantics.

All Adapter states are mapped to `*Error`, which can be checked with `errors.Is`, for example `ErrNotFound`, `ErrTypeMismatch`, `ErrTimeout`, `ErrRemoteRejected`, and `ErrClosed`; `StatusOf` can extract stable status codes. Context cancellation and deadline are kept as standard Go errors, not wrapped as Transport timeout.

## Context and Side Effects

Discovery waiting, publisher connection waiting, and event reading use short-period native calls, so they can respond to cancellation near `PollInterval`. They will not indefinitely hold the C handle.

Synchronous `RequestRaw` only executes once, avoiding Step, Reset, or other side-effecting requests from being automatically replayed. Gazebo Transport does not provide cancellation during execution for this synchronous API, so the native timeout takes the smaller value between the request timeout and context deadline. If the context is cancelled during request execution, Go returns the context error, but the caller must assume the remote may have already executed the request. The upper layer needs to use world generation, state query, or idempotent protocol to handle uncertain results.

`Publish` can safely retry in segments when waiting for a connection, because payload is not submitted before the connection is established; once `PublishRaw` succeeds, it immediately returns, without replaying the message.

## Concurrency and Closing

`Client` supports concurrent discovery, publish, request, and subscription operations. Each call must use its own input and output values. Native Adapter executes synchronous requests serially; multiple `NextEvent` calls are also serialized at the Go layer, avoiding multiple consumers unintentionally splitting the same queue.

`Close` uses an exclusive handle lock, waits for ongoing cgo calls to exit, and then closes and destroys the native handle sequentially. It can be called repeatedly. After calling `Close`, all operations return `ErrClosed`.

`NewEventStream` provides a single goroutine event pump and a bounded Go channel. When Go consumers slow down, the event pump will block, while the native bounded queue continues to execute merging and drop accounting, without generating an infinite second queue. Closing EventStream only stops the event pump, without closing the Client.

## Testing

General checks:

```bash
CGO_ENABLED=1 go test ./...
CGO_ENABLED=0 go test ./...
CGO_ENABLED=1 go vet ./...
CGO_ENABLED=1 go test -race ./...
```

Full Transport round-trip test uses a C++ peer that is compiled only under the test tag:

```bash
CGO_ENABLED=1 go test -tags=gztransporttest ./...
CGO_ENABLED=1 go test -race -tags=gztransporttest ./...
```

This peer validates StringMsg service requests/responses, StringMsg subscription events, and `gz.msgs.Double` publishes. Without this tag, the test peer does not enter the official build.

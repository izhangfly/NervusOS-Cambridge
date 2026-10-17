# Nervus Gazebo Motion Provider C++ Transport Adapter

This directory implements an in-process C++ Transport Adapter for Nervus Gazebo Motion Provider. It only links Gazebo Transport 15 and Msgs 12, not Gazebo Sim, GUI, Rendering, or physics engine, and does not start additional processes. Go system services call stable C ABI via cgo, C++ types and exceptions do not cross the boundary.

The Adapter is a generic, low-level serialized Transport channel, but this does not mean that the Provider allows arbitrary topic passthrough. The Go layer's signature motion Profile and compiled-in Adapter registry are responsible for limiting available topics, services, message types, and directions. This system service does not use this channel to provide camera, depth, audio, radar, tactile, or other sensor capabilities.

## Build

Use existing Jetty development libraries in `Nervus-Ubuntu`:

```bash
cmake -S native -B native/build \
  -DCMAKE_BUILD_TYPE=RelWithDebInfo \
  -DBUILD_TESTING=ON
cmake --build native/build --parallel
ctest --test-dir native/build --output-on-failure
```

Sanitizer Build:

```bash
cmake -S native -B native/build-asan \
  -DCMAKE_BUILD_TYPE=Debug \
  -DBUILD_TESTING=ON \
  -DNERVUS_ENABLE_SANITIZERS=ON
cmake --build native/build-asan --parallel
ctest --test-dir native/build-asan --output-on-failure
```

CMake requires versioned packages `gz-transport 15` and `gz-msgs 12`. It does not hardcode absolute paths to libraries, nor does it download, install, or copy Gazebo and model resources.

## Capabilities

Public headers are `include/nervus/gazebo/adapter.h`, current ABI version is 1:

- Create a `gz::transport::Node` configured with partition and namespace;
- List and query topics and services, checking for message type conflicts that have been discovered;
- Wait for a topic or service to appear within a bounded time;
- Use a serialized protobuf payload to call `PublishRaw`;
- Use a serialized request/response to call synchronous `RequestRaw`, distinguishing between timeout and remote rejection;
- Use `SubscribeRaw` to copy messages into a bounded native queue;
- Support latest-value merging, drop counting, message size limits, and interruptible event waiting;
- Prevent new operations, wake up blocked reads, explicitly unsubscribe, and wait for ongoing callbacks upon closing.

Adapter does not parse business protobuf, nor is it responsible for world, model, joint, motion semantics, permissions, Lease, Nervus IPC, Gazebo process, WSLg lifecycle, sensor interface, or Transfer. The Go layer handles these boundaries and only implements motion capabilities.

## Ownership

- After `gz_adapter_create` succeeds, the caller owns the opaque handle.
- `gz_adapter_close` can be called repeatedly; it only closes resources, not the handle.
- `gz_adapter_destroy` accepts a null pointer, but each valid handle can be called only once.
- Before calling `destroy`, the caller must stop and wait for all threads using the handle; no method on the handle can be called after destruction.
- Dynamic memory returned by list, info, request, and event must be separately released using the corresponding `*_clear` functions.
- `gz_adapter_versions` and `gz_adapter_status_name` return static read-only strings; the caller must not release them.
- Output structures cannot be reused without clearing; otherwise, the caller will lose the old pointer; Go encapsulation must immediately clear after copying data.

## Concurrency and Subscription

Different threads can simultaneously call discovery, publish, request, subscribe, and event APIs, but each call must use an independent output structure. Synchronous `RequestRaw` is executed serially within the Adapter. `close` can be concurrent with blocking operations; it will wait for these operations to exit within a bounded time.

Jetty Transport 15's `SubscribeRaw` returns `bool`, not RAII Subscriber; `Node::Unsubscribe` unsubscribes by topic. Therefore, an Adapter can hold at most one raw subscription for the same topic. Unsubscribe first disables the subscription in shared state, then calls Transport to prevent race conditions where callbacks are requeued after unsubscribe completes.

Callbacks do not call Go, nor do they call Transport again. They only copy topic, type, and serialized payload within a lock. Transport threads are not blocked when the queue is full or the message exceeds the limit; instead, drop count is accumulated. The number of messages dropped before the next successful enqueue is reported via `dropped_before`.

Type checking only guarantees specific types already discovered by Transport and cached Publisher types by Adapter. If a topic only has a generic subscriber without exposed specific types, the type match may not be determined before the first publish; the caller still must use the message type specified by the protocol.

The upper layer must also select the protocol from the compilation-registered registry; it must not expose the raw API here directly to Nervus caller, nor allow runtime configuration to write to arbitrary topics or protobuf types.

`adapter_test.cc` uses in-process Gazebo Transport peer to verify discovery, type checking, service success/rejection, publish, connection timeout, raw subscribe, merge, queue overflow, oversized message, concurrent unsubscribe and close wake-up. `header_c_test.c` is directly included by C compiler to verify C compatibility and ABI constants.

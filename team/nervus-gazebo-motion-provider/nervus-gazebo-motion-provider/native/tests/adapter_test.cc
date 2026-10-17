#include "nervus/gazebo/adapter.h"

#include <gz/msgs/double.pb.h>
#include <gz/msgs/stringmsg.pb.h>
#include <gz/transport/Node.hh>
#include <gz/transport/NodeOptions.hh>

#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <cstring>
#include <functional>
#include <iostream>
#include <mutex>
#include <stdexcept>
#include <string>
#include <thread>
#include <vector>

#include <unistd.h>

namespace
{
using namespace std::chrono_literals;

void Require(const bool _condition, const std::string &_message)
{
  if (!_condition)
    throw std::runtime_error(_message);
}

void RequireStatus(
    const gz_adapter_status_t _actual,
    const gz_adapter_status_t _expected,
    const gz_adapter_error_t &_error,
    const std::string &_operation)
{
  if (_actual == _expected)
    return;

  throw std::runtime_error(
      _operation + ": got " + gz_adapter_status_name(_actual) +
      ", expected " + gz_adapter_status_name(_expected) +
      ", detail: " + _error.message);
}

bool Contains(
    const gz_adapter_string_list_t &_list,
    const std::string &_value)
{
  for (size_t index = 0u; index < _list.count; ++index)
  {
    if (_list.items[index] != nullptr &&
        _value == _list.items[index])
    {
      return true;
    }
  }
  return false;
}

template <typename Predicate>
bool WaitUntil(
    Predicate &&_predicate,
    const std::chrono::milliseconds _timeout)
{
  const auto deadline = std::chrono::steady_clock::now() + _timeout;
  do
  {
    if (_predicate())
      return true;
    std::this_thread::sleep_for(10ms);
  }
  while (std::chrono::steady_clock::now() < deadline);

  return _predicate();
}

void TestInvalidConfiguration()
{
  gz_adapter_config_t config{};
  gz_adapter_config_init(&config);
  config.event_queue_capacity = 0u;

  gz_adapter_t *adapter = nullptr;
  gz_adapter_error_t error{};
  const auto status = gz_adapter_create(&config, &adapter, &error);
  RequireStatus(
      status,
      GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
      error,
      "create with zero queue capacity");
  Require(adapter == nullptr, "invalid create returned an adapter");
}

void TestTransportRoundTrip()
{
  const std::string partition =
      "nervus_adapter_test_" + std::to_string(::getpid());

  gz::transport::NodeOptions peerOptions;
  Require(
      peerOptions.SetPartition(partition),
      "unable to configure peer partition");
  gz::transport::Node peer(peerOptions);

  std::function<bool(
      const gz::msgs::StringMsg &,
      gz::msgs::StringMsg &)> echo =
      [](const gz::msgs::StringMsg &_request,
         gz::msgs::StringMsg &_response)
      {
        _response.set_data("echo:" + _request.data());
        return true;
      };
  Require(
      peer.Advertise<gz::msgs::StringMsg, gz::msgs::StringMsg>(
          "/adapter/echo", echo),
      "unable to advertise echo service");

  std::function<bool(
      const gz::msgs::StringMsg &,
      gz::msgs::StringMsg &)> reject =
      [](const gz::msgs::StringMsg &,
         gz::msgs::StringMsg &)
      {
        return false;
      };
  Require(
      peer.Advertise<gz::msgs::StringMsg, gz::msgs::StringMsg>(
          "/adapter/reject", reject),
      "unable to advertise rejecting service");

  std::mutex commandMutex;
  std::condition_variable commandReady;
  bool commandReceived = false;
  double commandValue = 0.0;
  std::function<void(const gz::msgs::Double &)> commandCallback =
      [&](const gz::msgs::Double &_message)
      {
        {
          std::lock_guard<std::mutex> lock(commandMutex);
          commandReceived = true;
          commandValue = _message.data();
        }
        commandReady.notify_all();
      };
  auto commandSubscriber =
      peer.CreateSubscriber("/adapter/command", commandCallback);
  Require(
      static_cast<bool>(commandSubscriber),
      "unable to create command subscriber");

  auto eventPublisher =
      peer.Advertise<gz::msgs::StringMsg>("/adapter/events");
  Require(
      static_cast<bool>(eventPublisher),
      "unable to advertise event topic");

  auto queuePublisher =
      peer.Advertise<gz::msgs::StringMsg>("/adapter/queue");
  Require(
      static_cast<bool>(queuePublisher),
      "unable to advertise queue test topic");

  gz_adapter_config_t config{};
  gz_adapter_config_init(&config);
  config.partition = partition.c_str();
  config.event_queue_capacity = 4u;
  config.max_message_bytes = 64u * 1024u;
  config.publisher_connect_timeout_ms = 2000u;

  gz_adapter_t *adapter = nullptr;
  gz_adapter_error_t error{};
  RequireStatus(
      gz_adapter_create(&config, &adapter, &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "create adapter");
  Require(adapter != nullptr, "adapter was not created");

  const auto *versions = gz_adapter_versions();
  Require(versions != nullptr, "versions pointer is null");
  Require(
      versions->abi_version == GZ_ADAPTER_ABI_VERSION,
      "unexpected C ABI version");
  Require(
      std::string(versions->transport_version).rfind("15.", 0u) == 0u,
      "adapter was not linked against Transport 15");
  Require(
      std::string(versions->msgs_version).rfind("12.", 0u) == 0u,
      "adapter was not linked against Msgs 12");

  RequireStatus(
      gz_adapter_wait_for_service(
          adapter,
          "/adapter/echo",
          "gz.msgs.StringMsg",
          "gz.msgs.StringMsg",
          3000u,
          &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "wait for echo service");

  RequireStatus(
      gz_adapter_wait_for_service(
          adapter,
          "/adapter/echo",
          nullptr,
          nullptr,
          3000u,
          &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "wait for echo service without expected types");

  RequireStatus(
      gz_adapter_wait_for_topic(
          adapter,
          "/adapter/events",
          "gz.msgs.StringMsg",
          3000u,
          &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "wait for event topic");

  RequireStatus(
      gz_adapter_wait_for_topic(
          adapter,
          "/adapter/missing",
          "gz.msgs.StringMsg",
          30u,
          &error),
      GZ_ADAPTER_STATUS_TIMEOUT,
      error,
      "wait for missing topic");

  RequireStatus(
      gz_adapter_wait_for_subscribers(
          adapter,
          "/adapter/command",
          "gz.msgs.Double",
          3000u,
          &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "probe command subscriber without publishing");
  RequireStatus(
      gz_adapter_wait_for_subscribers(
          adapter,
          "/adapter/command",
          "gz.msgs.StringMsg",
          100u,
          &error),
      GZ_ADAPTER_STATUS_TIMEOUT,
      error,
      "reject subscriber probe with an incompatible type");
  RequireStatus(
      gz_adapter_wait_for_subscribers(
          adapter,
          "/adapter/no-subscriber",
          "gz.msgs.Double",
          30u,
          &error),
      GZ_ADAPTER_STATUS_TIMEOUT,
      error,
      "time out subscriber probe without a controller");

  gz_adapter_topic_info_t topicInfo{};
  RequireStatus(
      gz_adapter_topic_info(
          adapter, "/adapter/events", &topicInfo, &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "event topic info");
  Require(topicInfo.publisher_count >= 1u, "topic has no publishers");
  Require(topicInfo.type_conflict == 0, "topic type conflict was reported");
  Require(
      topicInfo.message_type != nullptr &&
      std::string(topicInfo.message_type) == "gz.msgs.StringMsg",
      "unexpected topic message type");
  gz_adapter_topic_info_clear(&topicInfo);

  gz_adapter_service_info_t serviceInfo{};
  RequireStatus(
      gz_adapter_service_info(
          adapter, "/adapter/echo", &serviceInfo, &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "echo service info");
  Require(serviceInfo.provider_count >= 1u, "service has no providers");
  Require(serviceInfo.type_conflict == 0, "service type conflict was reported");
  Require(
      serviceInfo.request_type != nullptr &&
      std::string(serviceInfo.request_type) == "gz.msgs.StringMsg",
      "unexpected service request type");
  Require(
      serviceInfo.response_type != nullptr &&
      std::string(serviceInfo.response_type) == "gz.msgs.StringMsg",
      "unexpected service response type");
  gz_adapter_service_info_clear(&serviceInfo);

  gz_adapter_string_list_t topics{};
  RequireStatus(
      gz_adapter_list_topics(adapter, &topics, &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "list topics");
  Require(
      Contains(topics, "/adapter/events"),
      "event topic missing from discovery list");
  gz_adapter_string_list_clear(&topics);

  gz_adapter_string_list_t services{};
  RequireStatus(
      gz_adapter_list_services(adapter, &services, &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "list services");
  Require(
      Contains(services, "/adapter/echo"),
      "echo service missing from discovery list");
  gz_adapter_string_list_clear(&services);

  gz::msgs::StringMsg request;
  request.set_data("hello");
  const std::string requestWire = request.SerializeAsString();
  gz_adapter_buffer_t responseWire{};
  RequireStatus(
      gz_adapter_request(
          adapter,
          "/adapter/echo",
          "gz.msgs.StringMsg",
          "gz.msgs.StringMsg",
          reinterpret_cast<const uint8_t *>(requestWire.data()),
          requestWire.size(),
          3000u,
          &responseWire,
          &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "echo request");

  gz::msgs::StringMsg response;
  Require(
      response.ParseFromArray(
          responseWire.data,
          static_cast<int>(responseWire.size)),
      "unable to parse echo response");
  Require(response.data() == "echo:hello", "unexpected echo response");
  gz_adapter_buffer_clear(&responseWire);

  RequireStatus(
      gz_adapter_request(
          adapter,
          "/adapter/reject",
          "gz.msgs.StringMsg",
          "gz.msgs.StringMsg",
          reinterpret_cast<const uint8_t *>(requestWire.data()),
          requestWire.size(),
          3000u,
          &responseWire,
          &error),
      GZ_ADAPTER_STATUS_REMOTE_REJECTED,
      error,
      "rejecting request");

  RequireStatus(
      gz_adapter_request(
          adapter,
          "/adapter/echo",
          "gz.msgs.Double",
          "gz.msgs.StringMsg",
          reinterpret_cast<const uint8_t *>(requestWire.data()),
          requestWire.size(),
          3000u,
          &responseWire,
          &error),
      GZ_ADAPTER_STATUS_TYPE_MISMATCH,
      error,
      "request with wrong type");

  RequireStatus(
      gz_adapter_publish(
          adapter,
          "/adapter/no_subscriber",
          "gz.msgs.Double",
          nullptr,
          0u,
          1,
          30u,
          &error),
      GZ_ADAPTER_STATUS_NOT_CONNECTED,
      error,
      "publish without a subscriber");

  RequireStatus(
      gz_adapter_publish(
          adapter,
          "/adapter/oversized",
          "gz.msgs.Double",
          nullptr,
          config.max_message_bytes + 1u,
          0,
          0u,
          &error),
      GZ_ADAPTER_STATUS_MESSAGE_TOO_LARGE,
      error,
      "publish oversized message");

  RequireStatus(
      gz_adapter_publish(
          adapter,
          "/adapter/null_data",
          "gz.msgs.Double",
          nullptr,
          1u,
          0,
          0u,
          &error),
      GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
      error,
      "publish null data");

  gz::msgs::Double command;
  command.set_data(0.7);
  const std::string commandWire = command.SerializeAsString();
  RequireStatus(
      gz_adapter_publish(
          adapter,
          "/adapter/command",
          "gz.msgs.Double",
          reinterpret_cast<const uint8_t *>(commandWire.data()),
          commandWire.size(),
          1,
          2000u,
          &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "publish command");

  RequireStatus(
      gz_adapter_publish(
          adapter,
          "/adapter/command",
          "gz.msgs.StringMsg",
          reinterpret_cast<const uint8_t *>(requestWire.data()),
          requestWire.size(),
          1,
          100u,
          &error),
      GZ_ADAPTER_STATUS_TYPE_MISMATCH,
      error,
      "reuse cached publisher with wrong type");

  {
    std::unique_lock<std::mutex> lock(commandMutex);
    Require(
        commandReady.wait_for(lock, 3s, [&]()
        {
          return commandReceived;
        }),
        "command message was not received");
  }
  Require(
      commandValue > 0.699 && commandValue < 0.701,
      "command value did not round-trip");

  uint64_t subscriptionId = 0u;
  RequireStatus(
      gz_adapter_subscribe(
          adapter,
          "/adapter/events",
          "gz.msgs.StringMsg",
          0u,
          1,
          &subscriptionId,
          &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "subscribe to events");
  Require(subscriptionId != 0u, "subscription ID was not assigned");

  uint64_t duplicateId = 0u;
  RequireStatus(
      gz_adapter_subscribe(
          adapter,
          "/adapter/events",
          "gz.msgs.StringMsg",
          0u,
          1,
          &duplicateId,
          &error),
      GZ_ADAPTER_STATUS_ALREADY_EXISTS,
      error,
      "duplicate raw subscription");

  Require(
      WaitUntil(
          [&]()
          {
            return eventPublisher.HasConnections();
          },
          3s),
      "event publisher did not connect to adapter subscription");

  gz_adapter_event_t eventWire{};
  RequireStatus(
      gz_adapter_next_event(adapter, 0u, &eventWire, &error),
      GZ_ADAPTER_STATUS_TIMEOUT,
      error,
      "poll empty event queue");

  for (const std::string value : {"one", "two", "three"})
  {
    gz::msgs::StringMsg event;
    event.set_data(value);
    Require(eventPublisher.Publish(event), "unable to publish event");
  }

  std::this_thread::sleep_for(200ms);
  RequireStatus(
      gz_adapter_next_event(adapter, 3000u, &eventWire, &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "read coalesced event");
  Require(
      eventWire.subscription_id == subscriptionId,
      "event has an unexpected subscription ID");
  Require(
      eventWire.message_type != nullptr &&
      std::string(eventWire.message_type) == "gz.msgs.StringMsg",
      "event has an unexpected message type");

  gz::msgs::StringMsg event;
  Require(
      event.ParseFromArray(
          eventWire.data,
          static_cast<int>(eventWire.size)),
      "unable to parse event");
  Require(event.data() == "three", "latest event was not retained");
  Require(
      eventWire.dropped_before >= 2u,
      "coalesced event did not report replaced messages");
  gz_adapter_event_clear(&eventWire);

  RequireStatus(
      gz_adapter_unsubscribe(adapter, subscriptionId, &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "unsubscribe");
  RequireStatus(
      gz_adapter_unsubscribe(adapter, subscriptionId, &error),
      GZ_ADAPTER_STATUS_NOT_FOUND,
      error,
      "unsubscribe missing ID");

  uint64_t queueSubscriptionId = 0u;
  RequireStatus(
      gz_adapter_subscribe(
          adapter,
          "/adapter/queue",
          "gz.msgs.StringMsg",
          0u,
          0,
          &queueSubscriptionId,
          &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "subscribe to bounded queue test topic");
  Require(
      WaitUntil(
          [&]()
          {
            return queuePublisher.HasConnections();
          },
          3s),
      "queue publisher did not connect to adapter subscription");

  for (int index = 0; index < 10; ++index)
  {
    gz::msgs::StringMsg queuedMessage;
    queuedMessage.set_data("queued-" + std::to_string(index));
    Require(queuePublisher.Publish(queuedMessage), "unable to fill event queue");
    std::this_thread::sleep_for(2ms);
  }
  std::this_thread::sleep_for(200ms);

  for (int index = 0; index < 4; ++index)
  {
    RequireStatus(
        gz_adapter_next_event(adapter, 1000u, &eventWire, &error),
        GZ_ADAPTER_STATUS_OK,
        error,
        "drain bounded event queue");
    gz_adapter_event_clear(&eventWire);
  }

  gz::msgs::StringMsg afterOverflow;
  afterOverflow.set_data("after-overflow");
  Require(queuePublisher.Publish(afterOverflow), "unable to publish overflow marker");
  RequireStatus(
      gz_adapter_next_event(adapter, 3000u, &eventWire, &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "read overflow marker");
  Require(
      eventWire.dropped_before >= 1u,
      "bounded queue did not report dropped messages");
  gz_adapter_event_clear(&eventWire);

  gz::msgs::StringMsg oversizedIncoming;
  oversizedIncoming.set_data(std::string(70u * 1024u, 'x'));
  Require(
      queuePublisher.Publish(oversizedIncoming),
      "unable to publish oversized incoming message");
  gz::msgs::StringMsg afterOversized;
  afterOversized.set_data("after-oversized");
  Require(
      queuePublisher.Publish(afterOversized),
      "unable to publish message after oversized input");
  RequireStatus(
      gz_adapter_next_event(adapter, 3000u, &eventWire, &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "read message after oversized input");
  Require(
      eventWire.dropped_before >= 1u,
      "oversized incoming message was not counted as dropped");
  gz_adapter_event_clear(&eventWire);

  std::atomic<bool> keepPublishing{true};
  std::thread publisherThread([&]()
  {
    gz::msgs::StringMsg concurrentMessage;
    concurrentMessage.set_data("concurrent");
    while (keepPublishing.load(std::memory_order_relaxed))
    {
      queuePublisher.Publish(concurrentMessage);
      std::this_thread::sleep_for(100us);
    }
  });
  std::this_thread::sleep_for(20ms);
  const auto concurrentUnsubscribeStatus =
      gz_adapter_unsubscribe(adapter, queueSubscriptionId, &error);
  keepPublishing.store(false, std::memory_order_relaxed);
  publisherThread.join();
  RequireStatus(
      concurrentUnsubscribeStatus,
      GZ_ADAPTER_STATUS_OK,
      error,
      "unsubscribe during publication");
  RequireStatus(
      gz_adapter_next_event(adapter, 100u, &eventWire, &error),
      GZ_ADAPTER_STATUS_TIMEOUT,
      error,
      "event queue after concurrent unsubscribe");

  gz_adapter_status_t blockedWaitStatus = GZ_ADAPTER_STATUS_INTERNAL;
  gz_adapter_error_t blockedWaitError{};
  std::thread blockedWaiter([&]()
  {
    gz_adapter_event_t blockedEvent{};
    blockedWaitStatus = gz_adapter_next_event(
        adapter, 5000u, &blockedEvent, &blockedWaitError);
    gz_adapter_event_clear(&blockedEvent);
  });
  std::this_thread::sleep_for(50ms);

  const auto closeStatus = gz_adapter_close(adapter, &error);
  blockedWaiter.join();
  RequireStatus(
      closeStatus,
      GZ_ADAPTER_STATUS_OK,
      error,
      "close adapter");
  RequireStatus(
      blockedWaitStatus,
      GZ_ADAPTER_STATUS_CLOSED,
      blockedWaitError,
      "wake blocked event wait during close");
  RequireStatus(
      gz_adapter_close(adapter, &error),
      GZ_ADAPTER_STATUS_OK,
      error,
      "close adapter again");
  RequireStatus(
      gz_adapter_list_topics(adapter, &topics, &error),
      GZ_ADAPTER_STATUS_CLOSED,
      error,
      "list topics after close");

  gz_adapter_destroy(adapter);
}

}  // namespace

int main()
{
  try
  {
    TestInvalidConfiguration();
    TestTransportRoundTrip();
    std::cout << "nervus_gazebo_adapter_test: PASS" << std::endl;
    return 0;
  }
  catch (const std::exception &exception)
  {
    std::cerr
        << "nervus_gazebo_adapter_test: FAIL: "
        << exception.what()
        << std::endl;
    return 1;
  }
}

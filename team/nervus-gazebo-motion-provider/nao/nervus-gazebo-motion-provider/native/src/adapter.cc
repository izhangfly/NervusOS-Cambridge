#include "nervus/gazebo/adapter.h"

#include <gz/msgs/config.hh>
#include <gz/transport/MessageInfo.hh>
#include <gz/transport/Node.hh>
#include <gz/transport/NodeOptions.hh>
#include <gz/transport/Publisher.hh>
#include <gz/transport/SubscribeOptions.hh>
#include <gz/transport/TransportTypes.hh>
#include <gz/transport/config.hh>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdio>
#include <cstring>
#include <deque>
#include <limits>
#include <memory>
#include <mutex>
#include <new>
#include <set>
#include <stdexcept>
#include <string>
#include <thread>
#include <unordered_map>
#include <utility>
#include <vector>

namespace nervus::gazebo
{
namespace detail
{
constexpr size_t kDefaultQueueCapacity = 256u;
constexpr size_t kDefaultMaxMessageBytes = 16u * 1024u * 1024u;
constexpr uint32_t kDefaultPublisherConnectTimeoutMs = 1000u;
constexpr size_t kMaxQueueCapacity = 65536u;
constexpr size_t kMaxConfiguredMessageBytes = 1024u * 1024u * 1024u;
constexpr size_t kMaxTransportNameBytes = 4096u;
constexpr size_t kMaxMessageTypeBytes = 512u;
constexpr auto kDiscoveryPollInterval = std::chrono::milliseconds(20);

void ClearError(gz_adapter_error_t *_error) noexcept
{
  if (_error == nullptr)
    return;

  _error->code = GZ_ADAPTER_STATUS_OK;
  _error->message[0] = '\0';
}

gz_adapter_status_t Fail(
    gz_adapter_error_t *_error,
    const gz_adapter_status_t _status,
    const std::string &_message) noexcept
{
  if (_error != nullptr)
  {
    _error->code = _status;
    std::snprintf(
        _error->message,
        sizeof(_error->message),
        "%s",
        _message.c_str());
  }
  return _status;
}

template <typename Function>
gz_adapter_status_t Boundary(
    gz_adapter_error_t *_error,
    Function &&_function) noexcept
{
  ClearError(_error);
  try
  {
    const auto status = _function();
    if (_error != nullptr && status != GZ_ADAPTER_STATUS_OK &&
        _error->code == GZ_ADAPTER_STATUS_OK)
    {
      _error->code = status;
    }
    return status;
  }
  catch (const std::exception &exception)
  {
    return Fail(
        _error,
        GZ_ADAPTER_STATUS_NATIVE_EXCEPTION,
        std::string("C++ exception: ") + exception.what());
  }
  catch (...)
  {
    return Fail(
        _error,
        GZ_ADAPTER_STATUS_NATIVE_EXCEPTION,
        "Unknown C++ exception");
  }
}

bool ValidCString(
    const char *_value,
    const size_t _maxBytes,
    std::string &_output)
{
  if (_value == nullptr)
    return false;

  const size_t length = ::strnlen(_value, _maxBytes + 1u);
  if (length == 0u || length > _maxBytes)
    return false;

  _output.assign(_value, length);
  return true;
}

char *CopyCString(const std::string &_value) noexcept
{
  auto *copy = new (std::nothrow) char[_value.size() + 1u];
  if (copy == nullptr)
    return nullptr;

  std::memcpy(copy, _value.data(), _value.size());
  copy[_value.size()] = '\0';
  return copy;
}

struct QueuedEvent
{
  uint64_t subscriptionId{0u};
  uint64_t sequence{0u};
  uint64_t droppedBefore{0u};
  std::string topic;
  std::string messageType;
  std::string data;
};

struct SharedState
{
  explicit SharedState(
      const size_t _queueCapacity,
      const size_t _maxMessageBytes)
      : queueCapacity(_queueCapacity),
        maxMessageBytes(_maxMessageBytes)
  {
  }

  void OnMessage(
      const uint64_t _subscriptionId,
      const bool _coalesceLatest,
      const char *_data,
      const size_t _size,
      const gz::transport::MessageInfo &_info) noexcept
  {
    std::unique_lock<std::mutex> lock(this->mutex);
    ++this->callbacksInFlight;

    const auto finish = [this, &lock]()
    {
      --this->callbacksInFlight;
      if (this->callbacksInFlight == 0u)
        this->callbacksDone.notify_all();
      lock.unlock();
    };

    const auto active = this->subscriptionActive.find(_subscriptionId);
    if (this->closing || active == this->subscriptionActive.end() ||
        !active->second)
    {
      finish();
      return;
    }

    if (_size > this->maxMessageBytes || (_size > 0u && _data == nullptr))
    {
      this->RecordDropLocked(_subscriptionId);
      finish();
      return;
    }

    try
    {
      QueuedEvent incoming;
      incoming.subscriptionId = _subscriptionId;
      incoming.sequence = ++this->nextSequence;
      incoming.topic = _info.Topic();
      incoming.messageType = _info.Type();
      if (_size > 0u)
        incoming.data.assign(_data, _size);

      if (_coalesceLatest)
      {
        const auto existing = std::find_if(
            this->events.begin(),
            this->events.end(),
            [_subscriptionId](const QueuedEvent &_event)
            {
              return _event.subscriptionId == _subscriptionId;
            });
        if (existing != this->events.end())
        {
          incoming.droppedBefore = this->SaturatingAdd(
              this->SaturatingAdd(existing->droppedBefore, 1u),
              this->TakePendingDropsLocked(_subscriptionId));
          *existing = std::move(incoming);
          this->eventAvailable.notify_one();
          finish();
          return;
        }
      }

      if (this->events.size() >= this->queueCapacity)
      {
        this->RecordDropLocked(_subscriptionId);
        finish();
        return;
      }

      incoming.droppedBefore = this->TakePendingDropsLocked(_subscriptionId);
      this->events.push_back(std::move(incoming));
      this->eventAvailable.notify_one();
    }
    catch (...)
    {
      this->RecordDropLocked(_subscriptionId);
    }

    finish();
  }

  uint64_t TakePendingDropsLocked(const uint64_t _subscriptionId) noexcept
  {
    const auto found = this->droppedBySubscription.find(_subscriptionId);
    if (found == this->droppedBySubscription.end())
      return 0u;

    const uint64_t result = found->second;
    found->second = 0u;
    return result;
  }

  void RecordDropLocked(const uint64_t _subscriptionId) noexcept
  {
    const auto found = this->droppedBySubscription.find(_subscriptionId);
    if (found != this->droppedBySubscription.end() &&
        found->second != std::numeric_limits<uint64_t>::max())
    {
      ++found->second;
    }
  }

  static uint64_t SaturatingAdd(
      const uint64_t _left,
      const uint64_t _right) noexcept
  {
    const auto maximum = std::numeric_limits<uint64_t>::max();
    if (_right > maximum - _left)
      return maximum;
    return _left + _right;
  }

  void AddSubscription(const uint64_t _subscriptionId)
  {
    std::lock_guard<std::mutex> lock(this->mutex);
    this->droppedBySubscription.try_emplace(_subscriptionId, 0u);
    try
    {
      this->subscriptionActive.emplace(_subscriptionId, true);
    }
    catch (...)
    {
      this->droppedBySubscription.erase(_subscriptionId);
      throw;
    }
  }

  void DeactivateSubscription(const uint64_t _subscriptionId) noexcept
  {
    std::lock_guard<std::mutex> lock(this->mutex);
    const auto found = this->subscriptionActive.find(_subscriptionId);
    if (found != this->subscriptionActive.end())
      found->second = false;
  }

  void ReactivateSubscription(const uint64_t _subscriptionId) noexcept
  {
    std::lock_guard<std::mutex> lock(this->mutex);
    const auto found = this->subscriptionActive.find(_subscriptionId);
    if (found != this->subscriptionActive.end())
      found->second = true;
  }

  void RemoveSubscription(const uint64_t _subscriptionId) noexcept
  {
    std::lock_guard<std::mutex> lock(this->mutex);
    this->subscriptionActive.erase(_subscriptionId);
    this->events.erase(
        std::remove_if(
            this->events.begin(),
            this->events.end(),
            [_subscriptionId](const QueuedEvent &_event)
            {
              return _event.subscriptionId == _subscriptionId;
            }),
        this->events.end());
    this->droppedBySubscription.erase(_subscriptionId);
  }

  const size_t queueCapacity;
  const size_t maxMessageBytes;
  std::mutex mutex;
  std::condition_variable eventAvailable;
  std::condition_variable callbacksDone;
  std::deque<QueuedEvent> events;
  std::unordered_map<uint64_t, bool> subscriptionActive;
  std::unordered_map<uint64_t, uint64_t> droppedBySubscription;
  uint64_t nextSequence{0u};
  size_t callbacksInFlight{0u};
  bool closing{false};
};

struct PublisherEntry
{
  std::string messageType;
  std::shared_ptr<gz::transport::Node::Publisher> publisher;
};

struct SubscriptionEntry
{
  uint64_t id{0u};
  std::string topic;
  std::string messageType;
  bool coalesceLatest{false};
};

struct TopicSnapshot
{
  std::vector<gz::transport::MessagePublisher> publishers;
  std::vector<gz::transport::MessagePublisher> subscribers;
  std::string messageType;
  bool typeConflict{false};
};

struct ServiceSnapshot
{
  std::vector<gz::transport::ServicePublisher> providers;
  std::string requestType;
  std::string responseType;
  bool typeConflict{false};
};

class Adapter;

class OperationGuard
{
  public: explicit OperationGuard(Adapter &_adapter);

  public: ~OperationGuard();

  public: bool Active() const noexcept
  {
    return this->active;
  }

  private: Adapter &adapter;
  private: bool active{false};
};

class Adapter
{
  public: Adapter(
      const std::string &_partition,
      const std::string &_nameSpace,
      const size_t _queueCapacity,
      const size_t _maxMessageBytes,
      const uint32_t _publisherConnectTimeoutMs)
      : state(std::make_shared<SharedState>(
            _queueCapacity, _maxMessageBytes)),
        maxMessageBytes(_maxMessageBytes),
        publisherConnectTimeoutMs(_publisherConnectTimeoutMs)
  {
    gz::transport::NodeOptions options;
    if (!_partition.empty() && !options.SetPartition(_partition))
      throw std::invalid_argument("Invalid Gazebo Transport partition");
    if (!_nameSpace.empty() && !options.SetNameSpace(_nameSpace))
      throw std::invalid_argument("Invalid Gazebo Transport namespace");

    this->node = std::make_unique<gz::transport::Node>(options);
  }

  public: ~Adapter() noexcept
  {
    try
    {
      this->Close(nullptr);
    }
    catch (...)
    {
    }
  }

  public: bool BeginOperation()
  {
    std::lock_guard<std::mutex> lock(this->lifecycleMutex);
    if (this->closing || this->closed)
      return false;
    ++this->operationsInFlight;
    return true;
  }

  public: void EndOperation()
  {
    std::lock_guard<std::mutex> lock(this->lifecycleMutex);
    if (this->operationsInFlight > 0u)
      --this->operationsInFlight;
    if (this->operationsInFlight == 0u)
      this->operationsDone.notify_all();
  }

  public: bool IsClosing() const
  {
    std::lock_guard<std::mutex> lock(this->lifecycleMutex);
    return this->closing || this->closed;
  }

  public: gz_adapter_status_t Close(gz_adapter_error_t *_error)
  {
    {
      std::unique_lock<std::mutex> lock(this->lifecycleMutex);
      if (this->closed)
        return GZ_ADAPTER_STATUS_OK;
      if (this->closing)
      {
        this->operationsDone.wait(lock, [this]()
        {
          return this->closed;
        });
        return GZ_ADAPTER_STATUS_OK;
      }
      this->closing = true;
    }

    const auto completeClose = [this](Adapter *) noexcept
    {
      try
      {
        std::lock_guard<std::mutex> lock(this->lifecycleMutex);
        this->closed = true;
        this->operationsDone.notify_all();
      }
      catch (...)
      {
      }
    };
    std::unique_ptr<Adapter, decltype(completeClose)> closeCompletion(
        this, completeClose);

    {
      std::lock_guard<std::mutex> lock(this->state->mutex);
      this->state->closing = true;
      this->state->eventAvailable.notify_all();
    }

    {
      std::unique_lock<std::mutex> lock(this->lifecycleMutex);
      this->operationsDone.wait(lock, [this]()
      {
        return this->operationsInFlight == 0u;
      });
    }

    {
      std::lock_guard<std::mutex> subscriptionLock(this->subscriptionMutex);
      std::lock_guard<std::mutex> nodeLock(this->nodeMutex);
      if (this->node != nullptr)
      {
        for (const auto &item : this->subscriptionsById)
          this->node->Unsubscribe(item.second.topic);
      }
      this->subscriptionsById.clear();
      this->subscriptionIdByTopic.clear();
    }

    {
      std::unique_lock<std::mutex> lock(this->state->mutex);
      this->state->callbacksDone.wait(lock, [this]()
      {
        return this->state->callbacksInFlight == 0u;
      });
      this->state->events.clear();
      this->state->subscriptionActive.clear();
      this->state->droppedBySubscription.clear();
    }

    {
      std::lock_guard<std::mutex> lock(this->publisherMutex);
      this->publishers.clear();
    }

    {
      std::lock_guard<std::mutex> nodeLock(this->nodeMutex);
      this->node.reset();
    }

    ClearError(_error);
    return GZ_ADAPTER_STATUS_OK;
  }

  public: gz_adapter_status_t ListTopics(
      gz_adapter_string_list_t *_topics,
      gz_adapter_error_t *_error)
  {
    OperationGuard operation(*this);
    if (!operation.Active())
      return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closed");

    std::vector<std::string> topics;
    {
      std::lock_guard<std::mutex> lock(this->nodeMutex);
      this->node->TopicList(topics);
    }
    std::sort(topics.begin(), topics.end());
    topics.erase(std::unique(topics.begin(), topics.end()), topics.end());
    return this->CopyStringList(topics, _topics, _error);
  }

  public: gz_adapter_status_t ListServices(
      gz_adapter_string_list_t *_services,
      gz_adapter_error_t *_error)
  {
    OperationGuard operation(*this);
    if (!operation.Active())
      return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closed");

    std::vector<std::string> services;
    {
      std::lock_guard<std::mutex> lock(this->nodeMutex);
      this->node->ServiceList(services);
    }
    std::sort(services.begin(), services.end());
    services.erase(std::unique(services.begin(), services.end()), services.end());
    return this->CopyStringList(services, _services, _error);
  }

  public: gz_adapter_status_t TopicInfo(
      const std::string &_topic,
      gz_adapter_topic_info_t *_info,
      gz_adapter_error_t *_error)
  {
    OperationGuard operation(*this);
    if (!operation.Active())
      return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closed");

    TopicSnapshot snapshot;
    const auto status = this->ReadTopicInfo(_topic, snapshot, _error);
    if (status != GZ_ADAPTER_STATUS_OK)
      return status;

    _info->publisher_count = snapshot.publishers.size();
    _info->subscriber_count = snapshot.subscribers.size();
    _info->type_conflict = snapshot.typeConflict ? 1 : 0;
    _info->message_type = CopyCString(snapshot.messageType);
    if (_info->message_type == nullptr)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INTERNAL,
          "Unable to allocate topic info");
    }
    return GZ_ADAPTER_STATUS_OK;
  }

  public: gz_adapter_status_t ServiceInfo(
      const std::string &_service,
      gz_adapter_service_info_t *_info,
      gz_adapter_error_t *_error)
  {
    OperationGuard operation(*this);
    if (!operation.Active())
      return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closed");

    ServiceSnapshot snapshot;
    const auto status = this->ReadServiceInfo(_service, snapshot, _error);
    if (status != GZ_ADAPTER_STATUS_OK)
      return status;

    _info->provider_count = snapshot.providers.size();
    _info->type_conflict = snapshot.typeConflict ? 1 : 0;
    _info->request_type = CopyCString(snapshot.requestType);
    _info->response_type = CopyCString(snapshot.responseType);
    if (_info->request_type == nullptr || _info->response_type == nullptr)
    {
      delete[] _info->request_type;
      delete[] _info->response_type;
      _info->request_type = nullptr;
      _info->response_type = nullptr;
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INTERNAL,
          "Unable to allocate service info");
    }
    return GZ_ADAPTER_STATUS_OK;
  }

  public: gz_adapter_status_t WaitForTopic(
      const std::string &_topic,
      const std::string &_expectedType,
      const uint32_t _timeoutMs,
      gz_adapter_error_t *_error)
  {
    OperationGuard operation(*this);
    if (!operation.Active())
      return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closed");

    const auto deadline =
        std::chrono::steady_clock::now() + std::chrono::milliseconds(_timeoutMs);
    do
    {
      if (this->IsClosing())
        return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closing");

      TopicSnapshot snapshot;
      gz_adapter_error_t ignored{};
      const auto status = this->ReadTopicInfo(_topic, snapshot, &ignored);
      if (status == GZ_ADAPTER_STATUS_OK)
      {
        if (snapshot.typeConflict)
        {
          return Fail(
              _error,
              GZ_ADAPTER_STATUS_TYPE_MISMATCH,
              "Topic has conflicting message types");
        }
        if (!_expectedType.empty() && snapshot.messageType != _expectedType)
        {
          return Fail(
              _error,
              GZ_ADAPTER_STATUS_TYPE_MISMATCH,
              "Topic message type does not match the expected type");
        }
        return GZ_ADAPTER_STATUS_OK;
      }

      if (std::chrono::steady_clock::now() >= deadline)
        break;
      std::this_thread::sleep_for(kDiscoveryPollInterval);
    }
    while (true);

    return Fail(
        _error,
        GZ_ADAPTER_STATUS_TIMEOUT,
        "Timed out waiting for topic discovery");
  }

  public: gz_adapter_status_t WaitForService(
      const std::string &_service,
      const std::string &_expectedRequestType,
      const std::string &_expectedResponseType,
      const uint32_t _timeoutMs,
      gz_adapter_error_t *_error)
  {
    OperationGuard operation(*this);
    if (!operation.Active())
      return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closed");

    const auto deadline =
        std::chrono::steady_clock::now() + std::chrono::milliseconds(_timeoutMs);
    do
    {
      if (this->IsClosing())
        return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closing");

      ServiceSnapshot snapshot;
      gz_adapter_error_t ignored{};
      const auto status = this->ReadServiceInfo(_service, snapshot, &ignored);
      if (status == GZ_ADAPTER_STATUS_OK)
      {
        if (snapshot.typeConflict)
        {
          return Fail(
              _error,
              GZ_ADAPTER_STATUS_TYPE_MISMATCH,
              "Service has conflicting request or response types");
        }
        if ((!_expectedRequestType.empty() &&
             snapshot.requestType != _expectedRequestType) ||
            (!_expectedResponseType.empty() &&
             snapshot.responseType != _expectedResponseType))
        {
          return Fail(
              _error,
              GZ_ADAPTER_STATUS_TYPE_MISMATCH,
              "Service message types do not match the expected types");
        }
        return GZ_ADAPTER_STATUS_OK;
      }

      if (std::chrono::steady_clock::now() >= deadline)
        break;
      std::this_thread::sleep_for(kDiscoveryPollInterval);
    }
    while (true);

    return Fail(
        _error,
        GZ_ADAPTER_STATUS_TIMEOUT,
        "Timed out waiting for service discovery");
  }

  public: gz_adapter_status_t Publish(
      const std::string &_topic,
      const std::string &_messageType,
      const uint8_t *_data,
      const size_t _size,
      const bool _requireConnection,
      const uint32_t _connectionTimeoutMs,
      gz_adapter_error_t *_error)
  {
    OperationGuard operation(*this);
    if (!operation.Active())
      return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closed");

    if (_size > this->maxMessageBytes)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_MESSAGE_TOO_LARGE,
          "Published message exceeds the configured size limit");
    }
    if (_size > 0u && _data == nullptr)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Published data is null");
    }

    TopicSnapshot snapshot;
    gz_adapter_error_t ignored{};
    if (this->ReadTopicInfo(_topic, snapshot, &ignored) ==
        GZ_ADAPTER_STATUS_OK)
    {
      if (snapshot.typeConflict ||
          (!snapshot.messageType.empty() &&
           snapshot.messageType != _messageType))
      {
        return Fail(
            _error,
            GZ_ADAPTER_STATUS_TYPE_MISMATCH,
            "Published message type conflicts with the discovered topic type");
      }
    }

    std::shared_ptr<gz::transport::Node::Publisher> publisher;
    {
      std::lock_guard<std::mutex> lock(this->publisherMutex);
      const auto found = this->publishers.find(_topic);
      if (found != this->publishers.end())
      {
        if (found->second.messageType != _messageType)
        {
          return Fail(
              _error,
              GZ_ADAPTER_STATUS_TYPE_MISMATCH,
              "Publisher is already cached with another message type");
        }
        publisher = found->second.publisher;
      }
      else
      {
        gz::transport::Node::Publisher advertised;
        {
          std::lock_guard<std::mutex> nodeLock(this->nodeMutex);
          advertised = this->node->Advertise(_topic, _messageType);
        }
        if (!advertised)
        {
          return Fail(
              _error,
              GZ_ADAPTER_STATUS_INTERNAL,
              "Gazebo Transport rejected topic advertisement");
        }
        publisher =
            std::make_shared<gz::transport::Node::Publisher>(
                std::move(advertised));
        this->publishers.emplace(
            _topic, PublisherEntry{_messageType, publisher});
      }
    }

    if (_requireConnection)
    {
      const uint32_t requested =
          _connectionTimeoutMs == 0u ?
          this->publisherConnectTimeoutMs : _connectionTimeoutMs;
      const auto deadline =
          std::chrono::steady_clock::now() +
          std::chrono::milliseconds(requested);
      while (!publisher->HasConnections())
      {
        if (this->IsClosing())
          return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closing");
        if (std::chrono::steady_clock::now() >= deadline)
        {
          return Fail(
              _error,
              GZ_ADAPTER_STATUS_NOT_CONNECTED,
              "Topic publisher has no connected subscribers");
        }
        std::this_thread::sleep_for(kDiscoveryPollInterval);
      }
    }

    std::string data;
    if (_size > 0u)
    {
      data.assign(
          reinterpret_cast<const char *>(_data),
          _size);
    }
    if (!publisher->PublishRaw(data, _messageType))
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INTERNAL,
          "Gazebo Transport failed to publish the serialized message");
    }
    return GZ_ADAPTER_STATUS_OK;
  }

  public: gz_adapter_status_t Request(
      const std::string &_service,
      const std::string &_requestType,
      const std::string &_responseType,
      const uint8_t *_requestData,
      const size_t _requestSize,
      const uint32_t _timeoutMs,
      gz_adapter_buffer_t *_response,
      gz_adapter_error_t *_error)
  {
    OperationGuard operation(*this);
    if (!operation.Active())
      return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closed");

    if (_timeoutMs == 0u)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Service request timeout must be greater than zero");
    }
    if (_requestSize > this->maxMessageBytes)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_MESSAGE_TOO_LARGE,
          "Service request exceeds the configured size limit");
    }
    if (_requestSize > 0u && _requestData == nullptr)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Service request data is null");
    }

    ServiceSnapshot snapshot;
    const auto infoStatus =
        this->ReadServiceInfo(_service, snapshot, _error);
    if (infoStatus != GZ_ADAPTER_STATUS_OK)
      return infoStatus;
    if (snapshot.typeConflict ||
        snapshot.requestType != _requestType ||
        snapshot.responseType != _responseType)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_TYPE_MISMATCH,
          "Service message types do not match the requested types");
    }

    std::string requestData;
    if (_requestSize > 0u)
    {
      requestData.assign(
          reinterpret_cast<const char *>(_requestData),
          _requestSize);
    }

    std::string responseData;
    bool remoteResult = false;
    bool executed = false;
    {
      std::lock_guard<std::mutex> requestLock(this->requestMutex);
      if (this->IsClosing())
        return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closing");

      executed = this->node->RequestRaw(
          _service,
          requestData,
          _requestType,
          _responseType,
          _timeoutMs,
          responseData,
          remoteResult);
    }

    if (!executed)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_TIMEOUT,
          "Gazebo Transport service request timed out");
    }
    if (!remoteResult)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_REMOTE_REJECTED,
          "Gazebo service rejected the request");
    }
    if (responseData.size() > this->maxMessageBytes)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_MESSAGE_TOO_LARGE,
          "Service response exceeds the configured size limit");
    }

    if (!responseData.empty())
    {
      _response->data =
          new (std::nothrow) uint8_t[responseData.size()];
      if (_response->data == nullptr)
      {
        return Fail(
            _error,
            GZ_ADAPTER_STATUS_INTERNAL,
            "Unable to allocate service response");
      }
      std::memcpy(
          _response->data,
          responseData.data(),
          responseData.size());
      _response->size = responseData.size();
    }
    return GZ_ADAPTER_STATUS_OK;
  }

  public: gz_adapter_status_t Subscribe(
      const std::string &_topic,
      const std::string &_messageType,
      const uint64_t _maxMessagesPerSecond,
      const bool _coalesceLatest,
      uint64_t *_subscriptionId,
      gz_adapter_error_t *_error)
  {
    OperationGuard operation(*this);
    if (!operation.Active())
      return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closed");

    TopicSnapshot snapshot;
    gz_adapter_error_t ignored{};
    if (this->ReadTopicInfo(_topic, snapshot, &ignored) ==
        GZ_ADAPTER_STATUS_OK)
    {
      if (snapshot.typeConflict ||
          (!snapshot.messageType.empty() &&
           snapshot.messageType != _messageType))
      {
        return Fail(
            _error,
            GZ_ADAPTER_STATUS_TYPE_MISMATCH,
            "Subscription type conflicts with the discovered topic type");
      }
    }

    std::lock_guard<std::mutex> subscriptionLock(this->subscriptionMutex);
    if (this->subscriptionIdByTopic.find(_topic) !=
        this->subscriptionIdByTopic.end())
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_ALREADY_EXISTS,
          "The adapter already owns a raw subscription for this topic");
    }

    const uint64_t id = this->nextSubscriptionId++;
    const auto stateCopy = this->state;
    stateCopy->AddSubscription(id);
    gz::transport::SubscribeOptions options;
    if (_maxMessagesPerSecond > 0u)
      options.SetMsgsPerSec(_maxMessagesPerSecond);

    bool subscribed = false;
    {
      std::lock_guard<std::mutex> nodeLock(this->nodeMutex);
      subscribed = this->node->SubscribeRaw(
          _topic,
          [stateCopy, id, _coalesceLatest](
              const char *_data,
              const size_t _size,
              const gz::transport::MessageInfo &_info)
          {
            stateCopy->OnMessage(
                id, _coalesceLatest, _data, _size, _info);
          },
          _messageType,
          options);
    }
    if (!subscribed)
    {
      stateCopy->RemoveSubscription(id);
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INTERNAL,
          "Gazebo Transport rejected the raw subscription");
    }

    try
    {
      this->subscriptionsById.emplace(
          id,
          SubscriptionEntry{id, _topic, _messageType, _coalesceLatest});
      this->subscriptionIdByTopic.emplace(_topic, id);
    }
    catch (...)
    {
      stateCopy->DeactivateSubscription(id);
      {
        std::lock_guard<std::mutex> nodeLock(this->nodeMutex);
        this->node->Unsubscribe(_topic);
      }
      this->subscriptionsById.erase(id);
      this->subscriptionIdByTopic.erase(_topic);
      stateCopy->RemoveSubscription(id);
      throw;
    }
    *_subscriptionId = id;
    return GZ_ADAPTER_STATUS_OK;
  }

  public: gz_adapter_status_t Unsubscribe(
      const uint64_t _subscriptionId,
      gz_adapter_error_t *_error)
  {
    OperationGuard operation(*this);
    if (!operation.Active())
      return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closed");

    std::lock_guard<std::mutex> subscriptionLock(this->subscriptionMutex);
    const auto found = this->subscriptionsById.find(_subscriptionId);
    if (found == this->subscriptionsById.end())
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_NOT_FOUND,
          "Subscription ID was not found");
    }
    const std::string topic = found->second.topic;

    this->state->DeactivateSubscription(_subscriptionId);
    bool unsubscribed = false;
    {
      std::lock_guard<std::mutex> nodeLock(this->nodeMutex);
      unsubscribed = this->node->Unsubscribe(topic);
    }
    if (!unsubscribed)
    {
      this->state->ReactivateSubscription(_subscriptionId);
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INTERNAL,
          "Gazebo Transport failed to remove the raw subscription");
    }

    this->subscriptionsById.erase(_subscriptionId);
    this->subscriptionIdByTopic.erase(topic);
    this->state->RemoveSubscription(_subscriptionId);
    return GZ_ADAPTER_STATUS_OK;
  }

  public: gz_adapter_status_t NextEvent(
      const uint32_t _timeoutMs,
      gz_adapter_event_t *_event,
      gz_adapter_error_t *_error)
  {
    OperationGuard operation(*this);
    if (!operation.Active())
      return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closed");

    QueuedEvent queued;
    {
      std::unique_lock<std::mutex> lock(this->state->mutex);
      const auto ready = [this]()
      {
        return this->state->closing || !this->state->events.empty();
      };

      if (_timeoutMs == 0u)
      {
        if (!ready())
        {
          return Fail(
              _error,
              GZ_ADAPTER_STATUS_TIMEOUT,
              "No event is available");
        }
      }
      else if (!this->state->eventAvailable.wait_for(
                   lock,
                   std::chrono::milliseconds(_timeoutMs),
                   ready))
      {
        return Fail(
            _error,
            GZ_ADAPTER_STATUS_TIMEOUT,
            "Timed out waiting for an event");
      }

      if (this->state->closing)
        return Fail(_error, GZ_ADAPTER_STATUS_CLOSED, "Adapter is closing");
      if (this->state->events.empty())
      {
        return Fail(
            _error,
            GZ_ADAPTER_STATUS_TIMEOUT,
            "No event is available");
      }

      queued = std::move(this->state->events.front());
      this->state->events.pop_front();
    }

    _event->topic = CopyCString(queued.topic);
    _event->message_type = CopyCString(queued.messageType);
    if (_event->topic == nullptr || _event->message_type == nullptr)
    {
      delete[] _event->topic;
      delete[] _event->message_type;
      _event->topic = nullptr;
      _event->message_type = nullptr;
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INTERNAL,
          "Unable to allocate event metadata");
    }

    if (!queued.data.empty())
    {
      _event->data =
          new (std::nothrow) uint8_t[queued.data.size()];
      if (_event->data == nullptr)
      {
        delete[] _event->topic;
        delete[] _event->message_type;
        _event->topic = nullptr;
        _event->message_type = nullptr;
        return Fail(
            _error,
            GZ_ADAPTER_STATUS_INTERNAL,
            "Unable to allocate event data");
      }
      std::memcpy(
          _event->data,
          queued.data.data(),
          queued.data.size());
      _event->size = queued.data.size();
    }

    _event->subscription_id = queued.subscriptionId;
    _event->sequence = queued.sequence;
    _event->dropped_before = queued.droppedBefore;
    return GZ_ADAPTER_STATUS_OK;
  }

  private: gz_adapter_status_t ReadTopicInfo(
      const std::string &_topic,
      TopicSnapshot &_snapshot,
      gz_adapter_error_t *_error)
  {
    bool found = false;
    {
      std::lock_guard<std::mutex> lock(this->nodeMutex);
      found = this->node->TopicInfo(
          _topic, _snapshot.publishers, _snapshot.subscribers);
    }
    if (!found ||
        (_snapshot.publishers.empty() && _snapshot.subscribers.empty()))
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_NOT_FOUND,
          "Topic was not discovered");
    }

    std::set<std::string> types;
    for (const auto &publisher : _snapshot.publishers)
    {
      if (!publisher.MsgTypeName().empty())
        types.insert(publisher.MsgTypeName());
    }
    for (const auto &subscriber : _snapshot.subscribers)
    {
      if (!subscriber.MsgTypeName().empty() &&
          subscriber.MsgTypeName() != gz::transport::kGenericMessageType)
      {
        types.insert(subscriber.MsgTypeName());
      }
    }

    _snapshot.typeConflict = types.size() > 1u;
    if (types.size() == 1u)
      _snapshot.messageType = *types.begin();
    return GZ_ADAPTER_STATUS_OK;
  }

  private: gz_adapter_status_t ReadServiceInfo(
      const std::string &_service,
      ServiceSnapshot &_snapshot,
      gz_adapter_error_t *_error)
  {
    bool found = false;
    {
      std::lock_guard<std::mutex> lock(this->nodeMutex);
      found = this->node->ServiceInfo(_service, _snapshot.providers);
    }
    if (!found || _snapshot.providers.empty())
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_NOT_FOUND,
          "Service was not discovered");
    }

    std::set<std::pair<std::string, std::string>> types;
    for (const auto &provider : _snapshot.providers)
    {
      types.emplace(
          provider.ReqTypeName(),
          provider.RepTypeName());
    }

    _snapshot.typeConflict = types.size() > 1u;
    if (types.size() == 1u)
    {
      _snapshot.requestType = types.begin()->first;
      _snapshot.responseType = types.begin()->second;
    }
    return GZ_ADAPTER_STATUS_OK;
  }

  private: gz_adapter_status_t CopyStringList(
      const std::vector<std::string> &_source,
      gz_adapter_string_list_t *_destination,
      gz_adapter_error_t *_error)
  {
    if (_source.empty())
      return GZ_ADAPTER_STATUS_OK;

    auto **items = new (std::nothrow) char *[_source.size()]();
    if (items == nullptr)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INTERNAL,
          "Unable to allocate string list");
    }

    for (size_t index = 0u; index < _source.size(); ++index)
    {
      items[index] = CopyCString(_source[index]);
      if (items[index] == nullptr)
      {
        for (size_t cleanup = 0u; cleanup < index; ++cleanup)
          delete[] items[cleanup];
        delete[] items;
        return Fail(
            _error,
            GZ_ADAPTER_STATUS_INTERNAL,
            "Unable to allocate string list item");
      }
    }

    _destination->items = items;
    _destination->count = _source.size();
    return GZ_ADAPTER_STATUS_OK;
  }

  private: std::shared_ptr<SharedState> state;
  private: const size_t maxMessageBytes;
  private: const uint32_t publisherConnectTimeoutMs;

  private: mutable std::mutex lifecycleMutex;
  private: std::condition_variable operationsDone;
  private: size_t operationsInFlight{0u};
  private: bool closing{false};
  private: bool closed{false};

  private: std::mutex nodeMutex;
  private: std::unique_ptr<gz::transport::Node> node;

  private: std::mutex publisherMutex;
  private: std::unordered_map<std::string, PublisherEntry> publishers;

  private: std::mutex subscriptionMutex;
  private: uint64_t nextSubscriptionId{1u};
  private: std::unordered_map<uint64_t, SubscriptionEntry> subscriptionsById;
  private: std::unordered_map<std::string, uint64_t> subscriptionIdByTopic;

  private: std::mutex requestMutex;

  friend class OperationGuard;
};

OperationGuard::OperationGuard(Adapter &_adapter)
    : adapter(_adapter),
      active(_adapter.BeginOperation())
{
}

OperationGuard::~OperationGuard()
{
  if (this->active)
    this->adapter.EndOperation();
}

}  // namespace detail
}  // namespace nervus::gazebo

struct gz_adapter
{
  std::unique_ptr<nervus::gazebo::detail::Adapter> impl;
};

namespace
{
using nervus::gazebo::detail::Adapter;
using nervus::gazebo::detail::Boundary;
using nervus::gazebo::detail::ClearError;
using nervus::gazebo::detail::Fail;
using nervus::gazebo::detail::ValidCString;

gz_adapter_status_t ValidateAdapter(
    gz_adapter_t *_adapter,
    Adapter *&_impl,
    gz_adapter_error_t *_error)
{
  if (_adapter == nullptr || _adapter->impl == nullptr)
  {
    return Fail(
        _error,
        GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
        "Adapter handle is null");
  }
  _impl = _adapter->impl.get();
  return GZ_ADAPTER_STATUS_OK;
}

bool ReadTransportName(
    const char *_value,
    std::string &_output)
{
  return ValidCString(_value, 4096u, _output);
}

bool ReadMessageType(
    const char *_value,
    std::string &_output)
{
  return ValidCString(_value, 512u, _output);
}

}  // namespace

extern "C"
{

const gz_adapter_versions_t *gz_adapter_versions(void)
{
  static const gz_adapter_versions_t versions{
      GZ_ADAPTER_ABI_VERSION,
      GZ_TRANSPORT_VERSION_FULL,
      GZ_MSGS_VERSION_FULL};
  return &versions;
}

const char *gz_adapter_status_name(const gz_adapter_status_t _status)
{
  switch (_status)
  {
    case GZ_ADAPTER_STATUS_OK:
      return "OK";
    case GZ_ADAPTER_STATUS_INVALID_ARGUMENT:
      return "INVALID_ARGUMENT";
    case GZ_ADAPTER_STATUS_CLOSED:
      return "CLOSED";
    case GZ_ADAPTER_STATUS_NOT_FOUND:
      return "NOT_FOUND";
    case GZ_ADAPTER_STATUS_NOT_CONNECTED:
      return "NOT_CONNECTED";
    case GZ_ADAPTER_STATUS_TYPE_MISMATCH:
      return "TYPE_MISMATCH";
    case GZ_ADAPTER_STATUS_TIMEOUT:
      return "TIMEOUT";
    case GZ_ADAPTER_STATUS_REMOTE_REJECTED:
      return "REMOTE_REJECTED";
    case GZ_ADAPTER_STATUS_QUEUE_FULL:
      return "QUEUE_FULL";
    case GZ_ADAPTER_STATUS_ALREADY_EXISTS:
      return "ALREADY_EXISTS";
    case GZ_ADAPTER_STATUS_NATIVE_EXCEPTION:
      return "NATIVE_EXCEPTION";
    case GZ_ADAPTER_STATUS_INTERNAL:
      return "INTERNAL";
    case GZ_ADAPTER_STATUS_MESSAGE_TOO_LARGE:
      return "MESSAGE_TOO_LARGE";
  }
  return "UNKNOWN";
}

void gz_adapter_config_init(gz_adapter_config_t *_config)
{
  if (_config == nullptr)
    return;

  std::memset(_config, 0, sizeof(*_config));
  _config->struct_size = sizeof(*_config);
  _config->event_queue_capacity = 256u;
  _config->max_message_bytes = 16u * 1024u * 1024u;
  _config->publisher_connect_timeout_ms = 1000u;
}

void gz_adapter_error_clear(gz_adapter_error_t *_error)
{
  ClearError(_error);
}

gz_adapter_status_t gz_adapter_create(
    const gz_adapter_config_t *_config,
    gz_adapter_t **_adapter,
    gz_adapter_error_t *_error)
{
  return Boundary(_error, [&]() -> gz_adapter_status_t
  {
    if (_adapter == nullptr)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Adapter output pointer is null");
    }
    *_adapter = nullptr;

    gz_adapter_config_t config{};
    gz_adapter_config_init(&config);
    if (_config != nullptr)
    {
      if (_config->struct_size < sizeof(gz_adapter_config_t))
      {
        return Fail(
            _error,
            GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
            "Adapter config struct is smaller than ABI version 1");
      }
      config = *_config;
    }

    if (config.event_queue_capacity == 0u ||
        config.event_queue_capacity > 65536u)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Event queue capacity is outside the supported range");
    }
    if (config.max_message_bytes == 0u ||
        config.max_message_bytes > 1024u * 1024u * 1024u)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Maximum message size is outside the supported range");
    }

    std::string partition;
    if (config.partition != nullptr)
    {
      if (!ValidCString(config.partition, 4096u, partition))
      {
        return Fail(
            _error,
            GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
            "Gazebo Transport partition is invalid");
      }
    }

    std::string nameSpace;
    if (config.name_space != nullptr)
    {
      if (!ValidCString(config.name_space, 4096u, nameSpace))
      {
        return Fail(
            _error,
            GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
            "Gazebo Transport namespace is invalid");
      }
    }

    auto handle = std::make_unique<gz_adapter>();
    handle->impl = std::make_unique<Adapter>(
        partition,
        nameSpace,
        config.event_queue_capacity,
        config.max_message_bytes,
        config.publisher_connect_timeout_ms);
    *_adapter = handle.release();
    return GZ_ADAPTER_STATUS_OK;
  });
}

gz_adapter_status_t gz_adapter_close(
    gz_adapter_t *_adapter,
    gz_adapter_error_t *_error)
{
  return Boundary(_error, [&]() -> gz_adapter_status_t
  {
    Adapter *impl = nullptr;
    const auto status = ValidateAdapter(_adapter, impl, _error);
    if (status != GZ_ADAPTER_STATUS_OK)
      return status;
    return impl->Close(_error);
  });
}

void gz_adapter_destroy(gz_adapter_t *_adapter)
{
  if (_adapter == nullptr)
    return;

  try
  {
    if (_adapter->impl != nullptr)
      _adapter->impl->Close(nullptr);
    delete _adapter;
  }
  catch (...)
  {
    delete _adapter;
  }
}

gz_adapter_status_t gz_adapter_list_topics(
    gz_adapter_t *_adapter,
    gz_adapter_string_list_t *_topics,
    gz_adapter_error_t *_error)
{
  return Boundary(_error, [&]() -> gz_adapter_status_t
  {
    if (_topics == nullptr)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Topic list output is null");
    }
    _topics->items = nullptr;
    _topics->count = 0u;

    Adapter *impl = nullptr;
    const auto status = ValidateAdapter(_adapter, impl, _error);
    if (status != GZ_ADAPTER_STATUS_OK)
      return status;
    return impl->ListTopics(_topics, _error);
  });
}

gz_adapter_status_t gz_adapter_list_services(
    gz_adapter_t *_adapter,
    gz_adapter_string_list_t *_services,
    gz_adapter_error_t *_error)
{
  return Boundary(_error, [&]() -> gz_adapter_status_t
  {
    if (_services == nullptr)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Service list output is null");
    }
    _services->items = nullptr;
    _services->count = 0u;

    Adapter *impl = nullptr;
    const auto status = ValidateAdapter(_adapter, impl, _error);
    if (status != GZ_ADAPTER_STATUS_OK)
      return status;
    return impl->ListServices(_services, _error);
  });
}

void gz_adapter_string_list_clear(gz_adapter_string_list_t *_list)
{
  if (_list == nullptr)
    return;

  for (size_t index = 0u; index < _list->count; ++index)
    delete[] _list->items[index];
  delete[] _list->items;
  _list->items = nullptr;
  _list->count = 0u;
}

gz_adapter_status_t gz_adapter_topic_info(
    gz_adapter_t *_adapter,
    const char *_topic,
    gz_adapter_topic_info_t *_info,
    gz_adapter_error_t *_error)
{
  return Boundary(_error, [&]() -> gz_adapter_status_t
  {
    if (_info == nullptr)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Topic info output is null");
    }
    std::memset(_info, 0, sizeof(*_info));

    std::string topic;
    if (!ReadTransportName(_topic, topic))
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Topic name is invalid");
    }

    Adapter *impl = nullptr;
    const auto status = ValidateAdapter(_adapter, impl, _error);
    if (status != GZ_ADAPTER_STATUS_OK)
      return status;
    return impl->TopicInfo(topic, _info, _error);
  });
}

void gz_adapter_topic_info_clear(gz_adapter_topic_info_t *_info)
{
  if (_info == nullptr)
    return;

  delete[] _info->message_type;
  std::memset(_info, 0, sizeof(*_info));
}

gz_adapter_status_t gz_adapter_service_info(
    gz_adapter_t *_adapter,
    const char *_service,
    gz_adapter_service_info_t *_info,
    gz_adapter_error_t *_error)
{
  return Boundary(_error, [&]() -> gz_adapter_status_t
  {
    if (_info == nullptr)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Service info output is null");
    }
    std::memset(_info, 0, sizeof(*_info));

    std::string service;
    if (!ReadTransportName(_service, service))
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Service name is invalid");
    }

    Adapter *impl = nullptr;
    const auto status = ValidateAdapter(_adapter, impl, _error);
    if (status != GZ_ADAPTER_STATUS_OK)
      return status;
    return impl->ServiceInfo(service, _info, _error);
  });
}

void gz_adapter_service_info_clear(gz_adapter_service_info_t *_info)
{
  if (_info == nullptr)
    return;

  delete[] _info->request_type;
  delete[] _info->response_type;
  std::memset(_info, 0, sizeof(*_info));
}

gz_adapter_status_t gz_adapter_wait_for_topic(
    gz_adapter_t *_adapter,
    const char *_topic,
    const char *_expectedMessageType,
    const uint32_t _timeoutMs,
    gz_adapter_error_t *_error)
{
  return Boundary(_error, [&]() -> gz_adapter_status_t
  {
    std::string topic;
    if (!ReadTransportName(_topic, topic))
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Topic name is invalid");
    }

    std::string messageType;
    if (_expectedMessageType != nullptr &&
        !ReadMessageType(_expectedMessageType, messageType))
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Expected topic message type is invalid");
    }

    Adapter *impl = nullptr;
    const auto status = ValidateAdapter(_adapter, impl, _error);
    if (status != GZ_ADAPTER_STATUS_OK)
      return status;
    return impl->WaitForTopic(
        topic, messageType, _timeoutMs, _error);
  });
}

gz_adapter_status_t gz_adapter_wait_for_service(
    gz_adapter_t *_adapter,
    const char *_service,
    const char *_expectedRequestType,
    const char *_expectedResponseType,
    const uint32_t _timeoutMs,
    gz_adapter_error_t *_error)
{
  return Boundary(_error, [&]() -> gz_adapter_status_t
  {
    std::string service;
    std::string requestType;
    std::string responseType;
    if (!ReadTransportName(_service, service) ||
        (_expectedRequestType != nullptr &&
         !ReadMessageType(_expectedRequestType, requestType)) ||
        (_expectedResponseType != nullptr &&
         !ReadMessageType(_expectedResponseType, responseType)))
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Service name or expected message types are invalid");
    }

    Adapter *impl = nullptr;
    const auto status = ValidateAdapter(_adapter, impl, _error);
    if (status != GZ_ADAPTER_STATUS_OK)
      return status;
    return impl->WaitForService(
        service,
        requestType,
        responseType,
        _timeoutMs,
        _error);
  });
}

gz_adapter_status_t gz_adapter_publish(
    gz_adapter_t *_adapter,
    const char *_topic,
    const char *_messageType,
    const uint8_t *_data,
    const size_t _size,
    const int _requireConnection,
    const uint32_t _connectionTimeoutMs,
    gz_adapter_error_t *_error)
{
  return Boundary(_error, [&]() -> gz_adapter_status_t
  {
    std::string topic;
    std::string messageType;
    if (!ReadTransportName(_topic, topic) ||
        !ReadMessageType(_messageType, messageType))
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Topic name or message type is invalid");
    }

    Adapter *impl = nullptr;
    const auto status = ValidateAdapter(_adapter, impl, _error);
    if (status != GZ_ADAPTER_STATUS_OK)
      return status;
    return impl->Publish(
        topic,
        messageType,
        _data,
        _size,
        _requireConnection != 0,
        _connectionTimeoutMs,
        _error);
  });
}

gz_adapter_status_t gz_adapter_request(
    gz_adapter_t *_adapter,
    const char *_service,
    const char *_requestType,
    const char *_responseType,
    const uint8_t *_requestData,
    const size_t _requestSize,
    const uint32_t _timeoutMs,
    gz_adapter_buffer_t *_response,
    gz_adapter_error_t *_error)
{
  return Boundary(_error, [&]() -> gz_adapter_status_t
  {
    if (_response == nullptr)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Service response output is null");
    }
    _response->data = nullptr;
    _response->size = 0u;

    std::string service;
    std::string requestType;
    std::string responseType;
    if (!ReadTransportName(_service, service) ||
        !ReadMessageType(_requestType, requestType) ||
        !ReadMessageType(_responseType, responseType))
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Service name or message types are invalid");
    }

    Adapter *impl = nullptr;
    const auto status = ValidateAdapter(_adapter, impl, _error);
    if (status != GZ_ADAPTER_STATUS_OK)
      return status;
    return impl->Request(
        service,
        requestType,
        responseType,
        _requestData,
        _requestSize,
        _timeoutMs,
        _response,
        _error);
  });
}

void gz_adapter_buffer_clear(gz_adapter_buffer_t *_buffer)
{
  if (_buffer == nullptr)
    return;

  delete[] _buffer->data;
  _buffer->data = nullptr;
  _buffer->size = 0u;
}

gz_adapter_status_t gz_adapter_subscribe(
    gz_adapter_t *_adapter,
    const char *_topic,
    const char *_messageType,
    const uint64_t _maxMessagesPerSecond,
    const int _coalesceLatest,
    uint64_t *_subscriptionId,
    gz_adapter_error_t *_error)
{
  return Boundary(_error, [&]() -> gz_adapter_status_t
  {
    if (_subscriptionId == nullptr)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Subscription ID output is null");
    }
    *_subscriptionId = 0u;

    std::string topic;
    std::string messageType;
    if (!ReadTransportName(_topic, topic) ||
        !ReadMessageType(_messageType, messageType))
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Topic name or message type is invalid");
    }

    Adapter *impl = nullptr;
    const auto status = ValidateAdapter(_adapter, impl, _error);
    if (status != GZ_ADAPTER_STATUS_OK)
      return status;
    return impl->Subscribe(
        topic,
        messageType,
        _maxMessagesPerSecond,
        _coalesceLatest != 0,
        _subscriptionId,
        _error);
  });
}

gz_adapter_status_t gz_adapter_unsubscribe(
    gz_adapter_t *_adapter,
    const uint64_t _subscriptionId,
    gz_adapter_error_t *_error)
{
  return Boundary(_error, [&]() -> gz_adapter_status_t
  {
    if (_subscriptionId == 0u)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Subscription ID zero is reserved");
    }

    Adapter *impl = nullptr;
    const auto status = ValidateAdapter(_adapter, impl, _error);
    if (status != GZ_ADAPTER_STATUS_OK)
      return status;
    return impl->Unsubscribe(_subscriptionId, _error);
  });
}

gz_adapter_status_t gz_adapter_next_event(
    gz_adapter_t *_adapter,
    const uint32_t _timeoutMs,
    gz_adapter_event_t *_event,
    gz_adapter_error_t *_error)
{
  return Boundary(_error, [&]() -> gz_adapter_status_t
  {
    if (_event == nullptr)
    {
      return Fail(
          _error,
          GZ_ADAPTER_STATUS_INVALID_ARGUMENT,
          "Event output is null");
    }
    std::memset(_event, 0, sizeof(*_event));

    Adapter *impl = nullptr;
    const auto status = ValidateAdapter(_adapter, impl, _error);
    if (status != GZ_ADAPTER_STATUS_OK)
      return status;
    return impl->NextEvent(_timeoutMs, _event, _error);
  });
}

void gz_adapter_event_clear(gz_adapter_event_t *_event)
{
  if (_event == nullptr)
    return;

  delete[] _event->topic;
  delete[] _event->message_type;
  delete[] _event->data;
  std::memset(_event, 0, sizeof(*_event));
}

}  // extern "C"

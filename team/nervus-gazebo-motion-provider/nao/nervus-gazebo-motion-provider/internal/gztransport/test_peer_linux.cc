//go:build linux && cgo && gztransporttest

#include <gz/msgs/double.pb.h>
#include <gz/msgs/stringmsg.pb.h>
#include <gz/transport/Node.hh>
#include <gz/transport/NodeOptions.hh>

#include <functional>
#include <memory>
#include <mutex>
#include <stdexcept>
#include <string>

namespace nervus::gazebo::test
{
class TransportPeer
{
  public: explicit TransportPeer(const std::string &_partition)
  {
    gz::transport::NodeOptions options;
    if (!options.SetPartition(_partition))
      throw std::invalid_argument("invalid test partition");
    this->node = std::make_unique<gz::transport::Node>(options);

    std::function<bool(
        const gz::msgs::StringMsg &,
        gz::msgs::StringMsg &)> echo =
        [](const gz::msgs::StringMsg &_request,
           gz::msgs::StringMsg &_response)
        {
          _response.set_data("echo:" + _request.data());
          return true;
        };
    if (!this->node->Advertise<
            gz::msgs::StringMsg,
            gz::msgs::StringMsg>("/go/echo", echo))
    {
      throw std::runtime_error("unable to advertise test service");
    }

    std::function<void(const gz::msgs::Double &)> command =
        [this](const gz::msgs::Double &_message)
        {
          std::lock_guard<std::mutex> lock(this->commandMutex);
          this->commandValue = _message.data();
          this->commandReceived = true;
        };
    this->commandSubscriber =
        this->node->CreateSubscriber("/go/command", command);
    if (!this->commandSubscriber)
      throw std::runtime_error("unable to subscribe to test command");

    this->eventPublisher =
        this->node->Advertise<gz::msgs::StringMsg>("/go/event");
    if (!this->eventPublisher)
      throw std::runtime_error("unable to advertise test event");
  }

  public: bool PublishEvent(const std::string &_value)
  {
    gz::msgs::StringMsg message;
    message.set_data(_value);
    return this->eventPublisher.Publish(message);
  }

  public: bool EventHasConnections() const
  {
    return this->eventPublisher.HasConnections();
  }

  public: bool Command(double &_value)
  {
    std::lock_guard<std::mutex> lock(this->commandMutex);
    if (!this->commandReceived)
      return false;
    _value = this->commandValue;
    return true;
  }

  private: std::unique_ptr<gz::transport::Node> node;
  private: gz::transport::Node::Publisher eventPublisher;
  private: gz::transport::Node::Subscriber commandSubscriber;
  private: std::mutex commandMutex;
  private: bool commandReceived{false};
  private: double commandValue{0.0};
};
}  // namespace nervus::gazebo::test

struct gz_test_peer
{
  std::unique_ptr<nervus::gazebo::test::TransportPeer> impl;
};

extern "C"
{
int gz_test_peer_create(const char *_partition, gz_test_peer **_peer)
{
  if (_partition == nullptr || _peer == nullptr)
    return 0;
  *_peer = nullptr;
  try
  {
    auto peer = std::make_unique<gz_test_peer>();
    peer->impl =
        std::make_unique<nervus::gazebo::test::TransportPeer>(_partition);
    *_peer = peer.release();
    return 1;
  }
  catch (...)
  {
    return 0;
  }
}

void gz_test_peer_destroy(gz_test_peer *_peer)
{
  try
  {
    delete _peer;
  }
  catch (...)
  {
  }
}

int gz_test_peer_publish_event(gz_test_peer *_peer, const char *_value)
{
  if (_peer == nullptr || _peer->impl == nullptr || _value == nullptr)
    return 0;
  try
  {
    return _peer->impl->PublishEvent(_value) ? 1 : 0;
  }
  catch (...)
  {
    return 0;
  }
}

int gz_test_peer_event_has_connections(gz_test_peer *_peer)
{
  if (_peer == nullptr || _peer->impl == nullptr)
    return 0;
  try
  {
    return _peer->impl->EventHasConnections() ? 1 : 0;
  }
  catch (...)
  {
    return 0;
  }
}

int gz_test_peer_command(gz_test_peer *_peer, double *_value)
{
  if (_peer == nullptr || _peer->impl == nullptr || _value == nullptr)
    return 0;
  try
  {
    return _peer->impl->Command(*_value) ? 1 : 0;
  }
  catch (...)
  {
    return 0;
  }
}
}

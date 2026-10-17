#include <gz/msgs/boolean.pb.h>
#include <gz/msgs/double.pb.h>
#include <gz/msgs/model.pb.h>
#include <gz/msgs/odometry.pb.h>
#include <gz/msgs/world_control.pb.h>
#include <gz/transport/Node.hh>

#include <algorithm>
#include <array>
#include <chrono>
#include <cmath>
#include <functional>
#include <iostream>
#include <mutex>
#include <stdexcept>
#include <string>
#include <thread>
#include <unordered_map>
#include <utility>
#include <vector>

#include "include/FSM/FSMState_Walking.h"
#include "include/common/ControlFSMData.h"
#include "include/common/OrientationEstimator.h"
#include "include/common/PositionVelocityEstimator.h"

namespace
{
constexpr std::size_t kJointCount = 10;
constexpr auto kControlPeriod = std::chrono::microseconds(1000);

const std::array<std::string, kJointCount> kJointNames = {
    "L_hip_joint", "L_hip2_joint", "L_thigh_joint", "L_calf_joint", "L_toe_joint",
    "R_hip_joint", "R_hip2_joint", "R_thigh_joint", "R_calf_joint", "R_toe_joint"};

const std::array<double, kJointCount> kEffortLimits = {
    33.5, 33.5, 33.5, 67.0, 33.5, 33.5, 33.5, 33.5, 67.0, 33.5};

class AutomatedPanel final : public CmdPanel
{
public:
  AutomatedPanel()
  {
    userCmd = UserCommand::NONE;
    userValue.setZero();
  }
};

class JettyInterface final : public IOInterface
{
public:
  JettyInterface()
  {
    cmdPanel = &panel_;
  }

  void sendRecv(const LowlevelCmd *, LowlevelState *) override
  {
  }

private:
  AutomatedPanel panel_;
};

struct Feedback
{
  std::mutex mutex;
  LowlevelState state;
  bool haveJoints{false};
  bool haveOdometry{false};
};

bool SetPaused(gz::transport::Node &node, const bool paused)
{
  gz::msgs::WorldControl request;
  request.set_pause(paused);
  gz::msgs::Boolean reply;
  bool result = false;
  const bool executed = node.Request(
      "/world/nervus_hector_dynamic/control", request, 3000U, reply, result);
  return executed && result && reply.data();
}

void CopyFeedback(Feedback &feedback, LowlevelState &state, bool &ready)
{
  std::lock_guard<std::mutex> lock(feedback.mutex);
  state = feedback.state;
  ready = feedback.haveJoints && feedback.haveOdometry;
}
}  // namespace

int main(int argc, char **argv)
{
  try
  {
    double targetDistance = 1.0;
    double timeoutSeconds = 30.0;
    float commandLy = 0.4F;
    for (int index = 1; index < argc; ++index)
    {
      const std::string argument = argv[index];
      if (argument == "--distance" && index + 1 < argc)
        targetDistance = std::stod(argv[++index]);
      else if (argument == "--timeout" && index + 1 < argc)
        timeoutSeconds = std::stod(argv[++index]);
      else if (argument == "--ly" && index + 1 < argc)
        commandLy = std::stof(argv[++index]);
      else
        throw std::invalid_argument("usage: nervus_hector_jetty_probe [--distance M] [--timeout S] [--ly VALUE]");
    }
    if (targetDistance <= 0.0 || timeoutSeconds <= 0.0 || commandLy < -1.0F || commandLy > 1.0F)
      throw std::invalid_argument("invalid HECTOR probe limits");

    gz::transport::Node node;
    Feedback feedback;
    feedback.state.imu.quaternion[0] = 1.0F;
    feedback.state.imu.quaternion[1] = 0.0F;
    feedback.state.imu.quaternion[2] = 0.0F;
    feedback.state.imu.quaternion[3] = 0.0F;

    std::unordered_map<std::string, std::size_t> jointIndex;
    for (std::size_t index = 0; index < kJointNames.size(); ++index)
      jointIndex.emplace(kJointNames[index], index);

    const std::function<void(const gz::msgs::Model &)> jointCallback =
        [&feedback, &jointIndex](const gz::msgs::Model &message) {
          std::lock_guard<std::mutex> lock(feedback.mutex);
          std::size_t found = 0;
          for (const auto &joint : message.joint())
          {
            const auto iterator = jointIndex.find(joint.name());
            if (iterator == jointIndex.end() || !joint.has_axis1())
              continue;
            auto &motor = feedback.state.motorState[iterator->second];
            motor.q = static_cast<float>(joint.axis1().position());
            motor.dq = static_cast<float>(joint.axis1().velocity());
            motor.tauEst = 0.0F;
            ++found;
          }
          feedback.haveJoints = found == kJointCount;
        };
    if (!node.Subscribe(
        "/world/nervus_hector_dynamic/model/hector_dynamic/joint_state",
        jointCallback))
      throw std::runtime_error("failed to subscribe to HECTOR joint state");

    const std::function<void(const gz::msgs::Odometry &)> odometryCallback =
        [&feedback](const gz::msgs::Odometry &message) {
          const auto &position = message.pose().position();
          const auto &orientation = message.pose().orientation();
          const auto &linear = message.twist().linear();
          const auto &angular = message.twist().angular();
          const double quaternionNorm =
              orientation.w() * orientation.w() +
              orientation.x() * orientation.x() +
              orientation.y() * orientation.y() +
              orientation.z() * orientation.z();
          const bool finite =
              std::isfinite(position.x()) && std::isfinite(position.y()) &&
              std::isfinite(position.z()) && std::isfinite(orientation.w()) &&
              std::isfinite(orientation.x()) && std::isfinite(orientation.y()) &&
              std::isfinite(orientation.z()) && std::isfinite(linear.x()) &&
              std::isfinite(linear.y()) && std::isfinite(linear.z()) &&
              std::isfinite(angular.x()) && std::isfinite(angular.y()) &&
              std::isfinite(angular.z());
          if (!finite || quaternionNorm < 0.99 || quaternionNorm > 1.01 ||
              std::abs(linear.x()) > 10.0 || std::abs(linear.y()) > 10.0 ||
              std::abs(linear.z()) > 10.0 || std::abs(angular.x()) > 20.0 ||
              std::abs(angular.y()) > 20.0 || std::abs(angular.z()) > 20.0)
            return;
          std::lock_guard<std::mutex> lock(feedback.mutex);
          feedback.state.position[0] = static_cast<float>(position.x());
          feedback.state.position[1] = static_cast<float>(position.y());
          feedback.state.position[2] = static_cast<float>(position.z());
          feedback.state.vWorld[0] = static_cast<float>(linear.x());
          feedback.state.vWorld[1] = static_cast<float>(linear.y());
          feedback.state.vWorld[2] = static_cast<float>(linear.z());
          feedback.state.imu.quaternion[0] = static_cast<float>(orientation.w());
          feedback.state.imu.quaternion[1] = static_cast<float>(orientation.x());
          feedback.state.imu.quaternion[2] = static_cast<float>(orientation.y());
          feedback.state.imu.quaternion[3] = static_cast<float>(orientation.z());
          feedback.state.imu.gyroscope[0] = static_cast<float>(angular.x());
          feedback.state.imu.gyroscope[1] = static_cast<float>(angular.y());
          feedback.state.imu.gyroscope[2] = static_cast<float>(angular.z());
          feedback.haveOdometry = true;
        };
    if (!node.Subscribe("/model/hector_dynamic/odometry", odometryCallback))
      throw std::runtime_error("failed to subscribe to HECTOR odometry");

    std::vector<gz::transport::Node::Publisher> forcePublishers;
    forcePublishers.reserve(kJointCount);
    for (const auto &jointName : kJointNames)
    {
      const std::string topic = "/model/hector_dynamic/joint/" + jointName + "/cmd_force";
      auto publisher = node.Advertise<gz::msgs::Double>(topic);
      if (!publisher)
        throw std::runtime_error("failed to advertise " + topic);
      forcePublishers.push_back(std::move(publisher));
    }

    std::this_thread::sleep_for(std::chrono::milliseconds(800));
    if (!SetPaused(node, false))
      throw std::runtime_error("failed to unpause nervus_hector_dynamic");

    LowlevelState state;
    state.imu.quaternion[0] = 1.0F;
    LowlevelCmd command;
    Biped biped;
    LegController legController(biped);
    StateEstimate stateEstimate{};
    StateEstimatorContainer stateEstimator(&state, legController.data, &stateEstimate);
    stateEstimator.addEstimator<CheaterOrientationEstimator>();
    stateEstimator.addEstimator<CheaterPositionVelocityEstimator>();
    DesiredStateCommand desiredStateCommand(&stateEstimate, 0.001);
    JettyInterface jettyInterface;
    ControlFSMData controlData{
        &biped,
        &stateEstimator,
        &legController,
        &desiredStateCommand,
        &jettyInterface,
        &command,
        &state};
    FSMState_Walking walking(&controlData);
    walking.enter();

    const auto feedbackDeadline = std::chrono::steady_clock::now() + std::chrono::seconds(4);
    bool ready = false;
    while (!ready && std::chrono::steady_clock::now() < feedbackDeadline)
    {
      CopyFeedback(feedback, state, ready);
      std::this_thread::sleep_for(std::chrono::milliseconds(5));
    }
    if (!ready)
      throw std::runtime_error("timed out waiting for HECTOR joint or odometry feedback");

    const double initialX = state.position[0];
    const double initialY = state.position[1];
    const auto started = std::chrono::steady_clock::now();
    auto nextTick = started;
    auto nextLog = started;
    bool passed = false;
    std::string failure = "timeout before reaching 1 metre";

    while (std::chrono::duration<double>(std::chrono::steady_clock::now() - started).count() < timeoutSeconds)
    {
      nextTick += kControlPeriod;
      CopyFeedback(feedback, state, ready);
      if (!ready)
      {
        failure = "feedback became unavailable";
        break;
      }
      state.userCmd = UserCommand::NONE;
      state.userValue.setZero();
      state.userValue.ly = commandLy;

      walking.run();
      for (std::size_t index = 0; index < forcePublishers.size(); ++index)
      {
        const auto &motorCommand = command.motorCmd[index];
        const auto &motorState = state.motorState[index];
        double torque = motorCommand.tau +
            motorCommand.Kp * (motorCommand.q - motorState.q) +
            motorCommand.Kd * (motorCommand.dq - motorState.dq);
        if (!std::isfinite(torque))
        {
          failure = "controller produced a non-finite joint torque";
          ready = false;
          break;
        }
        torque = std::clamp(torque, -kEffortLimits[index], kEffortLimits[index]);
        gz::msgs::Double message;
        message.set_data(torque);
        if (!forcePublishers[index].Publish(message))
          throw std::runtime_error("HECTOR force command publish failed");
      }
      if (!ready)
        break;

      const double dx = static_cast<double>(state.position[0]) - initialX;
      const double dy = static_cast<double>(state.position[1]) - initialY;
      const double upright = stateEstimate.rBody(2, 2);
      if (state.position[2] < 0.32F || upright < 0.5)
      {
        failure = "HECTOR fell below height/orientation threshold";
        break;
      }
      if (dx >= targetDistance)
      {
        passed = true;
        failure.clear();
        break;
      }
      if (std::chrono::steady_clock::now() >= nextLog)
      {
        const double elapsed = std::chrono::duration<double>(
            std::chrono::steady_clock::now() - started).count();
        std::cout << "t=" << elapsed << " z=" << state.position[2]
                  << " dx=" << dx << " dy=" << dy
                  << " upright=" << upright << std::endl;
        nextLog += std::chrono::seconds(1);
      }
      std::this_thread::sleep_until(nextTick);
    }

    if (!SetPaused(node, true))
      std::cerr << "warning: failed to pause world after HECTOR probe" << std::endl;
    const double dx = static_cast<double>(state.position[0]) - initialX;
    const double dy = static_cast<double>(state.position[1]) - initialY;
    std::cout << (passed ? "PASS" : "FAIL") << " z=" << state.position[2]
              << " dx=" << dx << " dy=" << dy
              << " target=" << targetDistance << std::endl;
    if (!passed)
      std::cerr << failure << std::endl;
    return passed ? 0 : 2;
  }
  catch (const std::exception &error)
  {
    std::cerr << "ERROR: " << error.what() << std::endl;
    return 1;
  }
}

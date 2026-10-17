#include <onnxruntime_cxx_api.h>

#include <gz/msgs/boolean.pb.h>
#include <gz/msgs/double.pb.h>
#include <gz/msgs/imu.pb.h>
#include <gz/msgs/model.pb.h>
#include <gz/msgs/pose_v.pb.h>
#include <gz/msgs/world_control.pb.h>
#include <gz/transport/Node.hh>

#include <algorithm>
#include <array>
#include <atomic>
#include <chrono>
#include <cmath>
#include <cstdlib>
#include <deque>
#include <functional>
#include <iostream>
#include <mutex>
#include <stdexcept>
#include <string>
#include <thread>
#include <unordered_map>
#include <utility>
#include <vector>

namespace
{
constexpr std::size_t kJointCount = 29;
constexpr std::size_t kFrameSize = 96;
constexpr std::size_t kHistoryLength = 5;
constexpr std::size_t kObservationSize = kFrameSize * kHistoryLength;
constexpr auto kControlPeriod = std::chrono::milliseconds(20);

const std::array<std::string, kJointCount> kJointNames = {
    "left_hip_pitch_joint", "right_hip_pitch_joint", "waist_yaw_joint",
    "left_hip_roll_joint", "right_hip_roll_joint", "waist_roll_joint",
    "left_hip_yaw_joint", "right_hip_yaw_joint", "waist_pitch_joint",
    "left_knee_joint", "right_knee_joint", "left_shoulder_pitch_joint",
    "right_shoulder_pitch_joint", "left_ankle_pitch_joint", "right_ankle_pitch_joint",
    "left_shoulder_roll_joint", "right_shoulder_roll_joint", "left_ankle_roll_joint",
    "right_ankle_roll_joint", "left_shoulder_yaw_joint", "right_shoulder_yaw_joint",
    "left_elbow_joint", "right_elbow_joint", "left_wrist_roll_joint",
    "right_wrist_roll_joint", "left_wrist_pitch_joint", "right_wrist_pitch_joint",
    "left_wrist_yaw_joint", "right_wrist_yaw_joint"};

const std::array<float, kJointCount> kDefaultPosition = {
    -0.1F, -0.1F, 0.0F, 0.0F, 0.0F, 0.0F, 0.0F, 0.0F, 0.0F,
    0.3F, 0.3F, 0.3F, 0.3F, -0.2F, -0.2F, 0.25F, -0.25F, 0.0F,
    0.0F, 0.0F, 0.0F, 0.97F, 0.97F, 0.15F, -0.15F, 0.0F, 0.0F,
    0.0F, 0.0F};

struct State
{
  std::mutex mutex;
  std::array<float, kJointCount> position{};
  std::array<float, kJointCount> velocity{};
  std::array<float, 3> angularVelocity{};
  std::array<float, 4> orientation{0.0F, 0.0F, 0.0F, 1.0F};
  double x{0.0};
  double y{0.0};
  double z{0.0};
  bool haveJoints{false};
  bool haveImu{false};
  bool havePose{false};
};

struct Snapshot
{
  std::array<float, kJointCount> position{};
  std::array<float, kJointCount> velocity{};
  std::array<float, 3> angularVelocity{};
  std::array<float, 4> orientation{};
  double x{0.0};
  double y{0.0};
  double z{0.0};
  bool haveJoints{false};
  bool haveImu{false};
  bool havePose{false};
  bool ready{false};
};

Snapshot TakeSnapshot(State &state)
{
  std::lock_guard<std::mutex> lock(state.mutex);
  return Snapshot{state.position, state.velocity, state.angularVelocity,
      state.orientation, state.x, state.y, state.z,
      state.haveJoints, state.haveImu, state.havePose,
      state.haveJoints && state.haveImu && state.havePose};
}

std::array<float, 3> ProjectGravity(const std::array<float, 4> &q)
{
  const float x = q[0];
  const float y = q[1];
  const float z = q[2];
  const float w = q[3];
  // q^-1 * [0, 0, -1] * q, matching Unitree's Eigen quaternion conjugate.
  return {
      2.0F * (w * y - x * z),
      -2.0F * (y * z + w * x),
      2.0F * (x * x + y * y) - 1.0F};
}

std::array<float, kFrameSize> BuildFrame(
    const Snapshot &state,
    const std::array<float, kJointCount> &lastAction,
    const float velocityX)
{
  std::array<float, kFrameSize> frame{};
  std::size_t offset = 0;
  for (const float value : state.angularVelocity)
    frame[offset++] = value * 0.2F;
  for (const float value : ProjectGravity(state.orientation))
    frame[offset++] = value;
  frame[offset++] = velocityX;
  frame[offset++] = 0.0F;
  frame[offset++] = 0.0F;
  for (std::size_t index = 0; index < kJointCount; ++index)
    frame[offset++] = state.position[index] - kDefaultPosition[index];
  for (const float value : state.velocity)
    frame[offset++] = value * 0.05F;
  for (const float value : lastAction)
    frame[offset++] = value;
  if (offset != frame.size())
    throw std::logic_error("G1 observation frame has the wrong size");
  return frame;
}

class Policy
{
public:
  explicit Policy(const std::string &path)
      : env_(ORT_LOGGING_LEVEL_WARNING, "nervus-g1-probe"),
        session_(env_, path.c_str(), SessionOptions())
  {
    const auto inputShape = session_.GetInputTypeInfo(0).GetTensorTypeAndShapeInfo().GetShape();
    const auto outputShape = session_.GetOutputTypeInfo(0).GetTensorTypeAndShapeInfo().GetShape();
    if (inputShape != std::vector<int64_t>({1, static_cast<int64_t>(kObservationSize)}))
      throw std::runtime_error("official policy input is not [1,480]");
    if (outputShape != std::vector<int64_t>({1, static_cast<int64_t>(kJointCount)}))
      throw std::runtime_error("official policy output is not [1,29]");
  }

  std::array<float, kJointCount> Run(const std::array<float, kObservationSize> &observation)
  {
    std::array<float, kObservationSize> input = observation;
    const std::array<int64_t, 2> inputShape{1, static_cast<int64_t>(kObservationSize)};
    auto memory = Ort::MemoryInfo::CreateCpu(OrtArenaAllocator, OrtMemTypeDefault);
    auto tensor = Ort::Value::CreateTensor<float>(
        memory, input.data(), input.size(), inputShape.data(), inputShape.size());
    const char *inputNames[] = {"obs"};
    const char *outputNames[] = {"actions"};
    auto outputs = session_.Run(
        Ort::RunOptions{nullptr}, inputNames, &tensor, 1, outputNames, 1);
    const float *data = outputs.at(0).GetTensorData<float>();
    std::array<float, kJointCount> result{};
    std::copy_n(data, result.size(), result.begin());
    if (!std::all_of(result.begin(), result.end(), [](const float value) {
          return std::isfinite(value);
        }))
      throw std::runtime_error("official policy returned a non-finite action");
    return result;
  }

private:
  static Ort::SessionOptions SessionOptions()
  {
    Ort::SessionOptions options;
    options.SetIntraOpNumThreads(1);
    options.SetGraphOptimizationLevel(GraphOptimizationLevel::ORT_ENABLE_ALL);
    return options;
  }

  Ort::Env env_;
  Ort::Session session_;
};

bool SetPaused(gz::transport::Node &node, const bool paused)
{
  gz::msgs::WorldControl request;
  request.set_pause(paused);
  gz::msgs::Boolean reply;
  bool result = false;
  const bool executed = node.Request(
      "/world/nervus_g1_dynamic/control", request, 3000U, reply, result);
  return executed && result && reply.data();
}

void Usage(const char *program)
{
  std::cerr << "usage: " << program
            << " POLICY_ONNX [--vx VALUE] [--seconds VALUE]" << std::endl;
}
}  // namespace

int main(int argc, char **argv)
{
  try
  {
    if (argc < 2)
    {
      Usage(argv[0]);
      return 64;
    }
    const std::string policyPath = argv[1];
    float velocityX = 0.0F;
    double durationSeconds = 15.0;
    for (int index = 2; index < argc; ++index)
    {
      const std::string argument = argv[index];
      if (argument == "--vx" && index + 1 < argc)
        velocityX = std::stof(argv[++index]);
      else if (argument == "--seconds" && index + 1 < argc)
        durationSeconds = std::stod(argv[++index]);
      else
      {
        Usage(argv[0]);
        return 64;
      }
    }
    if (velocityX < -0.5F || velocityX > 1.0F || durationSeconds <= 0.0)
      throw std::invalid_argument("command is outside the official policy range");

    Policy policy(policyPath);
    gz::transport::Node node;
    State state;
    std::unordered_map<std::string, std::size_t> jointIndex;
    for (std::size_t index = 0; index < kJointNames.size(); ++index)
      jointIndex.emplace(kJointNames[index], index);

    const std::function<void(const gz::msgs::Model &)> jointCallback =
        [&state, &jointIndex](const gz::msgs::Model &message) {
          std::lock_guard<std::mutex> lock(state.mutex);
          std::size_t found = 0;
          for (const auto &joint : message.joint())
          {
            const auto iterator = jointIndex.find(joint.name());
            if (iterator == jointIndex.end() || !joint.has_axis1())
              continue;
            state.position[iterator->second] = static_cast<float>(joint.axis1().position());
            state.velocity[iterator->second] = static_cast<float>(joint.axis1().velocity());
            ++found;
          }
          state.haveJoints = found == kJointCount;
        };
    if (!node.Subscribe(
        "/world/nervus_g1_dynamic/model/nervus_g1_dynamic/joint_state",
        jointCallback))
      throw std::runtime_error("failed to subscribe to G1 joint state");

    const std::function<void(const gz::msgs::IMU &)> imuCallback =
        [&state](const gz::msgs::IMU &message) {
          std::lock_guard<std::mutex> lock(state.mutex);
          state.angularVelocity = {
              static_cast<float>(message.angular_velocity().x()),
              static_cast<float>(message.angular_velocity().y()),
              static_cast<float>(message.angular_velocity().z())};
          state.orientation = {
              static_cast<float>(message.orientation().x()),
              static_cast<float>(message.orientation().y()),
              static_cast<float>(message.orientation().z()),
              static_cast<float>(message.orientation().w())};
          state.haveImu = true;
        };
    if (!node.Subscribe("/model/nervus_g1_dynamic/imu", imuCallback))
      throw std::runtime_error("failed to subscribe to G1 IMU");

    const std::function<void(const gz::msgs::Pose_V &)> poseCallback =
        [&state](const gz::msgs::Pose_V &message) {
          for (const auto &pose : message.pose())
          {
            if (pose.name() != "nervus_g1_dynamic")
              continue;
            std::lock_guard<std::mutex> lock(state.mutex);
            state.x = pose.position().x();
            state.y = pose.position().y();
            state.z = pose.position().z();
            state.havePose = true;
            break;
          }
        };
    if (!node.Subscribe(
        "/world/nervus_g1_dynamic/dynamic_pose/info", poseCallback))
      throw std::runtime_error("failed to subscribe to G1 dynamic pose");

    std::vector<gz::transport::Node::Publisher> publishers;
    publishers.reserve(kJointCount);
    for (const auto &jointName : kJointNames)
    {
      const std::string topic = "/model/nervus_g1_dynamic/joint/" + jointName + "/0/cmd_pos";
      auto publisher = node.Advertise<gz::msgs::Double>(topic);
      if (!publisher)
        throw std::runtime_error("failed to advertise " + topic);
      publishers.push_back(std::move(publisher));
    }

    std::this_thread::sleep_for(std::chrono::milliseconds(800));
    for (std::size_t index = 0; index < publishers.size(); ++index)
    {
      gz::msgs::Double command;
      command.set_data(kDefaultPosition[index]);
      if (!publishers[index].Publish(command))
        throw std::runtime_error("failed to publish initial G1 posture");
    }
    if (!SetPaused(node, false))
      throw std::runtime_error("failed to unpause nervus_g1_dynamic");

    std::array<float, kJointCount> lastAction{};
    std::deque<std::array<float, kFrameSize>> history;
    const auto dataDeadline = std::chrono::steady_clock::now() + std::chrono::seconds(5);
    Snapshot snapshot;
    while (!(snapshot = TakeSnapshot(state)).ready &&
        std::chrono::steady_clock::now() < dataDeadline)
      std::this_thread::sleep_for(kControlPeriod);
    if (!snapshot.ready)
      throw std::runtime_error(
          "timed out waiting for feedback: joints=" + std::to_string(snapshot.haveJoints) +
          " imu=" + std::to_string(snapshot.haveImu) +
          " pose=" + std::to_string(snapshot.havePose));

    const double initialX = snapshot.x;
    const double initialY = snapshot.y;
    const auto started = std::chrono::steady_clock::now();
    auto nextTick = started;
    auto nextLog = started;
    bool passed = true;
    std::string failure;
    while (std::chrono::duration<double>(std::chrono::steady_clock::now() - started).count() < durationSeconds)
    {
      nextTick += kControlPeriod;
      snapshot = TakeSnapshot(state);
      if (!snapshot.ready)
      {
        passed = false;
        failure = "feedback became unavailable";
        break;
      }

      const auto frame = BuildFrame(snapshot, lastAction, velocityX);
      if (history.empty())
        for (std::size_t count = 0; count < kHistoryLength; ++count)
          history.push_back(frame);
      else
      {
        history.push_back(frame);
        history.pop_front();
      }
      std::array<float, kObservationSize> observation{};
      std::size_t observationOffset = 0;
      for (const auto &entry : history)
        for (const float value : entry)
          observation[observationOffset++] = value;

      lastAction = policy.Run(observation);
      for (std::size_t index = 0; index < publishers.size(); ++index)
      {
        gz::msgs::Double command;
        command.set_data(kDefaultPosition[index] + 0.25F * lastAction[index]);
        if (!publishers[index].Publish(command))
          throw std::runtime_error("joint command publish failed");
      }

      const auto gravity = ProjectGravity(snapshot.orientation);
      if (snapshot.z < 0.45 || gravity[2] > -0.55F)
      {
        passed = false;
        failure = "fell below height/orientation threshold";
        break;
      }
      if (std::chrono::steady_clock::now() >= nextLog)
      {
        const double elapsed = std::chrono::duration<double>(
            std::chrono::steady_clock::now() - started).count();
        std::cout << "t=" << elapsed << " z=" << snapshot.z
                  << " dx=" << (snapshot.x - initialX)
                  << " dy=" << (snapshot.y - initialY)
                  << " gravity_z=" << gravity[2] << std::endl;
        nextLog += std::chrono::seconds(1);
      }
      std::this_thread::sleep_until(nextTick);
    }

    snapshot = TakeSnapshot(state);
    const double displacementX = snapshot.x - initialX;
    const double lateralDrift = std::abs(snapshot.y - initialY);
    if (passed && velocityX == 0.0F &&
        (std::abs(displacementX) > 0.25 || lateralDrift > 0.25))
    {
      passed = false;
      failure = "zero-command drift exceeded 0.25 m";
    }
    if (passed && velocityX > 0.0F && displacementX < 1.0)
    {
      passed = false;
      failure = "forward displacement did not reach 1.0 m";
    }
    if (!SetPaused(node, true))
      std::cerr << "warning: failed to pause world after probe" << std::endl;

    std::cout << (passed ? "PASS" : "FAIL") << " z=" << snapshot.z
              << " dx=" << displacementX << " dy=" << (snapshot.y - initialY)
              << " command_vx=" << velocityX << std::endl;
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

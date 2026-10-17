cmake_minimum_required(VERSION 3.22)

project(nervus_hector_jetty_probe LANGUAGES CXX)

find_package(gz-msgs 12 REQUIRED CONFIG)
find_package(gz-transport 15 REQUIRED CONFIG)
find_package(Threads REQUIRED)

set(
    HECTOR_CONTROL_ROOT
    "${CMAKE_CURRENT_SOURCE_DIR}/../../../third_party/Hector_Simulation/Hector_ROS_Simulation/hector_control"
    CACHE PATH
    "HECTOR control source root"
)

if(NOT EXISTS "${HECTOR_CONTROL_ROOT}/ConvexMPC/ConvexMPCLocomotion.cpp")
    message(FATAL_ERROR "HECTOR control source is missing at ${HECTOR_CONTROL_ROOT}")
endif()

add_subdirectory(
    "${HECTOR_CONTROL_ROOT}/third_party/qpOASES"
    "${CMAKE_CURRENT_BINARY_DIR}/qpOASES"
    EXCLUDE_FROM_ALL
)

set(HECTOR_SOURCES
    "${HECTOR_CONTROL_ROOT}/ConvexMPC/Constraints.cpp"
    "${HECTOR_CONTROL_ROOT}/ConvexMPC/ConvexMPCLocomotion.cpp"
    "${HECTOR_CONTROL_ROOT}/ConvexMPC/GaitGenerator.cpp"
    "${HECTOR_CONTROL_ROOT}/ConvexMPC/RobotState.cpp"
    "${HECTOR_CONTROL_ROOT}/ConvexMPC/SolverMPC.cpp"
    "${HECTOR_CONTROL_ROOT}/ConvexMPC/convexMPC_interface.cpp"
    "${HECTOR_CONTROL_ROOT}/src/FSM/FSMState.cpp"
    "${HECTOR_CONTROL_ROOT}/src/FSM/FSMState_Walking.cpp"
    "${HECTOR_CONTROL_ROOT}/src/common/DesiredCommand.cpp"
    "${HECTOR_CONTROL_ROOT}/src/common/FootSwingTrajectory.cpp"
    "${HECTOR_CONTROL_ROOT}/src/common/LegController.cpp"
    "${HECTOR_CONTROL_ROOT}/src/common/OrientationEstimator.cpp"
    "${HECTOR_CONTROL_ROOT}/src/common/PositionVelocityEstimator.cpp"
    "${HECTOR_CONTROL_ROOT}/src/common/SwingLegController.cpp"
)

add_executable(nervus_hector_jetty_probe main.cc ${HECTOR_SOURCES})
target_compile_features(nervus_hector_jetty_probe PRIVATE cxx_std_17)
target_compile_definitions(
    nervus_hector_jetty_probe
    PRIVATE
        EIGEN_STACK_ALLOCATION_LIMIT=0
)
target_include_directories(
    nervus_hector_jetty_probe
    PRIVATE
        "${HECTOR_CONTROL_ROOT}"
        "${HECTOR_CONTROL_ROOT}/include"
        "${HECTOR_CONTROL_ROOT}/third_party/qpOASES/include"
)
target_link_libraries(
    nervus_hector_jetty_probe
    PRIVATE
        gz-transport::core
        gz-msgs::core
        qpOASES
        Threads::Threads
        lcm
)
target_compile_options(
    nervus_hector_jetty_probe
    PRIVATE
        -O3
        -Wall
        -Wextra
        -Wpedantic
)

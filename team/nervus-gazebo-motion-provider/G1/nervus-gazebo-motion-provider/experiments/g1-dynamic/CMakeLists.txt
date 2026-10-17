cmake_minimum_required(VERSION 3.22)

project(nervus_g1_dynamic_probe LANGUAGES CXX)

find_package(gz-msgs 12 REQUIRED CONFIG)
find_package(gz-transport 15 REQUIRED CONFIG)

set(ONNXRUNTIME_ROOT "/opt/onnxruntime" CACHE PATH "System ONNX Runtime root")
find_path(
    ONNXRUNTIME_INCLUDE_DIR
    onnxruntime_cxx_api.h
    PATHS "${ONNXRUNTIME_ROOT}/include"
    NO_DEFAULT_PATH
    REQUIRED
)
find_library(
    ONNXRUNTIME_LIBRARY
    onnxruntime
    PATHS "${ONNXRUNTIME_ROOT}/lib"
    NO_DEFAULT_PATH
    REQUIRED
)

add_executable(nervus_g1_dynamic_probe main.cc)
target_compile_features(nervus_g1_dynamic_probe PRIVATE cxx_std_17)
target_include_directories(nervus_g1_dynamic_probe PRIVATE "${ONNXRUNTIME_INCLUDE_DIR}")
target_link_libraries(
    nervus_g1_dynamic_probe
    PRIVATE
        gz-transport::core
        gz-msgs::core
        "${ONNXRUNTIME_LIBRARY}"
)
target_compile_options(
    nervus_g1_dynamic_probe
    PRIVATE
        -Wall
        -Wextra
        -Wpedantic
        -Wconversion
        -Wshadow
)

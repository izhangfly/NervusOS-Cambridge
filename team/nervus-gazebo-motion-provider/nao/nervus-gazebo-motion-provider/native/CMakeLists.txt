cmake_minimum_required(VERSION 3.22)

project(nervus_gazebo_adapter VERSION 0.1.0 LANGUAGES C CXX)

include(CTest)
find_package(gz-msgs 12 REQUIRED CONFIG)
find_package(gz-transport 15 REQUIRED CONFIG)

option(
    NERVUS_ENABLE_SANITIZERS
    "Enable AddressSanitizer and UndefinedBehaviorSanitizer"
    OFF
)

add_library(nervus_gazebo_adapter STATIC
    src/adapter.cc
)

target_compile_features(nervus_gazebo_adapter PUBLIC cxx_std_17)
target_include_directories(nervus_gazebo_adapter
    PUBLIC
        ${CMAKE_CURRENT_SOURCE_DIR}/include
)
target_link_libraries(nervus_gazebo_adapter
    PUBLIC
        gz-transport::core
        gz-msgs::core
)
set_target_properties(nervus_gazebo_adapter PROPERTIES
    CXX_EXTENSIONS OFF
    POSITION_INDEPENDENT_CODE ON
)

if(CMAKE_CXX_COMPILER_ID MATCHES "GNU|Clang")
    target_compile_options(nervus_gazebo_adapter PRIVATE
        -Wall
        -Wextra
        -Wpedantic
        -Wconversion
        -Wshadow
    )
endif()

function(nervus_enable_sanitizers target_name)
    if(NERVUS_ENABLE_SANITIZERS)
        if(NOT CMAKE_CXX_COMPILER_ID MATCHES "GNU|Clang")
            message(FATAL_ERROR "NERVUS_ENABLE_SANITIZERS requires GCC or Clang")
        endif()
        target_compile_options(${target_name} PRIVATE
            -fno-omit-frame-pointer
            -fsanitize=address,undefined
        )
        target_link_options(${target_name} PRIVATE
            -fno-omit-frame-pointer
            -fsanitize=address,undefined
        )
    endif()
endfunction()

nervus_enable_sanitizers(nervus_gazebo_adapter)

if(BUILD_TESTING)
    add_executable(nervus_gazebo_adapter_test
        tests/adapter_test.cc
    )
    target_compile_features(nervus_gazebo_adapter_test PRIVATE cxx_std_17)
    target_link_libraries(nervus_gazebo_adapter_test
        PRIVATE
            nervus_gazebo_adapter
    )
    if(CMAKE_CXX_COMPILER_ID MATCHES "GNU|Clang")
        target_compile_options(nervus_gazebo_adapter_test PRIVATE
            -Wall
            -Wextra
            -Wpedantic
            -Wconversion
            -Wshadow
        )
    endif()
    nervus_enable_sanitizers(nervus_gazebo_adapter_test)
    add_test(NAME nervus_gazebo_adapter_test COMMAND nervus_gazebo_adapter_test)
    set_tests_properties(nervus_gazebo_adapter_test PROPERTIES TIMEOUT 30)

    add_executable(nervus_gazebo_adapter_c_abi_test
        tests/header_c_test.c
    )
    target_link_libraries(nervus_gazebo_adapter_c_abi_test
        PRIVATE
            nervus_gazebo_adapter
    )
    set_target_properties(nervus_gazebo_adapter_c_abi_test PROPERTIES
        LINKER_LANGUAGE CXX
    )
    if(CMAKE_C_COMPILER_ID MATCHES "GNU|Clang")
        target_compile_options(nervus_gazebo_adapter_c_abi_test PRIVATE
            -Wall
            -Wextra
            -Wpedantic
            -Wconversion
            -Wshadow
        )
    endif()
    nervus_enable_sanitizers(nervus_gazebo_adapter_c_abi_test)
    add_test(
        NAME nervus_gazebo_adapter_c_abi_test
        COMMAND nervus_gazebo_adapter_c_abi_test
    )
    set_tests_properties(nervus_gazebo_adapter_c_abi_test PROPERTIES TIMEOUT 10)
endif()

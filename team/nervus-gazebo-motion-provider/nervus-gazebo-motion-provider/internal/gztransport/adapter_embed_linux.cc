//go:build linux && cgo

// cgo compiles this translation unit into the final Go binary. The adapter
// implementation remains canonical under native/ and is also built by CMake
// for standalone C++ tests.
#include "../../native/src/adapter.cc"

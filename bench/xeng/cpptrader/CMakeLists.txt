# Builds the CppTrader cross-engine adapter against a CppTrader checkout prepared
# by fetch.sh (sources plus the gil-linked modules). Only the adapter and the
# libraries it links are built: cpptrader, cppcommon, fmt.
#
#   cmake -S bench/xeng/cpptrader -B <build> -DCMAKE_BUILD_TYPE=Release -DCPPTRADER_DIR=<checkout>
#   cmake --build <build> -j
cmake_minimum_required(VERSION 3.20)
project(cpptrader-xeng C CXX)

set(CPPTRADER_DIR "" CACHE PATH "CppTrader checkout with its gil modules in place")
if(NOT EXISTS "${CPPTRADER_DIR}/include/trader/matching/market_manager.h")
  message(FATAL_ERROR "CPPTRADER_DIR does not point at a CppTrader checkout: '${CPPTRADER_DIR}'")
endif()
if(NOT EXISTS "${CPPTRADER_DIR}/cmake/SetCompilerFeatures.cmake")
  message(FATAL_ERROR "CppTrader modules missing; run fetch.sh (gil update) first")
endif()

# The engine asserts on a cancel of an unknown order, which the tape does on
# purpose, so only an NDEBUG build can replay it.
if(NOT CMAKE_BUILD_TYPE STREQUAL "Release")
  message(FATAL_ERROR "build with -DCMAKE_BUILD_TYPE=Release")
endif()

# Same standard CppTrader sets for itself (CppCMakeScripts/SetCompilerFeatures).
set(CMAKE_CXX_STANDARD 23)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS ON)

# CPPTRADER_MODULE skips CppTrader's examples, benchmarks and tests.
set(CPPTRADER_MODULE Y)
add_subdirectory("${CPPTRADER_DIR}" cpptrader EXCLUDE_FROM_ALL)

add_executable(cpptrader-xeng main.cpp)
target_include_directories(cpptrader-xeng SYSTEM PRIVATE "${CPPTRADER_DIR}/modules")
target_link_libraries(cpptrader-xeng cpptrader)

cmake_minimum_required(VERSION 3.20)
project(kavach VERSION 0.1.0 LANGUAGES C CXX)

set(CMAKE_C_STANDARD 11)
set(CMAKE_C_STANDARD_REQUIRED ON)
set(CMAKE_C_EXTENSIONS OFF)
set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_C_VISIBILITY_PRESET hidden)

option(KAVACH_WERROR "Compile with -Werror" ON)
option(KAVACH_SANITIZE "Build with -fsanitize=address,undefined" OFF)
option(KAVACH_BUILD_TESTS "Build and register the tests" ON)
option(KAVACH_BUILD_EXAMPLES "Build the examples" ON)

# The contract (SPEC.md, spec/host, spec/recorder/sdk) lives in the main
# checkout and is never copied. Override with -DKAVACH_SPEC_DIR=... or the
# KAVACH_SPEC_DIR environment variable.
if(DEFINED ENV{KAVACH_SPEC_DIR} AND NOT "$ENV{KAVACH_SPEC_DIR}" STREQUAL "")
  set(_spec_default "$ENV{KAVACH_SPEC_DIR}")
else()
  get_filename_component(_spec_default "${CMAKE_CURRENT_SOURCE_DIR}/../../spec" ABSOLUTE)
endif()
set(KAVACH_SPEC_DIR "${_spec_default}" CACHE PATH "Directory holding host/ and recorder/sdk/ conformance files")

find_package(Threads REQUIRED)

add_compile_definitions(_GNU_SOURCE _DARWIN_C_SOURCE)
add_compile_options(-Wall -Wextra -Wpedantic)
if(KAVACH_WERROR)
  add_compile_options(-Werror)
endif()
if(KAVACH_SANITIZE)
  add_compile_options(-fsanitize=address,undefined -fno-omit-frame-pointer -g)
  add_link_options(-fsanitize=address,undefined)
endif()

set(KAVACH_SOURCES src/util.c src/json.c src/recorder.c src/host.c)

add_library(kavach_objects OBJECT ${KAVACH_SOURCES})
target_include_directories(kavach_objects PUBLIC include PRIVATE src)
set_target_properties(kavach_objects PROPERTIES POSITION_INDEPENDENT_CODE ON)

add_library(kavach_static STATIC $<TARGET_OBJECTS:kavach_objects>)
set_target_properties(kavach_static PROPERTIES OUTPUT_NAME kavach)
target_include_directories(kavach_static PUBLIC include)
target_link_libraries(kavach_static PUBLIC Threads::Threads)

add_library(kavach_shared SHARED $<TARGET_OBJECTS:kavach_objects>)
set_target_properties(kavach_shared PROPERTIES OUTPUT_NAME kavach
  VERSION ${PROJECT_VERSION} SOVERSION 0)
target_include_directories(kavach_shared PUBLIC include)
target_link_libraries(kavach_shared PUBLIC Threads::Threads)

# C++ wrapper: header only.
add_library(kavach_cpp INTERFACE)
target_include_directories(kavach_cpp INTERFACE include)
target_link_libraries(kavach_cpp INTERFACE kavach_static)
target_compile_features(kavach_cpp INTERFACE cxx_std_17)

include(GNUInstallDirs)
install(TARGETS kavach_static kavach_shared)
install(DIRECTORY include/ DESTINATION ${CMAKE_INSTALL_INCLUDEDIR})

if(KAVACH_BUILD_TESTS)
  enable_testing()
endif()

if(KAVACH_BUILD_EXAMPLES)
  add_subdirectory(examples/ledger)
endif()

if(KAVACH_BUILD_TESTS)
  enable_testing()
  add_subdirectory(conformance)
  add_subdirectory(tests)
endif()

add_executable(test_api test_api.c)
target_link_libraries(test_api kavach_shared)
add_test(NAME api COMMAND test_api)

add_executable(test_cpp test_cpp.cpp)
target_link_libraries(test_cpp kavach_cpp)
add_test(NAME cpp-wrapper COMMAND test_cpp)

# The tests below run the real recorder, built from this repository with Go.
find_program(GO_EXECUTABLE go)
if(GO_EXECUTABLE)
  set(_recorder ${CMAKE_CURRENT_BINARY_DIR}/kavach-recorder)
  add_custom_target(kavach_recorder
    COMMAND ${GO_EXECUTABLE} build -o ${_recorder} ./cmd/kavach-recorder
    WORKING_DIRECTORY ${CMAKE_CURRENT_SOURCE_DIR}/../../..
    COMMENT "go build kavach-recorder")

  add_executable(test_ring test_ring.c)
  target_link_libraries(test_ring kavach_shared)
  add_dependencies(test_ring kavach_recorder)
  add_test(NAME ring COMMAND test_ring ${_recorder})

  add_executable(bench_step bench_step.c)
  target_link_libraries(bench_step kavach_shared)
  add_dependencies(bench_step kavach_recorder)
else()
  message(STATUS "go not found: skipping the tests that need kavach-recorder")
endif()

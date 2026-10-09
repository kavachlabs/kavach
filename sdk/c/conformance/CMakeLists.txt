set(_internal ${CMAKE_CURRENT_SOURCE_DIR}/../src)

# ---- C ----
add_library(conf_c STATIC conformance.c caseio.c)
target_include_directories(conf_c PUBLIC ${CMAKE_CURRENT_SOURCE_DIR} ${_internal})
target_link_libraries(conf_c PUBLIC kavach_static)

add_executable(kavach-conformance-host-c host_c.c)
target_link_libraries(kavach-conformance-host-c conf_c)

add_executable(kavach-recorder-runner-c runner_c.c)
target_link_libraries(kavach-recorder-runner-c conf_c)

# ---- C++ ----
add_executable(kavach-conformance-host-cpp host_cpp.cpp)
target_include_directories(kavach-conformance-host-cpp PRIVATE ${_internal})
target_link_libraries(kavach-conformance-host-cpp conf_c kavach_cpp)

add_executable(kavach-recorder-runner-cpp runner_cpp.cpp)
target_include_directories(kavach-recorder-runner-cpp PRIVATE ${_internal})
target_link_libraries(kavach-recorder-runner-cpp conf_c kavach_cpp)

# ---- tests ----
find_package(Python3 REQUIRED COMPONENTS Interpreter)
set(_host_dir ${KAVACH_SPEC_DIR}/host)
set(_rec_dir ${KAVACH_SPEC_DIR}/recorder/sdk)

if(NOT EXISTS ${_host_dir}/run.py OR NOT EXISTS ${_rec_dir}/fake_recorder.py)
  message(FATAL_ERROR "KAVACH_SPEC_DIR=${KAVACH_SPEC_DIR} does not hold host/run.py and recorder/sdk/fake_recorder.py")
endif()

add_test(NAME host-c
  COMMAND ${Python3_EXECUTABLE} ${_host_dir}/run.py --host $<TARGET_FILE:kavach-conformance-host-c>)
add_test(NAME host-cpp
  COMMAND ${Python3_EXECUTABLE} ${_host_dir}/run.py --host $<TARGET_FILE:kavach-conformance-host-cpp>)

file(GLOB _cases ${_rec_dir}/*.json)
foreach(case ${_cases})
  get_filename_component(name ${case} NAME_WE)
  foreach(lang c cpp)
    add_test(NAME recorder-${lang}-${name}
      COMMAND $<TARGET_FILE:kavach-recorder-runner-${lang}> ${case} ${_rec_dir}/fake_recorder.py
              ${CMAKE_CURRENT_BINARY_DIR}/result-${lang}-${name}.json)
  endforeach()
endforeach()

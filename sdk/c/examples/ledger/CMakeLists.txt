add_executable(ledger-buggy ledger.cpp)
target_link_libraries(ledger-buggy kavach_cpp)

add_executable(ledger-fixed ledger.cpp)
target_compile_definitions(ledger-fixed PRIVATE LEDGER_FIX=1)
target_link_libraries(ledger-fixed kavach_cpp)

if(KAVACH_BUILD_TESTS)
  # Without a recorder the ledger still runs; the buggy build must stop with a panic.
  set(_events ${CMAKE_CURRENT_SOURCE_DIR}/events.jsonl)
  add_test(NAME ledger-buggy-panics
    COMMAND ledger-buggy --in ${_events} --fixtures ${CMAKE_CURRENT_BINARY_DIR}/fx-buggy)
  set_tests_properties(ledger-buggy-panics PROPERTIES
    PASS_REGULAR_EXPRESSION "step 7 failed \\(panic\\)"
    ENVIRONMENT "KAVACH_RECORDER=/nonexistent/kavach-recorder")
  add_test(NAME ledger-fixed-runs
    COMMAND ledger-fixed --in ${_events} --fixtures ${CMAKE_CURRENT_BINARY_DIR}/fx-fixed)
  set_tests_properties(ledger-fixed-runs PROPERTIES
    PASS_REGULAR_EXPRESSION "ledger.rejections +\\{\"event\":\"evt-008\",\"reason\":\"missing amount\"\\}"
    ENVIRONMENT "KAVACH_RECORDER=/nonexistent/kavach-recorder")
endif()

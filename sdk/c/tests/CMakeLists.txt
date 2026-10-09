add_executable(test_api test_api.c)
target_link_libraries(test_api kavach_shared)
add_test(NAME api COMMAND test_api)

add_executable(test_cpp test_cpp.cpp)
target_link_libraries(test_cpp kavach_cpp)
add_test(NAME cpp-wrapper COMMAND test_cpp)

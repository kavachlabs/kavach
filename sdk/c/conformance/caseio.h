/* Loads a recorder-conformance case (spec/recorder/sdk) for the C and C++ runners. */
#ifndef KAVACH_CASEIO_H
#define KAVACH_CASEIO_H

#include <stddef.h>
#include <stdint.h>

typedef struct ans {
  int kind; /* 0 value, 1 error, 2 unset */
  char* text;       /* clock: decimal nanos; error: message */
  uint8_t* data;    /* rand/gateway/config value */
  size_t len;
} ans;

typedef struct ans_list {
  ans* v;
  size_t n, next;
} ans_list;

typedef struct case_action {
  int is_flush;
  int durable;
  char* source;
  char* position;
  uint8_t* data;
  size_t len;
  ans_list clock, rand, gateway, config;
} case_action;

typedef struct case_flag {
  char* key;
  char* value;
} case_flag;

typedef struct case_t {
  char* service;
  int start_snapshot;
  int snapshots;
  char* snapshot; /* decimal ASCII or NULL */
  case_flag* flags;
  size_t n_flags;
  case_action* actions;
  size_t n_actions;
} case_t;

case_t* case_load(const char* path, char* err, size_t errcap);
void case_free(case_t* c);
/* Reads result.json written by the fake recorder; returns 0 if it passed and read the stream over
 * `transport` ("pipe" or "ring"). Prints the error. */
int case_check_result(const char* path, const char* transport);

#endif

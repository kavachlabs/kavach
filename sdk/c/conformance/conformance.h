/* The conformance handler of SPEC 9.6, written against the C API. */
#ifndef KAVACH_CONFORMANCE_H
#define KAVACH_CONFORMANCE_H

#include "json.h"
#include "kavach/kavach.h"

typedef struct conf_state {
  long count;
  kj* ops;          /* parsed operations of the current step (freed lazily: panics longjmp) */
  uint8_t* held;    /* a buffer returned by kavach that the step has not released yet */
  uint8_t* scratch; /* random bytes */
  size_t scratch_cap;
} conf_state;

/* Fills h with the conformance handler operating on st (zero-initialised). */
void conf_handler(kavach_handler* h, conf_state* st);
void conf_state_clear(conf_state* st);

/* Factory for kavach_maybe_host. user is unused. */
int conf_factory(void* user, kavach_handler* out);

#endif

/* Public-API tests that need no recorder: failure mapping, delivery, recorder
 * failure handling. Linked against the shared library, so only kavach.h is used. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#include "kavach/kavach.h"

static int failures;
#define CHECK(c)                                                    \
  do {                                                              \
    if (!(c)) {                                                     \
      fprintf(stderr, "%s:%d: check failed: %s\n", __FILE__, __LINE__, #c); \
      failures++;                                                   \
    }                                                               \
  } while (0)

static int delivered;
static int logged;

static int deliver(void* user, const kavach_output* outs, size_t n) {
  (void)user;
  CHECK(n == 1);
  CHECK(strcmp(outs[0].sink, "out") == 0);
  CHECK(outs[0].len == 2 && memcmp(outs[0].data, "ok", 2) == 0);
  delivered++;
  return KAVACH_OK;
}

static void on_log(void* user, const char* msg) {
  (void)user;
  (void)msg;
  logged++;
}

/* The input's data is the mode: "ok", "error", "panic", "errorf", "rand", "clock". */
static int handle(void* state, kavach_env* env, const kavach_input* in) {
  (void)state;
  if (in->len == 2 && memcmp(in->data, "ok", 2) == 0) return kavach_emit(env, "out", "ok", 2, KAVACH_REMOTE);
  if (in->len == 5 && memcmp(in->data, "error", 5) == 0) {
    kavach_emit(env, "out", "ok", 2, KAVACH_REMOTE);
    return kavach_error(env, "amount is null");
  }
  if (in->len == 6 && memcmp(in->data, "errorf", 6) == 0) return kavach_errorf(env, "bad %d", 42);
  if (in->len == 5 && memcmp(in->data, "panic", 5) == 0) kavach_panic(env, "boom");
  if (in->len == 6 && memcmp(in->data, "panicf", 6) == 0) kavach_panicf(env, "boom %s", "x");
  if (in->len == 5 && memcmp(in->data, "clock", 5) == 0) {
    CHECK(kavach_now_ns(env) > 1600000000LL * 1000000000LL);
    uint8_t b[8] = {0};
    CHECK(kavach_random(env, b, sizeof b) == KAVACH_OK);
    uint8_t* r;
    size_t rl;
    char* err = NULL;
    CHECK(kavach_query(env, "nowhere", "q", 1, &r, &rl, &err) == KAVACH_QUERY_FAILED);
    CHECK(err && strstr(err, "nowhere"));
    kavach_free(err);
    int present = 1;
    uint8_t* v = NULL;
    size_t vl;
    CHECK(kavach_config(env, "k", &v, &vl, &present) == KAVACH_OK && !present && !v);
    return KAVACH_OK;
  }
  return KAVACH_ERROR; /* no message set */
}

static int always_broken(void* state, void* arg, char** detail) {
  (void)state;
  (void)arg;
  *detail = strdup("because");
  return KAVACH_ERROR;
}

static kavach_recorder* make(const char* const* argv, int required, int broken, char** err) {
  static const kavach_invariant inv[] = {{"always_broken", always_broken, NULL}};
  kavach_handler h;
  memset(&h, 0, sizeof h);
  h.handle = handle;
  if (broken) {
    h.invariants = inv;
    h.n_invariants = 1;
  }
  kavach_recorder_options o;
  memset(&o, 0, sizeof o);
  o.service = "t";
  o.recorder_argv = argv;
  o.required = required;
  o.deliver = deliver;
  o.log = on_log;
  kavach_recorder* r = NULL;
  int rc = kavach_recorder_new(&h, &o, &r, err);
  CHECK((rc == KAVACH_OK) == (r != NULL));
  return r;
}

static int step(kavach_recorder* r, const char* mode, kavach_result* res) {
  kavach_input in = {"test", "0", (const uint8_t*)mode, strlen(mode)};
  return kavach_recorder_step(r, &in, res);
}

int main(void) {
  /* A recorder that cannot start: the service still runs, and it is said so. */
  const char* missing[] = {"/nonexistent/kavach-recorder", NULL};
  char* err = NULL;
  kavach_recorder* r = make(missing, 0, 0, &err);
  CHECK(r && !kavach_recorder_active(r) && logged == 1);
  kavach_result res;

  delivered = 0;
  CHECK(step(r, "ok", &res) == KAVACH_OK && delivered == 1);
  kavach_result_clear(&res);

  /* Outputs are not delivered when the step fails. */
  CHECK(step(r, "error", &res) == KAVACH_ERROR && delivered == 1);
  CHECK(res.message && strcmp(res.message, "amount is null") == 0);
  kavach_result_clear(&res);
  CHECK(step(r, "errorf", &res) == KAVACH_ERROR && strcmp(res.message, "bad 42") == 0);
  kavach_result_clear(&res);
  CHECK(step(r, "panic", &res) == KAVACH_PANIC && strcmp(res.message, "boom") == 0);
  kavach_result_clear(&res);
  CHECK(step(r, "panicf", &res) == KAVACH_PANIC && strcmp(res.message, "boom x") == 0);
  kavach_result_clear(&res);
  CHECK(step(r, "other", &res) == KAVACH_ERROR && res.message && *res.message);
  kavach_result_clear(&res);
  /* The handler keeps working after a panic, and reads work unrecorded. */
  CHECK(step(r, "clock", &res) == KAVACH_OK);
  kavach_result_clear(&res);
  CHECK(step(r, "ok", NULL) == KAVACH_OK && delivered == 2);
  CHECK(kavach_recorder_flush(r, 1) == KAVACH_ERROR);
  kavach_recorder_free(r);

  /* required: startup fails with a message. */
  r = make(missing, 1, 0, &err);
  CHECK(r == NULL && err && strstr(err, "required"));
  kavach_free(err);

  /* A recorder that exits at once must not kill us with SIGPIPE or fail a step. */
  const char* quits[] = {"true", NULL};
  logged = 0;
  r = make(quits, 0, 0, &err);
  CHECK(r != NULL);
  struct timespec nap = {0, 200 * 1000000L};
  nanosleep(&nap, NULL);
  for (int i = 0; i < 5; i++) {
    CHECK(step(r, "ok", NULL) == KAVACH_OK);
  }
  CHECK(!kavach_recorder_active(r));
  CHECK(logged >= 1);
  kavach_recorder_free(r);

  /* A failed invariant is reported after an ok step, with its detail. */
  r = make(missing, 0, 1, &err);
  CHECK(step(r, "ok", &res) == KAVACH_INVARIANT);
  CHECK(strcmp(res.message, "always_broken") == 0 && strcmp(res.detail, "because") == 0);
  kavach_result_clear(&res);
  kavach_recorder_free(r);

  if (failures) {
    fprintf(stderr, "%d check(s) failed\n", failures);
    return 1;
  }
  puts("ok");
  return 0;
}

/* Recorder step overhead, the C counterpart of BenchmarkRecorderStep in
 * replay/replay_test.go: one clock read, one 8-byte random read, one emit per
 * step, a 33-byte input, 4 events per step (input, clock, rand, output).
 *   bench-step <kavach-recorder> pipe|ring [steps] [runs]
 * Prints ns/event for each run and their median. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#include "kavach/kavach.h"

static int handle(void* state, kavach_env* env, const kavach_input* in) {
  (void)state;
  kavach_now_ns(env);
  uint8_t id[8];
  kavach_random(env, id, sizeof id);
  return kavach_emit(env, "entries", in->data, in->len, KAVACH_REMOTE);
}

static int snapshot(void* state, uint8_t** data, size_t* len) {
  (void)state;
  *data = malloc(2);
  memcpy(*data, "{}", 2);
  *len = 2;
  return KAVACH_OK;
}

static int restore(void* state, const uint8_t* data, size_t len) {
  (void)state;
  (void)data;
  (void)len;
  return KAVACH_OK;
}

/* Seeded, so every run records the same bytes. */
static uint64_t rng = 1;
static int random_bytes(void* user, void* buf, size_t n) {
  (void)user;
  for (size_t i = 0; i < n; i++) {
    rng = rng * 6364136223846793005ULL + 1442695040888963407ULL;
    ((uint8_t*)buf)[i] = (uint8_t)(rng >> 56);
  }
  return 0;
}

static int cmp(const void* a, const void* b) {
  double x = *(const double*)a, y = *(const double*)b;
  return (x > y) - (x < y);
}

int main(int argc, char** argv) {
  if (argc < 3) {
    fprintf(stderr, "usage: %s kavach-recorder pipe|ring [steps] [runs]\n", argv[0]);
    return 2;
  }
  long steps = argc > 3 ? atol(argv[3]) : 200000;
  int runs = argc > 4 ? atoi(argv[4]) : 5;
  if (runs < 1 || runs > 64) return 2;
  double ns[64];
  for (int run = 0; run < runs; run++) {
    char dir[] = "/tmp/kavach-bench-XXXXXX";
    if (!mkdtemp(dir)) return 1;
    const char* rargv[] = {argv[1], NULL};
    kavach_handler h = {0};
    h.handle = handle;
    h.snapshot = snapshot;
    h.restore = restore;
    kavach_recorder_options o = {0};
    o.service = "bench";
    o.recorder_argv = rargv;
    o.dir = dir;
    o.required = 1;
    o.no_ring = strcmp(argv[2], "pipe") == 0;
    o.random_bytes = random_bytes;
    kavach_recorder* r;
    char* err = NULL;
    if (kavach_recorder_new(&h, &o, &r, &err) != KAVACH_OK) {
      fprintf(stderr, "%s\n", err);
      return 1;
    }
    static const uint8_t data[33] = "0123456789abcdef0123456789abcdef";
    kavach_input in = {"bench", "0", data, sizeof data};
    struct timespec t0, t1;
    clock_gettime(CLOCK_MONOTONIC, &t0);
    for (long i = 0; i < steps; i++) kavach_recorder_step(r, &in, NULL);
    clock_gettime(CLOCK_MONOTONIC, &t1);
    kavach_recorder_free(r);
    ns[run] = ((double)(t1.tv_sec - t0.tv_sec) * 1e9 + (double)(t1.tv_nsec - t0.tv_nsec)) / ((double)steps * 4);
    printf("run %d: %.1f ns/event\n", run + 1, ns[run]);
  }
  qsort(ns, (size_t)runs, sizeof ns[0], cmp);
  printf("%s median: %.1f ns/event\n", argv[2], ns[runs / 2]);
  return 0;
}

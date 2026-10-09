/* Tests against the real kavach-recorder (argv[1]), over the shared-memory ring
 * (SPEC 10.7) and the pipe. */
#include <glob.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#include "kavach/kavach.h"

static int failures;
#define CHECK(c)                                                              \
  do {                                                                        \
    if (!(c)) {                                                               \
      fprintf(stderr, "%s:%d: check failed: %s\n", __FILE__, __LINE__, #c);   \
      failures++;                                                             \
    }                                                                         \
  } while (0)

static void nap_ms(long ms) {
  struct timespec ts = {ms / 1000, (ms % 1000) * 1000000L};
  nanosleep(&ts, NULL);
}

static double now_s(void) {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
}

/* Kills its own process, as an abort or the OOM killer would, once the step's
 * input is on record. */
static int killer(void* state, kavach_env* env, const kavach_input* in) {
  (void)state;
  if (in->len == 3 && memcmp(in->data, "die", 3) == 0) {
    kill(getpid(), SIGKILL);
    for (;;) pause();
  }
  kavach_now_ns(env);
  return KAVACH_OK;
}

static int sink(void* state, kavach_env* env, const kavach_input* in) {
  (void)state;
  return kavach_emit(env, "out", in->data, in->len, KAVACH_REMOTE);
}

static kavach_recorder* start(const char* const* argv, int (*fn)(void*, kavach_env*, const kavach_input*),
                              const char* dir, int no_ring, uint64_t ring_bytes) {
  kavach_handler h;
  memset(&h, 0, sizeof h);
  h.handle = fn;
  kavach_recorder_options o;
  memset(&o, 0, sizeof o);
  o.service = "ringtest";
  o.recorder_argv = argv;
  o.dir = dir;
  o.compression = "none"; /* so the tests can look for strings in the files */
  o.no_ring = no_ring;
  o.ring_bytes = ring_bytes;
  kavach_recorder* r = NULL;
  kavach_recorder_new(&h, &o, &r, NULL);
  return r;
}

static int step(kavach_recorder* r, long pos, const void* data, size_t len) {
  char p[32];
  snprintf(p, sizeof p, "%ld", pos);
  kavach_input in = {"t", p, data, len};
  return kavach_recorder_step(r, &in, NULL);
}

static int file_has(const char* path, const char* needle) {
  FILE* f = fopen(path, "rb");
  if (!f) return 0;
  fseek(f, 0, SEEK_END);
  long n = ftell(f);
  rewind(f);
  char* b = malloc((size_t)n + 1);
  size_t got = b ? fread(b, 1, (size_t)n, f) : 0;
  fclose(f);
  int found = 0;
  size_t nl = strlen(needle);
  for (size_t i = 0; i + nl <= got && !found; i++) found = memcmp(b + i, needle, nl) == 0;
  free(b);
  return found;
}

/* A service killed right after publishing a step's input still leaves a crash
 * marker and a fixture. */
static void killed_service(const char* recorder, int no_ring) {
  char dir[] = "/tmp/kavach-ring-test-XXXXXX";
  CHECK(mkdtemp(dir));
  pid_t pid = fork();
  if (pid == 0) {
    const char* argv[] = {recorder, NULL};
    kavach_recorder* r = start(argv, killer, dir, no_ring, 0);
    step(r, 0, "fine", 4);
    step(r, 1, "die", 3);
    _exit(0);
  }
  int st = 0;
  waitpid(pid, &st, 0);
  CHECK(WIFSIGNALED(st) && WTERMSIG(st) == SIGKILL);

  /* The recorder is no child of ours: it finishes on its own. */
  char pat[256];
  snprintf(pat, sizeof pat, "%s/fixtures/*.kavach", dir);
  glob_t g;
  int found = 0;
  for (int i = 0; i < 500 && !found; i++) {
    found = glob(pat, 0, NULL, &g) == 0 && g.gl_pathc == 1;
    if (!found) {
      globfree(&g);
      nap_ms(20);
    }
  }
  CHECK(found);
  if (found) {
    CHECK(file_has(g.gl_pathv[0], "process ended during step"));
    CHECK(file_has(g.gl_pathv[0], "die"));
    globfree(&g);
  }
}

/* Frames larger than the ring, and a ring that keeps filling, lose nothing. */
static void small_ring(const char* recorder) {
  char dir[] = "/tmp/kavach-ring-test-XXXXXX";
  CHECK(mkdtemp(dir));
  const char* argv[] = {recorder, NULL};
  kavach_recorder* r = start(argv, sink, dir, 0, 64 << 10);
  size_t big_len = 200 << 10;
  char* big = malloc(big_len);
  memset(big, 'b', big_len);
  const int steps = 400;
  for (int i = 0; i < steps; i++) {
    if (i % 100 == 7)
      CHECK(step(r, i, big, big_len) == KAVACH_OK);
    else
      CHECK(step(r, i, "small", 5) == KAVACH_OK);
  }
  CHECK(kavach_recorder_close(r) == KAVACH_OK);
  kavach_recorder_free(r);
  free(big);
  char pat[256];
  snprintf(pat, sizeof pat, "%s/*.kavach", dir);
  glob_t g;
  CHECK(glob(pat, 0, NULL, &g) == 0 && g.gl_pathc == 1);
  if (g.gl_pathc == 1) {
    struct stat sb;
    CHECK(stat(g.gl_pathv[0], &sb) == 0);
    /* every large input is twice in the journal: as input and as output */
    CHECK((size_t)sb.st_size > 4 * 2 * big_len);
  }
  globfree(&g);
}

/* A recorder that stops reading while the ring is full does not hang the
 * service: recording stops (SPEC 10.1). */
static void dead_recorder(void) {
  const char* argv[] = {"sleep", "1", NULL};
  kavach_recorder* r = start(argv, sink, "/tmp/kavach-ring-test-unused", 0, 64 << 10);
  char* data = malloc(30 << 10);
  memset(data, 'x', 30 << 10);
  for (int i = 0; i < 20; i++) step(r, i, data, 30 << 10);
  CHECK(!kavach_recorder_active(r));
  free(data);
  kavach_recorder_free(r);
}

/* A recorder that never says ready delays construction once, by the startup
 * bound, and steps after that do not wait (SPEC 10.1). */
static void silent_recorder(void) {
  const char* argv[] = {"sleep", "4", NULL};
  double t0 = now_s();
  kavach_recorder* r = start(argv, sink, "/tmp/kavach-ring-test-unused", 0, 0);
  double d = now_s() - t0;
  CHECK(d > 1.0 && d < 3.0);
  t0 = now_s();
  for (int i = 0; i < 100; i++) step(r, i, "x", 1);
  CHECK(now_s() - t0 < 0.5);
  kavach_recorder_free(r);
}

int main(int argc, char** argv) {
  if (argc != 2) {
    fprintf(stderr, "usage: %s kavach-recorder\n", argv[0]);
    return 2;
  }
  killed_service(argv[1], 0);
  killed_service(argv[1], 1);
  small_ring(argv[1]);
  dead_recorder();
  silent_recorder();
  if (failures) {
    fprintf(stderr, "%d check(s) failed\n", failures);
    return 1;
  }
  puts("ok");
  return 0;
}

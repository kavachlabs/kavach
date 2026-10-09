/* The SDK half of the flight recorder (SPEC 3.6 and 10). */
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <spawn.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#include "json.h"
#include "util.h"

extern char** environ;

#define FRAME_OPEN 0x01
#define FRAME_RECORD 0x02
#define FRAME_STEP_END 0x03
#define FRAME_FACTS 0x04
#define FRAME_SNAPSHOT 0x05
#define FRAME_FLUSH 0x06
#define FRAME_CLOSE 0x07

#define REC_INPUT 0x01
#define REC_CLOCK 0x02
#define REC_RAND 0x03
#define REC_OUTPUT 0x04
#define REC_MARKER 0x05
#define REC_GATEWAY 0x07
#define REC_CONFIG 0x09

#define FLAG_CRITICAL 1

typedef struct out_item {
  char* sink;
  uint8_t* data;
  size_t len;
  kavach_scope scope;
} out_item;

struct kavach_recorder {
  kavach_handler h;
  kavach_recorder_options o; /* shallow copy; only the members copied below are used later */
  kavach_gateway* gws;
  size_t n_gws;
  char* config_source;

  pid_t pid;
  int in_fd, out_fd;
  pthread_t thr;
  int thr_started;

  pthread_mutex_t smu; /* serialises steps, flush and close: frames never interleave */
  pthread_mutex_t wmu; /* one writer on the pipe at a time */
  pthread_mutex_t mu;  /* control state below */
  pthread_cond_t cv;
  int ready, got_closed, ctl_eof;
  unsigned long durable_n;

  atomic_int active;
  atomic_int snap_req;
  int closed;
  int close_rc;

  /* per-step scratch (guarded by smu) */
  kbuf sb, tmp, fb;
  int rec_step; /* this step's frames are being gathered */
  out_item* outs;
  size_t n_outs, cap_outs;
  struct kavach_env env;
};

/* ---------------- logging ---------------- */

static void rec_logf(kavach_recorder* r, const char* fmt, ...) KAVACH_PRINTF(2, 3);
static void rec_logf(kavach_recorder* r, const char* fmt, ...) {
  char msg[1024];
  va_list ap;
  va_start(ap, fmt);
  vsnprintf(msg, sizeof msg, fmt, ap);
  va_end(ap);
  if (r->o.log)
    r->o.log(r->o.log_user, msg);
  else
    fprintf(stderr, "kavach: %s\n", msg);
}

static void stop_recording(kavach_recorder* r, const char* why) {
  if (atomic_exchange(&r->active, 0)) rec_logf(r, "recording stopped: %s", why);
  pthread_mutex_lock(&r->mu);
  pthread_cond_broadcast(&r->cv);
  pthread_mutex_unlock(&r->mu);
}

/* ---------------- writing to the pipe ---------------- */

static int write_all(int fd, const uint8_t* p, size_t n) {
#ifndef F_SETNOSIGPIPE
  sigset_t set, old, pend;
  sigemptyset(&set);
  sigaddset(&set, SIGPIPE);
  pthread_sigmask(SIG_BLOCK, &set, &old);
  int was_pending = sigpending(&pend) == 0 && sigismember(&pend, SIGPIPE) == 1;
#endif
  int err = 0;
  while (n > 0) {
    ssize_t w = write(fd, p, n);
    if (w < 0) {
      if (errno == EINTR) continue;
      err = errno;
      break;
    }
    p += w;
    n -= (size_t)w;
  }
#ifndef F_SETNOSIGPIPE
  if (err == EPIPE && !was_pending) {
#ifdef __linux__
    struct timespec zero = {0, 0};
    sigtimedwait(&set, NULL, &zero);
#else
    int sig;
    sigwait(&set, &sig);
#endif
  }
  pthread_sigmask(SIG_SETMASK, &old, NULL);
#endif
  if (err) errno = err;
  return err ? -1 : 0;
}

static int pipe_write(kavach_recorder* r, const void* p, size_t n) {
  if (!atomic_load(&r->active)) return -1;
  pthread_mutex_lock(&r->wmu);
  int rc = write_all(r->in_fd, p, n);
  int e = errno;
  pthread_mutex_unlock(&r->wmu);
  if (rc != 0) {
    char why[160];
    snprintf(why, sizeof why, "writing to the recorder failed: %s", strerror(e));
    stop_recording(r, why);
    return -1;
  }
  return 0;
}

/* Appends frame = uvarint(1+len) kind payload to b. */
static void put_frame(kbuf* b, uint8_t kind, const void* payload, size_t len) {
  kb_uvarint(b, 1 + len);
  kb_u8(b, kind);
  kb_put(b, payload, len);
}

static int send_frame(kavach_recorder* r, uint8_t kind, const void* payload, size_t len) {
  kb_clear(&r->fb);
  put_frame(&r->fb, kind, payload, len);
  if (r->fb.oom) {
    stop_recording(r, "out of memory");
    return -1;
  }
  return pipe_write(r, r->fb.p, r->fb.len);
}

/* Appends a record frame to the step buffer. r->tmp holds the payload. */
static void add_record(kavach_recorder* r, uint8_t type, uint8_t flags) {
  if (!r->rec_step) return;
  kb_uvarint(&r->sb, 3 + r->tmp.len);
  kb_u8(&r->sb, FRAME_RECORD);
  kb_u8(&r->sb, type);
  kb_u8(&r->sb, flags);
  kb_put(&r->sb, r->tmp.p, r->tmp.len);
}

/* ---------------- env operations while recording ---------------- */

static int op_now(kavach_env* e, int64_t* out) {
  kavach_recorder* r = e->ctx;
  int64_t v = r->o.clock_ns ? r->o.clock_ns(r->o.clock_user) : k_realtime_ns();
  kb_clear(&r->tmp);
  kb_i64le(&r->tmp, v);
  add_record(r, REC_CLOCK, 0);
  *out = v;
  return KAVACH_OK;
}

static int op_rand(kavach_env* e, void* buf, size_t n) {
  kavach_recorder* r = e->ctx;
  int rc = r->o.random_bytes ? r->o.random_bytes(r->o.random_user, buf, n) : k_os_random(buf, n);
  if (rc != 0) return KAVACH_ERROR;
  kb_clear(&r->tmp);
  kb_bytes(&r->tmp, buf, n);
  add_record(r, REC_RAND, 0);
  return KAVACH_OK;
}

static const kavach_gateway* find_gateway(kavach_recorder* r, const char* name) {
  const kavach_gateway* any = NULL;
  for (size_t i = 0; i < r->n_gws; i++) {
    if (strcmp(r->gws[i].name, name) == 0) return &r->gws[i];
    if (strcmp(r->gws[i].name, "*") == 0) any = &r->gws[i];
  }
  return any;
}

static int op_query(kavach_env* e, const char* name, const void* req, size_t req_len, uint8_t** resp,
                    size_t* resp_len, char** err) {
  kavach_recorder* r = e->ctx;
  const kavach_gateway* g = find_gateway(r, name);
  uint8_t* rb = NULL;
  size_t rl = 0;
  char* em = NULL;
  int rc;
  if (!g || !g->fn) {
    char msg[256];
    snprintf(msg, sizeof msg, "kavach: no gateway %s registered", name);
    em = k_strdup(msg);
    rc = KAVACH_QUERY_FAILED;
  } else {
    rc = g->fn(g->user, name, req, req_len, &rb, &rl, &em);
    if (rc != KAVACH_OK) {
      free(rb);
      rb = NULL;
      rl = 0;
      rc = KAVACH_QUERY_FAILED;
      if (!em || !*em) {
        free(em);
        em = k_strdup("query failed");
      }
    } else {
      free(em);
      em = NULL;
      if (!rb) rl = 0;
    }
  }
  kb_clear(&r->tmp);
  kb_string(&r->tmp, name);
  kb_bytes(&r->tmp, req, req_len);
  kb_bytes(&r->tmp, rb, rl);
  kb_string(&r->tmp, em);
  kb_u8(&r->tmp, (uint8_t)(g ? g->scope : KAVACH_REMOTE));
  add_record(r, REC_GATEWAY, FLAG_CRITICAL);
  *resp = rb;
  *resp_len = rl;
  *err = em;
  return rc;
}

static int op_config(kavach_env* e, const char* key, uint8_t** val, size_t* len, int* present) {
  kavach_recorder* r = e->ctx;
  uint8_t* v = NULL;
  size_t l = 0;
  int p = 0;
  if (r->o.config) {
    int rc = r->o.config(r->o.config_user, key, &v, &l, &p);
    if (rc != KAVACH_OK) {
      rec_logf(r, "config provider failed for %s; treating it as unset", key);
      free(v);
      v = NULL;
      l = 0;
      p = 0;
    }
  }
  if (!p) {
    free(v);
    v = NULL;
    l = 0;
  }
  if (!v) l = 0;
  kb_clear(&r->tmp);
  kb_string(&r->tmp, key);
  kb_u8(&r->tmp, p ? 1 : 0);
  kb_bytes(&r->tmp, v, l);
  kb_string(&r->tmp, r->config_source);
  add_record(r, REC_CONFIG, FLAG_CRITICAL);
  *val = v;
  *len = l;
  *present = p ? 1 : 0;
  return KAVACH_OK;
}

static int op_emit(kavach_env* e, const char* sink, const void* data, size_t len, kavach_scope scope) {
  kavach_recorder* r = e->ctx;
  kb_clear(&r->tmp);
  kb_string(&r->tmp, sink);
  kb_bytes(&r->tmp, data, len);
  kb_u8(&r->tmp, (uint8_t)scope);
  add_record(r, REC_OUTPUT, 0);
  if (r->n_outs == r->cap_outs) {
    size_t nc = r->cap_outs ? r->cap_outs * 2 : 8;
    out_item* no = realloc(r->outs, nc * sizeof *no);
    if (!no) return KAVACH_ERROR;
    r->outs = no;
    r->cap_outs = nc;
  }
  out_item* o = &r->outs[r->n_outs];
  o->sink = k_strdup(sink ? sink : "");
  o->data = k_memdup(data, len);
  o->len = len;
  o->scope = scope;
  if (!o->sink || (len && !o->data)) {
    free(o->sink);
    free(o->data);
    return KAVACH_ERROR;
  }
  r->n_outs++;
  return KAVACH_OK;
}

static const env_ops REC_OPS = {op_now, op_rand, op_query, op_config, op_emit};

static void clear_outs(kavach_recorder* r) {
  for (size_t i = 0; i < r->n_outs; i++) {
    free(r->outs[i].sink);
    free(r->outs[i].data);
  }
  r->n_outs = 0;
}

/* ---------------- control stream thread ---------------- */

static void handle_control(kavach_recorder* r, const char* line, size_t n) {
  char err[96];
  kj* m = kj_parse(line, n, err, sizeof err);
  if (!m) {
    rec_logf(r, "recorder sent an unreadable control message: %s", err);
    return;
  }
  const char* t = kj_str(kj_get(m, "t"));
  if (!t) {
    kj_free(m);
    return;
  }
  if (strcmp(t, "ready") == 0) {
    pthread_mutex_lock(&r->mu);
    r->ready = 1;
    pthread_cond_broadcast(&r->cv);
    pthread_mutex_unlock(&r->mu);
  } else if (strcmp(t, "snapshot_request") == 0) {
    atomic_store(&r->snap_req, 1);
  } else if (strcmp(t, "durable") == 0) {
    pthread_mutex_lock(&r->mu);
    r->durable_n++;
    pthread_cond_broadcast(&r->cv);
    pthread_mutex_unlock(&r->mu);
  } else if (strcmp(t, "fixture") == 0) {
    const char* file = kj_str(kj_get(m, "file"));
    const char* seq = kj_str(kj_get(m, "seq"));
    const char* failure = kj_str(kj_get(m, "failure"));
    rec_logf(r, "wrote fixture %s (input %s, %s)", file ? file : "?", seq ? seq : "?",
             failure ? failure : "?");
  } else if (strcmp(t, "error") == 0) {
    const char* msg = kj_str(kj_get(m, "message"));
    const kj* fatal = kj_get(m, "fatal");
    rec_logf(r, "recorder %s: %s", kj_is_true(fatal) ? "fatal error" : "error", msg ? msg : "?");
    if (kj_is_true(fatal)) stop_recording(r, "the recorder reported a fatal error");
  } else if (strcmp(t, "closed") == 0) {
    pthread_mutex_lock(&r->mu);
    r->got_closed = 1;
    pthread_cond_broadcast(&r->cv);
    pthread_mutex_unlock(&r->mu);
  }
  kj_free(m);
}

static void* control_main(void* arg) {
  kavach_recorder* r = arg;
  kbuf acc = {0};
  char chunk[4096];
  for (;;) {
    ssize_t n = read(r->out_fd, chunk, sizeof chunk);
    if (n < 0 && errno == EINTR) continue;
    if (n <= 0) break;
    kb_put(&acc, chunk, (size_t)n);
    size_t start = 0;
    for (size_t i = 0; i < acc.len; i++) {
      if (acc.p[i] == '\n') {
        if (i > start) handle_control(r, (const char*)acc.p + start, i - start);
        start = i + 1;
      }
    }
    if (start > 0) {
      memmove(acc.p, acc.p + start, acc.len - start);
      acc.len -= start;
    }
    if (acc.len > (16u << 20)) kb_clear(&acc); /* runaway line: drop it */
  }
  kb_free(&acc);
  pthread_mutex_lock(&r->mu);
  r->ctl_eof = 1;
  pthread_cond_broadcast(&r->cv);
  pthread_mutex_unlock(&r->mu);
  if (!r->closed && atomic_load(&r->active)) stop_recording(r, "the recorder exited unexpectedly");
  return NULL;
}

/* Waits on cv until pred() or the timeout; the caller holds r->mu. */
static void deadline(struct timespec* ts, int ms) {
  clock_gettime(CLOCK_REALTIME, ts);
  ts->tv_sec += ms / 1000;
  ts->tv_nsec += (long)(ms % 1000) * 1000000L;
  if (ts->tv_nsec >= 1000000000L) {
    ts->tv_sec++;
    ts->tv_nsec -= 1000000000L;
  }
}

/* ---------------- starting the recorder ---------------- */

static int fd_high(int fd) {
  if (fd > 2) return fd;
  int n = fcntl(fd, F_DUPFD_CLOEXEC, 3);
  if (n >= 0) close(fd);
  return n;
}

static int spawn_recorder(kavach_recorder* r, const char* const* argv_opt, char* errbuf, size_t errcap) {
  const char* const* argv = argv_opt;
  const char* single[2] = {NULL, NULL};
  if (!argv || !argv[0]) {
    const char* env = getenv("KAVACH_RECORDER");
    single[0] = env && *env ? env : "kavach-recorder";
    argv = single;
  }
  int in_p[2], out_p[2];
  if (pipe(in_p) != 0) {
    snprintf(errbuf, errcap, "pipe: %s", strerror(errno));
    return -1;
  }
  if (pipe(out_p) != 0) {
    snprintf(errbuf, errcap, "pipe: %s", strerror(errno));
    close(in_p[0]);
    close(in_p[1]);
    return -1;
  }
  in_p[0] = fd_high(in_p[0]);
  in_p[1] = fd_high(in_p[1]);
  out_p[0] = fd_high(out_p[0]);
  out_p[1] = fd_high(out_p[1]);
  if (in_p[0] < 0 || in_p[1] < 0 || out_p[0] < 0 || out_p[1] < 0) {
    snprintf(errbuf, errcap, "cannot allocate file descriptors");
    return -1;
  }
  fcntl(in_p[1], F_SETFD, FD_CLOEXEC);
  fcntl(out_p[0], F_SETFD, FD_CLOEXEC);
#ifdef F_SETNOSIGPIPE
  fcntl(in_p[1], F_SETNOSIGPIPE, 1);
#endif
#ifdef F_SETPIPE_SZ
  fcntl(in_p[1], F_SETPIPE_SZ, 1 << 20);
#endif

  posix_spawn_file_actions_t fa;
  posix_spawn_file_actions_init(&fa);
  posix_spawn_file_actions_adddup2(&fa, in_p[0], 0);
  posix_spawn_file_actions_adddup2(&fa, out_p[1], 1);
  posix_spawn_file_actions_addclose(&fa, in_p[0]);
  posix_spawn_file_actions_addclose(&fa, out_p[1]);
  pid_t pid;
  int rc = posix_spawnp(&pid, argv[0], &fa, NULL, (char* const*)argv, environ);
  posix_spawn_file_actions_destroy(&fa);
  close(in_p[0]);
  close(out_p[1]);
  if (rc != 0) {
    snprintf(errbuf, errcap, "cannot start %s: %s", argv[0], strerror(rc));
    close(in_p[1]);
    close(out_p[0]);
    return -1;
  }
  r->pid = pid;
  r->in_fd = in_p[1];
  r->out_fd = out_p[0];
  return 0;
}

static void put_fact(kbuf* b, const char* key, const void* v, size_t n) {
  kb_string(b, key);
  kb_u8(b, 0);
  kb_bytes(b, v, n);
}

static int send_open(kavach_recorder* r, const kavach_recorder_options* o) {
  kbuf j = {0};
  kb_puts(&j, "{\"protocol\":1,\"service\":");
  kb_json_cstr(&j, o->service);
  kb_puts(&j, o->start_from_snapshot ? ",\"start\":\"snapshot\"" : ",\"start\":\"genesis\"");
  kb_puts(&j, ",\"producer\":");
  kb_json_cstr(&j, o->producer ? o->producer : "kavach-c/" KAVACH_VERSION);
  if (o->handler_id) {
    kb_puts(&j, ",\"handler\":");
    kb_json_cstr(&j, o->handler_id);
  }
  kb_printf(&j, ",\"snapshots\":%s", (r->h.snapshot && !o->no_segments) ? "true" : "false");
  if (o->dir) {
    kb_puts(&j, ",\"dir\":");
    kb_json_cstr(&j, o->dir);
  }
  if (o->compression) {
    kb_puts(&j, ",\"compression\":");
    kb_json_cstr(&j, o->compression);
  }
  if (o->level) kb_printf(&j, ",\"level\":%d", o->level);
  if (o->block_bytes) kb_printf(&j, ",\"block_bytes\":%llu", (unsigned long long)o->block_bytes);
  if (o->flush_ms) kb_printf(&j, ",\"flush_ms\":%llu", (unsigned long long)o->flush_ms);
  if (o->segment_bytes) kb_printf(&j, ",\"segment_bytes\":%llu", (unsigned long long)o->segment_bytes);
  if (o->segment_seconds)
    kb_printf(&j, ",\"segment_seconds\":%llu", (unsigned long long)o->segment_seconds);
  if (o->retain_segments)
    kb_printf(&j, ",\"retain_segments\":%llu", (unsigned long long)o->retain_segments);
  if (o->secret_keys && o->secret_keys[0]) {
    kb_puts(&j, ",\"secret_keys\":[");
    for (size_t i = 0; o->secret_keys[i]; i++) {
      if (i) kb_putc(&j, ',');
      kb_json_cstr(&j, o->secret_keys[i]);
    }
    kb_putc(&j, ']');
  }
  kb_putc(&j, '}');
  int rc = j.oom ? -1 : send_frame(r, FRAME_OPEN, j.p, j.len);
  kb_free(&j);
  return rc;
}

static int send_facts(kavach_recorder* r, const kavach_recorder_options* o) {
  const kavach_flag* flags = NULL;
  size_t nf = o->flags ? o->flags(o->flags_user, &flags) : 0;
  if (!flags) nf = 0;
  kbuf p = {0};
  kb_uvarint(&p, 1 + nf);
  const char* rt = o->runtime ? o->runtime : KAVACH_RUNTIME;
  put_fact(&p, "host.runtime", rt, strlen(rt));
  for (size_t i = 0; i < nf; i++) put_fact(&p, flags[i].key, flags[i].value, flags[i].len);
  int rc = p.oom ? -1 : send_frame(r, FRAME_FACTS, p.p, p.len);
  kb_free(&p);
  return rc;
}

static int send_snapshot(kavach_recorder* r) {
  uint8_t* data = NULL;
  size_t len = 0;
  if (r->h.snapshot(r->h.state, &data, &len) != KAVACH_OK) {
    free(data);
    return -1;
  }
  kbuf p = {0};
  kb_bytes(&p, data, data ? len : 0);
  free(data);
  int rc = p.oom ? -1 : send_frame(r, FRAME_SNAPSHOT, p.p, p.len);
  kb_free(&p);
  return rc;
}

static void teardown(kavach_recorder* r) {
  if (r->in_fd >= 0) close(r->in_fd);
  r->in_fd = -1;
  if (r->thr_started) {
    pthread_join(r->thr, NULL);
    r->thr_started = 0;
  }
  if (r->out_fd >= 0) close(r->out_fd);
  r->out_fd = -1;
}

static void destroy(kavach_recorder* r) {
  clear_outs(r);
  free(r->outs);
  kb_free(&r->sb);
  kb_free(&r->tmp);
  kb_free(&r->fb);
  k_env_reset(&r->env);
  for (size_t i = 0; i < r->n_gws; i++) free((char*)r->gws[i].name);
  free(r->gws);
  free(r->config_source);
  pthread_mutex_destroy(&r->smu);
  pthread_mutex_destroy(&r->wmu);
  pthread_mutex_destroy(&r->mu);
  pthread_cond_destroy(&r->cv);
  free(r);
}

static int fail_new(char** err, const char* msg) {
  if (err) *err = k_strdup(msg);
  return KAVACH_ERROR;
}

int kavach_recorder_new(const kavach_handler* h, const kavach_recorder_options* o, kavach_recorder** out,
                        char** err) {
  if (err) *err = NULL;
  if (!out) return fail_new(err, "kavach_recorder_new: out is NULL");
  *out = NULL;
  if (!h || !h->handle) return fail_new(err, "kavach_recorder_new: handler has no handle function");
  if (!o || !o->service || !*o->service) return fail_new(err, "kavach_recorder_new: service is required");
  if (o->start_from_snapshot && !h->snapshot)
    return fail_new(err, "kavach_recorder_new: start_from_snapshot needs a snapshot function");

  kavach_recorder* r = calloc(1, sizeof *r);
  if (!r) return fail_new(err, "out of memory");
  r->h = *h;
  r->o = *o;
  r->pid = -1;
  r->in_fd = r->out_fd = -1;
  pthread_mutex_init(&r->smu, NULL);
  pthread_mutex_init(&r->wmu, NULL);
  pthread_mutex_init(&r->mu, NULL);
  pthread_cond_init(&r->cv, NULL);
  atomic_init(&r->active, 0);
  atomic_init(&r->snap_req, 0);
  k_env_init(&r->env, &REC_OPS, r, !!(h->flags & KAVACH_HANDLER_NOJUMP));
  r->config_source = k_strdup(o->config_source ? o->config_source : "config");
  if (o->n_gateways) {
    r->gws = calloc(o->n_gateways, sizeof *r->gws);
    if (!r->gws || !r->config_source) {
      destroy(r);
      return fail_new(err, "out of memory");
    }
    for (size_t i = 0; i < o->n_gateways; i++) {
      r->gws[i] = o->gateways[i];
      r->gws[i].name = k_strdup(o->gateways[i].name ? o->gateways[i].name : "");
      r->n_gws++;
    }
  }
  r->o.gateways = NULL;
  r->o.recorder_argv = NULL;
  r->o.secret_keys = NULL;
  r->o.dir = r->o.compression = r->o.handler_id = r->o.service = NULL;
  r->o.producer = NULL;
  r->o.runtime = NULL;
  r->o.config_source = NULL;

  char why[320];
  why[0] = 0;
  if (spawn_recorder(r, o->recorder_argv, why, sizeof why) != 0) goto unavailable;
  atomic_store(&r->active, 1);
  if (pthread_create(&r->thr, NULL, control_main, r) != 0) {
    snprintf(why, sizeof why, "cannot start the control thread");
    goto unavailable_started;
  }
  r->thr_started = 1;

  if (send_open(r, o) != 0 || send_facts(r, o) != 0) goto write_failed;
  if (o->start_from_snapshot && send_snapshot(r) != 0) {
    snprintf(why, sizeof why, "could not take the starting snapshot");
    goto unavailable_started;
  }

  if (o->required) {
    int ms = o->startup_timeout_ms > 0 ? o->startup_timeout_ms : 5000;
    struct timespec ts;
    deadline(&ts, ms);
    pthread_mutex_lock(&r->mu);
    while (!r->ready && !r->ctl_eof && atomic_load(&r->active)) {
      if (pthread_cond_timedwait(&r->cv, &r->mu, &ts) == ETIMEDOUT) break;
    }
    int ok = r->ready && atomic_load(&r->active);
    pthread_mutex_unlock(&r->mu);
    if (!ok) {
      snprintf(why, sizeof why, "the recorder did not become ready");
      goto unavailable_started;
    }
  }
  *out = r;
  return KAVACH_OK;

write_failed:
  snprintf(why, sizeof why, "could not write to the recorder");
unavailable_started:
  atomic_store(&r->active, 0);
  if (r->pid > 0) kill(r->pid, SIGKILL);
  teardown(r);
  if (r->pid > 0) waitpid(r->pid, NULL, 0);
  r->pid = -1;
unavailable:
  if (o->required) {
    char msg[400];
    snprintf(msg, sizeof msg, "kavach: recording is required but unavailable: %s", why);
    destroy(r);
    return fail_new(err, msg);
  }
  rec_logf(r, "recording is unavailable, the service runs unrecorded: %s", why);
  *out = r;
  return KAVACH_OK;
}

/* ---------------- steps ---------------- */

static void snapshot_if_requested(kavach_recorder* r) {
  if (!atomic_exchange(&r->snap_req, 0)) return;
  if (!atomic_load(&r->active) || !r->h.snapshot || r->o.no_segments) return;
  if (send_snapshot(r) != 0 && atomic_load(&r->active))
    rec_logf(r, "the handler could not take a snapshot; the recorder keeps the current segment");
}

static void add_marker(kavach_recorder* r, const char* kind, const char* msg, const char* detail) {
  kb_clear(&r->tmp);
  kb_string(&r->tmp, kind);
  kb_string(&r->tmp, msg);
  kb_string(&r->tmp, detail);
  add_record(r, REC_MARKER, 0);
}

int kavach_recorder_step(kavach_recorder* r, const kavach_input* in, kavach_result* result) {
  if (result) memset(result, 0, sizeof *result);
  pthread_mutex_lock(&r->smu);
  snapshot_if_requested(r);

  kb_clear(&r->sb);
  clear_outs(r);
  r->rec_step = 0;
  if (atomic_load(&r->active)) {
    kb_clear(&r->tmp);
    kb_string(&r->tmp, in->source);
    kb_string(&r->tmp, in->position);
    kb_bytes(&r->tmp, in->data, in->len);
    kb_clear(&r->fb);
    kb_uvarint(&r->fb, 3 + r->tmp.len);
    kb_u8(&r->fb, FRAME_RECORD);
    kb_u8(&r->fb, REC_INPUT);
    kb_u8(&r->fb, 0);
    kb_put(&r->fb, r->tmp.p, r->tmp.len);
    if (r->fb.oom) {
      stop_recording(r, "out of memory");
    } else if (pipe_write(r, r->fb.p, r->fb.len) == 0) {
      r->rec_step = 1; /* the input is on record before the handler runs */
    }
  }

  k_env_reset(&r->env);
  int rc = k_run_handler(&r->h, &r->env, in);
  if (rc == KAVACH_ABORTED) rc = KAVACH_ERROR; /* cannot happen while recording */

  char* msg = NULL;
  char* detail = NULL;
  int outcome = rc;
  if (rc == KAVACH_OK) {
    const char* name;
    if (k_check_invariants(&r->h, &name, &detail)) {
      outcome = KAVACH_INVARIANT;
      msg = k_strdup(name ? name : "invariant");
      add_marker(r, "invariant", name ? name : "invariant", detail);
    }
  } else {
    msg = r->env.fail_msg;
    r->env.fail_msg = NULL;
    add_marker(r, rc == KAVACH_PANIC ? "panic" : "error", msg ? msg : "", "");
  }

  if (r->rec_step) {
    put_frame(&r->sb, FRAME_STEP_END, NULL, 0);
    if (r->sb.oom)
      stop_recording(r, "out of memory");
    else
      pipe_write(r, r->sb.p, r->sb.len);
    r->rec_step = 0;
  }
  k_env_reset(&r->env);

  if (outcome == KAVACH_OK && r->o.deliver && r->n_outs > 0) {
    kavach_output* arr = calloc(r->n_outs, sizeof *arr);
    if (arr) {
      for (size_t i = 0; i < r->n_outs; i++) {
        arr[i].sink = r->outs[i].sink;
        arr[i].data = r->outs[i].data;
        arr[i].len = r->outs[i].len;
        arr[i].scope = r->outs[i].scope;
      }
      if (r->o.deliver(r->o.deliver_user, arr, r->n_outs) != KAVACH_OK) {
        outcome = KAVACH_DELIVER_FAILED;
        msg = k_strdup("deliver failed");
        rec_logf(r, "delivering the step's outputs failed");
      }
      free(arr);
    }
  }
  clear_outs(r);
  pthread_mutex_unlock(&r->smu);

  if (result) {
    result->outcome = outcome;
    result->message = msg;
    result->detail = detail;
  } else {
    free(msg);
    free(detail);
  }
  return outcome;
}

void kavach_result_clear(kavach_result* res) {
  if (!res) return;
  free(res->message);
  free(res->detail);
  memset(res, 0, sizeof *res);
}

int kavach_recorder_active(const kavach_recorder* r) { return r && atomic_load(&r->active); }

/* ---------------- flush, close ---------------- */

int kavach_recorder_flush(kavach_recorder* r, int durable) {
  if (!r || !atomic_load(&r->active)) return KAVACH_ERROR;
  pthread_mutex_lock(&r->smu);
  pthread_mutex_lock(&r->mu);
  unsigned long before = r->durable_n;
  pthread_mutex_unlock(&r->mu);
  uint8_t d = durable ? 1 : 0;
  int rc = send_frame(r, FRAME_FLUSH, &d, 1);
  pthread_mutex_unlock(&r->smu);
  if (rc != 0) return KAVACH_ERROR;
  if (!durable) return KAVACH_OK;
  struct timespec ts;
  deadline(&ts, r->o.flush_timeout_ms > 0 ? r->o.flush_timeout_ms : 10000);
  pthread_mutex_lock(&r->mu);
  while (r->durable_n == before && atomic_load(&r->active) && !r->ctl_eof) {
    if (pthread_cond_timedwait(&r->cv, &r->mu, &ts) == ETIMEDOUT) break;
  }
  int ok = r->durable_n != before;
  pthread_mutex_unlock(&r->mu);
  if (!ok) rec_logf(r, "flush: the recorder did not confirm durability in time");
  return ok ? KAVACH_OK : KAVACH_ERROR;
}

int kavach_recorder_close(kavach_recorder* r) {
  if (!r) return KAVACH_OK;
  pthread_mutex_lock(&r->smu);
  if (r->closed) {
    int rc = r->close_rc;
    pthread_mutex_unlock(&r->smu);
    return rc;
  }
  int rc = KAVACH_OK;
  int ms = r->o.close_timeout_ms > 0 ? r->o.close_timeout_ms : 10000;
  if (r->pid > 0) {
    if (atomic_load(&r->active)) {
      if (send_frame(r, FRAME_CLOSE, NULL, 0) != 0) {
        rc = KAVACH_ERROR;
      } else {
        struct timespec ts;
        deadline(&ts, ms);
        pthread_mutex_lock(&r->mu);
        while (!r->got_closed && !r->ctl_eof) {
          if (pthread_cond_timedwait(&r->cv, &r->mu, &ts) == ETIMEDOUT) break;
        }
        if (!r->got_closed) rc = KAVACH_ERROR;
        pthread_mutex_unlock(&r->mu);
        if (rc != KAVACH_OK) rec_logf(r, "the recorder did not confirm that it closed the journal");
      }
    } else {
      rc = KAVACH_ERROR; /* recording had stopped earlier; the journal is whatever the recorder finished */
    }
    r->closed = 1;
    atomic_store(&r->active, 0);
    close(r->in_fd);
    r->in_fd = -1;
    /* The recorder exits after `closed` or once its input ends. */
    struct timespec ts;
    deadline(&ts, ms);
    pthread_mutex_lock(&r->mu);
    while (!r->ctl_eof) {
      if (pthread_cond_timedwait(&r->cv, &r->mu, &ts) == ETIMEDOUT) break;
    }
    int eof = r->ctl_eof;
    pthread_mutex_unlock(&r->mu);
    if (!eof) {
      rec_logf(r, "the recorder did not exit; killing it");
      kill(r->pid, SIGKILL);
    }
    teardown(r);
    for (int i = 0; i < 200; i++) {
      pid_t w = waitpid(r->pid, NULL, WNOHANG);
      if (w != 0) break;
      struct timespec nap = {0, 10 * 1000000L};
      nanosleep(&nap, NULL);
    }
    r->pid = -1;
  }
  r->closed = 1;
  r->close_rc = rc;
  pthread_mutex_unlock(&r->smu);
  return rc;
}

void kavach_recorder_free(kavach_recorder* r) {
  if (!r) return;
  kavach_recorder_close(r);
  destroy(r);
}

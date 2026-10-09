/* The host side of the host protocol (SPEC 9). */
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

#include "json.h"
#include "util.h"

extern char** environ;

typedef struct local_out {
  char* sink;
  uint8_t* data;
  size_t len;
} local_out;

typedef struct host {
  int in_fd, out_fd;
  kbuf rbuf;
  size_t rpos;
  kbuf out;
  kavach_host_options o;
  kavach_handler_factory factory;
  kavach_handler h;
  int have_h;
  int sandbox;
  struct kavach_env env;
  local_out* locals;
  size_t n_locals, cap_locals;
  char* environment; /* cached ready.environment JSON */
} host;

static host H;

/* ---------------- I/O ---------------- */

static void write_line(host* h) {
  kb_putc(&h->out, '\n');
  if (h->out.oom) _exit(1);
  const uint8_t* p = h->out.p;
  size_t n = h->out.len;
  while (n > 0) {
    ssize_t w = write(h->out_fd, p, n);
    if (w < 0) {
      if (errno == EINTR) continue;
      exit(1); /* the driver went away */
    }
    p += w;
    n -= (size_t)w;
  }
  kb_clear(&h->out);
}

static void fatal(host* h, const char* msg) {
  kb_clear(&h->out);
  kb_puts(&h->out, "{\"t\":\"fatal\",\"message\":");
  kb_json_cstr(&h->out, msg);
  kb_putc(&h->out, '}');
  write_line(h);
  fprintf(stderr, "kavach: host: %s\n", msg);
  exit(1);
}

/* Returns 1 with the next line (valid until the next call), 0 at end of input. */
static int read_line(host* h, const char** line, size_t* len) {
  if (h->rpos > 0) {
    memmove(h->rbuf.p, h->rbuf.p + h->rpos, h->rbuf.len - h->rpos);
    h->rbuf.len -= h->rpos;
    h->rpos = 0;
  }
  size_t scanned = 0;
  for (;;) {
    for (; scanned < h->rbuf.len; scanned++) {
      if (h->rbuf.p[scanned] == '\n') {
        *line = (const char*)h->rbuf.p;
        *len = scanned;
        h->rpos = scanned + 1;
        return 1;
      }
    }
    char chunk[8192];
    ssize_t n = read(h->in_fd, chunk, sizeof chunk);
    if (n < 0 && errno == EINTR) continue;
    if (n <= 0) return 0;
    kb_put(&h->rbuf, chunk, (size_t)n);
    if (h->rbuf.oom) fatal(h, "out of memory");
  }
}

/* Reads and parses the next message; NULL at end of input. */
static kj* next_message(host* h) {
  const char* line;
  size_t len;
  if (!read_line(h, &line, &len)) return NULL;
  char err[96];
  kj* m = kj_parse(line, len, err, sizeof err);
  if (!m) {
    char msg[160];
    snprintf(msg, sizeof msg, "malformed message: %s", err);
    fatal(h, msg);
  }
  if (m->type != KJ_OBJ || !kj_is_str(kj_get(m, "t"))) fatal(h, "message has no string field t");
  return m;
}

static const char* mtype(const kj* m) { return kj_str(kj_get(m, "t")); }

static void put_scope(kbuf* b, kavach_scope s) {
  kb_puts(b, s == KAVACH_LOCAL ? "\"local\"" : "\"remote\"");
}

/* Waits for the answer to a request. NULL means the driver aborted the step. */
static kj* await(host* h, const char* want) {
  kj* m = next_message(h);
  if (!m) exit(1); /* the driver closed the pipe mid-step */
  const char* t = mtype(m);
  if (strcmp(t, "abort") == 0) {
    kj_free(m);
    return NULL;
  }
  if (strcmp(t, want) != 0) {
    char msg[160];
    snprintf(msg, sizeof msg, "expected %s, got %s", want, t);
    kj_free(m);
    fatal(h, msg);
  }
  return m;
}

static int decode_field(host* h, const kj* m, const char* key, uint8_t** out, size_t* len) {
  const kj* f = kj_get(m, key);
  if (!kj_is_str(f) || k_b64_decode(f->s, f->slen, out, len) != 0) {
    char msg[96];
    snprintf(msg, sizeof msg, "field %s is missing or not base64", key);
    fatal(h, msg);
  }
  return 1;
}

/* ---------------- env operations ---------------- */

static int op_now(kavach_env* e, int64_t* out) {
  host* h = e->ctx;
  kb_puts(&h->out, "{\"t\":\"clock\"}");
  write_line(h);
  kj* m = await(h, "clock");
  if (!m) return KAVACH_ABORTED;
  const kj* v = kj_get(m, "unix_nanos");
  if (kj_is_str(v))
    *out = (int64_t)strtoll(v->s, NULL, 10);
  else if (kj_is_num(v))
    *out = (int64_t)v->num;
  else
    fatal(h, "clock answer has no unix_nanos");
  kj_free(m);
  return KAVACH_OK;
}

static int op_rand(kavach_env* e, void* buf, size_t n) {
  host* h = e->ctx;
  kb_printf(&h->out, "{\"t\":\"rand\",\"n\":%zu}", n);
  write_line(h);
  kj* m = await(h, "rand");
  if (!m) return KAVACH_ABORTED;
  uint8_t* d;
  size_t dl;
  decode_field(h, m, "data", &d, &dl);
  if (dl != n) fatal(h, "rand answer has the wrong number of bytes");
  memcpy(buf, d, n);
  free(d);
  kj_free(m);
  return KAVACH_OK;
}

static const kavach_gateway* find_gateway(host* h, const char* name) {
  const kavach_gateway* any = NULL;
  for (size_t i = 0; i < h->o.n_gateways; i++) {
    if (h->o.gateways[i].name && strcmp(h->o.gateways[i].name, name) == 0) return &h->o.gateways[i];
    if (h->o.gateways[i].name && strcmp(h->o.gateways[i].name, "*") == 0) any = &h->o.gateways[i];
  }
  return any;
}

static int op_query(kavach_env* e, const char* name, const void* req, size_t req_len, uint8_t** resp,
                    size_t* resp_len, char** err) {
  host* h = e->ctx;
  const kavach_gateway* g = find_gateway(h, name);
  kb_puts(&h->out, "{\"t\":\"gateway\",\"gateway\":");
  kb_json_cstr(&h->out, name);
  kb_puts(&h->out, ",\"request\":");
  kb_json_b64(&h->out, req, req_len);
  kb_puts(&h->out, ",\"scope\":");
  put_scope(&h->out, g ? g->scope : KAVACH_REMOTE);
  kb_putc(&h->out, '}');
  write_line(h);
  kj* m = await(h, "gateway");
  if (!m) return KAVACH_ABORTED;
  int rc;
  if (kj_is_true(kj_get(m, "live"))) {
    /* A local query: execute it here and report what came back (SPEC 6.3). */
    uint8_t* rb = NULL;
    size_t rl = 0;
    char* em = NULL;
    if (!g || !g->fn) {
      em = k_strdup("no local gateway registered");
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
      }
    }
    kb_puts(&h->out, "{\"t\":\"observed\",");
    if (rc == KAVACH_OK) {
      kb_puts(&h->out, "\"response\":");
      kb_json_b64(&h->out, rb, rl);
    } else {
      kb_puts(&h->out, "\"error\":");
      kb_json_cstr(&h->out, em);
    }
    kb_putc(&h->out, '}');
    write_line(h);
    *resp = rb;
    *resp_len = rl;
    *err = em;
  } else if (kj_is_str(kj_get(m, "error"))) {
    *err = k_strdup(kj_get(m, "error")->s);
    rc = KAVACH_QUERY_FAILED;
  } else {
    decode_field(h, m, "response", resp, resp_len);
    rc = KAVACH_OK;
  }
  kj_free(m);
  return rc;
}

static int op_config(kavach_env* e, const char* key, uint8_t** val, size_t* len, int* present) {
  host* h = e->ctx;
  kb_puts(&h->out, "{\"t\":\"config\",\"key\":");
  kb_json_cstr(&h->out, key);
  kb_putc(&h->out, '}');
  write_line(h);
  kj* m = await(h, "config");
  if (!m) return KAVACH_ABORTED;
  if (kj_is_true(kj_get(m, "present"))) {
    decode_field(h, m, "value", val, len);
    *present = 1;
  }
  kj_free(m);
  return KAVACH_OK;
}

static int op_emit(kavach_env* e, const char* sink, const void* data, size_t len, kavach_scope scope) {
  host* h = e->ctx;
  kb_puts(&h->out, "{\"t\":\"emit\",\"sink\":");
  kb_json_cstr(&h->out, sink);
  kb_puts(&h->out, ",\"data\":");
  kb_json_b64(&h->out, data, len);
  kb_puts(&h->out, ",\"scope\":");
  put_scope(&h->out, scope);
  kb_putc(&h->out, '}');
  write_line(h);
  if (h->sandbox && scope == KAVACH_LOCAL && h->o.deliver) {
    if (h->n_locals == h->cap_locals) {
      size_t nc = h->cap_locals ? h->cap_locals * 2 : 8;
      local_out* nl = realloc(h->locals, nc * sizeof *nl);
      if (!nl) return KAVACH_ERROR;
      h->locals = nl;
      h->cap_locals = nc;
    }
    local_out* l = &h->locals[h->n_locals++];
    l->sink = k_strdup(sink);
    l->data = k_memdup(data, len);
    l->len = len;
  }
  return KAVACH_OK;
}

static const env_ops HOST_OPS = {op_now, op_rand, op_query, op_config, op_emit};

static void clear_locals(host* h) {
  for (size_t i = 0; i < h->n_locals; i++) {
    free(h->locals[i].sink);
    free(h->locals[i].data);
  }
  h->n_locals = 0;
}

/* ---------------- ready.environment ---------------- */

/* Runs `kavach-recorder facts`; returns its stdout, or NULL if unavailable. */
static char* run_facts(size_t* outlen) {
  const char* bin = getenv("KAVACH_RECORDER");
  if (!bin || !*bin) bin = "kavach-recorder";
  int p[2];
  if (pipe(p) != 0) return NULL;
  fcntl(p[0], F_SETFD, FD_CLOEXEC);
  posix_spawn_file_actions_t fa;
  posix_spawn_file_actions_init(&fa);
  posix_spawn_file_actions_adddup2(&fa, p[1], 1);
  posix_spawn_file_actions_addclose(&fa, p[1]);
  char* argv[] = {(char*)bin, (char*)"facts", NULL};
  pid_t pid;
  int rc = posix_spawnp(&pid, bin, &fa, NULL, argv, environ);
  posix_spawn_file_actions_destroy(&fa);
  close(p[1]);
  if (rc != 0) {
    close(p[0]);
    return NULL;
  }
  kbuf acc = {0};
  int ok = 1;
  for (;;) {
    struct pollfd pf = {p[0], POLLIN, 0};
    int pr = poll(&pf, 1, 10000);
    if (pr < 0 && errno == EINTR) continue;
    if (pr <= 0) {
      ok = 0;
      kill(pid, SIGKILL);
      break;
    }
    char chunk[4096];
    ssize_t n = read(p[0], chunk, sizeof chunk);
    if (n < 0 && errno == EINTR) continue;
    if (n <= 0) break;
    kb_put(&acc, chunk, (size_t)n);
    if (acc.len > (8u << 20)) {
      ok = 0;
      kill(pid, SIGKILL);
      break;
    }
  }
  close(p[0]);
  int st = 0;
  waitpid(pid, &st, 0);
  if (!ok || acc.oom || !WIFEXITED(st) || WEXITSTATUS(st) != 0) {
    kb_free(&acc);
    return NULL;
  }
  kb_putc(&acc, 0);
  *outlen = acc.len - 1;
  return (char*)acc.p;
}

static void build_environment(host* h) {
  const char* rt = h->o.runtime ? h->o.runtime : KAVACH_RUNTIME;
  kbuf b = {0};
  kb_putc(&b, '{');
  size_t n = 0;
  char* facts = run_facts(&n);
  if (facts) {
    char err[96];
    kj* m = kj_parse(facts, n, err, sizeof err);
    if (m && m->type == KJ_OBJ) {
      for (size_t i = 0; i < m->n; i++) {
        if (strcmp(m->keys[i], "host.runtime") == 0) continue; /* ours wins */
        kb_json_cstr(&b, m->keys[i]);
        kb_putc(&b, ':');
        kb_put(&b, m->v[i].raw, m->v[i].rawlen);
        kb_putc(&b, ',');
      }
    } else {
      fprintf(stderr, "kavach: host: ignoring unreadable output of kavach-recorder facts\n");
    }
    kj_free(m);
    free(facts);
  }
  kb_puts(&b, "\"host.runtime\":{\"value\":");
  kb_json_b64(&b, rt, strlen(rt));
  kb_puts(&b, "}}");
  kb_putc(&b, 0);
  if (b.oom) fatal(h, "out of memory");
  h->environment = (char*)b.p;
}

/* ---------------- protocol ---------------- */

static void drop_handler(host* h) {
  if (h->have_h && h->h.destroy) h->h.destroy(h->h.state);
  h->have_h = 0;
  memset(&h->h, 0, sizeof h->h);
}

static void on_hello(host* h, const kj* m) {
  const kj* proto = kj_get(m, "protocol");
  if (!kj_is_num(proto) || proto->num != 1) fatal(h, "unsupported protocol version");
  const char* start = kj_str(kj_get(m, "start"));
  const char* mode = kj_str(kj_get(m, "mode"));
  h->sandbox = mode && strcmp(mode, "sandbox") == 0;

  drop_handler(h);
  memset(&h->h, 0, sizeof h->h);
  if (!h->factory || h->factory(h->o.user, &h->h) != KAVACH_OK || !h->h.handle)
    fatal(h, "the handler could not be created");
  h->have_h = 1;

  if (start && strcmp(start, "snapshot") == 0) {
    uint8_t* snap;
    size_t sl;
    if (!h->h.restore) fatal(h, "the handler cannot restore a snapshot");
    decode_field(h, m, "snapshot", &snap, &sl);
    int rc = h->h.restore(h->h.state, snap, sl);
    free(snap);
    if (rc != KAVACH_OK) fatal(h, "the snapshot could not be restored");
  }
  if (h->sandbox && h->o.setup) {
    char* err = NULL;
    if (h->o.setup(h->o.setup_user, &err) != KAVACH_OK) {
      char msg[320];
      snprintf(msg, sizeof msg, "local setup failed: %s", err ? err : "unknown error");
      free(err);
      fatal(h, msg);
    }
    free(err);
  }
  k_env_reset(&h->env);
  k_env_init(&h->env, &HOST_OPS, h, !!(h->h.flags & KAVACH_HANDLER_NOJUMP));

  if (!h->environment) build_environment(h);
  kb_puts(&h->out, "{\"t\":\"ready\",\"protocol\":1,\"sdk\":");
  kb_json_cstr(&h->out, h->o.sdk ? h->o.sdk : "kavach-c/" KAVACH_VERSION);
  kb_puts(&h->out, ",\"invariants\":[");
  for (size_t i = 0; i < h->h.n_invariants; i++) {
    if (i) kb_putc(&h->out, ',');
    kb_json_cstr(&h->out, h->h.invariants[i].name);
  }
  kb_puts(&h->out, "],\"environment\":");
  kb_puts(&h->out, h->environment);
  kb_putc(&h->out, '}');
  write_line(h);
}

static void send_done(host* h, const char* outcome, const char* message, const char* detail) {
  kb_puts(&h->out, "{\"t\":\"done\",\"outcome\":");
  kb_json_cstr(&h->out, outcome);
  if (message) {
    kb_puts(&h->out, ",\"message\":");
    kb_json_cstr(&h->out, message);
  }
  if (detail && *detail) {
    kb_puts(&h->out, ",\"detail\":");
    kb_json_cstr(&h->out, detail);
  }
  kb_putc(&h->out, '}');
  write_line(h);
}

static void on_step(host* h, const kj* m) {
  if (!h->have_h) fatal(h, "step before hello");
  const char* source = kj_str(kj_get(m, "source"));
  const char* position = kj_str(kj_get(m, "position"));
  uint8_t* data;
  size_t dl;
  decode_field(h, m, "data", &data, &dl);
  kavach_input in = {source ? source : "", position ? position : "", data, dl};

  clear_locals(h);
  k_env_reset(&h->env);
  int rc = k_run_handler(&h->h, &h->env, &in);
  free(data);

  if (rc == KAVACH_ABORTED) {
    send_done(h, "aborted", NULL, NULL);
  } else if (rc == KAVACH_PANIC || rc == KAVACH_ERROR) {
    send_done(h, rc == KAVACH_PANIC ? "panic" : "error", h->env.fail_msg ? h->env.fail_msg : "", NULL);
  } else {
    const char* name;
    char* detail;
    if (k_check_invariants(&h->h, &name, &detail)) {
      send_done(h, "invariant", name, detail);
      free(detail);
    } else {
      int delivered = 1;
      if (h->n_locals > 0 && h->o.deliver) {
        kavach_output* arr = calloc(h->n_locals, sizeof *arr);
        if (arr) {
          for (size_t i = 0; i < h->n_locals; i++) {
            arr[i].sink = h->locals[i].sink;
            arr[i].data = h->locals[i].data;
            arr[i].len = h->locals[i].len;
            arr[i].scope = KAVACH_LOCAL;
          }
          delivered = h->o.deliver(h->o.deliver_user, arr, h->n_locals) == KAVACH_OK;
          free(arr);
        }
      }
      if (delivered)
        send_done(h, "ok", NULL, NULL);
      else
        send_done(h, "error", "local output delivery failed", NULL);
    }
  }
  clear_locals(h);
  k_env_reset(&h->env);
}

static void host_main(kavach_handler_factory factory, const kavach_host_options* opts) {
  host* h = &H;
  memset(h, 0, sizeof *h);
  if (opts) h->o = *opts;
  h->factory = factory;

  /* Take the protocol streams for ourselves (SPEC 9.1): the handler's stdout
   * becomes stderr and its stdin becomes empty. */
  h->in_fd = fcntl(0, F_DUPFD_CLOEXEC, 3);
  h->out_fd = fcntl(1, F_DUPFD_CLOEXEC, 3);
  if (h->in_fd < 0 || h->out_fd < 0) {
    fprintf(stderr, "kavach: host: standard input or output is not open\n");
    exit(2);
  }
  setvbuf(stdout, NULL, _IOLBF, 0);
  dup2(2, 1);
  int nul = open("/dev/null", O_RDONLY);
  if (nul >= 0) {
    dup2(nul, 0);
    close(nul);
  }
  signal(SIGPIPE, SIG_IGN);

  for (;;) {
    kj* m = next_message(h);
    if (!m) break; /* the driver closed our input without `end` */
    const char* t = mtype(m);
    if (strcmp(t, "hello") == 0) {
      on_hello(h, m);
    } else if (strcmp(t, "step") == 0) {
      on_step(h, m);
    } else if (strcmp(t, "end") == 0) {
      kj_free(m);
      break;
    } else if (strcmp(t, "abort") != 0) {
      /* Unknown or out-of-place driver messages: ignored (SPEC 9.2). */
    }
    kj_free(m);
  }
  drop_handler(h);
  exit(0);
}

int kavach_maybe_host(int argc, char** argv, kavach_handler_factory factory,
                      const kavach_host_options* opts) {
  if (argc < 2 || !argv || !argv[argc - 1] || strcmp(argv[argc - 1], "kavach-host") != 0) return 0;
  host_main(factory, opts);
  return 0; /* not reached */
}

#include "util.h"

#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>
#if defined(__linux__)
#include <sys/random.h>
#endif

/* ---------------- kbuf ---------------- */

void kb_free(kbuf* b) {
  free(b->p);
  b->p = NULL;
  b->len = b->cap = 0;
  b->oom = 0;
}

void kb_clear(kbuf* b) {
  b->len = 0;
  b->oom = 0;
}

static int kb_reserve(kbuf* b, size_t extra) {
  if (b->oom) return 0;
  if (extra > SIZE_MAX - b->len) {
    b->oom = 1;
    return 0;
  }
  size_t need = b->len + extra;
  if (need <= b->cap) return 1;
  size_t cap = b->cap ? b->cap : 256;
  while (cap < need) {
    if (cap > SIZE_MAX / 2) {
      cap = need;
      break;
    }
    cap *= 2;
  }
  uint8_t* np = realloc(b->p, cap);
  if (!np) {
    b->oom = 1;
    return 0;
  }
  b->p = np;
  b->cap = cap;
  return 1;
}

void kb_put(kbuf* b, const void* d, size_t n) {
  if (n == 0 || !kb_reserve(b, n)) return;
  memcpy(b->p + b->len, d, n);
  b->len += n;
}

void kb_putc(kbuf* b, int c) {
  uint8_t v = (uint8_t)c;
  kb_put(b, &v, 1);
}

void kb_puts(kbuf* b, const char* s) { kb_put(b, s, strlen(s)); }

void kb_vprintf(kbuf* b, const char* fmt, va_list ap) {
  va_list cp;
  va_copy(cp, ap);
  int n = vsnprintf(NULL, 0, fmt, cp);
  va_end(cp);
  if (n < 0) return;
  if (!kb_reserve(b, (size_t)n + 1)) return;
  vsnprintf((char*)b->p + b->len, (size_t)n + 1, fmt, ap);
  b->len += (size_t)n;
}

void kb_printf(kbuf* b, const char* fmt, ...) {
  va_list ap;
  va_start(ap, fmt);
  kb_vprintf(b, fmt, ap);
  va_end(ap);
}

void kb_u8(kbuf* b, uint8_t v) { kb_put(b, &v, 1); }

void kb_uvarint(kbuf* b, uint64_t v) {
  uint8_t tmp[10];
  size_t n = 0;
  while (v >= 0x80) {
    tmp[n++] = (uint8_t)(v | 0x80);
    v >>= 7;
  }
  tmp[n++] = (uint8_t)v;
  kb_put(b, tmp, n);
}

void kb_i64le(kbuf* b, int64_t v) {
  uint64_t u = (uint64_t)v;
  uint8_t tmp[8];
  for (int i = 0; i < 8; i++) tmp[i] = (uint8_t)(u >> (8 * i));
  kb_put(b, tmp, 8);
}

void kb_bytes(kbuf* b, const void* d, size_t n) {
  kb_uvarint(b, n);
  kb_put(b, d, n);
}

void kb_string(kbuf* b, const char* s) {
  if (!s) s = "";
  kb_bytes(b, s, strlen(s));
}

void kb_json_str(kbuf* b, const void* s, size_t n) {
  static const char hex[] = "0123456789abcdef";
  const uint8_t* p = s;
  kb_putc(b, '"');
  for (size_t i = 0; i < n; i++) {
    uint8_t c = p[i];
    switch (c) {
      case '"': kb_puts(b, "\\\""); break;
      case '\\': kb_puts(b, "\\\\"); break;
      case '\n': kb_puts(b, "\\n"); break;
      case '\r': kb_puts(b, "\\r"); break;
      case '\t': kb_puts(b, "\\t"); break;
      default:
        if (c < 0x20) {
          kb_puts(b, "\\u00");
          kb_putc(b, hex[c >> 4]);
          kb_putc(b, hex[c & 15]);
        } else {
          kb_putc(b, c);
        }
    }
  }
  kb_putc(b, '"');
}

void kb_json_cstr(kbuf* b, const char* s) {
  if (!s) s = "";
  kb_json_str(b, s, strlen(s));
}

static const char B64[] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

void kb_b64(kbuf* b, const void* d, size_t n) {
  const uint8_t* p = d;
  size_t i = 0;
  for (; i + 3 <= n; i += 3) {
    uint32_t v = (uint32_t)p[i] << 16 | (uint32_t)p[i + 1] << 8 | p[i + 2];
    char o[4] = {B64[v >> 18], B64[(v >> 12) & 63], B64[(v >> 6) & 63], B64[v & 63]};
    kb_put(b, o, 4);
  }
  if (n - i == 1) {
    uint32_t v = (uint32_t)p[i] << 16;
    char o[4] = {B64[v >> 18], B64[(v >> 12) & 63], '=', '='};
    kb_put(b, o, 4);
  } else if (n - i == 2) {
    uint32_t v = (uint32_t)p[i] << 16 | (uint32_t)p[i + 1] << 8;
    char o[4] = {B64[v >> 18], B64[(v >> 12) & 63], B64[(v >> 6) & 63], '='};
    kb_put(b, o, 4);
  }
}

void kb_json_b64(kbuf* b, const void* d, size_t n) {
  kb_putc(b, '"');
  kb_b64(b, d, n);
  kb_putc(b, '"');
}

static int b64val(char c) {
  if (c >= 'A' && c <= 'Z') return c - 'A';
  if (c >= 'a' && c <= 'z') return c - 'a' + 26;
  if (c >= '0' && c <= '9') return c - '0' + 52;
  if (c == '+') return 62;
  if (c == '/') return 63;
  return -1;
}

int k_b64_decode(const char* s, size_t n, uint8_t** out, size_t* outlen) {
  *out = NULL;
  *outlen = 0;
  if (n % 4 != 0) return -1;
  if (n == 0) return 0;
  size_t pad = 0;
  if (s[n - 1] == '=') pad++;
  if (s[n - 2] == '=') pad++;
  size_t len = n / 4 * 3 - pad;
  uint8_t* o = malloc(len ? len : 1);
  if (!o) return -1;
  size_t w = 0;
  for (size_t i = 0; i < n; i += 4) {
    int v[4];
    for (int j = 0; j < 4; j++) {
      char c = s[i + (size_t)j];
      if (c == '=') {
        if (i + 4 != n || j < 2 || (j == 2 && s[i + 3] != '=')) {
          free(o);
          return -1;
        }
        v[j] = 0;
      } else {
        v[j] = b64val(c);
        if (v[j] < 0) {
          free(o);
          return -1;
        }
      }
    }
    uint32_t x = (uint32_t)v[0] << 18 | (uint32_t)v[1] << 12 | (uint32_t)v[2] << 6 | (uint32_t)v[3];
    if (w < len) o[w++] = (uint8_t)(x >> 16);
    if (w < len) o[w++] = (uint8_t)(x >> 8);
    if (w < len) o[w++] = (uint8_t)x;
  }
  *out = o;
  *outlen = len;
  if (len == 0) {
    free(o);
    *out = NULL;
  }
  return 0;
}

/* ---------------- strings ---------------- */

char* k_strndup(const char* s, size_t n) {
  char* d = malloc(n + 1);
  if (!d) return NULL;
  memcpy(d, s, n);
  d[n] = 0;
  return d;
}

char* k_strdup(const char* s) { return k_strndup(s, strlen(s)); }

void* k_memdup(const void* p, size_t n) {
  if (n == 0) return NULL;
  void* d = malloc(n);
  if (d) memcpy(d, p, n);
  return d;
}

void kavach_free(void* p) { free(p); }

/* ---------------- clock and randomness ---------------- */

int64_t k_realtime_ns(void) {
  struct timespec ts;
  clock_gettime(CLOCK_REALTIME, &ts);
  return (int64_t)ts.tv_sec * 1000000000 + ts.tv_nsec;
}

int k_os_random(void* buf, size_t n) {
#if defined(__APPLE__) || defined(__FreeBSD__) || defined(__OpenBSD__) || defined(__NetBSD__)
  arc4random_buf(buf, n);
  return 0;
#else
  uint8_t* p = buf;
  size_t got = 0;
#if defined(__linux__)
  while (got < n) {
    ssize_t r = getrandom(p + got, n - got, 0);
    if (r < 0) {
      if (errno == EINTR) continue;
      break; /* ENOSYS etc: fall back to /dev/urandom */
    }
    got += (size_t)r;
  }
  if (got == n) return 0;
#endif
  int fd = open("/dev/urandom", O_RDONLY | O_CLOEXEC);
  if (fd < 0) return -1;
  while (got < n) {
    ssize_t r = read(fd, p + got, n - got);
    if (r < 0 && errno == EINTR) continue;
    if (r <= 0) {
      close(fd);
      return -1;
    }
    got += (size_t)r;
  }
  close(fd);
  return 0;
#endif
}

/* ---------------- env: public wrappers and step boundary ---------------- */

void k_env_init(struct kavach_env* e, const env_ops* ops, void* ctx, int nojump) {
  memset(e, 0, sizeof *e);
  e->ops = ops;
  e->ctx = ctx;
  e->nojump = nojump;
}

void k_env_reset(struct kavach_env* e) {
  free(e->fail_msg);
  e->fail_msg = NULL;
  e->fail_kind = 0;
  e->aborted = 0;
}

/* An aborted step unwinds like a panic, except in NOJUMP mode where the call
 * returns KAVACH_ABORTED. */
static int env_ret(kavach_env* e, int rc) {
  if (rc == KAVACH_ABORTED) {
    e->aborted = 1;
    if (!e->nojump) longjmp(e->jb, 2);
  }
  return rc;
}

int64_t kavach_now_ns(kavach_env* e) {
  if (e->aborted) return 0;
  int64_t v = 0;
  int rc = env_ret(e, e->ops->now(e, &v));
  return rc == KAVACH_OK ? v : 0;
}

int kavach_random(kavach_env* e, void* buf, size_t n) {
  if (e->aborted) return KAVACH_ABORTED;
  if (n == 0) return KAVACH_OK;
  return env_ret(e, e->ops->rand(e, buf, n));
}

int kavach_query(kavach_env* e, const char* gateway, const void* req, size_t req_len, uint8_t** resp,
                 size_t* resp_len, char** err) {
  uint8_t* r = NULL;
  size_t rl = 0;
  char* er = NULL;
  int rc;
  if (e->aborted)
    rc = KAVACH_ABORTED;
  else
    rc = env_ret(e, e->ops->query(e, gateway, req, req_len, &r, &rl, &er));
  if (resp)
    *resp = r;
  else
    free(r);
  if (resp_len) *resp_len = rl;
  if (err)
    *err = er;
  else
    free(er);
  return rc;
}

int kavach_config(kavach_env* e, const char* key, uint8_t** val, size_t* len, int* present) {
  uint8_t* v = NULL;
  size_t l = 0;
  int p = 0;
  int rc;
  if (e->aborted)
    rc = KAVACH_ABORTED;
  else
    rc = env_ret(e, e->ops->config(e, key, &v, &l, &p));
  if (val)
    *val = v;
  else
    free(v);
  if (len) *len = l;
  if (present) *present = p;
  return rc;
}

int kavach_emit(kavach_env* e, const char* sink, const void* data, size_t len, kavach_scope scope) {
  if (e->aborted) return KAVACH_ABORTED;
  return env_ret(e, e->ops->emit(e, sink, data, len, scope));
}

static int set_fail(kavach_env* e, int kind, char* msg) {
  free(e->fail_msg);
  e->fail_kind = kind;
  e->fail_msg = msg;
  return kind;
}

int kavach_error(kavach_env* e, const char* msg) {
  return set_fail(e, KAVACH_ERROR, k_strdup(msg ? msg : ""));
}

int kavach_fail_panic(kavach_env* e, const char* msg) {
  return set_fail(e, KAVACH_PANIC, k_strdup(msg ? msg : ""));
}

static char* vformat(const char* fmt, va_list ap) {
  kbuf b = {0};
  kb_vprintf(&b, fmt, ap);
  kb_putc(&b, 0);
  if (b.oom) {
    kb_free(&b);
    return NULL;
  }
  return (char*)b.p;
}

int kavach_errorf(kavach_env* e, const char* fmt, ...) {
  va_list ap;
  va_start(ap, fmt);
  char* m = vformat(fmt, ap);
  va_end(ap);
  return set_fail(e, KAVACH_ERROR, m ? m : k_strdup("out of memory"));
}

void kavach_panic(kavach_env* e, const char* msg) {
  kavach_fail_panic(e, msg);
  longjmp(e->jb, 1);
}

void kavach_panicf(kavach_env* e, const char* fmt, ...) {
  va_list ap;
  va_start(ap, fmt);
  char* m = vformat(fmt, ap);
  va_end(ap);
  set_fail(e, KAVACH_PANIC, m ? m : k_strdup("out of memory"));
  longjmp(e->jb, 1);
}

int kavach_aborted(const kavach_env* e) { return e->aborted; }

int k_run_handler(const kavach_handler* h, struct kavach_env* env, const kavach_input* in) {
  int rc;
  int jr = setjmp(env->jb);
  if (jr == 0)
    rc = h->handle(h->state, env, in);
  else if (jr == 1)
    rc = KAVACH_PANIC;
  else
    rc = KAVACH_ABORTED;
  if (env->aborted) return KAVACH_ABORTED;
  if (env->fail_kind == KAVACH_PANIC)
    rc = KAVACH_PANIC;
  else if (env->fail_kind == KAVACH_ERROR && rc == KAVACH_OK)
    rc = KAVACH_ERROR;
  if (rc == KAVACH_OK) return KAVACH_OK;
  if (rc != KAVACH_PANIC) rc = KAVACH_ERROR;
  if (!env->fail_msg)
    env->fail_msg = k_strdup(rc == KAVACH_PANIC ? "panic" : "handler returned an error");
  return rc;
}

int k_check_invariants(const kavach_handler* h, const char** name, char** detail) {
  *name = NULL;
  *detail = NULL;
  for (size_t i = 0; i < h->n_invariants; i++) {
    const kavach_invariant* inv = &h->invariants[i];
    char* d = NULL;
    if (inv->check(h->state, inv->arg, &d) != KAVACH_OK) {
      *name = inv->name;
      *detail = d;
      return 1;
    }
    free(d);
  }
  return 0;
}

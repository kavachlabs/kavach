/* Internal helpers shared by the recorder and the host. Not public API. */
#ifndef KAVACH_UTIL_H
#define KAVACH_UTIL_H

#include <setjmp.h>
#include <stdarg.h>
#include <stddef.h>
#include <stdint.h>

#include "kavach/kavach.h"

/* ---- growable byte buffer; allocation failure sets `oom` and drops writes ---- */
typedef struct kbuf {
  uint8_t* p;
  size_t len, cap;
  int oom;
} kbuf;

void kb_free(kbuf* b);
void kb_clear(kbuf* b);
void kb_put(kbuf* b, const void* d, size_t n);
void kb_putc(kbuf* b, int c);
void kb_puts(kbuf* b, const char* s);
void kb_printf(kbuf* b, const char* fmt, ...) KAVACH_PRINTF(2, 3);
void kb_vprintf(kbuf* b, const char* fmt, va_list ap);
void kb_u8(kbuf* b, uint8_t v);
void kb_uvarint(kbuf* b, uint64_t v);
void kb_i64le(kbuf* b, int64_t v);
void kb_bytes(kbuf* b, const void* d, size_t n); /* uvarint length + bytes */
void kb_string(kbuf* b, const char* s);          /* same, from a C string (NULL = "") */
void kb_json_str(kbuf* b, const void* s, size_t n); /* quoted, escaped */
void kb_json_cstr(kbuf* b, const char* s);
void kb_b64(kbuf* b, const void* d, size_t n);      /* base64 text, unquoted */
void kb_json_b64(kbuf* b, const void* d, size_t n); /* base64 text, quoted */

int k_b64_decode(const char* s, size_t n, uint8_t** out, size_t* outlen); /* 0 ok */

char* k_strdup(const char* s);
char* k_strndup(const char* s, size_t n);
void* k_memdup(const void* p, size_t n); /* NULL for n == 0 */

int64_t k_realtime_ns(void);
int k_os_random(void* buf, size_t n); /* 0 ok */

/* ---- the env handed to handlers ---- */
struct kavach_env;
typedef struct env_ops {
  int (*now)(struct kavach_env*, int64_t*);
  int (*rand)(struct kavach_env*, void*, size_t);
  int (*query)(struct kavach_env*, const char*, const void*, size_t, uint8_t**, size_t*, char**);
  int (*config)(struct kavach_env*, const char*, uint8_t**, size_t*, int*);
  int (*emit)(struct kavach_env*, const char*, const void*, size_t, kavach_scope);
} env_ops;

struct kavach_env {
  const env_ops* ops;
  void* ctx;
  jmp_buf jb;
  int nojump;
  int aborted;
  int fail_kind; /* 0, KAVACH_ERROR or KAVACH_PANIC */
  char* fail_msg;
};

void k_env_init(struct kavach_env* e, const env_ops* ops, void* ctx, int nojump);
void k_env_reset(struct kavach_env* e);

/* Runs handler->handle inside the step boundary. Returns KAVACH_OK, _ERROR,
 * _PANIC or _ABORTED. For failures, env->fail_msg holds the marker message. */
int k_run_handler(const kavach_handler* h, struct kavach_env* env, const kavach_input* in);

/* After an ok step: checks invariants in order. Returns 0 if all hold, else 1
 * with *name (from the handler) and *detail (malloc'd or NULL). */
int k_check_invariants(const kavach_handler* h, const char** name, char** detail);

#endif

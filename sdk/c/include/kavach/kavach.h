/*
 * Kavach C SDK -- public API.
 *
 * Kavach records what a single-writer handler did (its inputs, every read it
 * made of the clock, the random source, gateways and config, and its outputs)
 * so a failure can be replayed and a fix verified. See SPEC.md sections 4, 9
 * and 10 in the Kavach repository, and sdk/c/README.md for the full guide.
 *
 * Conventions
 *   - Functions return KAVACH_OK (0) or one of the other KAVACH_* codes.
 *   - Strings are NUL-terminated UTF-8. Byte buffers carry an explicit length.
 *   - Every buffer a kavach function hands to the caller ("out" parameters) is
 *     allocated with malloc and released with kavach_free (or free).
 *   - Every buffer the caller hands to kavach is only borrowed for the duration
 *     of the call, except where a field's comment says the pointer must stay
 *     valid longer.
 *   - Zero-initialise option structs; a zero field means "default".
 */
#ifndef KAVACH_KAVACH_H
#define KAVACH_KAVACH_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define KAVACH_VERSION "0.1.0"

#if defined(__GNUC__) || defined(__clang__)
#define KAVACH_API __attribute__((visibility("default")))
#define KAVACH_NORETURN __attribute__((noreturn))
#define KAVACH_PRINTF(a, b) __attribute__((format(printf, a, b)))
#else
#define KAVACH_API
#define KAVACH_NORETURN
#define KAVACH_PRINTF(a, b)
#endif

/* ---- host.runtime string (SPEC 4.8): "<language>-<compiler>-<version>" ---- */
#define KAVACH_STR_(x) #x
#define KAVACH_STR(x) KAVACH_STR_(x)
#if defined(__cplusplus)
#if __cplusplus >= 202302L
#define KAVACH_LANG "cxx23"
#elif __cplusplus >= 202002L
#define KAVACH_LANG "cxx20"
#elif __cplusplus >= 201703L
#define KAVACH_LANG "cxx17"
#else
#define KAVACH_LANG "cxx14"
#endif
#else
#if defined(__STDC_VERSION__) && __STDC_VERSION__ >= 202311L
#define KAVACH_LANG "c23"
#elif defined(__STDC_VERSION__) && __STDC_VERSION__ >= 201710L
#define KAVACH_LANG "c17"
#elif defined(__STDC_VERSION__) && __STDC_VERSION__ >= 201112L
#define KAVACH_LANG "c11"
#else
#define KAVACH_LANG "c99"
#endif
#endif
#if defined(__clang__)
#if defined(__apple_build_version__)
#define KAVACH_COMPILER "appleclang"
#else
#define KAVACH_COMPILER "clang"
#endif
#define KAVACH_COMPILER_VERSION \
  KAVACH_STR(__clang_major__) "." KAVACH_STR(__clang_minor__) "." KAVACH_STR(__clang_patchlevel__)
#elif defined(__GNUC__)
#define KAVACH_COMPILER "gcc"
#define KAVACH_COMPILER_VERSION \
  KAVACH_STR(__GNUC__) "." KAVACH_STR(__GNUC_MINOR__) "." KAVACH_STR(__GNUC_PATCHLEVEL__)
#else
#define KAVACH_COMPILER "cc"
#define KAVACH_COMPILER_VERSION "0"
#endif
/* e.g. "c11-clang-21.1.2" or "cxx17-gcc-15.2.0". Evaluated in the translation
 * unit that uses it, so the C++ wrapper reports "cxx17-...". */
#define KAVACH_RUNTIME KAVACH_LANG "-" KAVACH_COMPILER "-" KAVACH_COMPILER_VERSION

/* ---- return codes ---- */
enum {
  KAVACH_OK = 0,
  KAVACH_ERROR = 1,           /* handler error / generic failure */
  KAVACH_PANIC = 2,           /* handler panic (step outcome) */
  KAVACH_INVARIANT = 3,       /* a declared invariant failed (step outcome) */
  KAVACH_ABORTED = 4,         /* host mode: the driver aborted this step (SPEC 9.4) */
  KAVACH_DELIVER_FAILED = 5,  /* step ok, but the deliver callback failed */
  KAVACH_QUERY_FAILED = 6     /* kavach_query: the query failed, *err is set */
};

typedef enum { KAVACH_REMOTE = 0, KAVACH_LOCAL = 1 } kavach_scope;

/* The handle a handler uses to reach the world. Valid only during the step. */
typedef struct kavach_env kavach_env;

/* One event consumed by the handler (SPEC 4.1). Borrowed for the step. */
typedef struct kavach_input {
  const char* source;    /* e.g. "kafka:wallet-events" */
  const char* position;  /* e.g. "3:1042"; NULL means "" */
  const uint8_t* data;
  size_t len;
} kavach_input;

/* One effect the handler requested (SPEC 4.4). */
typedef struct kavach_output {
  const char* sink;
  const uint8_t* data;
  size_t len;
  kavach_scope scope;
} kavach_output;

/* A named property of handler state that must hold after every step.
 * check returns KAVACH_OK if it holds; otherwise non-zero and optionally a
 * malloc'd description in *detail, which kavach frees. */
typedef struct kavach_invariant {
  const char* name;
  int (*check)(void* state, void* arg, char** detail);
  void* arg;
} kavach_invariant;

#define KAVACH_HANDLER_NOJUMP 1u /* never longjmp out of this handler; see README */

/* The handler. All pointers except `state` and `handle` are optional.
 * The struct is copied; `invariants` must stay valid as long as the handler. */
typedef struct kavach_handler {
  void* state;
  /* Runs one step. Return KAVACH_OK, or kavach_error(env, msg) /
   * KAVACH_ERROR for an error. kavach_panic never returns. */
  int (*handle)(void* state, kavach_env* env, const kavach_input* in);
  /* Optional: serialise the state. On success set *data to a malloc'd buffer
   * (ownership passes to kavach) and *len. */
  int (*snapshot)(void* state, uint8_t** data, size_t* len);
  /* Optional: restore state from a snapshot. */
  int (*restore)(void* state, const uint8_t* data, size_t len);
  /* Optional: invariants, checked in order after every step that ended ok. */
  const kavach_invariant* invariants;
  size_t n_invariants;
  /* Optional: host mode only; called when the host replaces/ends the handler. */
  void (*destroy)(void* state);
  unsigned flags; /* KAVACH_HANDLER_* */
} kavach_handler;

/* ---- environment: what a handler may do (SPEC 1, 4.2-4.9) ---- */

/* Reads the clock; nanoseconds since the Unix epoch. */
KAVACH_API int64_t kavach_now_ns(kavach_env* env);

/* Fills buf with n random bytes. n == 0 is a no-op (nothing is recorded). */
KAVACH_API int kavach_random(kavach_env* env, void* buf, size_t n);

/* Queries a gateway. On KAVACH_OK, *resp is a malloc'd buffer (NULL when
 * *resp_len is 0) owned by the caller. On KAVACH_QUERY_FAILED, *err (if err is
 * not NULL) is a malloc'd message owned by the caller. */
KAVACH_API int kavach_query(kavach_env* env, const char* gateway, const void* req, size_t req_len,
                            uint8_t** resp, size_t* resp_len, char** err);

/* Reads a config value. *present is set to 0 or 1; when 1, *val is a malloc'd
 * buffer (NULL if empty) owned by the caller. */
KAVACH_API int kavach_config(kavach_env* env, const char* key, uint8_t** val, size_t* len,
                             int* present);

/* Requests an output. It is delivered only after the step succeeds. */
KAVACH_API int kavach_emit(kavach_env* env, const char* sink, const void* data, size_t len,
                           kavach_scope scope);

/* Fails the step as an `error` with exactly msg and returns KAVACH_ERROR, so a
 * handler writes `return kavach_error(env, "...");`. */
KAVACH_API int kavach_error(kavach_env* env, const char* msg);
KAVACH_API int kavach_errorf(kavach_env* env, const char* fmt, ...) KAVACH_PRINTF(2, 3);

/* Fails the step as a `panic` with exactly msg. Does not return: it longjmps
 * to the step boundary. Do not call it from C++ frames or from a handler
 * flagged KAVACH_HANDLER_NOJUMP; use kavach_fail_panic and return instead. */
KAVACH_API KAVACH_NORETURN void kavach_panic(kavach_env* env, const char* msg);
KAVACH_API KAVACH_NORETURN void kavach_panicf(kavach_env* env, const char* fmt, ...)
    KAVACH_PRINTF(2, 3);

/* Fails the step as a `panic` and returns KAVACH_PANIC normally (no longjmp).
 * For wrappers and for handlers that unwind by returning. */
KAVACH_API int kavach_fail_panic(kavach_env* env, const char* msg);

/* Nonzero once the driver aborted this step (host mode). Only observable by
 * handlers flagged KAVACH_HANDLER_NOJUMP: every env call then returns
 * KAVACH_ABORTED (kavach_now_ns returns 0) and the handler should return. */
KAVACH_API int kavach_aborted(const kavach_env* env);

/* Releases a buffer returned by kavach (same as free). */
KAVACH_API void kavach_free(void* p);

/* ---- gateways, config, flags ---- */

/* Executes a query. Return KAVACH_OK with *resp (malloc'd, ownership passes to
 * kavach) or non-zero with *err (malloc'd message, optional). */
typedef int (*kavach_gateway_fn)(void* user, const char* gateway, const uint8_t* req,
                                 size_t req_len, uint8_t** resp, size_t* resp_len, char** err);

typedef struct kavach_gateway {
  const char* name; /* "*" matches any gateway not registered by name */
  kavach_gateway_fn fn;
  void* user;
  kavach_scope scope; /* MUST NOT change between builds (SPEC 4.7) */
} kavach_gateway;

/* Reads a config value. Return KAVACH_OK; set *present and, when present, *val
 * (malloc'd, ownership passes to kavach). */
typedef int (*kavach_config_fn)(void* user, const char* key, uint8_t** val, size_t* len,
                                int* present);

typedef struct kavach_flag {
  const char* key; /* full fact key, e.g. "flag.ledger-v2" */
  const uint8_t* value;
  size_t len;
} kavach_flag;
/* Returns the flags and their count; the array is read immediately. */
typedef size_t (*kavach_flags_fn)(void* user, const kavach_flag** flags);

/* Delivers the outputs of a step that succeeded. Return KAVACH_OK. The array
 * and its buffers are valid only during the call. */
typedef int (*kavach_deliver_fn)(void* user, const kavach_output* outs, size_t n);

/* Receives log lines (no trailing newline). Default: stderr, "kavach: " prefix.
 * May be called from the recorder's control thread. */
typedef void (*kavach_log_fn)(void* user, const char* msg);

/* ---- flight recorder (SPEC 3.6, 10) ---- */

typedef struct kavach_recorder kavach_recorder;

typedef struct kavach_recorder_options {
  const char* service; /* required */
  /* How to start the recorder: this argv (NULL-terminated), else the
   * KAVACH_RECORDER environment variable, else "kavach-recorder" on PATH. */
  const char* const* recorder_argv;
  const char* dir;          /* journal directory (recorder default "kavach") */
  const char* compression;  /* "zstd" (default) or "none" */
  int level;                /* zstd level; 0 = recorder default */
  uint64_t block_bytes;     /* 0 = recorder default */
  uint64_t flush_ms;
  uint64_t segment_bytes;
  uint64_t segment_seconds;
  uint64_t retain_segments;
  const char* const* secret_keys; /* NULL-terminated env var names to hash */
  const char* handler_id;   /* build identity, e.g. a git revision */
  const char* producer;     /* default "kavach-c/" KAVACH_VERSION */
  const char* runtime;      /* host.runtime; default: this library's KAVACH_RUNTIME */
  int start_from_snapshot;  /* journal starts with a snapshot of the handler (needs snapshot) */
  int no_segments;          /* do not offer snapshots to the recorder, so it never segments */
  int required;             /* fail kavach_recorder_new if recording cannot start (SPEC 10.1) */
  int startup_timeout_ms;   /* with required: wait this long for `ready`; default 5000 */
  int flush_timeout_ms;     /* kavach_recorder_flush(durable) wait; default 10000 */
  int close_timeout_ms;     /* kavach_recorder_close wait for `closed`; default 10000 */
  /* Transport (SPEC 10.7). The shared-memory ring is the default; if it cannot
   * be set up the SDK logs that and uses the pipe. no_ring forces the pipe. */
  int no_ring;
  uint64_t ring_bytes;      /* ring capacity: a power of two >= 64 KiB; 0 = 8 MiB */

  const kavach_gateway* gateways; /* copied */
  size_t n_gateways;
  kavach_config_fn config;
  void* config_user;
  const char* config_source; /* recorded `source` of config reads; default "config" */
  kavach_flags_fn flags;
  void* flags_user;
  kavach_deliver_fn deliver;
  void* deliver_user;
  kavach_log_fn log;
  void* log_user;
  /* Test hooks: replace the real clock / random source. */
  int64_t (*clock_ns)(void* user);
  void* clock_user;
  int (*random_bytes)(void* user, void* buf, size_t n);
  void* random_user;
} kavach_recorder_options;

typedef struct kavach_result {
  int outcome;     /* KAVACH_OK, _ERROR, _PANIC, _INVARIANT, _DELIVER_FAILED */
  char* message;   /* marker message for failures (malloc'd), else NULL */
  char* detail;    /* optional detail, e.g. an invariant's description */
} kavach_result;

/* Starts the recorder and writes the open, facts (and snapshot) frames. If the
 * recorder cannot be started and `required` is not set, the failure is logged
 * and a recorder that records nothing is returned: steps still run. */
KAVACH_API int kavach_recorder_new(const kavach_handler* handler,
                                   const kavach_recorder_options* opts, kavach_recorder** out,
                                   char** err);

/* Records and runs one step. The input frame is written before the handler
 * runs. Returns the step's outcome (also in result->outcome when result is
 * not NULL). Free the result with kavach_result_clear. Not reentrant. */
KAVACH_API int kavach_recorder_step(kavach_recorder* rec, const kavach_input* in,
                                    kavach_result* result);

/* Asks the recorder to close its block; when durable, waits until it reports
 * `durable` (flush_timeout_ms). KAVACH_ERROR if not recording or on timeout. */
KAVACH_API int kavach_recorder_flush(kavach_recorder* rec, int durable);

/* True while the recorder is running and the SDK is writing to it. */
KAVACH_API int kavach_recorder_active(const kavach_recorder* rec);

/* Orderly shutdown (SPEC 10.5): sends `close`, waits for `closed`. Idempotent.
 * KAVACH_ERROR if the recorder did not confirm. */
KAVACH_API int kavach_recorder_close(kavach_recorder* rec);

/* Closes (if needed) and frees. NULL is allowed. */
KAVACH_API void kavach_recorder_free(kavach_recorder* rec);

KAVACH_API void kavach_result_clear(kavach_result* result);

/* ---- host (SPEC 9) ---- */

/* Creates a fresh handler for each `hello`. Fill *out and return KAVACH_OK. */
typedef int (*kavach_handler_factory)(void* user, kavach_handler* out);

typedef struct kavach_host_options {
  void* user;              /* passed to factory */
  const char* sdk;         /* ready.sdk; default "kavach-c/" KAVACH_VERSION */
  const char* runtime;     /* host.runtime; default this library's KAVACH_RUNTIME */
  const kavach_gateway* gateways; /* names and scopes reported on gateway requests (copied) */
  size_t n_gateways;
} kavach_host_options;

/* If the last argument is "kavach-host", runs the host protocol on stdin and
 * stdout and exits the process (never returns). Otherwise returns 0 at once.
 * Call it first in main(). */
KAVACH_API int kavach_maybe_host(int argc, char** argv, kavach_handler_factory factory,
                                 const kavach_host_options* opts);

#ifdef __cplusplus
}
#endif

#endif /* KAVACH_KAVACH_H */

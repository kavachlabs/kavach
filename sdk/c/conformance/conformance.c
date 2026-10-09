#include "conformance.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "util.h"

static void release_step(conf_state* st) {
  kj_free(st->ops);
  st->ops = NULL;
  free(st->held);
  st->held = NULL;
}

void conf_state_clear(conf_state* st) {
  release_step(st);
  free(st->scratch);
  st->scratch = NULL;
  st->scratch_cap = 0;
}

static const char* sfield(const kj* op, const char* key) {
  const kj* v = kj_get(op, key);
  return kj_is_str(v) ? v->s : "";
}

static int trace(kavach_env* env, const void* d, size_t n) {
  return kavach_emit(env, "trace", d, n, KAVACH_REMOTE);
}

static int conf_handle(void* state, kavach_env* env, const kavach_input* in) {
  conf_state* st = state;
  release_step(st);
  char err[96];
  st->ops = kj_parse((const char*)in->data, in->len, err, sizeof err);
  if (!st->ops || st->ops->type != KJ_ARR) return kavach_errorf(env, "invalid operations: %s", st->ops ? "not an array" : err);

  for (size_t i = 0; i < st->ops->n; i++) {
    const kj* op = &st->ops->v[i];
    const char* name = sfield(op, "op");
    if (strcmp(name, "clock") == 0) {
      char buf[64];
      int n = snprintf(buf, sizeof buf, "{\"clock\":\"%lld\"}", (long long)kavach_now_ns(env));
      trace(env, buf, (size_t)n);
    } else if (strcmp(name, "rand") == 0) {
      const kj* nv = kj_get(op, "n");
      size_t n = kj_is_num(nv) ? (size_t)nv->num : 0;
      if (n > st->scratch_cap) {
        free(st->scratch);
        st->scratch = malloc(n);
        st->scratch_cap = st->scratch ? n : 0;
        if (!st->scratch) return kavach_error(env, "out of memory");
      }
      if (kavach_random(env, st->scratch, n) != KAVACH_OK) return kavach_error(env, "random failed");
      trace(env, st->scratch, n);
    } else if (strcmp(name, "gateway") == 0) {
      const char* req = sfield(op, "request");
      uint8_t* resp = NULL;
      size_t rl = 0;
      char* e = NULL;
      int rc = kavach_query(env, sfield(op, "gateway"), req, strlen(req), &resp, &rl, &e);
      if (rc == KAVACH_OK) {
        st->held = resp;
        trace(env, resp, rl);
        free(st->held);
        st->held = NULL;
      } else {
        kbuf b = {0};
        kb_puts(&b, "{\"error\":");
        kb_json_cstr(&b, e);
        kb_putc(&b, '}');
        free(e);
        trace(env, b.p, b.len);
        kb_free(&b);
      }
    } else if (strcmp(name, "config") == 0) {
      uint8_t* val = NULL;
      size_t vl = 0;
      int present = 0;
      kavach_config(env, sfield(op, "key"), &val, &vl, &present);
      if (present) {
        st->held = val;
        trace(env, val, vl);
        free(st->held);
        st->held = NULL;
      } else {
        trace(env, "{\"unset\":true}", 14);
      }
    } else if (strcmp(name, "getenv") == 0) {
      const char* v = getenv(sfield(op, "name"));
      if (v)
        trace(env, v, strlen(v));
      else
        trace(env, "{\"unset\":true}", 14);
    } else if (strcmp(name, "emit") == 0) {
      const char* d = sfield(op, "data");
      kavach_emit(env, sfield(op, "sink"), d, strlen(d), KAVACH_REMOTE);
    } else if (strcmp(name, "panic") == 0) {
      kavach_panic(env, sfield(op, "message"));
    } else if (strcmp(name, "error") == 0) {
      return kavach_error(env, sfield(op, "message"));
    } else if (strcmp(name, "print") == 0) {
      printf("%s\n", sfield(op, "text"));
    } else if (strcmp(name, "count") == 0) {
      const kj* nv = kj_get(op, "n");
      st->count += kj_is_num(nv) ? (long)nv->num : 0; /* no rollback (SPEC 9.6) */
    } else {
      return kavach_errorf(env, "unknown operation %s", name);
    }
  }
  st->count += 1;
  return KAVACH_OK;
}

static int conf_snapshot(void* state, uint8_t** data, size_t* len) {
  conf_state* st = state;
  char buf[32];
  int n = snprintf(buf, sizeof buf, "%ld", st->count);
  *data = k_memdup(buf, (size_t)n);
  *len = (size_t)n;
  return *data ? KAVACH_OK : KAVACH_ERROR;
}

static int conf_restore(void* state, const uint8_t* data, size_t len) {
  conf_state* st = state;
  char buf[32];
  if (len == 0 || len >= sizeof buf) return KAVACH_ERROR;
  memcpy(buf, data, len);
  buf[len] = 0;
  char* end;
  st->count = strtol(buf, &end, 10);
  return *end ? KAVACH_ERROR : KAVACH_OK;
}

static int below_limit(void* state, void* arg, char** detail) {
  (void)arg;
  conf_state* st = state;
  if (st->count < 1000) return KAVACH_OK;
  char buf[64];
  snprintf(buf, sizeof buf, "count is %ld", st->count);
  *detail = k_strdup(buf);
  return KAVACH_ERROR;
}

static const kavach_invariant INVARIANTS[] = {{"below_limit", below_limit, NULL}};

void conf_handler(kavach_handler* h, conf_state* st) {
  memset(h, 0, sizeof *h);
  h->state = st;
  h->handle = conf_handle;
  h->snapshot = conf_snapshot;
  h->restore = conf_restore;
  h->invariants = INVARIANTS;
  h->n_invariants = 1;
}

static void conf_destroy(void* state) {
  conf_state_clear(state);
  free(state);
}

int conf_factory(void* user, kavach_handler* out) {
  (void)user;
  conf_state* st = calloc(1, sizeof *st);
  if (!st) return KAVACH_ERROR;
  conf_handler(out, st);
  out->destroy = conf_destroy;
  return KAVACH_OK;
}

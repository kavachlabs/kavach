/* Recorder-case runner (spec/recorder/sdk/README.md) for the C API.
 *   runner-c <case.json> <fake_recorder.py> <result.json> [pipe|ring] */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "caseio.h"
#include "conformance.h"
#include "util.h"

typedef struct serve {
  case_action* cur;
  const case_t* c;
} serve;

static ans* pop(ans_list* l) { return l->next < l->n ? &l->v[l->next++] : NULL; }

static int64_t clock_ns(void* user) {
  serve* s = user;
  ans* a = pop(&s->cur->clock);
  return a && a->text ? (int64_t)strtoll(a->text, NULL, 10) : 0;
}

static int random_bytes(void* user, void* buf, size_t n) {
  serve* s = user;
  ans* a = pop(&s->cur->rand);
  if (!a || a->len != n) return -1;
  memcpy(buf, a->data, n);
  return 0;
}

static int gateway(void* user, const char* name, const uint8_t* req, size_t rl, uint8_t** resp,
                   size_t* resp_len, char** err) {
  (void)name;
  (void)req;
  (void)rl;
  serve* s = user;
  ans* a = pop(&s->cur->gateway);
  if (!a) {
    *err = k_strdup("no answer scripted");
    return KAVACH_ERROR;
  }
  if (a->kind == 1) {
    *err = k_strdup(a->text);
    return KAVACH_ERROR;
  }
  *resp = k_memdup(a->data, a->len);
  *resp_len = a->len;
  return KAVACH_OK;
}

static int config(void* user, const char* key, uint8_t** val, size_t* len, int* present) {
  (void)key;
  serve* s = user;
  ans* a = pop(&s->cur->config);
  if (!a || a->kind == 2) {
    *present = 0;
    return KAVACH_OK;
  }
  *val = k_memdup(a->data, a->len);
  *len = a->len;
  *present = 1;
  return KAVACH_OK;
}

static size_t flags_fn(void* user, const kavach_flag** out) {
  serve* s = user;
  static kavach_flag fl[16];
  size_t n = s->c->n_flags < 16 ? s->c->n_flags : 16;
  for (size_t i = 0; i < n; i++) {
    fl[i].key = s->c->flags[i].key;
    fl[i].value = (const uint8_t*)s->c->flags[i].value;
    fl[i].len = strlen(s->c->flags[i].value);
  }
  *out = fl;
  return n;
}

int main(int argc, char** argv) {
  if (argc != 4 && argc != 5) {
    fprintf(stderr, "usage: %s case.json fake_recorder.py result.json [pipe|ring]\n", argv[0]);
    return 2;
  }
  char err[256];
  case_t* c = case_load(argv[1], err, sizeof err);
  if (!c) {
    fprintf(stderr, "%s\n", err);
    return 2;
  }
  conf_state st;
  memset(&st, 0, sizeof st);
  kavach_handler h;
  conf_handler(&h, &st);
  if (c->snapshot && h.restore(h.state, (const uint8_t*)c->snapshot, strlen(c->snapshot)) != KAVACH_OK) {
    fprintf(stderr, "bad snapshot in case\n");
    return 2;
  }

  serve sv = {NULL, c};
  const char* argvr[] = {"python3", argv[2], argv[1], argv[3], NULL};
  kavach_gateway gw = {"*", gateway, &sv, KAVACH_REMOTE};
  kavach_recorder_options o;
  memset(&o, 0, sizeof o);
  o.service = c->service;
  o.recorder_argv = argvr;
  o.start_from_snapshot = c->start_snapshot;
  o.no_segments = !c->snapshots;
  o.required = 1;
  o.no_ring = argc == 5 && strcmp(argv[4], "pipe") == 0;
  o.gateways = &gw;
  o.n_gateways = 1;
  o.config = config;
  o.config_user = &sv;
  o.config_source = "test";
  o.clock_ns = clock_ns;
  o.clock_user = &sv;
  o.random_bytes = random_bytes;
  o.random_user = &sv;
  if (c->n_flags) {
    o.flags = flags_fn;
    o.flags_user = &sv;
  }

  kavach_recorder* rec;
  char* e = NULL;
  if (kavach_recorder_new(&h, &o, &rec, &e) != KAVACH_OK) {
    fprintf(stderr, "recorder: %s\n", e ? e : "?");
    return 1;
  }
  int bad = 0;
  for (size_t i = 0; i < c->n_actions; i++) {
    case_action* a = &c->actions[i];
    if (a->is_flush) {
      if (kavach_recorder_flush(rec, a->durable) != KAVACH_OK) {
        fprintf(stderr, "flush failed\n");
        bad = 1;
      }
    } else {
      sv.cur = a;
      kavach_input in = {a->source, a->position, a->data, a->len};
      kavach_result res;
      kavach_recorder_step(rec, &in, &res);
      kavach_result_clear(&res);
    }
  }
  if (kavach_recorder_close(rec) != KAVACH_OK) {
    fprintf(stderr, "close failed\n");
    bad = 1;
  }
  kavach_recorder_free(rec);
  conf_state_clear(&st);
  int rc = case_check_result(argv[3], o.no_ring ? "pipe" : "ring");
  case_free(c);
  return bad || rc;
}

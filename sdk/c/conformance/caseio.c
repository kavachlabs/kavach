#include "caseio.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "json.h"
#include "util.h"

static char* slurp(const char* path, size_t* n) {
  FILE* f = fopen(path, "rb");
  if (!f) return NULL;
  size_t cap = 4096, len = 0;
  char* b = malloc(cap);
  size_t r;
  while (b && (r = fread(b + len, 1, cap - len, f)) > 0) {
    len += r;
    if (len == cap) {
      cap *= 2;
      b = realloc(b, cap);
    }
  }
  fclose(f);
  *n = len;
  return b;
}

static void load_list(ans_list* l, const kj* arr, int is_clock) {
  if (!arr || arr->type != KJ_ARR) return;
  l->v = calloc(arr->n ? arr->n : 1, sizeof *l->v);
  for (size_t i = 0; i < arr->n; i++) {
    const kj* a = &arr->v[i];
    ans* o = &l->v[l->n++];
    if (kj_is_str(a)) { /* clock (string) or rand (base64) */
      if (is_clock) {
        o->text = k_strdup(a->s);
      } else {
        k_b64_decode(a->s, a->slen, &o->data, &o->len);
      }
    } else if (kj_get(a, "error")) {
      o->kind = 1;
      o->text = k_strdup(kj_str(kj_get(a, "error")));
    } else if (kj_get(a, "unset")) {
      o->kind = 2;
    } else {
      const kj* v = kj_get(a, "response");
      if (!v) v = kj_get(a, "value");
      if (kj_is_str(v)) k_b64_decode(v->s, v->slen, &o->data, &o->len);
    }
  }
}

static void free_list(ans_list* l) {
  for (size_t i = 0; i < l->n; i++) {
    free(l->v[i].text);
    free(l->v[i].data);
  }
  free(l->v);
}

case_t* case_load(const char* path, char* err, size_t errcap) {
  size_t n;
  char* text = slurp(path, &n);
  if (!text) {
    snprintf(err, errcap, "cannot read %s", path);
    return NULL;
  }
  kj* root = kj_parse(text, n, err, errcap);
  free(text);
  if (!root) return NULL;
  case_t* c = calloc(1, sizeof *c);
  const kj* open = kj_get(root, "open");
  c->service = k_strdup(kj_str(kj_get(open, "service")) ? kj_str(kj_get(open, "service")) : "conformance");
  const char* start = kj_str(kj_get(open, "start"));
  c->start_snapshot = start && strcmp(start, "snapshot") == 0;
  c->snapshots = kj_is_true(kj_get(open, "snapshots"));
  if (kj_is_str(kj_get(root, "snapshot"))) c->snapshot = k_strdup(kj_str(kj_get(root, "snapshot")));
  const kj* flags = kj_get(root, "flags");
  if (flags && flags->type == KJ_OBJ) {
    c->flags = calloc(flags->n ? flags->n : 1, sizeof *c->flags);
    for (size_t i = 0; i < flags->n; i++) {
      c->flags[c->n_flags].key = k_strdup(flags->keys[i]);
      c->flags[c->n_flags].value = k_strdup(flags->v[i].s ? flags->v[i].s : "");
      c->n_flags++;
    }
  }
  const kj* acts = kj_get(root, "actions");
  if (acts && acts->type == KJ_ARR) {
    c->actions = calloc(acts->n ? acts->n : 1, sizeof *c->actions);
    for (size_t i = 0; i < acts->n; i++) {
      const kj* a = &acts->v[i];
      case_action* o = &c->actions[c->n_actions++];
      const kj* fl = kj_get(a, "flush");
      if (fl) {
        o->is_flush = 1;
        o->durable = kj_is_true(kj_get(fl, "durable"));
        continue;
      }
      const kj* st = kj_get(a, "step");
      o->source = k_strdup(kj_str(kj_get(st, "source")) ? kj_str(kj_get(st, "source")) : "");
      o->position = k_strdup(kj_str(kj_get(st, "position")) ? kj_str(kj_get(st, "position")) : "");
      const kj* d = kj_get(st, "data");
      if (kj_is_str(d)) k_b64_decode(d->s, d->slen, &o->data, &o->len);
      const kj* an = kj_get(a, "answers");
      load_list(&o->clock, kj_get(an, "clock"), 1);
      load_list(&o->rand, kj_get(an, "rand"), 0);
      load_list(&o->gateway, kj_get(an, "gateway"), 0);
      load_list(&o->config, kj_get(an, "config"), 0);
    }
  }
  kj_free(root);
  return c;
}

void case_free(case_t* c) {
  if (!c) return;
  free(c->service);
  free(c->snapshot);
  for (size_t i = 0; i < c->n_flags; i++) {
    free(c->flags[i].key);
    free(c->flags[i].value);
  }
  free(c->flags);
  for (size_t i = 0; i < c->n_actions; i++) {
    case_action* a = &c->actions[i];
    free(a->source);
    free(a->position);
    free(a->data);
    free_list(&a->clock);
    free_list(&a->rand);
    free_list(&a->gateway);
    free_list(&a->config);
  }
  free(c->actions);
  free(c);
}

int case_check_result(const char* path, const char* transport) {
  size_t n;
  char* text = slurp(path, &n);
  if (!text) {
    fprintf(stderr, "no result file %s (the fake recorder did not finish)\n", path);
    return 1;
  }
  char err[96];
  kj* r = kj_parse(text, n, err, sizeof err);
  free(text);
  if (!r) {
    fprintf(stderr, "unreadable result file: %s\n", err);
    return 1;
  }
  int pass = kj_is_true(kj_get(r, "pass"));
  if (!pass) {
    const char* e = kj_str(kj_get(r, "error"));
    fprintf(stderr, "case failed: %s\n", e ? e : "(no error)");
  }
  const char* got = kj_str(kj_get(r, "transport"));
  if (pass && (!got || strcmp(got, transport) != 0)) {
    fprintf(stderr, "case ran over the %s, expected the %s\n", got ? got : "(unknown transport)", transport);
    pass = 0;
  }
  kj_free(r);
  return pass ? 0 : 1;
}

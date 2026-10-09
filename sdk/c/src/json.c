#include "json.h"

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define MAX_DEPTH 64

typedef struct {
  const char* s;
  size_t n, i;
  char* err;
  size_t errcap;
} P;

static int fail(P* p, const char* msg) {
  if (p->err && p->errcap && p->err[0] == 0) snprintf(p->err, p->errcap, "%s at offset %zu", msg, p->i);
  return 0;
}

static void ws(P* p) {
  while (p->i < p->n && (p->s[p->i] == ' ' || p->s[p->i] == '\t' || p->s[p->i] == '\n' || p->s[p->i] == '\r')) p->i++;
}

static int hex4(P* p, uint32_t* out) {
  if (p->i + 4 > p->n) return fail(p, "short \\u escape");
  uint32_t v = 0;
  for (int k = 0; k < 4; k++) {
    char c = p->s[p->i + (size_t)k];
    v <<= 4;
    if (c >= '0' && c <= '9') v |= (uint32_t)(c - '0');
    else if (c >= 'a' && c <= 'f') v |= (uint32_t)(c - 'a' + 10);
    else if (c >= 'A' && c <= 'F') v |= (uint32_t)(c - 'A' + 10);
    else return fail(p, "bad \\u escape");
  }
  p->i += 4;
  *out = v;
  return 1;
}

static void utf8(char* o, size_t* w, uint32_t cp) {
  if (cp < 0x80) {
    o[(*w)++] = (char)cp;
  } else if (cp < 0x800) {
    o[(*w)++] = (char)(0xC0 | (cp >> 6));
    o[(*w)++] = (char)(0x80 | (cp & 0x3F));
  } else if (cp < 0x10000) {
    o[(*w)++] = (char)(0xE0 | (cp >> 12));
    o[(*w)++] = (char)(0x80 | ((cp >> 6) & 0x3F));
    o[(*w)++] = (char)(0x80 | (cp & 0x3F));
  } else {
    o[(*w)++] = (char)(0xF0 | (cp >> 18));
    o[(*w)++] = (char)(0x80 | ((cp >> 12) & 0x3F));
    o[(*w)++] = (char)(0x80 | ((cp >> 6) & 0x3F));
    o[(*w)++] = (char)(0x80 | (cp & 0x3F));
  }
}

/* p->i is just after the opening quote. */
static int parse_string(P* p, char** out, size_t* outlen) {
  size_t start = p->i;
  /* The decoded form is never longer than the encoded form. */
  size_t j = start;
  while (j < p->n && p->s[j] != '"') {
    if (p->s[j] == '\\') j++;
    j++;
  }
  if (j >= p->n) return fail(p, "unterminated string");
  char* o = malloc(j - start + 1);
  if (!o) return fail(p, "out of memory");
  size_t w = 0;
  while (p->i < p->n) {
    unsigned char c = (unsigned char)p->s[p->i];
    if (c == '"') {
      p->i++;
      o[w] = 0;
      *out = o;
      *outlen = w;
      return 1;
    }
    if (c < 0x20) {
      free(o);
      return fail(p, "control character in string");
    }
    if (c != '\\') {
      o[w++] = (char)c;
      p->i++;
      continue;
    }
    p->i++;
    if (p->i >= p->n) break;
    char e = p->s[p->i++];
    switch (e) {
      case '"': o[w++] = '"'; break;
      case '\\': o[w++] = '\\'; break;
      case '/': o[w++] = '/'; break;
      case 'b': o[w++] = '\b'; break;
      case 'f': o[w++] = '\f'; break;
      case 'n': o[w++] = '\n'; break;
      case 'r': o[w++] = '\r'; break;
      case 't': o[w++] = '\t'; break;
      case 'u': {
        uint32_t cp;
        if (!hex4(p, &cp)) {
          free(o);
          return 0;
        }
        if (cp >= 0xD800 && cp < 0xDC00) {
          uint32_t lo;
          if (p->i + 2 > p->n || p->s[p->i] != '\\' || p->s[p->i + 1] != 'u') {
            free(o);
            return fail(p, "lone high surrogate");
          }
          p->i += 2;
          if (!hex4(p, &lo)) {
            free(o);
            return 0;
          }
          if (lo < 0xDC00 || lo > 0xDFFF) {
            free(o);
            return fail(p, "bad surrogate pair");
          }
          cp = 0x10000 + ((cp - 0xD800) << 10) + (lo - 0xDC00);
        } else if (cp >= 0xDC00 && cp <= 0xDFFF) {
          free(o);
          return fail(p, "lone low surrogate");
        }
        utf8(o, &w, cp);
        break;
      }
      default:
        free(o);
        return fail(p, "bad escape");
    }
  }
  free(o);
  return fail(p, "unterminated string");
}

static int parse_value(P* p, kj* out, int depth);
static void release(kj* v);

static int parse_number(P* p, kj* out) {
  size_t st = p->i;
  if (p->i < p->n && p->s[p->i] == '-') p->i++;
  if (p->i >= p->n) return fail(p, "bad number");
  if (p->s[p->i] == '0') {
    p->i++;
  } else if (p->s[p->i] >= '1' && p->s[p->i] <= '9') {
    while (p->i < p->n && p->s[p->i] >= '0' && p->s[p->i] <= '9') p->i++;
  } else {
    return fail(p, "bad number");
  }
  if (p->i < p->n && p->s[p->i] == '.') {
    p->i++;
    size_t d = p->i;
    while (p->i < p->n && p->s[p->i] >= '0' && p->s[p->i] <= '9') p->i++;
    if (p->i == d) return fail(p, "bad fraction");
  }
  if (p->i < p->n && (p->s[p->i] == 'e' || p->s[p->i] == 'E')) {
    p->i++;
    if (p->i < p->n && (p->s[p->i] == '+' || p->s[p->i] == '-')) p->i++;
    size_t d = p->i;
    while (p->i < p->n && p->s[p->i] >= '0' && p->s[p->i] <= '9') p->i++;
    if (p->i == d) return fail(p, "bad exponent");
  }
  char tmp[64];
  size_t len = p->i - st;
  if (len >= sizeof tmp) return fail(p, "number too long");
  memcpy(tmp, p->s + st, len);
  tmp[len] = 0;
  out->type = KJ_NUM;
  out->num = strtod(tmp, NULL);
  return 1;
}

static int lit(P* p, const char* word) {
  size_t l = strlen(word);
  if (p->i + l > p->n || memcmp(p->s + p->i, word, l) != 0) return fail(p, "bad literal");
  p->i += l;
  return 1;
}

static int parse_value(P* p, kj* out, int depth) {
  memset(out, 0, sizeof *out);
  if (depth > MAX_DEPTH) return fail(p, "nesting too deep");
  ws(p);
  if (p->i >= p->n) return fail(p, "unexpected end");
  size_t st = p->i;
  char c = p->s[p->i];
  int ok;
  if (c == '{') {
    p->i++;
    out->type = KJ_OBJ;
    size_t cap = 0;
    ws(p);
    if (p->i < p->n && p->s[p->i] == '}') {
      p->i++;
      ok = 1;
    } else {
      ok = 0;
      for (;;) {
        ws(p);
        if (p->i >= p->n || p->s[p->i] != '"') {
          fail(p, "expected member name");
          break;
        }
        p->i++;
        char* key;
        size_t klen;
        if (!parse_string(p, &key, &klen)) break;
        if (strlen(key) != klen) {
          free(key);
          fail(p, "NUL in member name");
          break;
        }
        ws(p);
        if (p->i >= p->n || p->s[p->i] != ':') {
          free(key);
          fail(p, "expected ':'");
          break;
        }
        p->i++;
        if (out->n == cap) {
          size_t nc = cap ? cap * 2 : 4;
          kj* nv = realloc(out->v, nc * sizeof *nv);
          char** nk = realloc(out->keys, nc * sizeof *nk);
          if (nv) out->v = nv;
          if (nk) out->keys = nk;
          if (!nv || !nk) {
            free(key);
            fail(p, "out of memory");
            break;
          }
          cap = nc;
        }
        out->keys[out->n] = key;
        if (!parse_value(p, &out->v[out->n], depth + 1)) {
          free(key);
          break;
        }
        out->n++;
        ws(p);
        if (p->i < p->n && p->s[p->i] == ',') {
          p->i++;
          continue;
        }
        if (p->i < p->n && p->s[p->i] == '}') {
          p->i++;
          ok = 1;
        } else {
          fail(p, "expected ',' or '}'");
        }
        break;
      }
    }
  } else if (c == '[') {
    p->i++;
    out->type = KJ_ARR;
    size_t cap = 0;
    ws(p);
    if (p->i < p->n && p->s[p->i] == ']') {
      p->i++;
      ok = 1;
    } else {
      ok = 0;
      for (;;) {
        if (out->n == cap) {
          size_t nc = cap ? cap * 2 : 4;
          kj* nv = realloc(out->v, nc * sizeof *nv);
          if (!nv) {
            fail(p, "out of memory");
            break;
          }
          out->v = nv;
          cap = nc;
        }
        if (!parse_value(p, &out->v[out->n], depth + 1)) break;
        out->n++;
        ws(p);
        if (p->i < p->n && p->s[p->i] == ',') {
          p->i++;
          continue;
        }
        if (p->i < p->n && p->s[p->i] == ']') {
          p->i++;
          ok = 1;
        } else {
          fail(p, "expected ',' or ']'");
        }
        break;
      }
    }
  } else if (c == '"') {
    p->i++;
    out->type = KJ_STR;
    ok = parse_string(p, &out->s, &out->slen);
  } else if (c == 't') {
    out->type = KJ_BOOL;
    out->b = 1;
    ok = lit(p, "true");
  } else if (c == 'f') {
    out->type = KJ_BOOL;
    ok = lit(p, "false");
  } else if (c == 'n') {
    out->type = KJ_NULL;
    ok = lit(p, "null");
  } else {
    ok = parse_number(p, out);
  }
  if (!ok) {
    release(out);
    memset(out, 0, sizeof *out);
    return 0;
  }
  out->raw = p->s + st;
  out->rawlen = p->i - st;
  return 1;
}

kj* kj_parse(const char* s, size_t n, char* err, size_t errcap) {
  P p = {s, n, 0, err, errcap};
  if (err && errcap) err[0] = 0;
  kj* v = malloc(sizeof *v);
  if (!v) {
    if (err && errcap) snprintf(err, errcap, "out of memory");
    return NULL;
  }
  if (!parse_value(&p, v, 0)) {
    free(v);
    return NULL;
  }
  ws(&p);
  if (p.i != n) {
    fail(&p, "trailing characters");
    kj_free(v);
    return NULL;
  }
  return v;
}

static void release(kj* v) {
  free(v->s);
  for (size_t i = 0; i < v->n; i++) {
    release(&v->v[i]);
    if (v->keys) free(v->keys[i]);
  }
  free(v->v);
  free(v->keys);
}

void kj_free(kj* v) {
  if (!v) return;
  release(v);
  free(v);
}

const kj* kj_get(const kj* obj, const char* key) {
  if (!obj || obj->type != KJ_OBJ) return NULL;
  for (size_t i = obj->n; i-- > 0;)
    if (strcmp(obj->keys[i], key) == 0) return &obj->v[i];
  return NULL;
}

const char* kj_str(const kj* v) { return v && v->type == KJ_STR ? v->s : NULL; }
int kj_is_str(const kj* v) { return v && v->type == KJ_STR; }
int kj_is_num(const kj* v) { return v && v->type == KJ_NUM; }
int kj_is_true(const kj* v) { return v && v->type == KJ_BOOL && v->b; }

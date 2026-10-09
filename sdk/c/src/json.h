/* A small strict JSON parser (RFC 8259) for the host protocol, the recorder's
 * control stream and the conformance programs. Internal: not public API. */
#ifndef KAVACH_JSON_H
#define KAVACH_JSON_H

#include <stddef.h>

typedef enum { KJ_NULL, KJ_BOOL, KJ_NUM, KJ_STR, KJ_ARR, KJ_OBJ } kj_type;

typedef struct kj {
  kj_type type;
  int b;           /* KJ_BOOL */
  double num;      /* KJ_NUM */
  char* s;         /* KJ_STR: decoded, NUL-terminated (may contain NULs; see slen) */
  size_t slen;
  size_t n;        /* KJ_ARR / KJ_OBJ: element count */
  struct kj* v;    /* elements / member values */
  char** keys;     /* KJ_OBJ: member names */
  const char* raw; /* the value's text inside the parsed input (valid while it lives) */
  size_t rawlen;
} kj;

/* Parses exactly one JSON value spanning all of s (surrounding whitespace is
 * allowed). Returns NULL and writes a message to err on failure. */
kj* kj_parse(const char* s, size_t n, char* err, size_t errcap);
void kj_free(kj* v);

/* Object member lookup (the last member with that name); NULL if absent or not an object. */
const kj* kj_get(const kj* obj, const char* key);
/* Typed accessors with defaults. */
const char* kj_str(const kj* v);  /* NULL unless v is a string */
int kj_is_str(const kj* v);
int kj_is_num(const kj* v);
int kj_is_true(const kj* v);

#endif

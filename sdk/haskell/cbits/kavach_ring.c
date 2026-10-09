#include <stdatomic.h>
#include <stdint.h>

/* The ring's write and read words (SPEC.md section 10.7). GHC has no portable
   acquire load or release store on raw memory, so these are C11. */
uint64_t kavach_load_acquire(const uint64_t *p) {
  return atomic_load_explicit((const _Atomic uint64_t *)p, memory_order_acquire);
}

void kavach_store_release(uint64_t *p, uint64_t v) {
  atomic_store_explicit((_Atomic uint64_t *)p, v, memory_order_release);
}

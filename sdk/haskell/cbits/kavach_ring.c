#include <stdatomic.h>
#include <stddef.h>
#include <stdint.h>
#include <sys/mman.h>

/* Maps the file's header and data area, then the data area again right after
   it, inside one reserved region so nothing else can land between them
   (SPEC.md section 10.7). Returns NULL on failure. */
void *kavach_ring_map(int fd, size_t header, size_t cap) {
  size_t len = header + cap;
  char *mem = mmap(NULL, len + cap, PROT_NONE, MAP_PRIVATE | MAP_ANON, -1, 0);
  if (mem == MAP_FAILED) return NULL;
  int prot = PROT_READ | PROT_WRITE, flags = MAP_SHARED | MAP_FIXED;
  if (mmap(mem, len, prot, flags, fd, 0) == MAP_FAILED ||
      mmap(mem + len, cap, prot, flags, fd, (off_t)header) == MAP_FAILED) {
    munmap(mem, len + cap);
    return NULL;
  }
  return mem;
}

/* The ring's write and read words (SPEC.md section 10.7). GHC has no portable
   acquire load or release store on raw memory, so these are C11. */
uint64_t kavach_load_acquire(const uint64_t *p) {
  return atomic_load_explicit((const _Atomic uint64_t *)p, memory_order_acquire);
}

void kavach_store_release(uint64_t *p, uint64_t v) {
  atomic_store_explicit((_Atomic uint64_t *)p, v, memory_order_release);
}

#include <time.h>

/* Unix nanoseconds; cheaper than getPOSIXTime's Fixed arithmetic. */
int64_t kavach_now_nanos(void) {
  struct timespec ts;
  clock_gettime(CLOCK_REALTIME, &ts);
  return (int64_t)ts.tv_sec * 1000000000 + ts.tv_nsec;
}

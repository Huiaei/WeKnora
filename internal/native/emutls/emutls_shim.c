/* emutls runtime shim for mingw-w64.
 *
 * The official duckdb static libraries (lib/windows-amd64 in
 * github.com/duckdb/duckdb-go-bindings) were built with GCC 14.2.0
 * (MinGW-Builds, posix-seh), where thread_local variables were compiled
 * to the "emutls" model: accesses go through __emutls_get_address()
 * with a per-variable control block named __emutls_v.<mangled-name>.
 *
 * mingw-w64 removed the emutls runtime after v11 (newer toolchains use
 * the _tls_index / TlsAlloc model instead), so the current toolchain no
 * longer provides these symbols. This file reimplements the runtime,
 * with the exact layout and semantics reverse-engineered from the
 * emutls implementation that ships inside the official duckdb
 * extension DLLs:
 *
 *   struct __emutls { size_t size; size_t align; long long index; void *value; }
 *
 * - index is assigned lazily, on first use, from a process-wide counter.
 * - Each thread gets a table (indexed by index) via pthread_getspecific.
 * - The per-variable storage is malloc'd on first access, zero-initialised
 *   (or copied from value), with the malloc base stored at data[-1] so
 *   the pthread key destructor can free it.
 *
 * It also provides __once_proxy, which libstdc++'s std::call_once
 * protocol uses. The protocol (verified by disassembling the call_once
 * instantiation inside the official libraries):
 *
 * - __once_callable (TLS) holds a pointer to the invocation data
 * - __once_call     (TLS) holds the thunk that invokes the callable
 * - the caller passes __once_proxy to pthread_once; the proxy reads
 *   __once_call and calls the thunk; the thunk reads __once_callable,
 *   dereferences it and tail-jumps to the callable; after pthread_once
 *   returns the caller resets both slots to 0.
 */

#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <pthread.h>

typedef struct {
  size_t size; /* 0x00 - size of the TLS variable */
  size_t align; /* 0x08 - alignment of the TLS variable */
  long long index; /* 0x10 - per-process index, assigned on first use */
  void *value;  /* 0x18 - initial value, or NULL for zero-init */
} __emutls;

static long long emutls_size = 0;
static pthread_key_t emutls_key;
static pthread_once_t emutls_once = PTHREAD_ONCE_INIT;
static pthread_mutex_t emutls_mutex;

static void emutls_destroy(void *table_) {
  void **table = table_;
  long long hwm = (long long)table[0];
  for (long i = 1; i <= hwm; i++) {
    void *p = table[i];
    if (p)
      free(((void **)p)[-1]);
  }
  free(table);
}

static void emutls_init(void) {
  pthread_mutex_init(&emutls_mutex, NULL);
  if (pthread_key_create(&emutls_key, emutls_destroy) != 0)
    abort();
}

void *__emutls_get_address(__emutls *p) {
  long long idx = p->index;
  if (idx == 0) {
    pthread_once(&emutls_once, emutls_init);
    pthread_mutex_lock(&emutls_mutex);
    idx = p->index;
    if (idx == 0) {
      idx = ++emutls_size;
      p->index = idx;
    }
    pthread_mutex_unlock(&emutls_mutex);
  }

  void **table = pthread_getspecific(emutls_key);
  if (table == NULL) {
    table = calloc(idx + 0x21, 8);
    if (table == NULL)
      abort();
    table[0] = (void *)(idx + 0x20);
    pthread_setspecific(emutls_key, table);
  }
  if ((long long)table[0] < idx) {
    long long hwm = (long long)table[0];
    long long newhwm = hwm * 2;
    if (newhwm < idx + 0x20)
      newhwm = idx + 0x20;
    table = realloc(table, (newhwm + 1) * 8);
    if (table == NULL)
      abort();
    memset(&table[hwm + 1], 0, (newhwm - hwm) * 8);
    table[0] = (void *)newhwm;
    pthread_setspecific(emutls_key, table);
  }

  void *data = table[idx];
  if (data == NULL) {
    size_t size = p->size;
    size_t align = p->align;
    void *base;
    if (align > 8) {
      base = malloc(size + align + 7);
      if (base == NULL)
        abort();
      data = (void *)(((uintptr_t)base + align + 7) & ~(uintptr_t)(align - 1));
    } else {
      base = malloc(size + 8);
      if (base == NULL)
        abort();
      data = (char *)base + 8;
    }
    ((void **)data)[-1] = base;
    if (p->value)
      memcpy(data, p->value, size);
    else
      memset(data, 0, size);
    table[idx] = data;
  }
  return data;
}

/* libstdc++'s call_once proxy: call the thunk whose pointer is stored in
   the thread-local __once_call slot. */
extern __emutls __emutls_v__ZSt11__once_call asm("__emutls_v._ZSt11__once_call");

void __once_proxy(void) {
  void (**thunk)(void) = __emutls_get_address(&__emutls_v__ZSt11__once_call);
  (*thunk)();
}

/* Control blocks for the two libstdc++ thread_locals referenced by the
   official duckdb libraries (std::__once_call and std::__once_callable).
   Both are 8 bytes in libstdc++ and zero-initialised. */
__emutls __emutls_v__ZSt11__once_call asm("__emutls_v._ZSt11__once_call") = {8, 8, 0, 0};
__emutls __emutls_v__ZSt15__once_callable asm("__emutls_v._ZSt15__once_callable") = {8, 8, 0, 0};

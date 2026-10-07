// Package emutls provides the mingw-w64 "emutls" runtime that the
// official prebuilt duckdb static libraries (windows/amd64) require at
// link time.
//
// Those libraries were built with GCC 14.2.0 (MinGW-Builds, posix-seh),
// where thread_local variables are compiled to the emutls model: every
// access goes through __emutls_get_address() with a per-variable control
// block named __emutls_v.<mangled-name>. mingw-w64 removed the emutls
// runtime after v11 - current toolchains (GCC 16, LLVM-MinGW) use the
// _tls_index / TlsAlloc model - so the symbols are undefined when linking
// the official libraries with a modern cross-compiler.
//
// emutls_shim.c (compiled by cgo together with this package)
// reimplements the removed runtime and the libstdc++ __once_proxy
// thunk, with semantics reverse-engineered from the emutls
// implementation shipped inside the official duckdb extension DLLs.
//
// Import this package (blank import) from the desktop entry point so
// the shim lands on the link line ahead of the duckdb archives.
package emutls

/*
#include <stdlib.h>
*/
import "C"

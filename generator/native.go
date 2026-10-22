package generator

/*
#cgo LDFLAGS: -L${SRCDIR}/../native/target/release -lsteel_bridge
#cgo darwin LDFLAGS: -framework Security -framework CoreFoundation -framework SystemConfiguration -liconv
#cgo linux LDFLAGS: -ldl -lm -lpthread
#include "../native/steel.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

// nativeContext owns a Rust handle. Generator's lock guards its lifetime.
type nativeContext struct{ handle *C.SteelContext }

// openNative acquires the process's shared Steel context for this seed.
func openNative(seed int64, workers, cacheChunks int, deterministic bool) (*nativeContext, error) {
	var mode C.uint8_t
	if deterministic {
		mode = 1
	}
	var result C.SteelBuffer
	handle := C.steel_context_new(C.int64_t(seed), C.uint32_t(workers), C.uint32_t(cacheChunks), mode, &result)
	defer C.steel_buffer_free(result)
	if result.failed != 0 {
		return nil, errors.New(string(unsafe.Slice((*byte)(unsafe.Pointer(result.data)), int(result.len))))
	}
	return &nativeContext{handle: handle}, nil
}

// generate copies an owned Rust result before releasing the native allocation.
func (n *nativeContext) generate(x, z int32) ([]byte, error) {
	result := C.steel_generate_chunk(n.handle, C.int32_t(x), C.int32_t(z))
	defer C.steel_buffer_free(result)
	data := append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(result.data)), int(result.len))...)
	if result.failed != 0 {
		return nil, errors.New(string(data))
	}
	return data, nil
}

// close releases this handle after all its generation calls have finished.
func (n *nativeContext) close() { C.steel_context_free(n.handle) }

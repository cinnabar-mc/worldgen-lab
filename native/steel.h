#ifndef STEEL_BRIDGE_H
#define STEEL_BRIDGE_H
#include <stdint.h>
#include <stddef.h>
/* Opaque, shared per-seed context. Never free it while generation is in flight. */
typedef struct SteelContext SteelContext;
/* All returned bytes belong to Rust and must be released exactly once. */
typedef struct { uint8_t *data; size_t len; uint8_t failed; } SteelBuffer;
SteelContext *steel_context_new(int64_t seed, uint32_t workers, uint32_t cache_chunks, SteelBuffer *error);
SteelBuffer steel_generate_chunk(const SteelContext *context, int32_t cx, int32_t cz);
void steel_context_free(SteelContext *context);
void steel_buffer_free(SteelBuffer buffer);
#endif

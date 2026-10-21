//! SteelMC generation exposed through an owned-buffer C interface.
mod encoding;
mod host;
use host::Context;
use rustc_hash::FxHashMap as HashMap;
use std::panic::{AssertUnwindSafe, catch_unwind};
use std::sync::{Arc, LazyLock, Mutex, Weak};

static CONTEXTS: LazyLock<Mutex<HashMap<i64, Weak<Context>>>> = LazyLock::new(Mutex::default);
/// Rust-owned bytes; callers must release them once using steel_buffer_free.
#[repr(C)]
pub struct Buffer {
    pub data: *mut u8,
    pub len: usize,
    pub failed: u8,
}

impl Buffer {
    /// Transfers a byte slice to the C caller.
    fn new(data: Vec<u8>, failed: bool) -> Self {
        let mut bytes = data.into_boxed_slice();
        let result = Self {
            data: bytes.as_mut_ptr(),
            len: bytes.len(),
            failed: u8::from(failed),
        };
        std::mem::forget(bytes);
        result
    }
}

/// Contains a panic inside Rust and returns its text as an error buffer.
fn guarded(f: impl FnOnce() -> Vec<u8>) -> Buffer {
    match catch_unwind(AssertUnwindSafe(f)) {
        Ok(bytes) => Buffer::new(bytes, false),
        Err(error) => Buffer::new(host::panic_message(error).into_bytes(), true),
    }
}

/// Acquires a shared context for a seed. The error output must point to writable Buffer storage.
///
/// # Safety
/// `error` must be valid for one Buffer write. Free the returned handle once, after all calls finish.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn steel_context_new(
    seed: i64,
    workers: u32,
    cache_chunks: u32,
    error: *mut Buffer,
) -> *mut Arc<Context> {
    let mut handle = std::ptr::null_mut();
    let result = guarded(|| {
        let workers = workers as usize;
        let cache_chunks = cache_chunks as usize;
        assert!(
            (1..=256).contains(&workers),
            "native workers must be 1..256"
        );
        assert!(
            (1..=4096).contains(&cache_chunks),
            "cache chunks must be 1..4096"
        );
        let mut contexts = CONTEXTS.lock().expect("context registry lock");
        contexts.retain(|_, value| value.strong_count() != 0);
        let context = if let Some(context) = contexts.get(&seed).and_then(Weak::upgrade) {
            if context.workers != workers || context.cache_chunks != cache_chunks {
                drop(contexts);
                panic!("seed already open with different generation settings");
            }
            context
        } else {
            let context = Arc::new(Context::new(seed, workers, cache_chunks));
            contexts.insert(seed, Arc::downgrade(&context));
            context
        };
        handle = Box::into_raw(Box::new(context));
        Vec::new()
    });
    unsafe {
        error.write(result);
    }
    handle
}

/// Generates one chunk using a live context; concurrent calls may share the same handle.
///
/// # Safety
/// `handle` must come from steel_context_new and stay alive until the call returns.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn steel_generate_chunk(
    handle: *const Arc<Context>,
    cx: i32,
    cz: i32,
) -> Buffer {
    guarded(|| unsafe { (&*handle).generate(cx, cz) })
}

/// Releases one context handle after its final generation call.
///
/// # Safety
/// `handle` must be a live handle from steel_context_new and must be freed only once.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn steel_context_free(handle: *mut Arc<Context>) {
    unsafe {
        drop(Box::from_raw(handle));
    }
}

/// Releases a buffer allocated by this library, including error buffers.
///
/// # Safety
/// The buffer must have been returned by this library and must be freed only once.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn steel_buffer_free(buffer: Buffer) {
    unsafe {
        drop(Box::from_raw(std::ptr::slice_from_raw_parts_mut(
            buffer.data,
            buffer.len,
        )));
    }
}

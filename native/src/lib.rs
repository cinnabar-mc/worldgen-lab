//! SteelMC generation exposed through an owned-buffer C interface.

use std::collections::HashMap;
use std::panic::{AssertUnwindSafe, catch_unwind};
use std::sync::{Arc, LazyLock, Mutex, Weak};
use std::time::{Duration, Instant};

use steel_core::bootstrap::init_globals;
use steel_core::chunk::chunk_request::ChunkTicketKind;
use steel_core::chunk::status::ChunkStatus;
use steel_core::entity::damage::DamageHistory;
use steel_core::level_data::{GameTimeSource, WorldGenerationSettings};
use steel_core::world::{World, WorldConfig, WorldStorageConfig};
use steel_core::worldgen::ChunkGeneratorType;
use steel_core::worldgen::generator::context::OverworldGenerator;
use steel_registry::blocks::block_state_ext::BlockStateExt;
use steel_registry::{REGISTRY, RegistryExt, vanilla_dimension_types};
use steel_utils::types::{Difficulty, GameType};
use steel_utils::{BlockStateId, ChunkPos, Identifier};
use steel_worldgen::biomes::BiomeSourceKind;
use tokio::runtime::Runtime;

static RUNTIME: LazyLock<Arc<Runtime>> = LazyLock::new(|| {
    Arc::new(
        tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .expect("runtime"),
    )
});
static POOL: LazyLock<Arc<rayon::ThreadPool>> = LazyLock::new(|| {
    Arc::new(
        rayon::ThreadPoolBuilder::new()
            .num_threads(4)
            .stack_size(16 * 1024 * 1024)
            .build()
            .expect("generation pool"),
    )
});
static CONTEXTS: LazyLock<Mutex<HashMap<i64, Weak<Context>>>> = LazyLock::new(Mutex::default);

/// One world and scheduler per seed, shared by all live handles for that seed.
pub struct Context {
    world: Arc<World>,
    scheduler: Mutex<()>,
    _damage_history: Arc<DamageHistory>,
}

impl Context {
    /// Creates an in-memory Steel world without opening a network listener or ticking gameplay.
    fn new(seed: i64) -> Self {
        init_globals();
        let generator = Arc::new(ChunkGeneratorType::Overworld(OverworldGenerator::new(
            None,
            BiomeSourceKind::overworld(seed as u64),
            seed as u64,
            &POOL,
        )));
        let damage_history = Arc::new(DamageHistory::default());
        let dimension = &vanilla_dimension_types::OVERWORLD;
        let config = WorldConfig {
            damage_history: Arc::clone(&damage_history),
            game_time_source: GameTimeSource::Primary,
            storage: WorldStorageConfig::RamOnly,
            level_data_path: None,
            generator,
            generation_settings: WorldGenerationSettings::from_generator_config(
                Identifier::vanilla_static("overworld"),
                &toml::Value::Table(Default::default()),
                dimension.key.clone(),
                -64,
                384,
            ),
            view_distance: 2,
            simulation_distance: 2,
            max_chained_neighbor_updates: 1_000_000,
            compression: None,
            is_flat: false,
            sea_level: 63,
            default_gamemode: GameType::Survival,
            difficulty: Difficulty::Normal,
        };
        let world = RUNTIME
            .block_on(World::new_with_config(
                Arc::clone(&RUNTIME),
                Identifier::vanilla_static("overworld"),
                dimension,
                seed,
                config,
                Arc::clone(&POOL),
            ))
            .expect("create Steel world");
        Self {
            world,
            scheduler: Mutex::new(()),
            _damage_history: damage_history,
        }
    }

    /// Runs the upstream dependency pyramid through Full so neighboring decorations are complete.
    fn generate(&self, cx: i32, cz: i32) -> Vec<u8> {
        assert!(
            cx.abs_diff(0) <= 1_800_000 && cz.abs_diff(0) <= 1_800_000,
            "chunk outside supported world border"
        );
        let map = &self.world.chunk_map;
        let request = map.request_chunk(
            ChunkPos::new(cx, cz),
            ChunkStatus::Full,
            ChunkTicketKind::Pregen,
        );
        let start = Instant::now();
        loop {
            {
                let _lock = self.scheduler.lock().expect("scheduler lock");
                // This upstream entry point advances only chunk work, without random ticks or gameplay.
                map.flint_advance_scheduling();
            }
            if let Some(ready) = request.ready_chunks() {
                let chunk = ready.holders[0]
                    .try_chunk(ChunkStatus::Full)
                    .expect("ready chunk");
                let result = encode(chunk);
                drop(request);
                let _lock = self.scheduler.lock().expect("scheduler lock");
                map.flint_advance_scheduling();
                return result;
            }
            assert!(
                start.elapsed() < Duration::from_secs(120),
                "Steel chunk generation timed out"
            );
            std::thread::sleep(Duration::from_millis(1));
        }
    }
}

impl Drop for Context {
    /// Stops and drains this world's background work before releasing its memory.
    fn drop(&mut self) {
        let map = &self.world.chunk_map;
        map.stop_generation_refill_loop();
        map.cancel_token.cancel();
        map.task_tracker.close();
        RUNTIME.block_on(map.task_tracker.wait());
    }
}

/// Writes a length-prefixed UTF-8 string to the bridge stream.
fn string(out: &mut Vec<u8>, value: &str) {
    let len = u16::try_from(value.len()).expect("state name too long");
    out.extend_from_slice(&len.to_le_bytes());
    out.extend_from_slice(value.as_bytes());
}

/// Formats a Java state by name and sorted properties, independently of registry numbering.
fn state_key(id: BlockStateId) -> String {
    let block = id.get_block();
    let mut key = block.key.to_string();
    let mut properties = REGISTRY.blocks.get_properties(id);
    properties.sort_unstable();
    if !properties.is_empty() {
        key.push('[');
        key.push_str(
            &properties
                .into_iter()
                .map(|(k, v)| format!("{k}={v}"))
                .collect::<Vec<_>>()
                .join(","),
        );
        key.push(']');
    }
    key
}

/// Exports section palettes, x-fastest block indices and 4×4×4 biome indices.
fn encode(chunk: &steel_core::chunk::Chunk) -> Vec<u8> {
    let mut out = Vec::with_capacity(210_000);
    out.extend_from_slice(b"SWG1");
    out.extend_from_slice(&(-64_i16).to_le_bytes());
    out.extend_from_slice(&24_u16.to_le_bytes());
    for holder in &chunk.sections().sections {
        let section = holder.read();
        let mut palette = Vec::new();
        let mut lookup = HashMap::new();
        let mut indices = Vec::with_capacity(4096);
        for y in 0..16 {
            for z in 0..16 {
                for x in 0..16 {
                    let id = section.states.get(x, y, z);
                    let n = *lookup.entry(id.0).or_insert_with(|| {
                        palette.push(id);
                        (palette.len() - 1) as u16
                    });
                    indices.push(n);
                }
            }
        }
        out.extend_from_slice(&(palette.len() as u16).to_le_bytes());
        for id in palette {
            string(&mut out, &state_key(id));
            string(&mut out, &state_key(id.get_block().default_state()));
            // Bit 1 also covers intrinsically water-filled plants such as kelp.
            let water = steel_registry::fluid::is_water_fluid(id.get_fluid_state().fluid_id)
                && id.get_block().key != steel_registry::vanilla_blocks::WATER.key;
            out.push(u8::from(id.is_solid()) | (u8::from(water) << 1));
        }
        for index in indices {
            out.extend_from_slice(&index.to_le_bytes());
        }
        let mut palette = Vec::new();
        let mut lookup = HashMap::new();
        let mut indices = Vec::with_capacity(64);
        for y in 0..4 {
            for z in 0..4 {
                for x in 0..4 {
                    let id = section.biomes.get(x, y, z);
                    let n = *lookup.entry(id).or_insert_with(|| {
                        palette.push(id);
                        (palette.len() - 1) as u16
                    });
                    indices.push(n);
                }
            }
        }
        out.extend_from_slice(&(palette.len() as u16).to_le_bytes());
        for id in palette {
            string(
                &mut out,
                &REGISTRY
                    .biomes
                    .by_id(usize::from(id))
                    .expect("biome")
                    .key
                    .to_string(),
            );
        }
        for index in indices {
            out.extend_from_slice(&index.to_le_bytes());
        }
    }
    out
}

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
        Err(error) => {
            let message = error
                .downcast_ref::<String>()
                .map(String::as_str)
                .or_else(|| error.downcast_ref::<&str>().copied())
                .unwrap_or("Steel generation panicked");
            Buffer::new(message.as_bytes().to_vec(), true)
        }
    }
}

/// Acquires a shared context for a seed. The error output must point to writable Buffer storage.
///
/// # Safety
/// `error` must be valid for one Buffer write. Free the returned handle once, after all calls finish.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn steel_context_new(seed: i64, error: *mut Buffer) -> *mut Arc<Context> {
    let mut handle = std::ptr::null_mut();
    let result = guarded(|| {
        let context = POOL.install(|| {
            let mut contexts = CONTEXTS.lock().expect("context registry lock");
            contexts.retain(|_, value| value.strong_count() != 0);
            if let Some(context) = contexts.get(&seed).and_then(Weak::upgrade) {
                return context;
            }
            let context = Arc::new(Context::new(seed));
            contexts.insert(seed, Arc::downgrade(&context));
            context
        });
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

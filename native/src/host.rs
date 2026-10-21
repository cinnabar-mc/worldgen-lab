//! A bounded per-seed generation host with event-driven chunk completion.
use crate::encoding::DESCRIPTORS;
use crate::region;
use rustc_hash::FxHashMap;
use std::collections::VecDeque;
use std::sync::{Arc, LazyLock, Mutex, mpsc};
use std::thread::JoinHandle;
use std::time::Duration;
use steel_core::bootstrap::init_globals;
use steel_core::entity::damage::DamageHistory;
use steel_core::level_data::{GameTimeSource, WorldGenerationSettings};
use steel_core::world::{World, WorldConfig, WorldStorageConfig};
use steel_core::worldgen::ChunkGeneratorType;
use steel_core::worldgen::generator::context::OverworldGenerator;
use steel_registry::vanilla_dimension_types;
use steel_utils::Identifier;
use steel_utils::types::{Difficulty, GameType};
use steel_worldgen::biomes::BiomeSourceKind;
use tokio::{runtime::Runtime, sync::mpsc as queue};

static RUNTIME: LazyLock<Arc<Runtime>> = LazyLock::new(|| {
    Arc::new(
        tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .expect("runtime"),
    )
});

/// A bounded request queue and one per-seed host; generation itself runs on Rayon.
pub struct Context {
    pub(crate) workers: usize,
    pub(crate) cache_chunks: usize,
    jobs: queue::Sender<Option<Job>>,
    host: Mutex<Option<JoinHandle<()>>>,
}

/// One requested chunk and the channel that wakes its synchronous cgo caller.
type ChunkBytes = Result<Arc<[u8]>, String>;

struct Job {
    pos: (i32, i32),
    reply: mpsc::SyncSender<ChunkBytes>,
}

impl Context {
    /// Starts a bounded host and waits for initialization before publishing the handle.
    pub(crate) fn new(seed: i64, workers: usize, cache_chunks: usize) -> Self {
        let (jobs, receiver) = queue::channel(workers * 2);
        let (ready, initialized) = mpsc::sync_channel(1);
        let host = std::thread::Builder::new()
            .name("steel-host".into())
            .spawn(move || {
                let pool = Arc::new(
                    rayon::ThreadPoolBuilder::new()
                        .num_threads(workers)
                        .stack_size(16 * 1024 * 1024)
                        .build()
                        .expect("generation pool"),
                );
                let generator = pool.install(|| create_generator(seed, &pool));
                let damage_history = Arc::new(DamageHistory::default());
                if ready.send(()).is_err() {
                    return;
                }
                RUNTIME.block_on(serve(
                    seed,
                    generator,
                    pool,
                    damage_history,
                    receiver,
                    cache_chunks,
                ));
            })
            .expect("host thread");
        initialized.recv().expect("Steel host initialization");
        Self {
            workers,
            cache_chunks,
            jobs,
            host: Mutex::new(Some(host)),
        }
    }

    /// Waits on a completion channel; callers never poll or lock the scheduler.
    pub(crate) fn generate(&self, cx: i32, cz: i32) -> Vec<u8> {
        assert!(
            cx.abs_diff(0) <= 1_800_000 && cz.abs_diff(0) <= 1_800_000,
            "chunk outside supported world border"
        );
        let (reply, result) = mpsc::sync_channel(1);
        self.jobs
            .blocking_send(Some(Job {
                pos: (cx, cz),
                reply,
            }))
            .expect("Steel host stopped");
        result
            .recv_timeout(Duration::from_secs(120))
            .expect("Steel chunk generation timed out")
            .unwrap_or_else(|error| panic!("{error}"))
            .to_vec()
    }
}

impl Drop for Context {
    /// Drains the owning host after the final handle and all cgo calls have finished.
    fn drop(&mut self) {
        let _ = self.jobs.blocking_send(None);
        if let Some(host) = self.host.lock().expect("host lock").take() {
            let _ = host.join();
        }
    }
}

/// Serves a bounded cache from fixed regions, independently of caller arrival order.
async fn serve(
    seed: i64,
    generator: Arc<ChunkGeneratorType>,
    pool: Arc<rayon::ThreadPool>,
    damage_history: Arc<DamageHistory>,
    mut jobs: queue::Receiver<Option<Job>>,
    cache_chunks: usize,
) {
    let mut cache: FxHashMap<(i32, i32), Arc<[u8]>> = FxHashMap::default();
    let mut order = VecDeque::new();
    let mut regions: VecDeque<((i32, i32), region::Outputs)> = VecDeque::new();
    while let Some(Some(job)) = jobs.recv().await {
        if let Some(bytes) = cache.get(&job.pos) {
            let _ = job.reply.send(Ok(Arc::clone(bytes)));
            continue;
        }
        let key = (job.pos.0.div_euclid(region::WIDTH), job.pos.1);
        if !regions.iter().any(|(current, _)| *current == key) {
            match region::generate(seed, key, Arc::clone(&generator), &damage_history, &pool).await
            {
                Ok(outputs) => {
                    regions.push_back((key, outputs));
                    while regions.len() > 2 {
                        regions.pop_front();
                    }
                }
                Err(error) => {
                    let _ = job.reply.send(Err(error));
                    continue;
                }
            }
        }
        let Some((_, outputs)) = regions.iter().find(|(current, _)| *current == key) else {
            unreachable!("generated region");
        };
        let bytes = Arc::clone(&outputs[job.pos.0.rem_euclid(region::WIDTH) as usize]);
        cache.insert(job.pos, Arc::clone(&bytes));
        order.push_back(job.pos);
        while cache.len() > cache_chunks {
            if let Some(oldest) = order.pop_front() {
                cache.remove(&oldest);
            }
        }
        let _ = job.reply.send(Ok(bytes));
    }
}

/// Initializes immutable seeded generator data once for the owning context.
fn create_generator(seed: i64, pool: &Arc<rayon::ThreadPool>) -> Arc<ChunkGeneratorType> {
    init_globals();
    LazyLock::force(&DESCRIPTORS);
    Arc::new(ChunkGeneratorType::Overworld(OverworldGenerator::new(
        None,
        BiomeSourceKind::overworld(seed as u64),
        seed as u64,
        pool,
    )))
}

/// Opens a disposable world using the existing immutable, seeded generator.
pub(crate) async fn open_world(
    seed: i64,
    generator: Arc<ChunkGeneratorType>,
    damage_history: &Arc<DamageHistory>,
    pool: &Arc<rayon::ThreadPool>,
) -> Arc<World> {
    let dimension = &vanilla_dimension_types::OVERWORLD;
    let config = WorldConfig {
        damage_history: Arc::clone(damage_history),
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
    World::new_with_config(
        Arc::clone(&RUNTIME),
        Identifier::vanilla_static("overworld"),
        dimension,
        seed,
        config,
        Arc::clone(pool),
    )
    .await
    .expect("create Steel world")
}

/// Drains generation and unload jobs before discarding a bounded generation region.
pub(crate) async fn close_world(world: &Arc<World>) {
    let map = &world.chunk_map;
    map.stop_generation_refill_loop();
    map.cancel_token.cancel();
    map.task_tracker.close();
    map.task_tracker.wait().await;
}

/// Converts a host panic into an error returned through the C interface.
pub(crate) fn panic_message(error: Box<dyn std::any::Any + Send>) -> String {
    error
        .downcast_ref::<String>()
        .cloned()
        .or_else(|| {
            error
                .downcast_ref::<&str>()
                .map(|message| (*message).to_owned())
        })
        .unwrap_or_else(|| "Steel generation panicked".to_owned())
}

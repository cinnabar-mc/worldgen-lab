//! A bounded per-seed generation host with event-driven chunk completion.
use crate::encoding::{DESCRIPTORS, encode};
use futures::{StreamExt, stream::FuturesUnordered};
use rustc_hash::FxHashMap;
use std::collections::VecDeque;
use std::panic::{AssertUnwindSafe, catch_unwind};
use std::sync::{Arc, LazyLock, Mutex, mpsc};
use std::thread::JoinHandle;
use std::time::Duration;
use steel_core::bootstrap::init_globals;
use steel_core::chunk::chunk_request::{ChunkRequest, ChunkTicketKind};
use steel_core::chunk::status::ChunkStatus;
use steel_core::chunk_saver::ChunkStorage;
use steel_core::entity::damage::DamageHistory;
use steel_core::level_data::{GameTimeSource, WorldGenerationSettings};
use steel_core::world::{World, WorldConfig, WorldStorageConfig};
use steel_core::worldgen::ChunkGeneratorType;
use steel_core::worldgen::generator::context::OverworldGenerator;
use steel_registry::vanilla_dimension_types;
use steel_utils::types::{Difficulty, GameType};
use steel_utils::{ChunkPos, Identifier};
use steel_worldgen::biomes::BiomeSourceKind;
use tokio::{
    runtime::Runtime,
    sync::{mpsc as queue, oneshot},
};

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
                let (world, damage_history) = pool.install(|| create_world(seed, &pool));
                RUNTIME.block_on(async {
                    if let ChunkStorage::RamOnly(storage) = world.chunk_map.storage.as_ref() {
                        storage.set_cache_capacity(cache_chunks).await;
                    }
                });
                if ready.send(()).is_err() {
                    return;
                }
                RUNTIME.block_on(serve(
                    seed,
                    world,
                    pool,
                    damage_history,
                    receiver,
                    workers * 2,
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

/// Batches queued tickets, pipelines their futures, and replies as each chunk finishes.
async fn serve(
    seed: i64,
    mut world: Arc<World>,
    pool: Arc<rayon::ThreadPool>,
    damage_history: Arc<DamageHistory>,
    mut jobs: queue::Receiver<Option<Job>>,
    capacity: usize,
    cache_chunks: usize,
) {
    let mut pending = FuturesUnordered::new();
    let mut cache: FxHashMap<(i32, i32), Arc<[u8]>> = FxHashMap::default();
    let mut order = VecDeque::new();
    let mut epoch_chunks = 0;
    // Bound auxiliary world indexes as well as explicitly configured chunk caches.
    const EPOCH_CHUNKS: usize = 1024;
    loop {
        if epoch_chunks >= EPOCH_CHUNKS && pending.is_empty() {
            let generator = Arc::clone(&world.chunk_map.world_gen_context.generator);
            close_world(&world).await;
            world = open_world(seed, generator, &damage_history, &pool).await;
            if let ChunkStorage::RamOnly(storage) = world.chunk_map.storage.as_ref() {
                storage.set_cache_capacity(cache_chunks).await;
            }
            epoch_chunks = 0;
        }
        let map = Arc::clone(&world.chunk_map);
        tokio::select! {
            job=jobs.recv(), if pending.len()<capacity && epoch_chunks<EPOCH_CHUNKS => {
                let Some(Some(first))=job else { break; };
                if let Some(bytes)=cache.get(&first.pos) {
                    let _=first.reply.send(Ok(Arc::clone(bytes)));
                    continue;
                }
                let mut batch=vec![first];
                while batch.len()+pending.len()<capacity {
                    match jobs.try_recv() { Ok(Some(job))=>batch.push(job), _=>break }
                }
                let positions=batch.iter().map(|job|ChunkPos::new(job.pos.0,job.pos.1)).collect();
                let request=Arc::new(map.request_chunks(ChunkRequest{positions,status:ChunkStatus::Full,ticket_kind:ChunkTicketKind::Pregen}));
                map.advance_scheduling();
                let holders=request.chunk_holders().expect("committed chunk request");
                // Requests may contain duplicate positions, so resolve by coordinate rather than zip order.
                for job in batch {
                    let holder=Arc::clone(holders.iter().find(|holder|holder.get_pos()==ChunkPos::new(job.pos.0,job.pos.1)).expect("requested holder"));
                    pending.push(complete(job,holder,Arc::clone(&request),Arc::clone(&pool),Arc::clone(&map)));
                }
            }
            Some((job,result))=pending.next(), if !pending.is_empty() => {
                epoch_chunks+=1;
                // Full publication is reconciled before the caller receives the encoded chunk.
                map.advance_scheduling();
                if let Ok(bytes)=&result {
                    if !cache.contains_key(&job.pos) { order.push_back(job.pos); }
                    cache.insert(job.pos,Arc::clone(bytes));
                    while cache.len()>cache_chunks {
                        if let Some(oldest)=order.pop_front() { cache.remove(&oldest); }
                    }
                }
                let _=job.reply.send(result);
            }
        }
    }
    close_world(&world).await;
}

/// Awaits the upstream status notification and encodes on Rayon without blocking the host.
async fn complete(
    job: Job,
    holder: Arc<steel_core::chunk::chunk_holder::ChunkHolder>,
    request: Arc<steel_core::chunk::chunk_request::ChunkRequestHandle>,
    pool: Arc<rayon::ThreadPool>,
    map: Arc<steel_core::chunk::chunk_map::ChunkMap>,
) -> (Job, ChunkBytes) {
    let ready = tokio::time::timeout(
        Duration::from_secs(120),
        holder.await_chunk_status(ChunkStatus::Full),
    )
    .await;
    let result = match ready {
        Ok(Some(_)) => {
            map.advance_scheduling();
            let (sender, receiver) = oneshot::channel();
            pool.spawn(move || {
                let result = catch_unwind(AssertUnwindSafe(|| {
                    encode(holder.try_chunk(ChunkStatus::Full).expect("ready chunk"))
                }))
                .map(Arc::<[u8]>::from)
                .map_err(panic_message);
                let _ = sender.send(result);
            });
            receiver
                .await
                .unwrap_or_else(|_| Err("Steel encoder stopped".to_owned()))
        }
        _ => Err("Steel chunk generation timed out or was cancelled".to_owned()),
    };
    drop(request);
    (job, result)
}

/// Creates an ephemeral Steel world sharing only immutable registry and descriptor data.
fn create_world(seed: i64, pool: &Arc<rayon::ThreadPool>) -> (Arc<World>, Arc<DamageHistory>) {
    init_globals();
    LazyLock::force(&DESCRIPTORS);
    let generator = Arc::new(ChunkGeneratorType::Overworld(OverworldGenerator::new(
        None,
        BiomeSourceKind::overworld(seed as u64),
        seed as u64,
        pool,
    )));
    let damage_history = Arc::new(DamageHistory::default());
    let world = RUNTIME.block_on(open_world(seed, generator, &damage_history, pool));
    (world, damage_history)
}

/// Opens a disposable world using the existing immutable, seeded generator.
async fn open_world(
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

/// Drains generation and unload jobs before discarding a bounded world epoch.
async fn close_world(world: &Arc<World>) {
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
